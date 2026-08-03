package reasoner_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/agentdock/agentdock-verify/internal/policy"
	"github.com/agentdock/agentdock-verify/internal/reasoner"
	"github.com/agentdock/agentdock-verify/internal/sandbox"
	"github.com/agentdock/agentdock-verify/internal/tools"
)

type codingAgentSandbox struct {
	workspace string
	scope     sandbox.Scope
	requests  []sandbox.Request
}

func (instance *codingAgentSandbox) Execute(_ context.Context, request sandbox.Request) (sandbox.Result, error) {
	instance.requests = append(instance.requests, request)
	return sandbox.Result{Stdout: []byte("ok")}, nil
}
func (instance *codingAgentSandbox) Scope() sandbox.Scope          { return instance.scope }
func (instance *codingAgentSandbox) Workspace() string             { return instance.workspace }
func (instance *codingAgentSandbox) Destroy(context.Context) error { return nil }

func TestCodingAgentRoutesEveryToolCallThroughToolInvoker(t *testing.T) {
	request := phase5Request(t, 100)
	cassette := reasoner.Cassette{
		Version:               reasoner.CurrentCassetteVersion,
		SystemContractVersion: reasoner.CodingAgentSystemContractVersion,
		ScenarioID:            "normalize-name",
		RecordingMode:         "recorded",
		Redacted:              true,
		Turns: [][]reasoner.Event{
			{
				{Type: reasoner.EventToolCall, ToolCall: &reasoner.ToolCall{ID: "call-1", Name: "repo.read", Arguments: `{"path":"internal/user/name.go"}`}},
				{Type: reasoner.EventUsage, Usage: &reasoner.Usage{TotalTokens: 10}},
				{Type: reasoner.EventFinish, Finish: &reasoner.Finish{Reason: "tool_calls"}},
			},
			{
				{Type: reasoner.EventTextDelta, Text: "done"},
				{Type: reasoner.EventUsage, Usage: &reasoner.Usage{TotalTokens: 8}},
				{Type: reasoner.EventFinish, Finish: &reasoner.Finish{Reason: "stop"}},
			},
		},
	}
	replay, err := reasoner.NewReplayReasoner(cassette)
	if err != nil {
		t.Fatal(err)
	}
	registry, err := tools.NewBuiltinRegistry()
	if err != nil {
		t.Fatal(err)
	}
	audit := policy.NewMemoryAuditRecorder()
	engine := policy.New(policy.Config{Version: "phase5-test", Default: "deny", Rules: []policy.Rule{{
		Tool: "repo.read", Capability: "repo:read", ReadOnly: true, Paths: []string{"."},
		MaxTimeout: 10 * time.Second, MaxOutputBytes: 256 << 10,
	}}}, audit)
	instance := &codingAgentSandbox{
		workspace: t.TempDir(), scope: sandbox.Scope{RunID: "run-phase5", AttemptID: "attempt-1"},
	}
	service := tools.NewService(registry, engine, instance, audit)
	agent, err := reasoner.NewCodingAgent(replay, service, 4)
	if err != nil {
		t.Fatalf("NewCodingAgent() error = %v", err)
	}
	runRequest := reasoner.CodingAgentRequest{RunID: "run-phase5", AttemptID: "attempt-1", Reasoner: request}
	result, err := agent.Run(context.Background(), runRequest)
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if result.Text != "done" || len(instance.requests) != 1 {
		t.Fatalf("result=%#v sandbox requests=%#v", result, instance.requests)
	}
	sandboxRequest := instance.requests[0]
	if sandboxRequest.ToolName != "repo.read" || len(sandboxRequest.Command) != 3 || sandboxRequest.Command[1] != "read" {
		t.Fatalf("Tool Service did not construct scoped repo.read request: %#v", sandboxRequest)
	}
	var payload tools.Response
	if err := json.Unmarshal([]byte(result.Messages[len(result.Messages)-2].Content), &payload); err != nil || payload.Stdout != "ok" {
		t.Fatalf("Tool response transcript = %q, error = %v", result.Messages[len(result.Messages)-2].Content, err)
	}
}

