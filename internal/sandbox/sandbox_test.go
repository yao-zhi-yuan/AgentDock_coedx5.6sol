package sandbox

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/agentdock/agentdock-verify/internal/policy"
)

func TestDockerProviderRequiresAuditBeforeProvisioning(t *testing.T) {
	provider := NewDockerProvider(DockerConfig{Image: "does-not-matter"})
	_, err := provider.Create(context.Background(), Spec{
		RunID: "run", AttemptID: "attempt", Repository: "/not/used", Revision: "HEAD",
	})
	if err == nil || err.Error() != "phase-4 audit recorder is required" {
		t.Fatalf("Create error = %v", err)
	}
}

func TestDockerProviderRejectsInvalidResourceLimitsAndAuditsCreateFailure(t *testing.T) {
	for name, mutate := range map[string]func(*DockerConfig){
		"cpu":     func(config *DockerConfig) { config.CPU = "0" },
		"memory":  func(config *DockerConfig) { config.Memory = "0" },
		"pids":    func(config *DockerConfig) { config.PIDs = -1 },
		"timeout": func(config *DockerConfig) { config.CommandTimeout = -1 },
		"output":  func(config *DockerConfig) { config.OutputLimit = -1 },
	} {
		t.Run(name, func(t *testing.T) {
			recorder := policy.NewMemoryAuditRecorder()
			config := DockerConfig{
				Image:          "not-inspected-for-invalid-config",
				CPU:            "1",
				Memory:         "128m",
				PIDs:           32,
				CommandTimeout: time.Second,
				OutputLimit:    1024,
				Audit:          recorder,
			}
			mutate(&config)
			provider := NewDockerProvider(config)
			_, err := provider.Create(context.Background(), Spec{
				RunID: "invalid-config", AttemptID: name, Repository: "/not/used", Revision: "HEAD",
			})
			if err == nil {
				t.Fatal("invalid resource limit was accepted")
			}
			events := recorder.Events()
			if len(events) != 1 || events[0].Kind != policy.AuditSandboxCreateFailed {
				t.Fatalf("create-failure audit events = %#v", events)
			}
		})
	}
}

func TestGitWorktreeProviderRejectsScopeTextDelimiters(t *testing.T) {
	provider := NewGitWorktreeProvider("")
	for name, spec := range map[string]Spec{
		"run tab":         {RunID: "run\tother", AttemptID: "attempt"},
		"run newline":     {RunID: "run\nother", AttemptID: "attempt"},
		"attempt return":  {RunID: "run", AttemptID: "attempt\rother"},
		"attempt newline": {RunID: "run", AttemptID: "attempt\nother"},
	} {
		t.Run(name, func(t *testing.T) {
			spec.Repository = "/not/used"
			spec.Revision = "HEAD"
			if _, err := provider.Create(context.Background(), spec); err == nil ||
				!strings.Contains(err.Error(), "safe revision") {
				t.Fatalf("Create(%#v) error = %v", spec, err)
			}
		})
	}
}

func TestSandboxOwnerTokensAreRandomAndScopeLabelsMustMatchExactly(t *testing.T) {
	seen := make(map[string]bool)
	for index := 0; index < 128; index++ {
		token, err := newOwnerToken()
		if err != nil {
			t.Fatal(err)
		}
		if len(token) != 32 || seen[token] {
			t.Fatalf("owner token is malformed or repeated: %q", token)
		}
		seen[token] = true
	}
	sandbox := &dockerSandbox{
		spec:       Spec{RunID: "run-a", AttemptID: "attempt-a"},
		ownerToken: "00112233445566778899aabbccddeeff",
	}
	matching := map[string]string{
		"agentdock.phase":       "4",
		"agentdock.run_id":      "run-a",
		"agentdock.attempt_id":  "attempt-a",
		"agentdock.owner_token": sandbox.ownerToken,
	}
	if !sandbox.ownsLabels(matching) {
		t.Fatal("exact owner and scope labels were rejected")
	}
	for name, value := range map[string]string{
		"agentdock.phase":       "3",
		"agentdock.run_id":      "run-b",
		"agentdock.attempt_id":  "attempt-b",
		"agentdock.owner_token": "ffeeddccbbaa99887766554433221100",
	} {
		forged := make(map[string]string, len(matching))
		for key, original := range matching {
			forged[key] = original
		}
		forged[name] = value
		if sandbox.ownsLabels(forged) {
			t.Fatalf("mismatched label %s=%q was accepted", name, value)
		}
	}
}

