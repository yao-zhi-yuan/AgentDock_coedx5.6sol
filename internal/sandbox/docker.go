package sandbox

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/agentdock/agentdock-verify/internal/policy"
)

const TestImage = "agentdock-sandbox:phase4"

const sanitizedGitPointer = "gitdir: /agentdock/not-mounted\n"

const containerReinspectAttempts = 5

type DockerConfig struct {
	Image          string
	WorktreeRoot   string
	CPU            string
	Memory         string
	PIDs           int
	CommandTimeout time.Duration
	OutputLimit    int
	EnvAllowlist   []string
	Audit          policy.AuditRecorder
}

type DockerProvider struct {
	config    DockerConfig
	mu        sync.Mutex
	counter   atomic.Uint64
	owners    sync.Map // owner token -> Scope, stored before external side effects
	pending   sync.Map // owner token -> *dockerSandbox until Destroy audit succeeds
	worktrees *GitWorktreeProvider
}

func NewDockerProvider(config DockerConfig) *DockerProvider {
	if config.Image == "" {
		config.Image = TestImage
	}
	if config.CPU == "" {
		config.CPU = "1"
	}
	if config.Memory == "" {
		config.Memory = "512m"
	}
	if config.PIDs == 0 {
		config.PIDs = 128
	}
	if config.CommandTimeout == 0 {
		config.CommandTimeout = 60 * time.Second
	}
	if config.OutputLimit == 0 {
		config.OutputLimit = 1 << 20
	}
	return &DockerProvider{
		config:    config,
		worktrees: NewGitWorktreeProvider(config.WorktreeRoot),
	}
}

func (provider *DockerProvider) Create(ctx context.Context, spec Spec) (Sandbox, error) {
	provider.mu.Lock()
	defer provider.mu.Unlock()
	if provider.config.Audit == nil {
		return nil, errors.New("phase-4 audit recorder is required")
	}
	if err := validateDockerConfig(provider.config); err != nil {
		return nil, provider.recordCreateFailed(spec, "configuration validation", "", err)
	}
	immutableImage, err := resolveImage(ctx, provider.config.Image)
	if err != nil {
		return nil, provider.recordCreateFailed(spec, "immutable image resolution", "", err)
	}
	ownerToken, err := newOwnerToken()
	if err != nil {
		return nil, provider.recordCreateFailed(spec, "owner token generation", immutableImage, err)
	}
	provider.owners.Store(ownerToken, Scope{RunID: spec.RunID, AttemptID: spec.AttemptID})
	config := provider.config
	config.Image = immutableImage
	worktree, err := provider.worktrees.Create(ctx, spec)
	if err != nil {
		if worktree == nil {
			provider.owners.Delete(ownerToken)
			return nil, provider.recordCreateFailed(spec, "disposable worktree creation", immutableImage, err)
		}
		instance := provider.newPendingSandbox(config, spec, worktree, ownerToken)
		return instance, provider.recordCreateFailed(spec, "disposable worktree creation", immutableImage, err)
	}
	instance := provider.newPendingSandbox(config, spec, worktree, ownerToken)
	if err := prepareWorkspacePermissions(worktree.Workspace()); err != nil {
		return provider.failProvisioning(instance, "workspace permission setup", err)
	}
	gitPointer, err := hideWorktreeGitPointer(worktree.Workspace())
	if err != nil {
		return provider.failProvisioning(instance, "Git pointer sanitization", err)
	}
	instance.gitPointer = gitPointer
	instance.gitPointerSanitized = true
	if err := instance.recordCreate(); err != nil {
		return provider.failProvisioning(instance, "sandbox_created audit", err)
	}
	instance.provisioningFailed = false
	return instance, nil
}

func (provider *DockerProvider) newPendingSandbox(
	config DockerConfig,
	spec Spec,
	worktree Sandbox,
	ownerToken string,
) *dockerSandbox {
	instance := &dockerSandbox{
		provider:            provider,
		config:              config,
		spec:                spec,
		worktree:            worktree,
		ownerToken:          ownerToken,
		containers:          make(map[string]string),
		uncertainContainers: make(map[string]time.Time),
		provisioningFailed:  true,
	}
	provider.pending.Store(ownerToken, instance)
	return instance
}

func (provider *DockerProvider) failProvisioning(
	instance *dockerSandbox,
	stage string,
	cause error,
) (Sandbox, error) {
	cleanupErr := instance.Destroy(context.Background())
	reported := provider.recordCreateFailed(
		instance.spec,
		stage,
		instance.config.Image,
		errors.Join(cause, cleanupErr),
	)
	if cleanupErr == nil {
		return nil, reported
	}
	return instance, reported
}

// Cleanup retries every Sandbox still owned by this Provider. DockerProvider
// wraps every non-nil worktree Create result in a pending dockerSandbox before
// returning it. Worktrees must therefore be cleaned only by their owning
// Sandbox, after its container set converges; independently draining the
// worktree Provider could remove a workspace beneath a late-visible container.
func (provider *DockerProvider) Cleanup(ctx context.Context) error {
	provider.mu.Lock()
	defer provider.mu.Unlock()
	var cleanupErr error
	provider.pending.Range(func(_, value any) bool {
		cleanupErr = errors.Join(cleanupErr, value.(*dockerSandbox).Destroy(ctx))
		return true
	})
	return cleanupErr
}

func validateDockerConfig(config DockerConfig) error {
	cpu, err := strconv.ParseFloat(config.CPU, 64)
	if err != nil || cpu <= 0 || math.IsInf(cpu, 0) || math.IsNaN(cpu) {
		return fmt.Errorf("Docker CPU limit %q must be a positive finite number", config.CPU)
	}
	memory := strings.ToLower(config.Memory)
	if memory == "" {
		return errors.New("Docker memory limit is required")
	}
	number := memory
	last := memory[len(memory)-1]
	if last == 'b' || last == 'k' || last == 'm' || last == 'g' {
		number = memory[:len(memory)-1]
	}
	value, err := strconv.ParseUint(number, 10, 64)
	if err != nil || value == 0 {
		return fmt.Errorf("Docker memory limit %q must be a positive integer with optional b/k/m/g suffix", config.Memory)
	}
	if config.PIDs <= 0 {
		return fmt.Errorf("Docker PID limit must be positive, got %d", config.PIDs)
	}
	if config.CommandTimeout <= 0 {
		return fmt.Errorf("Docker command timeout must be positive, got %s", config.CommandTimeout)
	}
	if config.OutputLimit <= 0 {
		return fmt.Errorf("Docker output limit must be positive, got %d", config.OutputLimit)
	}
	return nil
}

