package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/agentdock/agentdock-verify/internal/policy"
	"github.com/agentdock/agentdock-verify/internal/sandbox"
	"github.com/agentdock/agentdock-verify/internal/tools"
)

func main() {
	policyPath := flag.String("policy", "configs/policy.yaml", "static YAML policy")
	image := flag.String("image", sandbox.TestImage, "pinned phase-4 sandbox image")
	auditPath := flag.String("audit", "", "JSONL audit artifact path")
	flag.Parse()
	if err := run(context.Background(), *policyPath, *image, *auditPath); err != nil {
		fmt.Fprintln(os.Stderr, "phase-4 demo:", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, policyPath, image, auditPath string) error {
	repository, cleanup, err := createFixtureRepository(ctx)
	if err != nil {
		return err
	}
	defer cleanup()
	if auditPath == "" {
		auditPath = filepath.Join(
			os.TempDir(),
			"agentdock-phase4-audit-"+time.Now().UTC().Format("20060102T150405.000000000Z")+".jsonl",
		)
	}
	recorder, err := policy.NewFileAuditRecorder(auditPath)
	if err != nil {
		return err
	}
	defer recorder.Close()
	content, err := os.ReadFile(policyPath)
	if err != nil {
		return err
	}
	config, err := policy.Parse(content)
	if err != nil {
		return err
	}
	registry, err := tools.NewBuiltinRegistry()
	if err != nil {
		return err
	}
	beforeDigest, err := repositoryDigest(ctx, repository)
	if err != nil {
		return err
	}
	beforeStatus, err := git(ctx, repository, "status", "--porcelain=v1")
	if err != nil {
		return err
	}
	fmt.Printf("origin_before digest=%s status=%q\n", beforeDigest, beforeStatus)

	provider := sandbox.NewDockerProvider(sandbox.DockerConfig{
		Image:          image,
		CPU:            "0.5",
		Memory:         "256m",
		PIDs:           64,
		CommandTimeout: 60 * time.Second,
		OutputLimit:    1 << 20,
		Audit:          recorder,
	})
	instance, err := provider.Create(ctx, sandbox.Spec{
		RunID: "phase4-demo", AttemptID: "attempt-1", Repository: repository, Revision: "HEAD",
	})
	if err != nil {
		return err
	}
	destroyed := false
	defer func() {
		if !destroyed {
			_ = instance.Destroy(context.Background())
		}
	}()
	fmt.Printf("sandbox_created workspace=%s\n", instance.Workspace())
	service := tools.NewService(registry, policy.New(config, recorder), instance, recorder)

	for _, invocation := range []tools.Invocation{
		toolInvocation("repo.list", `{"path":"."}`),
		toolInvocation("repo.read", `{"path":"main.go","startLine":1,"endLine":20}`),
		toolInvocation("repo.search", `{"path":".","pattern":"before"}`),
	} {
		response, invokeErr := service.Invoke(ctx, invocation)
		if invokeErr != nil {
			return fmt.Errorf("%s: %w", invocation.ToolName, invokeErr)
		}
		fmt.Printf("%s exit=%d output=%q\n", invocation.ToolName, response.ExitCode, strings.TrimSpace(response.Stdout))
	}
	patchResponse, err := service.Invoke(ctx, toolInvocation(
		"repo.apply_patch",
		`{"path":"main.go","old":"return \"before\"","new":"return \"after\""}`,
	))
	if err != nil {
		return err
	}
	fmt.Printf("worktree_modified exit=%d output=%q\n", patchResponse.ExitCode, strings.TrimSpace(patchResponse.Stdout))

	_, sensitiveErr := service.Invoke(ctx, toolInvocation("repo.read", `{"path":"/etc/passwd"}`))
	if !errors.Is(sensitiveErr, policy.ErrUnsafePath) {
		return fmt.Errorf("absolute host path was not rejected: %v", sensitiveErr)
	}
	fmt.Printf("host_sensitive_path_rejected error=%q\n", sensitiveErr)

	testResponse, err := service.Invoke(ctx, toolInvocation(
		"repo.test",
		`{"packages":["./..."],"verbose":true}`,
	))
	if err != nil {
		return fmt.Errorf("repo.test: %w\n%s", err, testResponse.Stderr)
	}
	fmt.Printf("network_attempt_and_tests exit=%d output=%q\n", testResponse.ExitCode, strings.TrimSpace(testResponse.Stdout))

	if err := instance.Destroy(ctx); err != nil {
		return err
	}
	destroyed = true
	if containers := provider.ActiveContainers(ctx); len(containers) != 0 {
		return fmt.Errorf("phase-4 containers remain: %v", containers)
	}
	afterDigest, err := repositoryDigest(ctx, repository)
	if err != nil {
		return err
	}
	afterStatus, err := git(ctx, repository, "status", "--porcelain=v1")
	if err != nil {
		return err
	}
	fmt.Printf("origin_after digest=%s status=%q\n", afterDigest, afterStatus)
	if beforeDigest != afterDigest || beforeStatus != afterStatus {
		return errors.New("origin repository changed")
	}
	fmt.Printf("destroy_verified containers=0 worktree_removed=true origin_unchanged=true\n")
	auditArtifact, err := recorder.Artifact()
	if err != nil {
		return err
	}
	fmt.Printf(
		"audit_artifact path=%s digest=%s size=%d type=%s\n",
		auditArtifact.Path,
		auditArtifact.Digest,
		auditArtifact.Size,
		auditArtifact.Type,
	)
	return nil
}

func toolInvocation(name, input string) tools.Invocation {
	return tools.Invocation{
		RunID: "phase4-demo", AttemptID: "attempt-1", ToolName: name,
		Input: json.RawMessage(input),
	}
}

func createFixtureRepository(ctx context.Context) (string, func(), error) {
	repository, err := os.MkdirTemp("", "agentdock-phase4-demo-origin-")
	if err != nil {
		return "", nil, err
	}
	cleanup := func() { _ = os.RemoveAll(repository) }
	files := map[string]string{
		"go.mod":  "module example.test/phase4demo\n\ngo 1.25.0\n",
		"main.go": "package demo\n\nfunc Value() string { return \"before\" }\n",
		"main_test.go": `package demo

import (
	"fmt"
	"net"
	"testing"
	"time"
)

func TestPatchedValue(t *testing.T) {
	if Value() != "after" {
		t.Fatalf("Value() = %q", Value())
	}
}

func TestNetworkIsDenied(t *testing.T) {
	connection, err := net.DialTimeout("tcp", "1.1.1.1:80", 250*time.Millisecond)
	if err == nil {
		connection.Close()
		t.Fatal("network unexpectedly available")
	}
	fmt.Println("network-request-denied")
}
`,
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(repository, name), []byte(content), 0o600); err != nil {
			cleanup()
			return "", nil, err
		}
	}
	for _, arguments := range [][]string{
		{"init"},
		{"add", "."},
		{"-c", "user.name=AgentDock", "-c", "user.email=agentdock@example.test", "commit", "-m", "fixture"},
	} {
		if _, err := git(ctx, repository, arguments...); err != nil {
			cleanup()
			return "", nil, err
		}
	}
	return repository, cleanup, nil
}

func repositoryDigest(ctx context.Context, repository string) (string, error) {
	output, err := gitBytes(ctx, repository, "ls-files", "-z")
	if err != nil {
		return "", err
	}
	paths := strings.Split(strings.TrimSuffix(string(output), "\x00"), "\x00")
	sort.Strings(paths)
	hasher := sha256.New()
	for _, name := range paths {
		if name == "" {
			continue
		}
		content, err := os.ReadFile(filepath.Join(repository, filepath.FromSlash(name)))
		if err != nil {
			return "", err
		}
		_, _ = hasher.Write([]byte(name))
		_, _ = hasher.Write([]byte{0})
		_, _ = hasher.Write(content)
		_, _ = hasher.Write([]byte{0})
	}
	return fmt.Sprintf("sha256:%x", hasher.Sum(nil)), nil
}

func git(ctx context.Context, repository string, arguments ...string) (string, error) {
	output, err := gitBytes(ctx, repository, arguments...)
	return strings.TrimSpace(string(output)), err
}

func gitBytes(ctx context.Context, repository string, arguments ...string) ([]byte, error) {
	command := exec.CommandContext(ctx, "git", arguments...)
	command.Dir = repository
	command.Env = append(os.Environ(), "GIT_TRACE2_EVENT=0")
	output, err := command.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("git %s: %w: %s", strings.Join(arguments, " "), err, output)
	}
	return output, nil
}