func TestSafeGitCommandDisablesHostExecutionAndLazyObjectFetch(t *testing.T) {
	command := safeGitCommand(context.Background(), t.TempDir(), "rev-parse", "HEAD")
	for _, expected := range []string{
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_NO_LAZY_FETCH=1",
		"GIT_NO_REPLACE_OBJECTS=1",
		"GIT_PROTOCOL_FROM_USER=0",
		"GIT_TERMINAL_PROMPT=0",
	} {
		if !slices.Contains(command.Env, expected) {
			t.Fatalf("safe Git environment misses %q: %v", expected, command.Env)
		}
	}
}

func TestCommandSurfaceRejectsShellAndGoTestEscapeFlags(t *testing.T) {
	for name, command := range map[string][]string{
		"shell":     {"sh", "-c", "id"},
		"host path": {"/bin/sh", "-c", "id"},
		"raw go":    {"go", "test", "./..."},
		"raw git":   {"git", "status"},
	} {
		t.Run(name, func(t *testing.T) {
			if err := validateContainerCommand(command); !errors.Is(err, ErrCommandNotAllowed) {
				t.Fatalf("validateContainerCommand(%v) error = %v", command, err)
			}
		})
	}
	for _, command := range [][]string{
		{"agentdock-sandbox-helper", "read", "README.md"},
		{"agentdock-sandbox-helper", "test"},
	} {
		if err := validateContainerCommand(command); err != nil {
			t.Fatalf("safe command %v rejected: %v", command, err)
		}
	}
}

func TestLimitedBufferConsumesButBoundsOutput(t *testing.T) {
	writer := &limitedBuffer{limit: 4}
	if written, err := writer.Write([]byte("abcdefgh")); err != nil || written != 8 {
		t.Fatalf("Write = %d, %v", written, err)
	}
	if got := string(writer.Bytes()); got != "abcd" || !writer.truncated {
		t.Fatalf("buffer=%q truncated=%t", got, writer.truncated)
	}
}

func TestLimitedBuffersShareOneCombinedOutputBudget(t *testing.T) {
	budget := &outputBudget{limit: 5}
	stdout := &limitedBuffer{budget: budget}
	stderr := &limitedBuffer{budget: budget}
	_, _ = stdout.Write([]byte("abcd"))
	_, _ = stderr.Write([]byte("wxyz"))
	if got := len(stdout.Bytes()) + len(stderr.Bytes()); got != 5 {
		t.Fatalf("combined stored output = %d, want 5", got)
	}
	if !budget.truncated || budget.total != 8 {
		t.Fatalf("budget = %#v", budget)
	}
}

func TestBoundedCommandOutputCapsCombinedStreams(t *testing.T) {
	command := exec.Command(os.Args[0], "-test.run=TestBoundedCommandOutputHelperProcess")
	command.Env = append(os.Environ(), "AGENTDOCK_BOUNDED_OUTPUT_HELPER=1")
	output, truncated, err := boundedCommandOutput(command, 64)
	if err != nil {
		t.Fatalf("bounded helper command: %v", err)
	}
	if !truncated || len(output) != 64 {
		t.Fatalf("bounded output length=%d truncated=%t, want 64/true", len(output), truncated)
	}
	if suffix := truncatedSuffix(truncated); suffix != " [output truncated]" {
		t.Fatalf("truncation suffix = %q", suffix)
	}
}