func (provider *DockerProvider) recordCreateFailed(
	spec Spec,
	stage string,
	immutableImage string,
	cause error,
) error {
	auditErr := policy.RecordBounded(provider.config.Audit, policy.AuditEvent{
		Kind:      policy.AuditSandboxCreateFailed,
		RunID:     spec.RunID,
		AttemptID: spec.AttemptID,
		Reason:    "sandbox provisioning failed during " + stage,
		ImageID:   immutableImage,
		CPU:       provider.config.CPU,
		Memory:    provider.config.Memory,
		PIDs:      provider.config.PIDs,
	})
	if auditErr != nil {
		auditErr = fmt.Errorf("persist sandbox-create-failure audit: %w", auditErr)
	}
	return errors.Join(cause, auditErr)
}

func newOwnerToken() (string, error) {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", fmt.Errorf("generate sandbox owner token: %w", err)
	}
	return hex.EncodeToString(value[:]), nil
}

func hideWorktreeGitPointer(workspace string) ([]byte, error) {
	gitPointer := filepath.Join(workspace, ".git")
	info, err := os.Lstat(gitPointer)
	if err != nil {
		return nil, fmt.Errorf("inspect disposable worktree Git pointer: %w", err)
	}
	if !info.Mode().IsRegular() {
		return nil, errors.New("disposable worktree .git pointer is not a regular file")
	}
	original, err := os.ReadFile(gitPointer)
	if err != nil {
		return nil, fmt.Errorf("read disposable worktree Git pointer: %w", err)
	}
	rollback := func(cause error) ([]byte, error) {
		writeErr := os.WriteFile(gitPointer, original, info.Mode().Perm())
		chmodErr := os.Chmod(gitPointer, info.Mode().Perm())
		return nil, errors.Join(cause, writeErr, chmodErr)
	}
	if err := os.WriteFile(gitPointer, []byte(sanitizedGitPointer), 0o444); err != nil {
		return nil, fmt.Errorf("sanitize disposable worktree Git pointer: %w", err)
	}
	if err := os.Chmod(gitPointer, 0o444); err != nil {
		return rollback(fmt.Errorf("protect disposable worktree Git pointer: %w", err))
	}
	rootInfo, err := os.Stat(workspace)
	if err != nil {
		return rollback(fmt.Errorf("stat disposable worktree root: %w", err))
	}
	// The private 0700 parent root prevents unrelated host users from reaching
	// this disposable directory. Do not use the sticky bit here: the sandbox
	// UID must be able to atomically replace host-owned top-level files. The
	// sanitized .git pointer is protected separately by a read-only bind mount.
	if err := os.Chmod(workspace, rootInfo.Mode().Perm()|0o007); err != nil {
		return rollback(fmt.Errorf("protect disposable worktree root: %w", err))
	}
	return original, nil
}

func restoreWorktreeGitPointer(workspace string, original []byte) error {
	gitPointer := filepath.Join(workspace, ".git")
	if err := os.Chmod(gitPointer, 0o600); err != nil {
		return fmt.Errorf("make disposable worktree Git pointer restorable: %w", err)
	}
	if err := os.WriteFile(gitPointer, original, 0o600); err != nil {
		return fmt.Errorf("restore disposable worktree Git pointer: %w", err)
	}
	return nil
}

func rollbackWorktreeAfterCreateFailure(ctx context.Context, worktree Sandbox, original []byte) error {
	restoreErr := restoreWorktreeGitPointer(worktree.Workspace(), original)
	destroyErr := worktree.Destroy(ctx)
	var sanitizeErr error
	if destroyErr != nil {
		_, sanitizeErr = hideWorktreeGitPointer(worktree.Workspace())
	}
	return errors.Join(restoreErr, destroyErr, sanitizeErr)
}

func resolveImage(ctx context.Context, reference string) (string, error) {
	command := exec.CommandContext(
		ctx,
		"docker",
		"image",
		"inspect",
		"--format",
		"{{.Id}}",
		reference,
	)
	output, err := command.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("resolve sandbox image %q: %w: %s", reference, err, strings.TrimSpace(string(output)))
	}
	imageID := strings.TrimSpace(string(output))
	if !strings.HasPrefix(imageID, "sha256:") || len(imageID) != len("sha256:")+64 {
		return "", fmt.Errorf("sandbox image %q resolved to invalid ID %q", reference, imageID)
	}
	return imageID, nil
}

func (provider *DockerProvider) ActiveContainers(ctx context.Context) []string {
	command := exec.CommandContext(
		ctx,
		"docker",
		"ps",
		"-a",
		"--no-trunc",
		"--filter",
		"label=agentdock.phase=4",
		"--format",
		"{{.Label \"agentdock.owner_token\"}}\t{{.Label \"agentdock.run_id\"}}\t{{.Label \"agentdock.attempt_id\"}}\t{{.Names}}",
	)
	output, err := command.Output()
	if err != nil {
		return []string{"<docker-query-failed>"}
	}
	var containers []string
	for _, line := range strings.Split(string(output), "\n") {
		fields := strings.SplitN(strings.TrimSpace(line), "\t", 4)
		if len(fields) != 4 {
			continue
		}
		ownedScope, ok := provider.owners.Load(fields[0])
		if !ok {
			continue
		}
		scope := ownedScope.(Scope)
		if scope.RunID == fields[1] && scope.AttemptID == fields[2] && fields[3] != "" {
			containers = append(containers, fields[3])
		}
	}
	sort.Strings(containers)
	return containers
}

