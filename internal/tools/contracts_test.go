package tools

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/agentdock/agentdock-verify/internal/policy"
	"github.com/agentdock/agentdock-verify/internal/sandbox"
)

func TestBuiltinContractsAreCompleteAndLimitedToPhase4Surface(t *testing.T) {
	registry, err := NewBuiltinRegistry()
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"repo.apply_patch",
		"repo.list",
		"repo.read",
		"repo.search",
		"repo.test",
	}
	if got := registry.Names(); !equalStrings(got, want) {
		t.Fatalf("builtin tools = %v, want %v", got, want)
	}
	for _, name := range want {
		contract, ok := registry.Get(name)
		if !ok {
			t.Fatalf("missing contract %s", name)
		}
		if err := contract.Validate(); err != nil {
			t.Fatalf("%s contract: %v", name, err)
		}
		if contract.Name == "" ||
			contract.Version == "" ||
			len(contract.InputSchema) == 0 ||
			len(contract.OutputSchema) == 0 ||
			contract.Capability == "" ||
			contract.Timeout <= 0 ||
			contract.OutputLimit <= 0 ||
			len(contract.AllowedPaths) == 0 ||
			contract.Idempotency == "" {
			t.Fatalf("incomplete contract: %#v", contract)
		}
	}
	if _, ok := registry.Get("host.exec"); ok {
		t.Fatal("arbitrary host shell must not be registered")
	}
}

func TestRegistryOwnsAndReturnsImmutableContractSnapshots(t *testing.T) {
	registry, err := NewBuiltinRegistry()
	if err != nil {
		t.Fatal(err)
	}
	contracts := registry.Contracts()
	if len(contracts) == 0 {
		t.Fatal("Registry.Contracts returned no contracts")
	}
	name := contracts[0].Name
	contracts[0].Name = "host.exec"
	contracts[0].AllowedPaths[0] = "/"
	contracts[0].InputSchema[0] = 'x'
	contract, ok := registry.Get(name)
	if !ok {
		t.Fatalf("registry lost %q after snapshot mutation", name)
	}
	if contract.Name != name || contract.AllowedPaths[0] == "/" || contract.InputSchema[0] == 'x' {
		t.Fatalf("caller mutation changed Registry contract: %#v", contract)
	}
	contract.AllowedPaths[0] = "/"
	again, _ := registry.Get(name)
	if again.AllowedPaths[0] == "/" {
		t.Fatal("Registry.Get returned caller-mutable contract backing storage")
	}
}

func TestContractValidationRejectsMissingUnknownAndWrongTypedInput(t *testing.T) {
	registry, err := NewBuiltinRegistry()
	if err != nil {
		t.Fatal(err)
	}
	contract, _ := registry.Get("repo.read")
	for name, input := range map[string]string{
		"missing":  `{"startLine":1}`,
		"unknown":  `{"path":"README.md","hostCommand":"id"}`,
		"type":     `{"path":42}`,
		"minimum":  `{"path":"README.md","startLine":0}`,
		"trailing": `{"path":"README.md"}{"path":"other"}`,
	} {
		t.Run(name, func(t *testing.T) {
			if err := contract.ValidateInput(json.RawMessage(input)); !errors.Is(err, ErrSchemaValidation) {
				t.Fatalf("ValidateInput error = %v, want ErrSchemaValidation", err)
			}
		})
	}
	if err := contract.ValidateInput(json.RawMessage(`{"path":"README.md","startLine":1,"endLine":20}`)); err != nil {
		t.Fatalf("valid input rejected: %v", err)
	}
}