func TestBoundedCommandOutputHelperProcess(t *testing.T) {
	if os.Getenv("AGENTDOCK_BOUNDED_OUTPUT_HELPER") != "1" {
		return
	}
	_, _ = os.Stdout.WriteString(strings.Repeat("o", 80))
	_, _ = os.Stderr.WriteString(strings.Repeat("e", 80))
}

func TestLateDockerCreateRemainsOwnedUntilDestroySeesIt(t *testing.T) {
	workspace := t.TempDir()
	if err := os.WriteFile(filepath.Join(workspace, ".git"), []byte(sanitizedGitPointer), 0o444); err != nil {
		t.Fatal(err)
	}
	recorder := policy.NewMemoryAuditRecorder()
	provider := NewDockerProvider(DockerConfig{Audit: recorder})
	ownerToken := "00112233445566778899aabbccddeeff"
	lateID := strings.Repeat("a", 64)
	lateVisible := false
	removed := false
	worktree := &destroyTestWorktree{workspace: workspace}
	sandbox := &dockerSandbox{
		provider:   provider,
		config:     DockerConfig{Audit: recorder, CommandTimeout: time.Second, OutputLimit: 1024},
		spec:       Spec{RunID: "late-create", AttemptID: "attempt-1"},
		worktree:   worktree,
		gitPointer: []byte("gitdir: /host/real/worktree\n"),
		ownerToken: ownerToken,
		containers: make(map[string]string),
		createContainerOverride: func(context.Context, string, Request) (string, error) {
			return "", ErrContainerOutcomeUnknown
		},
		inspectContainerOverride: func(string) (string, bool, bool, error) {
			if !lateVisible {
				return "", false, false, nil
			}
			return lateID, true, true, nil
		},
		removeOwnedContainerOverride: func(reference string) (bool, error) {
			if reference != lateID {
				t.Fatalf("remove reference = %q, want late ID", reference)
			}
			removed = true
			return true, nil
		},
		containerReinspectDelay: -1,
		scanOwnedContainersOverride: func() error {
			return nil
		},
		cleanupWorkspaceOverride: func(context.Context) error { return nil },
	}
	provider.owners.Store(ownerToken, sandbox.Scope())
	provider.pending.Store(ownerToken, sandbox)
	_, executeErr := sandbox.Execute(context.Background(), Request{
		Command:  []string{"agentdock-sandbox-helper", "probe", "user"},
		ToolName: "late-create-test",
	})
	if !errors.Is(executeErr, ErrContainerOutcomeUnknown) {
		t.Fatalf("Execute error = %v, want outcome unknown", executeErr)
	}
	if len(sandbox.containers) != 1 {
		t.Fatalf("unknown create name was forgotten: %#v", sandbox.containers)
	}
	lateVisible = true
	if err := sandbox.Destroy(context.Background()); err != nil {
		t.Fatalf("Destroy late-created container: %v", err)
	}
	if !removed || len(sandbox.containers) != 0 {
		t.Fatalf("late container cleanup removed=%t tracked=%#v", removed, sandbox.containers)
	}
	if _, ok := provider.owners.Load(ownerToken); ok {
		t.Fatal("late-create owner remains after converged Destroy")
	}
	if _, ok := provider.pending.Load(ownerToken); ok {
		t.Fatal("late-create pending handle remains after converged Destroy")
	}
}