type dockerSandbox struct {
	mu                  sync.Mutex
	provider            *DockerProvider
	config              DockerConfig
	spec                Spec
	worktree            Sandbox
	gitPointer          []byte
	gitPointerSanitized bool
	ownerToken          string
	containers          map[string]string
	uncertainContainers map[string]time.Time
	provisioningFailed  bool
	destroying          bool
	destroyed           bool
	destroyAudited      bool

	// removeOwnedContainerOverride is an internal failure-injection seam. It is
	// nil in production and lets integration tests prove cleanup errors are
	// returned, audited, and retained for a later Destroy retry.
	removeOwnedContainerOverride func(string) (bool, error)
	cleanupWorkspaceOverride     func(context.Context) error
	createContainerOverride      func(context.Context, string, Request) (string, error)
	inspectContainerOverride     func(string) (string, bool, bool, error)
	scanOwnedContainersOverride  func() error
	containerReinspectDelay      time.Duration
}

func (sandbox *dockerSandbox) Workspace() string {
	return sandbox.worktree.Workspace()
}

func (sandbox *dockerSandbox) Scope() Scope {
	return Scope{RunID: sandbox.spec.RunID, AttemptID: sandbox.spec.AttemptID}
}

func (sandbox *dockerSandbox) Execute(ctx context.Context, request Request) (Result, error) {
	sandbox.mu.Lock()
	defer sandbox.mu.Unlock()
	if sandbox.destroyed || sandbox.destroying || sandbox.provisioningFailed {
		return Result{}, sandbox.recordDenied(request, ErrDestroyed)
	}
	if err := validateContainerCommand(request.Command); err != nil {
		return Result{}, sandbox.recordDenied(request, err)
	}
	if err := sandbox.validateEnvironment(request.Environment); err != nil {
		return Result{}, sandbox.recordDenied(request, err)
	}
	timeout := request.Timeout
	if timeout <= 0 || timeout > sandbox.config.CommandTimeout {
		timeout = sandbox.config.CommandTimeout
	}
	limit := request.OutputLimit
	if limit <= 0 || limit > sandbox.config.OutputLimit {
		limit = sandbox.config.OutputLimit
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return sandbox.interruptedBeforeStart(request, timeout, limit, ctxErr, nil)
	}
	name := sandbox.nextContainerName("exec")
	sandbox.containers[name] = ""
	containerID, err := sandbox.createContainer(ctx, name, request)
	if err != nil {
		sandbox.markContainerCreateUnknown(name)
		cleanupErr := sandbox.cleanupFailedCreate(name)
		if ctxErr := ctx.Err(); ctxErr != nil {
			return sandbox.interruptedBeforeStart(
				request,
				timeout,
				limit,
				ctxErr,
				errors.Join(err, cleanupErr),
			)
		}
		auditErr := policy.RecordBounded(sandbox.config.Audit, sandbox.executionAuditEvent(
			policy.AuditExecutionFailed,
			request,
			timeout,
			limit,
			"Docker container creation failed",
		))
		return Result{}, errors.Join(err, cleanupErr, auditErr)
	}
	sandbox.containers[name] = containerID
	delete(sandbox.uncertainContainers, name)

	runContext, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	budget := &outputBudget{limit: limit}
	stdout := &limitedBuffer{budget: budget}
	stderr := &limitedBuffer{budget: budget}
	startErr := sandbox.startOwnedContainer(
		runContext,
		containerID,
		bytes.NewReader(request.Stdin),
		stdout,
		stderr,
	)
	result := Result{
		Stdout:    stdout.Bytes(),
		Stderr:    stderr.Bytes(),
		ExitCode:  exitCode(startErr),
		Truncated: budget.truncated,
	}
	outputBytes := budget.total
	if errors.Is(runContext.Err(), context.DeadlineExceeded) {
		result.TimedOut = true
		killErr := sandbox.killTrackedContainer(name)
		removeErr := sandbox.removeTrackedContainer(name)
		auditErr := policy.RecordBounded(sandbox.config.Audit, policy.AuditEvent{
			Kind:            policy.AuditExecutionTimedOut,
			RunID:           sandbox.spec.RunID,
			AttemptID:       sandbox.spec.AttemptID,
			ToolName:        request.ToolName,
			ContractVersion: request.ContractVersion,
			Reason:          "command deadline exceeded; owned container termination was requested",
			OutputBytes:     outputBytes,
			TimeoutMillis:   timeout.Milliseconds(),
			OutputLimit:     limit,
			ImageID:         sandbox.config.Image,
			CPU:             sandbox.config.CPU,
			Memory:          sandbox.config.Memory,
			PIDs:            sandbox.config.PIDs,
		})
		if result.Truncated {
			auditErr = errors.Join(auditErr, sandbox.recordTruncation(request, outputBytes, limit))
		}
		if removeErr != nil {
			auditErr = errors.Join(auditErr, sandbox.recordContainerCleanupFailure(request, timeout, limit))
		}
		return result, errors.Join(ErrTimeout, killErr, removeErr, auditErr)
	}
	if errors.Is(runContext.Err(), context.Canceled) {
		killErr := sandbox.killTrackedContainer(name)
		removeErr := sandbox.removeTrackedContainer(name)
		auditErr := policy.RecordBounded(sandbox.config.Audit, policy.AuditEvent{
			Kind:            policy.AuditExecutionCanceled,
			RunID:           sandbox.spec.RunID,
			AttemptID:       sandbox.spec.AttemptID,
			ToolName:        request.ToolName,
			ContractVersion: request.ContractVersion,
			Reason:          "caller canceled command; owned container termination was requested",
			OutputBytes:     outputBytes,
			TimeoutMillis:   timeout.Milliseconds(),
			OutputLimit:     limit,
			ImageID:         sandbox.config.Image,
			CPU:             sandbox.config.CPU,
			Memory:          sandbox.config.Memory,
			PIDs:            sandbox.config.PIDs,
		})
		if result.Truncated {
			auditErr = errors.Join(auditErr, sandbox.recordTruncation(request, outputBytes, limit))
		}
		if removeErr != nil {
			auditErr = errors.Join(auditErr, sandbox.recordContainerCleanupFailure(request, timeout, limit))
		}
		return result, errors.Join(context.Canceled, killErr, removeErr, auditErr)
	}
	removeErr := sandbox.removeTrackedContainer(name)
	var truncationErr error
	if result.Truncated {
		if err := sandbox.recordTruncation(request, outputBytes, limit); err != nil {
			truncationErr = fmt.Errorf("persist output-truncation audit: %w", err)
		}
	}
	if startErr != nil {
		exitErr := &ExitError{Code: result.ExitCode}
		executionErr := errors.Join(exitErr, startErr)
		auditErr := policy.RecordBounded(sandbox.config.Audit, policy.AuditEvent{
			Kind:            policy.AuditExecutionFailed,
			RunID:           sandbox.spec.RunID,
			AttemptID:       sandbox.spec.AttemptID,
			ToolName:        request.ToolName,
			ContractVersion: request.ContractVersion,
			ExitCode:        result.ExitCode,
			OutputBytes:     outputBytes,
			Reason:          "container command returned non-zero",
			TimeoutMillis:   timeout.Milliseconds(),
			OutputLimit:     limit,
			ImageID:         sandbox.config.Image,
			CPU:             sandbox.config.CPU,
			Memory:          sandbox.config.Memory,
			PIDs:            sandbox.config.PIDs,
		})
		if removeErr != nil {
			auditErr = errors.Join(auditErr, sandbox.recordContainerCleanupFailure(request, timeout, limit))
		}
		if auditErr != nil {
			return result, errors.Join(
				executionErr,
				removeErr,
				truncationErr,
				fmt.Errorf("persist execution-failure audit: %w", auditErr),
			)
		}
		return result, errors.Join(executionErr, removeErr, truncationErr)
	}
	if removeErr != nil {
		auditErr := policy.RecordBounded(sandbox.config.Audit, sandbox.executionAuditEvent(
			policy.AuditExecutionFailed,
			request,
			timeout,
			limit,
			"container command completed but owned-container cleanup failed",
		))
		return result, errors.Join(removeErr, truncationErr, auditErr)
	}
	if truncationErr != nil {
		return result, truncationErr
	}
	if err := policy.RecordBounded(sandbox.config.Audit, policy.AuditEvent{
		Kind:            policy.AuditExecutionCompleted,
		RunID:           sandbox.spec.RunID,
		AttemptID:       sandbox.spec.AttemptID,
		ToolName:        request.ToolName,
		ContractVersion: request.ContractVersion,
		OutputBytes:     outputBytes,
		TimeoutMillis:   timeout.Milliseconds(),
		OutputLimit:     limit,
		ImageID:         sandbox.config.Image,
		CPU:             sandbox.config.CPU,
		Memory:          sandbox.config.Memory,
		PIDs:            sandbox.config.PIDs,
	}); err != nil {
		return result, fmt.Errorf("persist execution-completion audit: %w", err)
	}
	return result, nil
}

