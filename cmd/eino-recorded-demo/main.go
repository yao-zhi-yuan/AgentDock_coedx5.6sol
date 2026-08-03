package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/agentdock/agentdock-verify/internal/policy"
	"github.com/agentdock/agentdock-verify/internal/reasoner"
	"github.com/agentdock/agentdock-verify/internal/sandbox"
	"github.com/agentdock/agentdock-verify/internal/tools"
)

type recordedScenario struct {
	id              string
	cassette        string
	prompt          string
	failureEvidence string
	allowedPath     string
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "demo-eino-recorded:", err)
		os.Exit(1)
	}
}

func run() error {
	repositoryRoot, err := os.Getwd()
	if err != nil {
		return err
	}
	policyBytes, err := os.ReadFile(filepath.Join(repositoryRoot, "configs", "policy.yaml"))
	if err != nil {
		return err
	}
	policyConfig, err := policy.Parse(policyBytes)
	if err != nil {
		return err
	}
	registry, err := tools.NewBuiltinRegistry()
	if err != nil {
		return err
	}
	baseContracts := registry.Contracts()

	scenarios := []recordedScenario{
		{
			id: "normalize-name", cassette: "phase5-normalize-name.json",
			prompt:          "Fix NormalizeName so surrounding whitespace is removed before lowercasing.",
			failureEvidence: "TestNormalizeNameTrimsWhitespace: got surrounding spaces",
			allowedPath:     "internal/user/",
		},
		{
			id: "divide-zero", cassette: "phase5-divide-zero.json",
			prompt:          "Fix Divide so a non-positive count returns zero.",
			failureEvidence: "TestDivideNonPositiveCountReturnsZero: integer divide by zero",
			allowedPath:     "internal/mathutil/",
		},
	}
	for _, scenario := range scenarios {
		if err := runScenario(repositoryRoot, policyConfig, baseContracts, scenario); err != nil {
			return fmt.Errorf("scenario %s: %w", scenario.id, err)
		}
	}
	fmt.Printf("recorded_demo_complete scenarios=%d credentials=not-required phase6_verification=not-implemented\n", len(scenarios))
	return nil
}