func TestDockerProviderCleanupRetainsWorktreeUntilContainersConverge(t *testing.T) {
	workspace := filepath.Join(t.TempDir(), "owned-worktree")
	if err := os.Mkdir(workspace, 0o700); err != nil {
		t.Fatal(err)
	}
	recorder := policy.NewMemoryAuditRecorder()
	provider := NewDockerProvider(DockerConfig{Audit: recorder})
	worktreeCleanupCalls := 0
	provider.worktrees.cleanupOverride = func(context.Context, string, string, string) error {
		worktreeCleanupCalls++
		return os.RemoveAll(workspace)
	}
	worktree := &gitWorktree{
		provider:  provider.worktrees,
		root:      filepath.Dir(workspace),
		workspace: workspace,
		scope:     Scope{RunID: "provider-cleanup", AttemptID: "attempt-1"},
	}
	provider.worktrees.pending.Store(workspace, worktree)
	ownerToken := "11223344556677889900aabbccddeeff"
	lateID := strings.Repeat("b", 64)
	lateVisible := false
	removed := false
	sandbox := &dockerSandbox{
		provider:            provider,
		config:              DockerConfig{Audit: recorder},
		spec:                Spec{RunID: "provider-cleanup", AttemptID: "attempt-1"},
		worktree:            worktree,
		ownerToken:          ownerToken,
		containers:          map[string]string{"late-create": ""},
		uncertainContainers: map[string]time.Time{"late-create": time.Now().Add(time.Minute)},
		inspectContainerOverride: func(string) (string, bool, bool, error) {
			if !lateVisible {
				return "", false, false, nil
			}
			return lateID, true, true, nil
		},
		removeOwnedContainerOverride: func(reference string) (bool, error) {
			if reference != lateID {
				t.Fatalf("remove reference = %q, want %q", reference, lateID)
			}
			removed = true
			return true, nil
		},
		scanOwnedContainersOverride: func() error { return nil },
		cleanupWorkspaceOverride: func(context.Context) error {
			_, err := os.Stat(workspace)
			return err
		},
	}
	provider.owners.Store(ownerToken, sandbox.Scope())
	provider.pending.Store(ownerToken, sandbox)

	firstErr := provider.Cleanup(context.Background())
	if !errors.Is(firstErr, ErrContainerOutcomeUnknown) {
		t.Fatalf("first Provider Cleanup error = %v, want outcome unknown", firstErr)
	}
	if worktreeCleanupCalls != 0 {
		t.Fatalf("Provider cleaned worktree before containers converged: calls=%d", worktreeCleanupCalls)
	}
	if _, err := os.Stat(workspace); err != nil {
		t.Fatalf("worktree not retained after unresolved container: %v", err)
	}

	lateVisible = true
	if err := provider.Cleanup(context.Background()); err != nil {
		t.Fatalf("retry Provider Cleanup: %v", err)
	}
	if !removed || worktreeCleanupCalls != 1 {
		t.Fatalf("retry cleanup removed=%t worktree_calls=%d", removed, worktreeCleanupCalls)
	}
	if _, err := os.Stat(workspace); !os.IsNotExist(err) {
		t.Fatalf("worktree remains after converged cleanup: %v", err)
	}
	if len(sandbox.containers) != 0 || len(provider.worktrees.PendingWorktrees()) != 0 {
		t.Fatalf("resources remain: containers=%#v worktrees=%#v", sandbox.containers, provider.worktrees.PendingWorktrees())
	}
	if _, ok := provider.pending.Load(ownerToken); ok {
		t.Fatal("sandbox pending handle remains after converged cleanup")
	}
	if _, ok := provider.owners.Load(ownerToken); ok {
		t.Fatal("owner token remains after converged cleanup")
	}
}

func TestDestroyRetriesAuditAfterResourcesAreAlreadyGone(t *testing.T) {
	recorder := &failOnceAuditRecorder{}
	sandbox := &dockerSandbox{
		config:    DockerConfig{Audit: recorder},
		spec:      Spec{RunID: "run", AttemptID: "attempt"},
		destroyed: true,
	}
	if err := sandbox.Destroy(context.Background()); err == nil {
		t.Fatal("first Destroy unexpectedly hid audit persistence failure")
	}
	if sandbox.destroyAudited {
		t.Fatal("failed destroy audit was marked complete")
	}
	if err := sandbox.Destroy(context.Background()); err != nil {
		t.Fatalf("retry Destroy: %v", err)
	}
	if !sandbox.destroyAudited {
		t.Fatal("successful destroy audit retry was not retained")
	}
	if events := recorder.Events(); len(events) != 1 || events[0].Kind != policy.AuditSandboxDestroyed {
		t.Fatalf("destroy audit events = %#v", events)
	}
}