func (sandbox *dockerSandbox) recordContainerCleanupFailure(
	request Request,
	timeout time.Duration,
	outputLimit int,
) error {
	return policy.RecordBounded(sandbox.config.Audit, sandbox.executionAuditEvent(
		policy.AuditExecutionFailed,
		request,
		timeout,
		outputLimit,
		"owned-container cleanup failed; container remains tracked for Destroy retry",
	))
}

func (sandbox *dockerSandbox) interruptedBeforeStart(
	request Request,
	timeout time.Duration,
	outputLimit int,
	cause error,
	provisioningErr error,
) (Result, error) {
	kind := policy.AuditExecutionCanceled
	reason := "caller canceled before the owned container started"
	result := Result{}
	resultErr := context.Canceled
	if errors.Is(cause, context.DeadlineExceeded) {
		kind = policy.AuditExecutionTimedOut
		reason = "caller deadline expired before the owned container started"
		result.TimedOut = true
		resultErr = ErrTimeout
	}
	auditErr := policy.RecordBounded(sandbox.config.Audit, sandbox.executionAuditEvent(
		kind,
		request,
		timeout,
		outputLimit,
		reason,
	))
	return result, errors.Join(resultErr, provisioningErr, auditErr)
}

func (sandbox *dockerSandbox) executionAuditEvent(
	kind policy.AuditKind,
	request Request,
	timeout time.Duration,
	outputLimit int,
	reason string,
) policy.AuditEvent {
	return policy.AuditEvent{
		Kind:            kind,
		RunID:           sandbox.spec.RunID,
		AttemptID:       sandbox.spec.AttemptID,
		ToolName:        request.ToolName,
		ContractVersion: request.ContractVersion,
		Reason:          reason,
		TimeoutMillis:   timeout.Milliseconds(),
		OutputLimit:     outputLimit,
		ImageID:         sandbox.config.Image,
		CPU:             sandbox.config.CPU,
		Memory:          sandbox.config.Memory,
		PIDs:            sandbox.config.PIDs,
	}
}

func (sandbox *dockerSandbox) recordDenied(request Request, cause error) error {
	reason := "sandbox execution request rejected by fixed command or environment boundary"
	if errors.Is(cause, ErrDestroyed) {
		reason = "sandbox execution request rejected because Sandbox destruction started"
	}
	auditErr := policy.RecordBounded(sandbox.config.Audit, policy.AuditEvent{
		Kind:            policy.AuditPolicyDenied,
		RunID:           sandbox.spec.RunID,
		AttemptID:       sandbox.spec.AttemptID,
		ToolName:        request.ToolName,
		ContractVersion: request.ContractVersion,
		Reason:          reason,
		TimeoutMillis:   sandbox.config.CommandTimeout.Milliseconds(),
		OutputLimit:     sandbox.config.OutputLimit,
		ImageID:         sandbox.config.Image,
		CPU:             sandbox.config.CPU,
		Memory:          sandbox.config.Memory,
		PIDs:            sandbox.config.PIDs,
	})
	if auditErr != nil {
		auditErr = fmt.Errorf("persist sandbox denial audit: %w", auditErr)
	}
	return errors.Join(cause, auditErr)
}

