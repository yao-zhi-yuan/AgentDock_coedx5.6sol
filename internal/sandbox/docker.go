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
	config  DockerConfig
	counter atomic.Uint64
	owners  sync.Map // owner token -> Scope
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
	return &DockerProvider{config: config}
}

func (provider *DockerProvider) Create(ctx context.Context, spec Spec) (Sandbox, error) {
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
	worktree, err := NewGitWorktreeProvider(provider.config.WorktreeRoot).Create(ctx, spec)
	if err != nil {
		return nil, provider.recordCreateFailed(spec, "disposable worktree creation", immutableImage, err)
	}
	if err := prepareWorkspacePermissions(worktree.Workspace()); err != nil {
		cleanupErr := worktree.Destroy(context.Background())
		if cleanupErr != nil {
			cleanupErr = fmt.Errorf("clean worktree after permission setup failure: %w", cleanupErr)
		}
		return nil, provider.recordCreateFailed(
			spec,
			"workspace permission setup",
			immutableImage,
			errors.Join(err, cleanupErr),
		)
	}
	gitPointer, err := hideWorktreeGitPointer(worktree.Workspace())
	if err != nil {
		cleanupErr := worktree.Destroy(context.Background())
		if cleanupErr != nil {
			cleanupErr = fmt.Errorf("clean worktree after Git pointer setup failure: %w", cleanupErr)
		}
		return nil, provider.recordCreateFailed(
			spec,
			"Git pointer sanitization",
			immutableImage,
			errors.Join(err, cleanupErr),
		)
	}
	config := provider.config
	config.Image = immutableImage
	instance := &dockerSandbox{
		provider:   provider,
		config:     config,
		spec:       spec,
		worktree:   worktree,
		gitPointer: gitPointer,
		ownerToken: ownerToken,
		containers: make(map[string]string),
	}
	if err := instance.recordCreate(); err != nil {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		return nil, errors.Join(err, rollbackWorktreeAfterCreateFailure(cleanupCtx, worktree, gitPointer))
	}
	provider.owners.Store(ownerToken, instance.Scope())
	return instance, nil
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
	mu             sync.Mutex
	provider       *DockerProvider
	config         DockerConfig
	spec           Spec
	worktree       Sandbox
	gitPointer     []byte
	ownerToken     string
	containers     map[string]string
	destroying     bool
	destroyed      bool
	destroyAudited bool

	// removeOwnedContainerOverride is an internal failure-injection seam. It is
	// nil in production and lets integration tests prove cleanup errors are
	// returned, audited, and retained for a later Destroy retry.
	removeOwnedContainerOverride func(string) (bool, error)
	cleanupWorkspaceOverride     func(context.Context) error
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
	if sandbox.destroyed || sandbox.destroying {
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
	ctx context.Context,
	name string,
	request Request,
) (string, error) {
	createContext, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
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

var ErrContainerOwnership = errors.New("container ownership labels do not match sandbox scope")

type containerInspection struct {
	ID     string            `json:"id"`
	Labels map[string]string `json:"labels"`
}

func (sandbox *dockerSandbox) inspectContainer(reference string) (string, bool, bool, error) {
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
			delete(sandbox.containers, name)
			return nil
		}
		if !owned {
			delete(sandbox.containers, name)
			return ErrContainerOwnership
		}
		containerID = ownedID
		sandbox.containers[name] = containerID
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

func (sandbox *dockerSandbox) cleanupFailedCreate(name string) error {
	containerID, exists, owned, err := sandbox.inspectContainer(name)
	if err != nil {
		return err
	}
	if !exists {
		delete(sandbox.containers, name)
		return nil
	}
	if !owned {
		delete(sandbox.containers, name)
		return ErrContainerOwnership
	}
	sandbox.containers[name] = containerID
	return sandbox.removeTrackedContainer(name)
}

func (sandbox *dockerSandbox) Destroy(_ context.Context) (resultErr error) {
	sandbox.mu.Lock()
	defer sandbox.mu.Unlock()
	defer func() {
		if resultErr != nil && !sandbox.destroyed {
			resultErr = errors.Join(resultErr, sandbox.recordDestroyFailed())
		}
	}()
	if sandbox.destroyed {
		if sandbox.destroyAudited {
			return nil
		}
		return sandbox.recordDestroy()
	}
	// Destruction is one-way. Once it starts, no further command may observe a
	// partially cleaned workspace or a temporarily restored real .git pointer.
	sandbox.destroying = true
	cleanupCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	for name := range sandbox.containers {
		_ = sandbox.killTrackedContainer(name)
		if err := sandbox.removeTrackedContainer(name); err != nil {
			return err
		}
	}
	cleanupWorkspace := sandbox.cleanupWorkspace
	if sandbox.cleanupWorkspaceOverride != nil {
		cleanupWorkspace = sandbox.cleanupWorkspaceOverride
	}
	if err := cleanupWorkspace(cleanupCtx); err != nil {
		return err
	}
	restoreErr := restoreWorktreeGitPointer(sandbox.worktree.Workspace(), sandbox.gitPointer)
	destroyErr := sandbox.worktree.Destroy(cleanupCtx)
	if destroyErr == nil {
		// The disposable worktree is the resource boundary. Once its provider
		// confirms removal, converge lifecycle state even if restoring the linked
		// .git pointer reported a diagnostic error: there is no workspace left to
		// clean on a retry.
		sandbox.destroyed = true
		sandbox.provider.owners.Delete(sandbox.ownerToken)
		return errors.Join(restoreErr, sandbox.recordDestroy())
	}
	var sanitizeErr error
	_, sanitizeErr = hideWorktreeGitPointer(sandbox.worktree.Workspace())
	return errors.Join(restoreErr, destroyErr, sanitizeErr)
}

func (sandbox *dockerSandbox) recordDestroyFailed() error {
	if err := policy.RecordBounded(sandbox.config.Audit, policy.AuditEvent{
		Kind:      policy.AuditSandboxDestroyFailed,
		RunID:     sandbox.spec.RunID,
		AttemptID: sandbox.spec.AttemptID,
		Reason:    "sandbox cleanup failed and may be retried",
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
		cleanupErr := sandbox.cleanupFailedCreate(name)
		return errors.Join(fmt.Errorf("create workspace cleanup container: %w", err), cleanupErr)
	}
	sandbox.containers[name] = containerID
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