func TestCreateRollbackAttemptsDestroyAndKeepsFailedWorktreeSanitized(t *testing.T) {
	workspace := t.TempDir()
	pointer := filepath.Join(workspace, ".git")
	if err := os.WriteFile(pointer, []byte(sanitizedGitPointer), 0o444); err != nil {
		t.Fatal(err)
	}
	destroyErr := errors.New("injected worktree destroy failure")
	worktree := &destroyTestWorktree{workspace: workspace, destroyErr: destroyErr}
	original := []byte("gitdir: /host/real/worktree\n")
	if err := rollbackWorktreeAfterCreateFailure(context.Background(), worktree, original); !errors.Is(err, destroyErr) {
		t.Fatalf("rollback error = %v, want %v", err, destroyErr)
	}
	if worktree.destroyCalls != 1 {
		t.Fatalf("worktree Destroy calls = %d, want 1", worktree.destroyCalls)
	}
	if content, err := os.ReadFile(pointer); err != nil || string(content) != sanitizedGitPointer {
		t.Fatalf("failed rollback pointer = %q, %v", content, err)
	}

	if err := os.Remove(pointer); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(pointer, 0o700); err != nil {
		t.Fatal(err)
	}
	restoreFailure := &destroyTestWorktree{workspace: workspace}
	if err := rollbackWorktreeAfterCreateFailure(context.Background(), restoreFailure, original); err == nil {
		t.Fatal("rollback unexpectedly hid Git pointer restore failure")
	}
	if restoreFailure.destroyCalls != 1 {
		t.Fatalf("restore failure skipped worktree Destroy: calls=%d", restoreFailure.destroyCalls)
	}
}

func TestDestroyFailureIsOneWayAndResanitizesGitPointer(t *testing.T) {
	workspace := t.TempDir()
	pointer := filepath.Join(workspace, ".git")
	if err := os.WriteFile(pointer, []byte(sanitizedGitPointer), 0o444); err != nil {
		t.Fatal(err)
	}
	destroyErr := errors.New("injected worktree destroy failure")
	worktree := &destroyTestWorktree{workspace: workspace, destroyErr: destroyErr}
	recorder := policy.NewMemoryAuditRecorder()
	provider := NewDockerProvider(DockerConfig{Audit: recorder})
	ownerToken := "00112233445566778899aabbccddeeff"
	sandbox := &dockerSandbox{
		provider:                    provider,
		config:                      DockerConfig{Audit: recorder},
		spec:                        Spec{RunID: "run", AttemptID: "attempt"},
		worktree:                    worktree,
		gitPointer:                  []byte("gitdir: /host/real/worktree\n"),
		gitPointerSanitized:         true,
		ownerToken:                  ownerToken,
		containers:                  make(map[string]string),
		scanOwnedContainersOverride: func() error { return nil },
		cleanupWorkspaceOverride:    func(context.Context) error { return nil },
	}
	provider.owners.Store(ownerToken, sandbox.Scope())
	if err := sandbox.Destroy(context.Background()); !errors.Is(err, destroyErr) {
		t.Fatalf("Destroy error = %v, want %v", err, destroyErr)
	}
	if !sandbox.destroying || sandbox.destroyed {
		t.Fatalf("destroy state = destroying:%t destroyed:%t", sandbox.destroying, sandbox.destroyed)
	}
	if content, err := os.ReadFile(pointer); err != nil || string(content) != sanitizedGitPointer {
		t.Fatalf("failed Destroy pointer = %q, %v", content, err)
	}
	if _, err := sandbox.Execute(context.Background(), Request{
		Command:  []string{"agentdock-sandbox-helper", "probe", "user"},
		ToolName: "destroy-in-progress",
	}); !errors.Is(err, ErrDestroyed) {
		t.Fatalf("Execute after failed Destroy error = %v, want ErrDestroyed", err)
	}
	if _, ok := provider.owners.Load(ownerToken); !ok {
		t.Fatal("failed Destroy removed provider ownership needed for retry")
	}

	worktree.destroyErr = nil
	if err := sandbox.Destroy(context.Background()); err != nil {
		t.Fatalf("Destroy retry: %v", err)
	}
	if _, ok := provider.owners.Load(ownerToken); ok {
		t.Fatal("successful Destroy retained provider owner registry entry")
	}
}

