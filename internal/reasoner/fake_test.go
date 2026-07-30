package reasoner_test

import (
	"context"
	"errors"
	"testing"

	"github.com/agentdock/agentdock-verify/internal/reasoner"
)

func TestFakeReasonerReturnsFrameworkNeutralResult(t *testing.T) {
	fake := reasoner.NewFakeReasoner()

	result, err := fake.Reason(context.Background(), reasoner.Request{RunID: "run-001", AttemptID: "attempt-001"})
	if err != nil {
		t.Fatalf("Reason() error = %v", err)
	}
	if result.ToolCall == nil {
		t.Fatal("Reason() returned no phase-1 tool call")
	}
	if result.ToolCall.Name != reasoner.Phase1PatchTool {
		t.Fatalf("tool name = %q, want %q", result.ToolCall.Name, reasoner.Phase1PatchTool)
	}
	if fake.CallCount() != 1 {
		t.Fatalf("call count = %d, want 1", fake.CallCount())
	}
}

func TestFakeReasonerCanScriptIllegalToolCall(t *testing.T) {
	fake := reasoner.NewFakeReasonerWithResult(reasoner.Result{
		ToolCall: &reasoner.ToolCall{Name: "host.shell", Arguments: `{}`},
	})

	result, err := fake.Reason(context.Background(), reasoner.Request{RunID: "run-001"})
	if err != nil {
		t.Fatalf("Reason() error = %v", err)
	}
	err = reasoner.ValidatePhase1Result(result)
	if !errors.Is(err, reasoner.ErrIllegalToolCall) {
		t.Fatalf("ValidatePhase1Result() error = %v, want illegal tool call", err)
	}
}