func TestServiceRejectsReplayablePatchAndEndLineWithoutStartLine(t *testing.T) {
	registry, err := NewBuiltinRegistry()
	if err != nil {
		t.Fatal(err)
	}
	service := &Service{sandbox: &fakeSandbox{workspace: t.TempDir()}}
	patch, _ := registry.Get("repo.apply_patch")
	if _, _, _, err := service.buildRequest(
		patch,
		json.RawMessage(`{"path":"main.go","old":"x","new":"xy"}`),
	); !errors.Is(err, ErrSchemaValidation) {
		t.Fatalf("replayable patch error = %v, want ErrSchemaValidation", err)
	}
	read, _ := registry.Get("repo.read")
	if _, _, _, err := service.buildRequest(
		read,
		json.RawMessage(`{"path":"main.go","endLine":5}`),
	); !errors.Is(err, ErrSchemaValidation) {
		t.Fatalf("endLine without startLine error = %v, want ErrSchemaValidation", err)
	}
}

func TestRegistryRejectsUnsupportedOrMalformedSchemaKeywords(t *testing.T) {
	base := builtinContracts()[0]
	for name, invalid := range map[string]string{
		"unsupported keyword": `{"type":"object","pattern":"x","properties":{}}`,
		"required type":       `{"type":"object","required":"path","properties":{"path":{"type":"string"}}}`,
		"child keyword":       `{"type":"object","properties":{"path":{"type":"string","maxLength":4}}}`,
		"fractional minimum":  `{"type":"object","properties":{"count":{"type":"integer","minimum":1.5}}}`,
	} {
		t.Run(name, func(t *testing.T) {
			candidate := base
			candidate.InputSchema = json.RawMessage(invalid)
			if _, err := NewRegistry(candidate); !errors.Is(err, ErrInvalidContract) {
				t.Fatalf("NewRegistry error = %v, want ErrInvalidContract", err)
			}
		})
	}
}