func TestDestroyConvergesWhenGitRestoreFailsAfterWorktreeRemoval(t *testing.T) {
	workspace := t.TempDir()
	pointer := filepath.Join(workspace, ".git")
	if err := os.Mkdir(pointer, 0o700); err != nil {
		t.Fatal(err)
	}
	worktree := &destroyTestWorktree{workspace: workspace}
	recorder := policy.NewMemoryAuditRecorder()
	provider := NewDockerProvider(DockerConfig{Audit: recorder})
	ownerToken := "ffeeddccbbaa99887766554433221100"
	sandbox := &dockerSandbox{
		provider:                    provider,
		config:                      DockerConfig{Audit: recorder},
		spec:                        Spec{RunID: "run", AttemptID: "attempt"},
		worktree:                    worktree,
		gitPointer:                  []byte("gitdir: /host/real/worktree\n"),
		gitPointerSanitized:         true,
		ownerToken:                  ownerToken,
		containers:                  make(map[string]string),
		scanOwnedContainersOverride: func() error { return nil },
		cleanupWorkspaceOverride:    func(context.Context) error { return nil },
	}
	provider.owners.Store(ownerToken, sandbox.Scope())
	if err := sandbox.Destroy(context.Background()); err == nil {
		t.Fatal("Destroy unexpectedly hid Git pointer restore diagnostic")
	}
	if !sandbox.destroyed || !sandbox.destroyAudited || worktree.destroyCalls != 1 {
		t.Fatalf(
			"Destroy did not converge: destroyed=%t audited=%t calls=%d",
			sandbox.destroyed,
			sandbox.destroyAudited,
			worktree.destroyCalls,
		)
	}
	if _, ok := provider.owners.Load(ownerToken); ok {
		t.Fatal("converged Destroy retained provider owner registry entry")
	}
	if err := sandbox.Destroy(context.Background()); err != nil {
		t.Fatalf("converged Destroy retry: %v", err)
	}
	if worktree.destroyCalls != 1 {
		t.Fatalf("converged Destroy retried removed worktree: calls=%d", worktree.destroyCalls)
	}
}

type destroyTestWorktree struct {
	workspace    string
	destroyErr   error
	destroyCalls int
}

func (worktree *destroyTestWorktree) Execute(context.Context, Request) (Result, error) {
	return Result{}, ErrCommandNotAllowed
}

func (worktree *destroyTestWorktree) Scope() Scope { return Scope{} }

func (worktree *destroyTestWorktree) Workspace() string { return worktree.workspace }

func (worktree *destroyTestWorktree) Destroy(context.Context) error {
	worktree.destroyCalls++
	return worktree.destroyErr
}

type failOnceAuditRecorder struct {
	mu     sync.Mutex
	calls  int
	events []policy.AuditEvent
}

func (recorder *failOnceAuditRecorder) Record(_ context.Context, event policy.AuditEvent) error {
	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	recorder.calls++
	if recorder.calls == 1 {
		return errors.New("audit unavailable")
	}
	recorder.events = append(recorder.events, event)
	return nil
}

func (recorder *failOnceAuditRecorder) Events() []policy.AuditEvent {
	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	return append([]policy.AuditEvent(nil), recorder.events...)
}