func (sandbox *dockerSandbox) nextContainerName(purpose string) string {
	return sandbox.containerName(purpose, sandbox.provider.counter.Add(1))
}

func (sandbox *dockerSandbox) containerName(purpose string, sequence uint64) string {
	return fmt.Sprintf(
		"agentdock-p4-%s-%s-%s-%s-%d",
		safeName(sandbox.spec.RunID),
		safeName(sandbox.spec.AttemptID),
		sandbox.ownerToken[:12],
		purpose,
		sequence,
	)
}

func (sandbox *dockerSandbox) createContainer(
	_ context.Context,
	name string,
	request Request,
) (string, error) {
	// Docker create is a remote side effect. Do not bind it to the caller's
	// cancellation: wait on an independent bounded context, then classify a
	// local timeout as outcome-unknown and retain the pre-registered name.
	createContext, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if sandbox.createContainerOverride != nil {
		return sandbox.createContainerOverride(createContext, name, request)
	}
	arguments := []string{
		"create",
		"--interactive",
		"--name", name,
		"--label", "agentdock.phase=4",
		"--label", "agentdock.run_id=" + sandbox.spec.RunID,
		"--label", "agentdock.attempt_id=" + sandbox.spec.AttemptID,
		"--label", "agentdock.owner_token=" + sandbox.ownerToken,
		"--network", "none",
		"--ipc", "none",
		"--read-only",
		"--user", "65532:65532",
		"--cap-drop", "ALL",
		"--security-opt", "no-new-privileges",
		"--cpus", sandbox.config.CPU,
		"--memory", sandbox.config.Memory,
		"--pids-limit", strconv.Itoa(sandbox.config.PIDs),
		"--mount", "type=bind,src=" + sandbox.Workspace() + ",dst=/workspace",
		"--mount", "type=bind,src=" + filepath.Join(sandbox.Workspace(), ".git") + ",dst=/workspace/.git,readonly",
		"--workdir", "/workspace",
	}
	fixedEnvironment := map[string]string{
		"GOCACHE":     "/workspace/.agentdock/go-cache",
		"GOENV":       "off",
		"GOFLAGS":     "",
		"GOTOOLCHAIN": "local",
		"GOTMPDIR":    "/workspace/.agentdock/tmp",
		"HOME":        "/workspace/.agentdock/home",
		"TMPDIR":      "/workspace/.agentdock/tmp",
	}
	keys := make([]string, 0, len(fixedEnvironment)+len(request.Environment))
	for key := range fixedEnvironment {
		keys = append(keys, key)
	}
	for key := range request.Environment {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	seen := make(map[string]struct{}, len(keys))
	for _, key := range keys {
		if _, exists := seen[key]; exists {
			continue
		}
		seen[key] = struct{}{}
		value, ok := request.Environment[key]
		if !ok {
			value = fixedEnvironment[key]
		}
		arguments = append(arguments, "--env", key+"="+value)
	}
	arguments = append(arguments, sandbox.config.Image)
	arguments = append(arguments, request.Command...)
	command := exec.CommandContext(createContext, "docker", arguments...)
	output, err := command.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("create Docker sandbox container: %w: %s", err, strings.TrimSpace(string(output)))
	}
	containerID := strings.TrimSpace(string(output))
	if len(containerID) != 64 || !isObjectID(containerID) {
		return "", fmt.Errorf("Docker create returned invalid container ID %q", containerID)
	}
	ownedID, exists, owned, inspectErr := sandbox.inspectContainerContext(createContext, containerID)
	if inspectErr != nil {
		return "", inspectErr
	}
	if !exists || !owned || ownedID != containerID {
		return "", errors.New("created container does not match sandbox ownership labels")
	}
	return containerID, nil
}

func (sandbox *dockerSandbox) markContainerCreateUnknown(name string) {
	if sandbox.uncertainContainers == nil {
		sandbox.uncertainContainers = make(map[string]time.Time)
	}
	if _, tracked := sandbox.uncertainContainers[name]; !tracked {
		// The timestamp is diagnostic metadata for how long reconciliation has
		// remained unresolved. It is never a deadline for releasing ownership:
		// absence from one or more Docker snapshots does not prove that an
		// in-flight daemon create cannot become visible later.
		sandbox.uncertainContainers[name] = time.Now()
	}
}

func prepareWorkspacePermissions(workspace string) error {
	err := filepath.WalkDir(workspace, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		mode := info.Mode()
		if entry.IsDir() {
			mode |= 0o007
		} else if mode.IsRegular() {
			mode |= 0o006
		}
		return os.Chmod(path, mode)
	})
	if err != nil {
		return fmt.Errorf("prepare disposable worktree permissions: %w", err)
	}
	return nil
}

func (sandbox *dockerSandbox) validateEnvironment(environment map[string]string) error {
	allowed := make(map[string]struct{}, len(sandbox.config.EnvAllowlist))
	for _, key := range sandbox.config.EnvAllowlist {
		allowed[key] = struct{}{}
	}
	for key := range environment {
		switch key {
		case "GOCACHE", "GOENV", "GOFLAGS", "GOTOOLCHAIN", "GOTMPDIR", "HOME", "TMPDIR":
			return fmt.Errorf("%w: environment key %q is fixed by the sandbox", ErrCommandNotAllowed, key)
		}
		if _, ok := allowed[key]; !ok {
			return fmt.Errorf("%w: environment key %q", ErrCommandNotAllowed, key)
		}
	}
	return nil
}

func (sandbox *dockerSandbox) recordTruncation(request Request, outputBytes, outputLimit int) error {
	return policy.RecordBounded(sandbox.config.Audit, policy.AuditEvent{
		Kind:            policy.AuditOutputTruncated,
		RunID:           sandbox.spec.RunID,
		AttemptID:       sandbox.spec.AttemptID,
		ToolName:        request.ToolName,
		ContractVersion: request.ContractVersion,
		OutputBytes:     outputBytes,
		Reason:          "combined stdout and stderr exceeded configured limit",
		OutputLimit:     outputLimit,
		ImageID:         sandbox.config.Image,
		CPU:             sandbox.config.CPU,
		Memory:          sandbox.config.Memory,
		PIDs:            sandbox.config.PIDs,
	})
}