func TestCodingAgentRejectsReasonerAndServiceContractDriftBeforeReasoning(t *testing.T) {
	request := phase5Request(t, 100)
	request.Tools[0].AllowedPaths = []string{"internal/user/"}
	fake := reasoner.NewFakeReasonerWithEvents(
		reasoner.Event{Type: reasoner.EventUsage, Usage: &reasoner.Usage{TotalTokens: 1}},
		reasoner.Event{Type: reasoner.EventFinish, Finish: &reasoner.Finish{Reason: "stop"}},
	)
	service, _ := codingToolService(t, ".")
	agent, err := reasoner.NewCodingAgent(fake, service, 2)
	if err != nil {
		t.Fatal(err)
	}
	_, err = agent.Run(context.Background(), reasoner.CodingAgentRequest{
		RunID: "run-phase5", AttemptID: "attempt-1", Reasoner: request,
	})
	if err == nil || fake.CallCount() != 0 {
		t.Fatalf("contract drift error = %v, Reasoner calls = %d; want pre-Reasoner rejection", err, fake.CallCount())
	}
}

func TestCodingAgentScopedContractsRejectOtherScenarioPathBeforeSandbox(t *testing.T) {
	service, instance := codingToolService(t, "internal/user/")
	contracts, err := service.Contracts()
	if err != nil {
		t.Fatal(err)
	}
	cassette := reasoner.Cassette{
		Version: reasoner.CurrentCassetteVersion, SystemContractVersion: reasoner.CodingAgentSystemContractVersion,
		ScenarioID: "cross-scenario-path", RecordingMode: "recorded", Redacted: true,
		Turns: [][]reasoner.Event{{
			{Type: reasoner.EventToolCall, ToolCall: &reasoner.ToolCall{ID: "cross", Name: "repo.read", Arguments: `{"path":"internal/mathutil/divide.go"}`}},
			{Type: reasoner.EventUsage, Usage: &reasoner.Usage{TotalTokens: 1}},
			{Type: reasoner.EventFinish, Finish: &reasoner.Finish{Reason: "tool_calls"}},
		}},
	}
	replay, err := reasoner.NewReplayReasoner(cassette)
	if err != nil {
		t.Fatal(err)
	}
	agent, err := reasoner.NewCodingAgent(replay, service, 2)
	if err != nil {
		t.Fatal(err)
	}
	_, err = agent.Run(context.Background(), reasoner.CodingAgentRequest{
		RunID: "run-phase5", AttemptID: "attempt-1",
		Reasoner: reasoner.Request{
			Messages: []reasoner.Message{{Role: reasoner.RoleUser, Content: "cross path"}}, Tools: contracts,
			TaskSummary: "cross path", Budget: reasoner.Budget{TokenLimit: 10},
		},
	})
	if !errors.Is(err, policy.ErrDenied) || len(instance.requests) != 0 {
		t.Fatalf("out-of-scope error = %v, Sandbox requests = %#v; want denial before Sandbox", err, instance.requests)
	}
}

func codingToolService(t *testing.T, allowedPath string) (*tools.Service, *codingAgentSandbox) {
	t.Helper()
	builtin, err := tools.NewBuiltinRegistry()
	if err != nil {
		t.Fatal(err)
	}
	contracts := builtin.Contracts()
	for index := range contracts {
		contracts[index].AllowedPaths = []string{allowedPath}
	}
	registry, err := tools.NewRegistry(contracts...)
	if err != nil {
		t.Fatal(err)
	}
	audit := policy.NewMemoryAuditRecorder()
	rules := make([]policy.Rule, 0, len(contracts))
	for _, contract := range contracts {
		rules = append(rules, policy.Rule{
			Tool: contract.Name, Capability: contract.Capability, ReadOnly: contract.ReadOnly,
			Paths: []string{allowedPath}, Network: contract.Network,
			MaxTimeout: contract.Timeout, MaxOutputBytes: contract.OutputLimit,
		})
	}
	engine := policy.New(policy.Config{Version: "phase5-test", Default: "deny", Rules: rules}, audit)
	instance := &codingAgentSandbox{
		workspace: t.TempDir(), scope: sandbox.Scope{RunID: "run-phase5", AttemptID: "attempt-1"},
	}
	return tools.NewService(registry, engine, instance, audit), instance
}