func runScenario(
	repositoryRoot string,
	policyConfig policy.Config,
	baseContracts []tools.Contract,
	scenario recordedScenario,
) (resultErr error) {
	contracts := make([]tools.Contract, len(baseContracts))
	for index, contract := range baseContracts {
		contracts[index] = contract
		contracts[index].InputSchema = append([]byte(nil), contract.InputSchema...)
		contracts[index].OutputSchema = append([]byte(nil), contract.OutputSchema...)
		contracts[index].AllowedPaths = []string{scenario.allowedPath}
	}
	registry, err := tools.NewRegistry(contracts...)
	if err != nil {
		return err
	}
	policyConfig = scopePolicy(policyConfig, scenario.allowedPath)
	temporary, err := os.MkdirTemp("", "agentdock-phase5-recorded-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(temporary)
	fixture := filepath.Join(temporary, "buggy-go-service")
	if err := os.MkdirAll(fixture, 0o700); err != nil {
		return err
	}
	if err := os.CopyFS(fixture, os.DirFS(filepath.Join(repositoryRoot, "examples", "buggy-go-service"))); err != nil {
		return fmt.Errorf("copy fixed example repository: %w", err)
	}
	if err := initializeGitRepository(fixture); err != nil {
		return err
	}
	originCommit, err := gitOutput(fixture, "rev-parse", "HEAD")
	if err != nil {
		return err
	}
	originStatus, err := gitOutput(fixture, "status", "--porcelain=v1")
	if err != nil {
		return err
	}

	cassetteFile, err := os.Open(filepath.Join(repositoryRoot, "testdata", "cassettes", scenario.cassette))
	if err != nil {
		return err
	}
	cassette, loadErr := reasoner.LoadCassette(cassetteFile)
	closeErr := cassetteFile.Close()
	if loadErr != nil || closeErr != nil {
		return errors.Join(loadErr, closeErr)
	}
	replay, err := reasoner.NewReplayReasoner(cassette)
	if err != nil {
		return err
	}

	audit := policy.NewMemoryAuditRecorder()
	provider := sandbox.NewDockerProvider(sandbox.DockerConfig{
		Image: sandbox.TestImage, WorktreeRoot: filepath.Join(temporary, "worktrees"), Audit: audit,
	})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	instance, err := provider.Create(ctx, sandbox.Spec{
		RunID: "phase5-recorded-" + scenario.id, AttemptID: "attempt-1",
		Repository: fixture, Revision: "HEAD",
	})
	if err != nil {
		if instance != nil {
			err = errors.Join(err, instance.Destroy(context.Background()))
		}
		return err
	}
	workspace := instance.Workspace()
	defer func() {
		resultErr = errors.Join(resultErr, instance.Destroy(context.Background()), provider.Cleanup(context.Background()))
	}()

	engine := policy.New(policyConfig, audit)
	service := tools.NewService(registry, engine, instance, audit)
	agent, err := reasoner.NewCodingAgent(replay, service, 8)
	if err != nil {
		return err
	}
	result, err := agent.Run(ctx, reasoner.CodingAgentRequest{
		RunID: "phase5-recorded-" + scenario.id, AttemptID: "attempt-1",
		Reasoner: reasoner.Request{
			Messages: []reasoner.Message{{Role: reasoner.RoleUser, Content: scenario.prompt}},
			Tools:    contracts, TaskSummary: scenario.prompt, FailureEvidence: scenario.failureEvidence,
			Budget: reasoner.Budget{TokenLimit: 200},
		},
	})
	if err != nil {
		return err
	}
	if result.ToolCallCount != 3 || result.Turns != 4 || result.Text == "" {
		return fmt.Errorf("unexpected Coding Agent result: turns=%d tools=%d text=%q", result.Turns, result.ToolCallCount, result.Text)
	}
	if err := instance.Destroy(context.Background()); err != nil {
		return err
	}
	if _, err := os.Stat(workspace); !os.IsNotExist(err) {
		return fmt.Errorf("disposable workspace still exists: %v", err)
	}
	if active := provider.ActiveContainers(context.Background()); len(active) != 0 {
		return fmt.Errorf("owned containers remain: %v", active)
	}
	afterCommit, err := gitOutput(fixture, "rev-parse", "HEAD")
	if err != nil {
		return err
	}
	afterStatus, err := gitOutput(fixture, "status", "--porcelain=v1")
	if err != nil {
		return err
	}
	if strings.TrimSpace(originCommit) != strings.TrimSpace(afterCommit) || originStatus != afterStatus || strings.TrimSpace(afterStatus) != "" {
		return fmt.Errorf("origin repository changed: commit %q -> %q status %q -> %q", originCommit, afterCommit, originStatus, afterStatus)
	}
	fmt.Printf(
		"recorded_scenario id=%s allowed_path=%s turns=%d tool_calls=%d total_tokens=%d origin_unchanged=true containers=0 audit_events=%d\n",
		scenario.id, scenario.allowedPath, result.Turns, result.ToolCallCount, result.Usage.TotalTokens, len(audit.Events()),
	)
	return nil
}

func scopePolicy(config policy.Config, allowedPath string) policy.Config {
	scoped := config
	scoped.Environment.Allow = append([]string(nil), config.Environment.Allow...)
	scoped.Rules = make([]policy.Rule, len(config.Rules))
	for index, rule := range config.Rules {
		scoped.Rules[index] = rule
		scoped.Rules[index].Paths = []string{allowedPath}
	}
	return scoped
}

func initializeGitRepository(repository string) error {
	commands := [][]string{
		{"init", "--quiet"},
		{"add", "--all"},
		{"commit", "--quiet", "-m", "phase5 recorded fixture"},
	}
	for _, arguments := range commands {
		if _, err := gitOutput(repository, arguments...); err != nil {
			return err
		}
	}
	return nil
}

func gitOutput(repository string, arguments ...string) (string, error) {
	safeArguments := []string{
		"-c", "core.hooksPath=/dev/null",
		"-c", "core.fsmonitor=false",
		"-c", "protocol.allow=never",
	}
	safeArguments = append(safeArguments, arguments...)
	command := exec.Command("git", safeArguments...)
	command.Dir = repository
	command.Env = []string{
		"GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_NO_LAZY_FETCH=1",
		"GIT_NO_REPLACE_OBJECTS=1", "GIT_OPTIONAL_LOCKS=0", "GIT_PROTOCOL_FROM_USER=0",
		"GIT_TERMINAL_PROMPT=0", "GIT_AUTHOR_NAME=AgentDock", "GIT_AUTHOR_EMAIL=agentdock@example.invalid",
		"GIT_COMMITTER_NAME=AgentDock", "GIT_COMMITTER_EMAIL=agentdock@example.invalid",
		"HOME=/nonexistent", "LANG=C", "LC_ALL=C", "TMPDIR=/tmp",
	}
	output, err := command.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("git %s: %w: %s", strings.Join(arguments, " "), err, strings.TrimSpace(string(output)))
	}
	return string(output), nil
}
