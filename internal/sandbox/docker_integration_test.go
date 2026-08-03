//go:build integration

package sandbox

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/agentdock/agentdock-verify/internal/policy"
)

func TestDockerSandboxSecurityControlsAndAudit(t *testing.T) {
	repository := newFixtureRepository(t)
	audit := policy.NewMemoryAuditRecorder()
	provider := NewDockerProvider(DockerConfig{
		Image:          TestImage,
		CPU:            "0.5",
		Memory:         "128m",
		PIDs:           32,
		CommandTimeout: 5 * time.Second,
		OutputLimit:    1024,
		Audit:          audit,
	})
	instance, err := provider.Create(context.Background(), Spec{
		RunID:      "security-run",
		AttemptID:  "attempt-1",
		Repository: repository,
		Revision:   "HEAD",
	})
	if err != nil {
		t.Fatal(err)
	}
	workspace := instance.Workspace()
	t.Cleanup(func() { _ = instance.Destroy(context.Background()) })
	pointer := string(mustReadFile(t, filepath.Join(workspace, ".git")))
	if pointer != sanitizedGitPointer ||
		strings.Contains(pointer, repository) ||
		strings.Contains(pointer, "/Users/") {
		t.Fatalf("worktree Git pointer was not sanitized: %q", pointer)
	}

	assertProbeContains(t, instance, "user", "uid=", false)
	user := executeProbe(t, instance, "user")
	if strings.Contains(string(user.Stdout), "uid=0") {
		t.Fatalf("container ran as root: %s", user.Stdout)
	}
	limits := executeProbe(t, instance, "limits")
	for _, expected := range []string{
		"cpu.max=50000 100000",
		"memory.max=134217728",
		"pids.max=32",
	} {
		if !strings.Contains(string(limits.Stdout), expected) {
			t.Fatalf("missing cgroup limit %q in %s", expected, limits.Stdout)
		}
	}
	environment := executeProbe(t, instance, "environment-keys")
	for _, forbidden := range []string{
		"OPENAI_API_KEY",
		"ANTHROPIC_API_KEY",
		"AGENTDOCK_DATABASE_URL",
		"AWS_SECRET_ACCESS_KEY",
	} {
		if strings.Contains(string(environment.Stdout), forbidden+"\n") {
			t.Fatalf("host credential key %s reached container: %s", forbidden, environment.Stdout)
		}
	}
	fixedEnvironment := executeProbe(t, instance, "fixed-environment")
	if !strings.Contains(string(fixedEnvironment.Stdout), "fixed-environment-ok") {
		t.Fatalf("sandbox fixed environment mismatch: %#v", fixedEnvironment)
	}
	toctou := executeProbe(t, instance, "toctou")
	if !strings.Contains(string(toctou.Stdout), "toctou-escape-observed=false") {
		t.Fatalf("TOCTOU probe did not prove containment: %s", toctou.Stdout)
	}

	rootWrite, err := instance.Execute(context.Background(), Request{
		Command: []string{"agentdock-sandbox-helper", "probe", "rootfs-write"},
	})
	if err == nil || rootWrite.ExitCode == 0 {
		t.Fatalf("read-only RootFS write unexpectedly succeeded: result=%#v err=%v", rootWrite, err)
	}
	nonWorkspace := executeProbe(t, instance, "nonworkspace-write")
	if !strings.Contains(string(nonWorkspace.Stdout), "non-workspace-write-denied") {
		t.Fatalf("non-workspace path was writable: %#v", nonWorkspace)
	}
	workspaceWrite := executeProbe(t, instance, "workspace-write")
	if !strings.Contains(string(workspaceWrite.Stdout), "workspace-write-ok") {
		t.Fatalf("workspace was not writable: %#v", workspaceWrite)
	}
	workspaceMode := executeProbe(t, instance, "workspace-directory-mode")
	if !strings.Contains(string(workspaceMode.Stdout), "workspace-sticky=false") ||
		!strings.Contains(string(workspaceMode.Stdout), "workspace-other-write=true") {
		t.Fatalf("workspace mode does not permit portable top-level replacement: %s", workspaceMode.Stdout)
	}
	gitPointer := executeProbe(t, instance, "git-pointer-delete")
	if !strings.Contains(string(gitPointer.Stdout), "git-pointer-delete-denied") {
		t.Fatalf("sanitized Git pointer was not protected by a read-only mount: %#v", gitPointer)
	}
	if err := os.WriteFile(filepath.Join(workspace, "overlap.txt"), []byte("aaa"), 0o666); err != nil {
		t.Fatal(err)
	}
	overlap, overlapErr := instance.Execute(context.Background(), Request{
		Command:  []string{"agentdock-sandbox-helper", "apply-patch"},
		Stdin:    []byte(`{"path":"overlap.txt","old":"aa","new":"a"}`),
		ToolName: "repo.apply_patch",
	})
	if overlapErr == nil || overlap.ExitCode == 0 {
		t.Fatalf("replayable overlapping patch succeeded: result=%#v err=%v", overlap, overlapErr)
	}
	if content := string(mustReadFile(t, filepath.Join(workspace, "overlap.txt"))); content != "aaa" {
		t.Fatalf("rejected overlapping patch changed content: %q", content)
	}
	patched, err := instance.Execute(context.Background(), Request{
		Command: []string{"agentdock-sandbox-helper", "apply-patch"},
		Stdin: []byte(
			`{"path":"main.go","old":"func main() {}","new":"func main() {\n}"}`,
		),
		ToolName: "repo.apply_patch",
	})
	if err != nil {
		t.Fatalf("top-level atomic apply-patch failed: %v\n%s", err, patched.Stderr)
	}
	if content := string(mustReadFile(t, filepath.Join(workspace, "main.go"))); !strings.Contains(content, "func main() {\n}") {
		t.Fatalf("top-level patch was not published: %q", content)
	}

	network, err := instance.Execute(context.Background(), Request{
		Command: []string{"agentdock-sandbox-helper", "probe", "network"},
	})
	if err == nil || network.ExitCode == 0 {
		t.Fatalf("network request unexpectedly succeeded: result=%#v err=%v", network, err)
	}

	preCanceledContext, cancelBeforeStart := context.WithCancel(context.Background())
	cancelBeforeStart()
	preCanceled, err := instance.Execute(preCanceledContext, Request{
		Command: []string{"agentdock-sandbox-helper", "probe", "sleep"},
	})
	if !errors.Is(err, context.Canceled) || preCanceled.TimedOut {
		t.Fatalf("pre-start canceled result=%#v err=%v", preCanceled, err)
	}

	timeoutContext, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	timed, err := instance.Execute(timeoutContext, Request{
		Command: []string{"agentdock-sandbox-helper", "probe", "sleep"},
	})
	if !errors.Is(err, ErrTimeout) || !timed.TimedOut {
		t.Fatalf("timeout result=%#v err=%v", timed, err)
	}

	forked, err := instance.Execute(context.Background(), Request{
		Command: []string{"agentdock-sandbox-helper", "probe", "pids"},
	})
	if err != nil {
		t.Fatalf("PID probe: %v (%s)", err, forked.Stderr)
	}
	if !strings.Contains(string(forked.Stdout), "pids-limited=true") {
		t.Fatalf("fork probe did not hit PID limit: %s", forked.Stdout)
	}

	large, err := instance.Execute(context.Background(), Request{
		Command:     []string{"agentdock-sandbox-helper", "probe", "large-output"},
		OutputLimit: 128,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !large.Truncated || len(large.Stdout) > 128 {
		t.Fatalf("large output was not bounded: len=%d result=%#v", len(large.Stdout), large)
	}

	if _, err := instance.Execute(context.Background(), Request{
		Command: []string{"sh", "-c", "id"},
	}); !errors.Is(err, ErrCommandNotAllowed) {
		t.Fatalf("arbitrary command error = %v, want ErrCommandNotAllowed", err)
	}
	for _, injected := range []string{"-run=^$", "-exec=sh", "-toolexec=sh"} {
		if _, err := instance.Execute(context.Background(), Request{
			Command:     []string{"agentdock-sandbox-helper", "test"},
			Environment: map[string]string{"GOFLAGS": injected},
		}); !errors.Is(err, ErrCommandNotAllowed) {
			t.Fatalf("GOFLAGS %q error = %v, want ErrCommandNotAllowed", injected, err)
		}
	}

	cancelContext, cancelExecution := context.WithCancel(context.Background())
	go func() {
		time.Sleep(100 * time.Millisecond)
		cancelExecution()
	}()
	canceled, err := instance.Execute(cancelContext, Request{
		Command: []string{"agentdock-sandbox-helper", "probe", "sleep"},
	})
	if !errors.Is(err, context.Canceled) || canceled.TimedOut {
		t.Fatalf("canceled result=%#v err=%v", canceled, err)
	}

	restricted := executeProbe(t, instance, "restrict-workspace")
	if !strings.Contains(string(restricted.Stdout), "workspace-restricted") {
		t.Fatalf("workspace restriction probe failed: %#v", restricted)
	}

	if _, err := os.Stat(workspace); err != nil {
		t.Fatalf("worktree vanished before Destroy: %v", err)
	}
	if err := instance.Destroy(context.Background()); err != nil {
		t.Fatal(err)
	}
	if docker, ok := instance.(*dockerSandbox); !ok {
		t.Fatal("Docker provider returned an unexpected Sandbox implementation")
	} else if _, owned := provider.owners.Load(docker.ownerToken); owned {
		t.Fatal("Destroy retained the provider owner registry entry")
	}
	if _, err := os.Stat(workspace); !os.IsNotExist(err) {
		t.Fatalf("worktree remains after Destroy: %v", err)
	}
	if containers := provider.ActiveContainers(context.Background()); len(containers) != 0 {
		t.Fatalf("containers remain after Destroy: %v", containers)
	}
	eventsBeforeDestroyedDenial := len(audit.Events())
	if _, err := instance.Execute(context.Background(), Request{
		Command:  []string{"agentdock-sandbox-helper", "probe", "user"},
		ToolName: "destroyed-sandbox-test",
	}); !errors.Is(err, ErrDestroyed) {
		t.Fatalf("Execute after Destroy error = %v, want ErrDestroyed", err)
	}
	eventsAfterDestroyedDenial := audit.Events()
	if len(eventsAfterDestroyedDenial) != eventsBeforeDestroyedDenial+1 ||
		eventsAfterDestroyedDenial[len(eventsAfterDestroyedDenial)-1].Kind != policy.AuditPolicyDenied {
		t.Fatalf("Execute-after-Destroy denial was not audited: %#v", eventsAfterDestroyedDenial)
	}

	kinds := map[policy.AuditKind]bool{}
	for _, event := range audit.Events() {
		kinds[event.Kind] = true
	}
	for _, required := range []policy.AuditKind{
		policy.AuditSandboxCreated,
		policy.AuditExecutionTimedOut,
		policy.AuditExecutionCanceled,
		policy.AuditOutputTruncated,
		policy.AuditSandboxDestroyed,
	} {
		if !kinds[required] {
			t.Fatalf("missing audit kind %s in %#v", required, audit.Events())
		}
	}
}

func TestDockerExecuteReportsCleanupFailureAndRetainsContainerForDestroy(t *testing.T) {
	repository := newFixtureRepository(t)
	audit := policy.NewMemoryAuditRecorder()
	provider := NewDockerProvider(DockerConfig{Image: TestImage, Audit: audit})
	instance, err := provider.Create(context.Background(), Spec{
		RunID: "cleanup-run", AttemptID: "attempt-1", Repository: repository, Revision: "HEAD",
	})
	if err != nil {
		t.Fatal(err)
	}
	docker := instance.(*dockerSandbox)
	t.Cleanup(func() { _ = instance.Destroy(context.Background()) })
	injected := errors.New("injected container remove failure")
	docker.removeOwnedContainerOverride = func(string) (bool, error) {
		return false, injected
	}
	t.Cleanup(func() { docker.removeOwnedContainerOverride = nil })
	result, executeErr := instance.Execute(context.Background(), Request{
		Command:  []string{"agentdock-sandbox-helper", "probe", "user"},
		ToolName: "cleanup-failure-test",
	})
	if !errors.Is(executeErr, injected) || result.ExitCode != 0 {
		t.Fatalf("cleanup failure result=%#v error=%v", result, executeErr)
	}
	if len(docker.containers) != 1 {
		t.Fatalf("failed cleanup was not retained for Destroy: %#v", docker.containers)
	}
	hasFailed := false
	hasCompleted := false
	for _, event := range audit.Events() {
		if event.ToolName != "cleanup-failure-test" {
			continue
		}
		hasFailed = hasFailed || event.Kind == policy.AuditExecutionFailed
		hasCompleted = hasCompleted || event.Kind == policy.AuditExecutionCompleted
	}
	if !hasFailed || hasCompleted {
		t.Fatalf("cleanup failure audit was not fail-closed: %#v", audit.Events())
	}
	docker.removeOwnedContainerOverride = nil
	if err := instance.Destroy(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestDockerAuditsCleanupFailureOnTimeoutCancelAndNonzeroExit(t *testing.T) {
	for name, execute := range map[string]func(Sandbox) error{
		"timeout": func(instance Sandbox) error {
			_, err := instance.Execute(context.Background(), Request{
				Command:  []string{"agentdock-sandbox-helper", "probe", "sleep"},
				Timeout:  100 * time.Millisecond,
				ToolName: "cleanup-timeout",
			})
			return err
		},
		"cancel": func(instance Sandbox) error {
			ctx, cancel := context.WithCancel(context.Background())
			go func() {
				time.Sleep(100 * time.Millisecond)
				cancel()
			}()
			_, err := instance.Execute(ctx, Request{
				Command:  []string{"agentdock-sandbox-helper", "probe", "sleep"},
				ToolName: "cleanup-cancel",
			})
			return err
		},
		"nonzero": func(instance Sandbox) error {
			_, err := instance.Execute(context.Background(), Request{
				Command:  []string{"agentdock-sandbox-helper", "probe", "rootfs-write"},
				ToolName: "cleanup-nonzero",
			})
			return err
		},
	} {
		t.Run(name, func(t *testing.T) {
			repository := newFixtureRepository(t)
			audit := policy.NewMemoryAuditRecorder()
			provider := NewDockerProvider(DockerConfig{Image: TestImage, Audit: audit})
			instance, err := provider.Create(context.Background(), Spec{
				RunID: "cleanup-" + name, AttemptID: "attempt-1", Repository: repository, Revision: "HEAD",
			})
			if err != nil {
				t.Fatal(err)
			}
			docker := instance.(*dockerSandbox)
			injected := errors.New("injected " + name + " remove failure")
			docker.removeOwnedContainerOverride = func(string) (bool, error) { return false, injected }
			executeErr := execute(instance)
			if !errors.Is(executeErr, injected) {
				t.Fatalf("Execute error = %v, want injected cleanup failure", executeErr)
			}
			hasCleanupFailure := false
			for _, event := range audit.Events() {
				if event.Kind == policy.AuditExecutionFailed && strings.Contains(event.Reason, "cleanup failed") {
					hasCleanupFailure = true
				}
			}
			if !hasCleanupFailure {
				t.Fatalf("cleanup failure was not explicitly audited: %#v", audit.Events())
			}
			docker.removeOwnedContainerOverride = nil
			if err := instance.Destroy(context.Background()); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestDestroyRecoversUnresolvedOwnedContainerName(t *testing.T) {
	repository := newFixtureRepository(t)
	provider := NewDockerProvider(DockerConfig{
		Image: TestImage,
		Audit: policy.NewMemoryAuditRecorder(),
	})
	instance, err := provider.Create(context.Background(), Spec{
		RunID: "unresolved-run", AttemptID: "attempt-1", Repository: repository, Revision: "HEAD",
	})
	if err != nil {
		t.Fatal(err)
	}
	docker := instance.(*dockerSandbox)
	t.Cleanup(func() { _ = instance.Destroy(context.Background()) })
	name := docker.nextContainerName("exec")
	containerID := createForeignContainer(t, name, docker.config.Image, map[string]string{
		"agentdock.phase":       "4",
		"agentdock.run_id":      docker.spec.RunID,
		"agentdock.attempt_id":  docker.spec.AttemptID,
		"agentdock.owner_token": docker.ownerToken,
	})
	docker.containers[name] = ""
	if err := instance.Destroy(context.Background()); err != nil {
		t.Fatalf("Destroy did not recover unresolved owned name: %v", err)
	}
	if output, inspectErr := exec.Command("docker", "inspect", containerID).CombinedOutput(); inspectErr == nil {
		t.Fatalf("resolved owned container remains after Destroy: %s", output)
	}
}

func TestDockerCreateAuditRollbackFailureReturnsProviderCleanupHandle(t *testing.T) {
	repository := newFixtureRepository(t)
	beforeDigest := repositoryDigest(t, repository)
	beforeStatus := gitTestOutput(t, repository, "status", "--porcelain=v1")
	recorder := &failOnceAuditRecorder{}
	provider := NewDockerProvider(DockerConfig{Image: TestImage, Audit: recorder})
	cleanupErr := errors.New("injected worktree destroy failure")
	provider.worktrees.cleanupOverride = func(context.Context, string, string, string) error {
		return cleanupErr
	}
	instance, createErr := provider.Create(context.Background(), Spec{
		RunID: "audit-rollback", AttemptID: "attempt-1", Repository: repository, Revision: "HEAD",
	})
	if instance == nil || !errors.Is(createErr, cleanupErr) {
		t.Fatalf("Create handle=%#v error=%v", instance, createErr)
	}
	docker := instance.(*dockerSandbox)
	workspace := instance.Workspace()
	if _, ok := provider.pending.Load(docker.ownerToken); !ok {
		t.Fatal("provider lost failed-provisioning cleanup handle")
	}
	if registered, err := worktreeRegistered(context.Background(), repository, workspace); err != nil || !registered {
		t.Fatalf("failed rollback registration registered=%t err=%v", registered, err)
	}
	provider.worktrees.cleanupOverride = nil
	if err := provider.Cleanup(context.Background()); err != nil {
		t.Fatalf("provider retry cleanup: %v", err)
	}
	if registered, err := worktreeRegistered(context.Background(), repository, workspace); err != nil || registered {
		t.Fatalf("registration after retry registered=%t err=%v", registered, err)
	}
	if _, err := os.Stat(workspace); !os.IsNotExist(err) {
		t.Fatalf("temporary directory remains after retry: %v", err)
	}
	if containers := provider.ActiveContainers(context.Background()); len(containers) != 0 {
		t.Fatalf("owned containers remain after retry: %v", containers)
	}
	if pending := provider.worktrees.PendingWorktrees(); len(pending) != 0 {
		t.Fatalf("pending worktrees remain after retry: %v", pending)
	}
	if _, ok := provider.pending.Load(docker.ownerToken); ok {
		t.Fatal("provider sandbox cleanup handle remains after retry")
	}
	if _, ok := provider.owners.Load(docker.ownerToken); ok {
		t.Fatal("provider owner token remains after retry")
	}
	if after := repositoryDigest(t, repository); after != beforeDigest {
		t.Fatalf("origin digest changed: before=%s after=%s", beforeDigest, after)
	}
	if after := gitTestOutput(t, repository, "status", "--porcelain=v1"); after != beforeStatus {
		t.Fatalf("origin Git status changed: before=%q after=%q", beforeStatus, after)
	}
}

func TestDestroyScansOwnedContainersMissingFromTracking(t *testing.T) {
	repository := newFixtureRepository(t)
	provider := NewDockerProvider(DockerConfig{Image: TestImage, Audit: policy.NewMemoryAuditRecorder()})
	instance, err := provider.Create(context.Background(), Spec{
		RunID: "scan-fallback", AttemptID: "attempt-1", Repository: repository, Revision: "HEAD",
	})
	if err != nil {
		t.Fatal(err)
	}
	docker := instance.(*dockerSandbox)
	containerID := createForeignContainer(t, docker.nextContainerName("late"), docker.config.Image, map[string]string{
		"agentdock.phase":       "4",
		"agentdock.run_id":      docker.spec.RunID,
		"agentdock.attempt_id":  docker.spec.AttemptID,
		"agentdock.owner_token": docker.ownerToken,
	})
	if len(docker.containers) != 0 {
		t.Fatalf("fixture unexpectedly tracked fallback container: %#v", docker.containers)
	}
	if err := instance.Destroy(context.Background()); err != nil {
		t.Fatalf("Destroy provider scan fallback: %v", err)
	}
	if output, inspectErr := exec.Command("docker", "inspect", containerID).CombinedOutput(); inspectErr == nil {
		t.Fatalf("untracked owned container remains after Destroy: %s", output)
	}
}

func TestDestroyFallbackScanSkipsCrossProviderAndForgedScope(t *testing.T) {
	repository := newFixtureRepository(t)
	spec := Spec{
		RunID: "fallback-owner-run", AttemptID: "attempt-1", Repository: repository, Revision: "HEAD",
	}
	providerA := NewDockerProvider(DockerConfig{Image: TestImage, Audit: policy.NewMemoryAuditRecorder()})
	providerB := NewDockerProvider(DockerConfig{Image: TestImage, Audit: policy.NewMemoryAuditRecorder()})
	instanceA, err := providerA.Create(context.Background(), spec)
	if err != nil {
		t.Fatal(err)
	}
	instanceB, err := providerB.Create(context.Background(), spec)
	if err != nil {
		_ = instanceA.Destroy(context.Background())
		t.Fatal(err)
	}
	first := instanceA.(*dockerSandbox)
	second := instanceB.(*dockerSandbox)
	crossProviderID := createForeignContainer(
		t,
		second.nextContainerName("fallback"),
		second.config.Image,
		map[string]string{
			"agentdock.phase":       "4",
			"agentdock.run_id":      second.spec.RunID,
			"agentdock.attempt_id":  second.spec.AttemptID,
			"agentdock.owner_token": second.ownerToken,
		},
	)
	forgedScopeID := createForeignContainer(
		t,
		first.nextContainerName("forged"),
		first.config.Image,
		map[string]string{
			"agentdock.phase":       "4",
			"agentdock.run_id":      first.spec.RunID,
			"agentdock.attempt_id":  "foreign-attempt",
			"agentdock.owner_token": first.ownerToken,
		},
	)
	t.Cleanup(func() {
		_, _ = exec.Command("docker", "rm", "-f", crossProviderID, forgedScopeID).CombinedOutput()
	})

	if err := instanceA.Destroy(context.Background()); err != nil {
		t.Fatalf("Destroy provider A: %v", err)
	}
	for _, foreignID := range []string{crossProviderID, forgedScopeID} {
		if output, inspectErr := exec.Command("docker", "inspect", foreignID).CombinedOutput(); inspectErr != nil {
			t.Fatalf("provider A removed foreign container %s: %v\n%s", foreignID, inspectErr, output)
		}
	}
	if err := instanceB.Destroy(context.Background()); err != nil {
		t.Fatalf("Destroy provider B: %v", err)
	}
	if output, inspectErr := exec.Command("docker", "inspect", crossProviderID).CombinedOutput(); inspectErr == nil {
		t.Fatalf("provider B did not remove its untracked owned container: %s", output)
	}
	if output, inspectErr := exec.Command("docker", "inspect", forgedScopeID).CombinedOutput(); inspectErr != nil {
		t.Fatalf("forged-scope container was removed: %v\n%s", inspectErr, output)
	}
}

func TestSandboxHelperRejectsAbsoluteTraversalAndSymlinkEscape(t *testing.T) {
	repository := newFixtureRepository(t)
	outside := filepath.Join(t.TempDir(), "outside-secret")
	if err := os.WriteFile(outside, []byte("host secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(repository, "escape")); err != nil {
		t.Fatal(err)
	}
	runGit(t, repository, "add", "escape")
	runGit(t, repository, "-c", "user.name=AgentDock", "-c", "user.email=agentdock@example.test", "commit", "-m", "symlink")

	provider := NewDockerProvider(DockerConfig{
		Image: TestImage,
		Audit: policy.NewMemoryAuditRecorder(),
	})
	instance, err := provider.Create(context.Background(), Spec{
		RunID: "path-run", AttemptID: "attempt-1", Repository: repository, Revision: "HEAD",
	})
	if err != nil {
		t.Fatal(err)
	}
	defer instance.Destroy(context.Background())
	for _, candidate := range []string{"../secret", "/etc/passwd", "escape"} {
		result, executeErr := instance.Execute(context.Background(), Request{
			Command: []string{"agentdock-sandbox-helper", "read", candidate},
		})
		if executeErr == nil || result.ExitCode == 0 {
			t.Fatalf("unsafe path %q succeeded: result=%#v err=%v", candidate, result, executeErr)
		}
	}
}

func TestDockerOwnershipPreventsCrossProviderAndCollisionCleanup(t *testing.T) {
	repository := newFixtureRepository(t)
	auditA := policy.NewMemoryAuditRecorder()
	providerA := NewDockerProvider(DockerConfig{
		Image: TestImage,
		Audit: auditA,
	})
	providerB := NewDockerProvider(DockerConfig{
		Image: TestImage,
		Audit: policy.NewMemoryAuditRecorder(),
	})
	spec := Spec{
		RunID: "owner-run", AttemptID: "owner-attempt", Repository: repository, Revision: "HEAD",
	}
	firstInstance, err := providerA.Create(context.Background(), spec)
	if err != nil {
		t.Fatal(err)
	}
	defer firstInstance.Destroy(context.Background())
	secondInstance, err := providerB.Create(context.Background(), spec)
	if err != nil {
		t.Fatal(err)
	}
	defer secondInstance.Destroy(context.Background())
	first := firstInstance.(*dockerSandbox)
	second := secondInstance.(*dockerSandbox)
	if first.ownerToken == second.ownerToken ||
		first.containerName("exec", 1) == second.containerName("exec", 1) {
		t.Fatalf(
			"independent providers share ownership identity: first=%s second=%s",
			first.ownerToken,
			second.ownerToken,
		)
	}

	for name, labels := range map[string]map[string]string{
		"different owner token": {
			"agentdock.phase":       "4",
			"agentdock.run_id":      spec.RunID,
			"agentdock.attempt_id":  spec.AttemptID,
			"agentdock.owner_token": second.ownerToken,
		},
		"forged token wrong scope": {
			"agentdock.phase":       "4",
			"agentdock.run_id":      spec.RunID,
			"agentdock.attempt_id":  "other-attempt",
			"agentdock.owner_token": first.ownerToken,
		},
		"forged token wrong phase": {
			"agentdock.phase":       "other",
			"agentdock.run_id":      spec.RunID,
			"agentdock.attempt_id":  spec.AttemptID,
			"agentdock.owner_token": first.ownerToken,
		},
	} {
		t.Run(name, func(t *testing.T) {
			providerA.counter.Store(0)
			collisionName := first.containerName("exec", 1)
			foreignID := createForeignContainer(t, collisionName, first.config.Image, labels)
			for _, active := range providerA.ActiveContainers(context.Background()) {
				if active == collisionName {
					t.Fatalf("provider reported foreign owner container as active: %s", active)
				}
			}
			if startErr := first.startOwnedContainer(
				context.Background(),
				foreignID,
				bytes.NewReader(nil),
				&limitedBuffer{limit: 128},
				&limitedBuffer{limit: 128},
			); !errors.Is(startErr, ErrContainerOwnership) {
				t.Fatalf("foreign start error = %v, want ErrContainerOwnership", startErr)
			}
			trackingKey := "foreign-" + strings.ReplaceAll(name, " ", "-")
			first.containers[trackingKey] = foreignID
			if killErr := first.killTrackedContainer(trackingKey); !errors.Is(killErr, ErrContainerOwnership) {
				t.Fatalf("foreign kill error = %v, want ErrContainerOwnership", killErr)
			}
			if removeErr := first.removeTrackedContainer(trackingKey); !errors.Is(removeErr, ErrContainerOwnership) {
				t.Fatalf("foreign remove error = %v, want ErrContainerOwnership", removeErr)
			}
			delete(first.containers, trackingKey)
			_, executeErr := first.Execute(context.Background(), Request{
				Command: []string{"agentdock-sandbox-helper", "probe", "user"},
			})
			if !errors.Is(executeErr, ErrContainerOwnership) {
				t.Fatalf("collision error = %v, want ErrContainerOwnership", executeErr)
			}
			if output, inspectErr := exec.Command("docker", "inspect", foreignID).CombinedOutput(); inspectErr != nil {
				t.Fatalf("foreign container was removed: %v\n%s", inspectErr, output)
			}
			if _, tracked := first.containers[collisionName]; tracked {
				t.Fatalf("foreign collision remained in sandbox tracking: %s", collisionName)
			}
			removeForeignContainer(t, foreignID)
		})
	}
}

func TestDockerDestroyOwnershipFailureIsOneWayAndRetryable(t *testing.T) {
	repository := newFixtureRepository(t)
	audit := policy.NewMemoryAuditRecorder()
	provider := NewDockerProvider(DockerConfig{Image: TestImage, Audit: audit})
	instance, err := provider.Create(context.Background(), Spec{
		RunID: "destroy-owner-run", AttemptID: "attempt-1", Repository: repository, Revision: "HEAD",
	})
	if err != nil {
		t.Fatal(err)
	}
	docker := instance.(*dockerSandbox)
	trackingKey := "foreign-destroy"
	foreignID := createForeignContainer(t, docker.nextContainerName("foreign"), docker.config.Image, map[string]string{
		"agentdock.phase":       "4",
		"agentdock.run_id":      docker.spec.RunID,
		"agentdock.attempt_id":  docker.spec.AttemptID,
		"agentdock.owner_token": "ffeeddccbbaa99887766554433221100",
	})
	docker.containers[trackingKey] = foreignID
	if destroyErr := instance.Destroy(context.Background()); !errors.Is(destroyErr, ErrContainerOwnership) {
		t.Fatalf("foreign Destroy error = %v, want ErrContainerOwnership", destroyErr)
	}
	hasDestroyFailure := false
	for _, event := range audit.Events() {
		if event.Kind == policy.AuditSandboxDestroyFailed {
			hasDestroyFailure = true
		}
	}
	if !hasDestroyFailure {
		t.Fatalf("Destroy ownership denial was not audited: %#v", audit.Events())
	}
	beforeDenial := len(audit.Events())
	if _, executeErr := instance.Execute(context.Background(), Request{
		Command:  []string{"agentdock-sandbox-helper", "probe", "user"},
		ToolName: "destroy-owner-denial",
	}); !errors.Is(executeErr, ErrDestroyed) {
		t.Fatalf("Execute after failed Destroy error = %v, want ErrDestroyed", executeErr)
	}
	afterDenial := audit.Events()
	if len(afterDenial) != beforeDenial+1 || afterDenial[len(afterDenial)-1].Kind != policy.AuditPolicyDenied {
		t.Fatalf("Execute after failed Destroy was not denied and audited: %#v", afterDenial)
	}
	if output, inspectErr := exec.Command("docker", "inspect", foreignID).CombinedOutput(); inspectErr != nil {
		t.Fatalf("foreign container was removed: %v\n%s", inspectErr, output)
	}
	delete(docker.containers, trackingKey)
	removeForeignContainer(t, foreignID)
	if err := instance.Destroy(context.Background()); err != nil {
		t.Fatalf("Destroy retry after foreign cleanup: %v", err)
	}
}

func createForeignContainer(
	t *testing.T,
	name string,
	image string,
	labels map[string]string,
) string {
	t.Helper()
	arguments := []string{"create", "--name", name}
	for _, key := range []string{
		"agentdock.phase",
		"agentdock.run_id",
		"agentdock.attempt_id",
		"agentdock.owner_token",
	} {
		arguments = append(arguments, "--label", key+"="+labels[key])
	}
	arguments = append(arguments, image, "agentdock-sandbox-helper", "probe", "user")
	output, err := exec.Command("docker", arguments...).CombinedOutput()
	if err != nil {
		t.Fatalf("create foreign collision container: %v\n%s", err, output)
	}
	containerID := strings.TrimSpace(string(output))
	t.Cleanup(func() { _ = exec.Command("docker", "rm", "-f", containerID).Run() })
	return containerID
}

func removeForeignContainer(t *testing.T, containerID string) {
	t.Helper()
	if output, err := exec.Command("docker", "rm", "-f", containerID).CombinedOutput(); err != nil {
		t.Fatalf("remove foreign collision container: %v\n%s", err, output)
	}
}

func executeProbe(t *testing.T, instance Sandbox, name string) Result {
	t.Helper()
	result, err := instance.Execute(context.Background(), Request{
		Command: []string{"agentdock-sandbox-helper", "probe", name},
	})
	if err != nil {
		t.Fatalf("probe %s: %v\nstdout=%s\nstderr=%s", name, err, result.Stdout, result.Stderr)
	}
	return result
}

func assertProbeContains(t *testing.T, instance Sandbox, name, text string, stderr bool) {
	t.Helper()
	result := executeProbe(t, instance, name)
	output := result.Stdout
	if stderr {
		output = result.Stderr
	}
	if !strings.Contains(string(output), text) {
		t.Fatalf("probe %s output %q does not contain %q", name, output, text)
	}
}