var (
	ErrContainerOwnership      = errors.New("container ownership labels do not match sandbox scope")
	ErrContainerOutcomeUnknown = errors.New("container create outcome is not yet settled")
)

type containerInspection struct {
	ID     string            `json:"id"`
	Labels map[string]string `json:"labels"`
}

func (sandbox *dockerSandbox) inspectContainer(reference string) (string, bool, bool, error) {
	if sandbox.inspectContainerOverride != nil {
		return sandbox.inspectContainerOverride(reference)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return sandbox.inspectContainerContext(ctx, reference)
}

func (sandbox *dockerSandbox) inspectContainerContext(
	ctx context.Context,
	reference string,
) (string, bool, bool, error) {
	output, err := exec.CommandContext(
		ctx,
		"docker",
		"inspect",
		"--format",
		`{"id":{{json .Id}},"labels":{{json .Config.Labels}}}`,
		reference,
	).CombinedOutput()
	if err != nil {
		if strings.Contains(string(output), "No such object") ||
			strings.Contains(string(output), "No such container") {
			return "", false, false, nil
		}
		return "", false, false, fmt.Errorf(
			"inspect container ownership: %w: %s",
			err,
			strings.TrimSpace(string(output)),
		)
	}
	var inspection containerInspection
	if err := json.Unmarshal(output, &inspection); err != nil {
		return "", false, false, fmt.Errorf("decode container ownership: %w", err)
	}
	if len(inspection.ID) != 64 || !isObjectID(inspection.ID) {
		return "", false, false, errors.New("Docker inspect returned invalid container ID")
	}
	return inspection.ID, true, sandbox.ownsLabels(inspection.Labels), nil
}

func (sandbox *dockerSandbox) ownsLabels(labels map[string]string) bool {
	return labels["agentdock.phase"] == "4" &&
		labels["agentdock.run_id"] == sandbox.spec.RunID &&
		labels["agentdock.attempt_id"] == sandbox.spec.AttemptID &&
		labels["agentdock.owner_token"] == sandbox.ownerToken
}

func (sandbox *dockerSandbox) startOwnedContainer(
	ctx context.Context,
	containerID string,
	stdin *bytes.Reader,
	stdout, stderr *limitedBuffer,
) error {
	// Ownership is a cleanup/lifecycle invariant, so resolve it with a short,
	// independent context. The caller may cancel while this check is in flight;
	// using that canceled context would make ownership unresolved and prevent the
	// subsequent kill/remove path from safely cleaning up this container.
	ownedID, exists, owned, err := sandbox.inspectContainer(containerID)
	if err != nil {
		return err
	}
	if !exists || !owned || ownedID != containerID {
		return ErrContainerOwnership
	}
	command := exec.CommandContext(ctx, "docker", "start", "-a", "-i", containerID)
	command.Stdin = stdin
	command.Stdout = stdout
	command.Stderr = stderr
	return command.Run()
}

func (sandbox *dockerSandbox) killTrackedContainer(name string) error {
	containerID, ok := sandbox.containers[name]
	if !ok || containerID == "" {
		return nil
	}
	ownedID, exists, owned, err := sandbox.inspectContainer(containerID)
	if err != nil {
		return err
	}
	if !exists {
		return nil
	}
	if !owned || ownedID != containerID {
		return ErrContainerOwnership
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	output, err := exec.CommandContext(ctx, "docker", "kill", containerID).CombinedOutput()
	if err != nil && !strings.Contains(string(output), "is not running") {
		return fmt.Errorf("kill owned container: %w: %s", err, strings.TrimSpace(string(output)))
	}
	return nil
}

func (sandbox *dockerSandbox) removeTrackedContainer(name string) error {
	containerID, ok := sandbox.containers[name]
	if !ok {
		return nil
	}
	if containerID == "" {
		ownedID, exists, owned, err := sandbox.inspectContainer(name)
		if err != nil {
			// Preserve the unresolved name so Destroy can retry after a transient
			// Docker-daemon/inspect failure.
			return err
		}
		if !exists {
			if _, uncertain := sandbox.uncertainContainers[name]; uncertain {
				// An outcome-unknown create has no safe absence proof in the Docker
				// API. Keep its name, owner token, Sandbox and worktree pending until
				// an owned immutable ID becomes visible and is removed. Repeated empty
				// name/label snapshots remain explicit retryable failure in this MVP.
				return ErrContainerOutcomeUnknown
			}
			delete(sandbox.containers, name)
			delete(sandbox.uncertainContainers, name)
			return nil
		}
		if !owned {
			delete(sandbox.containers, name)
			delete(sandbox.uncertainContainers, name)
			return ErrContainerOwnership
		}
		containerID = ownedID
		sandbox.containers[name] = containerID
		delete(sandbox.uncertainContainers, name)
	}
	remove := sandbox.removeOwnedContainer
	if sandbox.removeOwnedContainerOverride != nil {
		remove = sandbox.removeOwnedContainerOverride
	}
	removed, err := remove(containerID)
	if err != nil {
		return err
	}
	if removed {
		delete(sandbox.containers, name)
		delete(sandbox.uncertainContainers, name)
	}
	return nil
}

func (sandbox *dockerSandbox) recordCreate() error {
	if err := policy.RecordBounded(sandbox.config.Audit, policy.AuditEvent{
		Kind:      policy.AuditSandboxCreated,
		RunID:     sandbox.spec.RunID,
		AttemptID: sandbox.spec.AttemptID,
		Reason:    "disposable worktree provisioned; no execution container started",
		ImageID:   sandbox.config.Image,
		CPU:       sandbox.config.CPU,
		Memory:    sandbox.config.Memory,
		PIDs:      sandbox.config.PIDs,
	}); err != nil {
		return fmt.Errorf("persist sandbox-create audit: %w", err)
	}
	return nil
}

func (sandbox *dockerSandbox) removeOwnedContainer(reference string) (bool, error) {
	containerID, exists, owned, err := sandbox.inspectContainer(reference)
	if err != nil {
		return false, err
	}
	if !exists {
		return true, nil
	}
	if !owned {
		return false, ErrContainerOwnership
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	output, err := exec.CommandContext(ctx, "docker", "rm", "-f", containerID).CombinedOutput()
	if err != nil && !strings.Contains(string(output), "No such container") {
		return false, fmt.Errorf("remove owned container: %w: %s", err, strings.TrimSpace(string(output)))
	}
	return true, nil
}

func (sandbox *dockerSandbox) removeProviderOwnedContainers() error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	command := exec.CommandContext(
		ctx,
		"docker",
		"ps",
		"-a",
		"--no-trunc",
		"--filter",
		"label=agentdock.owner_token="+sandbox.ownerToken,
		"--format",
		"{{.ID}}\t{{.Names}}",
	)
	output, truncated, err := boundedCommandOutput(command, 1<<20)
	if err != nil {
		return fmt.Errorf("scan provider-owned containers: %w: %s%s", err, strings.TrimSpace(string(output)), truncatedSuffix(truncated))
	}
	if truncated {
		return errors.New("scan provider-owned containers exceeded 1 MiB")
	}
	var cleanupErr error
	for _, line := range strings.Split(strings.TrimSpace(string(output)), "\n") {
		if line == "" {
			continue
		}
		containerID, name, ok := strings.Cut(line, "\t")
		if !ok || !isObjectID(containerID) || name == "" {
			cleanupErr = errors.Join(cleanupErr, fmt.Errorf("invalid provider-owned container row %q", line))
			continue
		}
		ownedID, exists, owned, inspectErr := sandbox.inspectContainer(containerID)
		if inspectErr != nil {
			cleanupErr = errors.Join(cleanupErr, inspectErr)
			continue
		}
		if !exists || !owned || ownedID != containerID {
			// A forged token with a mismatched phase/Run/Attempt is foreign and
			// must never be removed by a token-only provider scan.
			continue
		}
		removed, removeErr := sandbox.removeOwnedContainer(containerID)
		if removeErr != nil {
			cleanupErr = errors.Join(cleanupErr, removeErr)
			continue
		}
		if !removed {
			cleanupErr = errors.Join(cleanupErr, fmt.Errorf("owned container %s was not removed", containerID))
			continue
		}
		for trackedName, trackedID := range sandbox.containers {
			if trackedName == name || trackedID == containerID {
				delete(sandbox.containers, trackedName)
				delete(sandbox.uncertainContainers, trackedName)
			}
		}
	}
	return cleanupErr
}

func (sandbox *dockerSandbox) cleanupFailedCreate(name string) error {
	for attempt := 0; attempt < containerReinspectAttempts; attempt++ {
		containerID, exists, owned, err := sandbox.inspectContainer(name)
		if err != nil {
			return err
		}
		if exists {
			if !owned {
				delete(sandbox.containers, name)
				delete(sandbox.uncertainContainers, name)
				return ErrContainerOwnership
			}
			sandbox.containers[name] = containerID
			delete(sandbox.uncertainContainers, name)
			return sandbox.removeTrackedContainer(name)
		}
		if attempt+1 < containerReinspectAttempts {
			delay := sandbox.containerReinspectDelay
			if delay == 0 {
				delay = 100 * time.Millisecond
			}
			if delay > 0 {
				time.Sleep(delay)
			}
		}
	}
	// One not-found (or even a short series) is not proof that a canceled
	// Docker client did not leave a late-visible daemon-side container.
	return ErrContainerOutcomeUnknown
}

func (sandbox *dockerSandbox) Destroy(_ context.Context) (resultErr error) {
	sandbox.mu.Lock()
	defer sandbox.mu.Unlock()
	defer func() {
		if resultErr != nil && !sandbox.destroyed {
			resultErr = errors.Join(resultErr, sandbox.recordDestroyFailed(resultErr))
		}
	}()
	if sandbox.destroyed {
		if sandbox.destroyAudited {
			sandbox.forgetProviderOwnership()
			return nil
		}
		auditErr := sandbox.recordDestroy()
		if auditErr == nil {
			sandbox.forgetProviderOwnership()
		}
		return auditErr
	}
	// Destruction is one-way. Once it starts, no further command may observe a
	// partially cleaned workspace or a temporarily restored real .git pointer.
	sandbox.destroying = true
	cleanupCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var containerErr error
	for name := range sandbox.containers {
		containerErr = errors.Join(containerErr, sandbox.killTrackedContainer(name))
		if err := sandbox.removeTrackedContainer(name); err != nil &&
			!errors.Is(err, ErrContainerOutcomeUnknown) {
			containerErr = errors.Join(containerErr, err)
		}
	}
	scanOwned := sandbox.removeProviderOwnedContainers
	if sandbox.scanOwnedContainersOverride != nil {
		scanOwned = sandbox.scanOwnedContainersOverride
	}
	containerErr = errors.Join(containerErr, scanOwned())
	for name := range sandbox.containers {
		containerErr = errors.Join(containerErr, sandbox.removeTrackedContainer(name))
	}
	if containerErr != nil || len(sandbox.containers) != 0 {
		if containerErr == nil {
			containerErr = ErrContainerOutcomeUnknown
		}
		return containerErr
	}
	cleanupWorkspace := sandbox.cleanupWorkspace
	if sandbox.cleanupWorkspaceOverride != nil {
		cleanupWorkspace = sandbox.cleanupWorkspaceOverride
	}
	if !sandbox.provisioningFailed {
		if err := cleanupWorkspace(cleanupCtx); err != nil {
			return err
		}
	}
	var restoreErr error
	if sandbox.gitPointerSanitized {
		restoreErr = restoreWorktreeGitPointer(sandbox.worktree.Workspace(), sandbox.gitPointer)
	}
	destroyErr := sandbox.worktree.Destroy(cleanupCtx)
	if destroyErr == nil {
		// The disposable worktree is the resource boundary. Once its provider
		// confirms removal, converge lifecycle state even if restoring the linked
		// .git pointer reported a diagnostic error: there is no workspace left to
		// clean on a retry.
		sandbox.destroyed = true
		auditErr := sandbox.recordDestroy()
		if auditErr == nil {
			sandbox.forgetProviderOwnership()
		}
		return errors.Join(restoreErr, auditErr)
	}
	var sanitizeErr error
	if sandbox.gitPointerSanitized {
		_, sanitizeErr = hideWorktreeGitPointer(sandbox.worktree.Workspace())
	}
	return errors.Join(restoreErr, destroyErr, sanitizeErr)
}

func (sandbox *dockerSandbox) forgetProviderOwnership() {
	if sandbox.provider == nil {
		return
	}
	sandbox.provider.pending.Delete(sandbox.ownerToken)
	sandbox.provider.owners.Delete(sandbox.ownerToken)
}

func (sandbox *dockerSandbox) recordDestroyFailed(cause error) error {
	reason := "sandbox cleanup failed and may be retried"
	if errors.Is(cause, ErrContainerOutcomeUnknown) {
		reason = "Docker create outcome remains unknown; Sandbox owner and worktree retained for explicit retry"
	}
	if err := policy.RecordBounded(sandbox.config.Audit, policy.AuditEvent{
		Kind:      policy.AuditSandboxDestroyFailed,
		RunID:     sandbox.spec.RunID,
		AttemptID: sandbox.spec.AttemptID,
		Reason:    reason,
		ImageID:   sandbox.config.Image,
		CPU:       sandbox.config.CPU,
		Memory:    sandbox.config.Memory,
		PIDs:      sandbox.config.PIDs,
	}); err != nil {
		return fmt.Errorf("persist sandbox-destroy-failure audit: %w", err)
	}
	return nil
}

func (sandbox *dockerSandbox) recordDestroy() error {
	if err := policy.RecordBounded(sandbox.config.Audit, policy.AuditEvent{
		Kind:      policy.AuditSandboxDestroyed,
		RunID:     sandbox.spec.RunID,
		AttemptID: sandbox.spec.AttemptID,
		Reason:    "container set and disposable worktree removed",
		ImageID:   sandbox.config.Image,
		CPU:       sandbox.config.CPU,
		Memory:    sandbox.config.Memory,
		PIDs:      sandbox.config.PIDs,
	}); err != nil {
		return fmt.Errorf("persist sandbox-destroy audit: %w", err)
	}
	sandbox.destroyAudited = true
	return nil
}

func (sandbox *dockerSandbox) cleanupWorkspace(ctx context.Context) error {
	name := sandbox.nextContainerName("cleanup")
	request := Request{Command: []string{"agentdock-sandbox-helper", "cleanup"}}
	sandbox.containers[name] = ""
	containerID, err := sandbox.createContainer(ctx, name, request)
	if err != nil {
		sandbox.markContainerCreateUnknown(name)
		cleanupErr := sandbox.cleanupFailedCreate(name)
		return errors.Join(fmt.Errorf("create workspace cleanup container: %w", err), cleanupErr)
	}
	sandbox.containers[name] = containerID
	delete(sandbox.uncertainContainers, name)
	runContext, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	var output limitedBuffer
	output.limit = 4096
	runErr := sandbox.startOwnedContainer(
		runContext,
		containerID,
		bytes.NewReader(nil),
		&output,
		&output,
	)
	removeErr := sandbox.removeTrackedContainer(name)
	if runErr != nil {
		return errors.Join(
			fmt.Errorf("cleanup disposable workspace permissions: %w: %s", runErr, strings.TrimSpace(string(output.Bytes()))),
			removeErr,
		)
	}
	if removeErr != nil {
		return removeErr
	}
	return nil
}

func validateContainerCommand(command []string) error {
	if len(command) == 0 {
		return ErrCommandNotAllowed
	}
	switch command[0] {
	case "agentdock-sandbox-helper":
		if len(command) < 2 {
			return ErrCommandNotAllowed
		}
		switch command[1] {
		case "list", "read", "search", "apply-patch", "test", "probe", "cleanup":
			return nil
		default:
			return ErrCommandNotAllowed
		}
	}
	return ErrCommandNotAllowed
}

func exitCode(err error) int {
	if err == nil {
		return 0
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return exitErr.ExitCode()
	}
	return -1
}

type limitedBuffer struct {
	buffer    bytes.Buffer
	limit     int
	total     int
	truncated bool
	budget    *outputBudget
}

type outputBudget struct {
	mu        sync.Mutex
	limit     int
	stored    int
	total     int
	truncated bool
}

func (writer *limitedBuffer) Write(content []byte) (int, error) {
	if writer.budget != nil {
		writer.budget.mu.Lock()
		defer writer.budget.mu.Unlock()
		original := len(content)
		writer.total += original
		writer.budget.total += original
		remaining := writer.budget.limit - writer.budget.stored
		if remaining > 0 {
			if len(content) > remaining {
				_, _ = writer.buffer.Write(content[:remaining])
				writer.budget.stored += remaining
				writer.truncated = true
				writer.budget.truncated = true
			} else {
				_, _ = writer.buffer.Write(content)
				writer.budget.stored += len(content)
			}
		} else if len(content) > 0 {
			writer.truncated = true
			writer.budget.truncated = true
		}
		return original, nil
	}
	original := len(content)
	writer.total += original
	remaining := writer.limit - writer.buffer.Len()
	if remaining > 0 {
		if len(content) > remaining {
			_, _ = writer.buffer.Write(content[:remaining])
			writer.truncated = true
		} else {
			_, _ = writer.buffer.Write(content)
		}
	} else if len(content) > 0 {
		writer.truncated = true
	}
	return original, nil
}

func (writer *limitedBuffer) Bytes() []byte {
	if writer.budget != nil {
		writer.budget.mu.Lock()
		defer writer.budget.mu.Unlock()
	}
	return append([]byte(nil), writer.buffer.Bytes()...)
}
