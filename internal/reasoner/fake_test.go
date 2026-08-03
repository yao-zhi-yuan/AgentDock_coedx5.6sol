package reasoner_test

import (
	"context"
	"errors"
	"testing"

	"github.com/agentdock/agentdock-verify/internal/reasoner"
)

func TestFakeReasonerReturnsFrameworkNeutralResult(t *testing.T) {
	fake := reasoner.NewFakeReasoner()

	result, err := reasoner.Collect(fake.Reason(context.Background(), reasoner.Request{
		Budget: reasoner.Budget{TokenLimit: 1},
	}))
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
		ToolCall: &reasoner.ToolCall{ID: "bad-call", Name: "host.shell", Arguments: `{}`},
	})

	_, err := reasoner.Collect(fake.Reason(context.Background(), reasoner.Request{
		Budget: reasoner.Budget{TokenLimit: 1},
	}))
	var streamErr *reasoner.StreamError
	if !errors.As(err, &streamErr) || streamErr.Class != reasoner.ErrorInvalidTool {
		t.Fatalf("Collect() error = %v, want invalid Tool", err)
	}
}