func TestServiceAlwaysValidatesContractPolicyAndAudits(t *testing.T) {
	registry, err := NewBuiltinRegistry()
	if err != nil {
		t.Fatal(err)
	}
	contract, _ := registry.Get("repo.read")
	audit := policy.NewMemoryAuditRecorder()
	engine := policy.New(policy.Config{
		Version: "v1",
		Default: "deny",
		Rules: []policy.Rule{{
			Tool:           "repo.read",
			Capability:     "repo:read",
			ReadOnly:       true,
			Paths:          []string{"."},
			MaxTimeout:     10 * time.Second,
			MaxOutputBytes: 256 << 10,
		}},
	}, audit)
	fake := &fakeSandbox{
		workspace: t.TempDir(),
		result:    sandbox.Result{Stdout: []byte("hello")},
		scope:     sandbox.Scope{RunID: "run-1", AttemptID: "attempt-1"},
	}
	service := NewService(registry, engine, fake, audit)

	response, err := service.Invoke(context.Background(), Invocation{
		RunID:     "run-1",
		AttemptID: "attempt-1",
		ToolName:  "repo.read",
		Input:     json.RawMessage(`{"path":"README.md"}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if fake.calls != 1 || response.ExitCode != 0 {
		t.Fatalf("sandbox calls=%d response=%#v", fake.calls, response)
	}

	_, err = service.Invoke(context.Background(), Invocation{
		RunID:     "run-1",
		AttemptID: "attempt-1",
		ToolName:  "host.exec",
		Input:     json.RawMessage(`{"command":"id"}`),
	})
	if !errors.Is(err, ErrUnknownTool) {
		t.Fatalf("host.exec error = %v, want ErrUnknownTool", err)
	}
	if fake.calls != 1 {
		t.Fatalf("unknown tool reached sandbox, calls=%d", fake.calls)
	}

	restricted := contract
	restricted.AllowedPaths = []string{"docs/"}
	restrictedRegistry, err := NewRegistry(restricted)
	if err != nil {
		t.Fatal(err)
	}
	restrictedService := NewService(restrictedRegistry, engine, fake, audit)
	_, err = restrictedService.Invoke(context.Background(), Invocation{
		RunID:     "run-1",
		AttemptID: "attempt-1",
		ToolName:  "repo.read",
		Input:     json.RawMessage(`{"path":"README.md"}`),
	})
	if !errors.Is(err, policy.ErrDenied) {
		t.Fatalf("contract path error = %v, want policy ErrDenied", err)
	}
	if fake.calls != 1 {
		t.Fatalf("contract-denied path reached sandbox, calls=%d", fake.calls)
	}
	events := audit.Events()
	if len(events) < 3 {
		t.Fatalf("audit events = %#v, want allow, execution, and deny evidence", events)
	}
}

func TestServiceReportsAuditFailureAfterExecution(t *testing.T) {
	registry, err := NewBuiltinRegistry()
	if err != nil {
		t.Fatal(err)
	}
	policyAudit := policy.NewMemoryAuditRecorder()
	engine := policy.New(policy.Config{
		Rules: []policy.Rule{{
			Tool: "repo.read", Capability: "repo:read", ReadOnly: true,
			Paths: []string{"."}, MaxTimeout: 10 * time.Second, MaxOutputBytes: 256 << 10,
		}},
	}, policyAudit)
	fake := &fakeSandbox{
		workspace: t.TempDir(),
		scope:     sandbox.Scope{RunID: "run", AttemptID: "attempt"},
	}
	service := NewService(registry, engine, fake, failingToolAudit{})
	_, err = service.Invoke(context.Background(), Invocation{
		RunID: "run", AttemptID: "attempt", ToolName: "repo.read",
		Input: json.RawMessage(`{"path":"README.md"}`),
	})
	if err == nil || !strings.Contains(err.Error(), "persist Tool execution audit") {
		t.Fatalf("Invoke error = %v", err)
	}
	if fake.calls != 1 {
		t.Fatalf("sandbox calls = %d, want 1", fake.calls)
	}
}

func TestServiceRejectsMismatchedScopeWithoutExecuting(t *testing.T) {
	registry, err := NewBuiltinRegistry()
	if err != nil {
		t.Fatal(err)
	}
	audit := policy.NewMemoryAuditRecorder()
	engine := policy.New(policy.Config{Version: "policy-v1"}, audit)
	fake := &fakeSandbox{
		workspace: t.TempDir(),
		scope:     sandbox.Scope{RunID: "bound-run", AttemptID: "bound-attempt"},
	}
	service := NewService(registry, engine, fake, audit)
	_, err = service.Invoke(context.Background(), Invocation{
		RunID: "other-run", AttemptID: "other-attempt", ToolName: "repo.read",
		Input: json.RawMessage(`{"path":"README.md"}`),
	})
	if !errors.Is(err, ErrScopeMismatch) {
		t.Fatalf("Invoke error = %v, want ErrScopeMismatch", err)
	}
	if fake.calls != 0 {
		t.Fatalf("mismatched scope reached sandbox, calls=%d", fake.calls)
	}
	events := audit.Events()
	if len(events) != 1 ||
		events[0].RunID != "bound-run" ||
		events[0].AttemptID != "bound-attempt" ||
		events[0].RequestedRunID != "other-run" ||
		events[0].RequestedAttemptID != "other-attempt" ||
		events[0].PolicyVersion != "policy-v1" ||
		events[0].Kind != policy.AuditPolicyDenied {
		t.Fatalf("scope denial audit = %#v", events)
	}
}

func equalStrings(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

type fakeSandbox struct {
	calls     int
	workspace string
	result    sandbox.Result
	err       error
	scope     sandbox.Scope
}

type failingToolAudit struct{}

func (failingToolAudit) Record(context.Context, policy.AuditEvent) error {
	return errors.New("audit unavailable")
}

func (fake *fakeSandbox) Execute(_ context.Context, request sandbox.Request) (sandbox.Result, error) {
	fake.calls++
	return fake.result, fake.err
}

func (fake *fakeSandbox) Workspace() string {
	return fake.workspace
}

func (fake *fakeSandbox) Scope() sandbox.Scope {
	return fake.scope
}

func (fake *fakeSandbox) Destroy(context.Context) error {
	return nil
}
