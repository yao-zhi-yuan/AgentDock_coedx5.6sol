package domain_test

import (
	"testing"

	"github.com/agentdock/agentdock-verify/internal/domain"
)

func TestDecideUsesStableActionIDForTheSameState(t *testing.T) {
	state, err := domain.Reduce([]domain.Event{
		event(1, domain.EventRunCreated, "created", domain.EventData{}),
		event(2, domain.EventAttemptStarted, "attempt", domain.EventData{AttemptID: "attempt-001"}),
		event(3, domain.EventWorkspaceProvisioned, "workspace", domain.EventData{}),
	})
	if err != nil {
		t.Fatalf("Reduce() error = %v", err)
	}

	first := domain.Decide(state)
	second := domain.Decide(state)
	if first.Type != domain.CommandRunReasoner {
		t.Fatalf("Decide() type = %s, want %s", first.Type, domain.CommandRunReasoner)
	}
	if first.ActionID == "" || first.ActionID != second.ActionID {
		t.Fatalf("action IDs are not stable: first=%q second=%q", first.ActionID, second.ActionID)
	}
}

func TestDecideReusesDurablePendingActionID(t *testing.T) {
	state, err := domain.Reduce([]domain.Event{
		event(1, domain.EventRunCreated, "created", domain.EventData{}),
		event(2, domain.EventAttemptStarted, "attempt", domain.EventData{AttemptID: "attempt-001"}),
		event(3, domain.EventWorkspaceProvisioned, "workspace", domain.EventData{}),
		event(4, domain.EventReasoningPlanned, "reason-plan", domain.EventData{ActionID: "durable-action-id"}),
	})
	if err != nil {
		t.Fatalf("Reduce() error = %v", err)
	}

	got := domain.Decide(state)
	if got.Type != domain.CommandRunReasoner || got.ActionID != "durable-action-id" {
		t.Fatalf("Decide(pending) = type:%s action:%q, want RunReasoner/durable-action-id", got.Type, got.ActionID)
	}
}

func TestPauseResumePreservesDurablePendingActionID(t *testing.T) {
	state, err := domain.Reduce([]domain.Event{
		event(1, domain.EventRunCreated, "created", domain.EventData{}),
		event(2, domain.EventAttemptStarted, "attempt", domain.EventData{AttemptID: "attempt-001"}),
		event(3, domain.EventWorkspaceProvisioned, "workspace", domain.EventData{}),
		event(4, domain.EventReasoningPlanned, "reason-plan", domain.EventData{ActionID: "durable-action-id"}),
		event(5, domain.EventDesiredStateChanged, "pause", domain.EventData{DesiredState: domain.DesiredPaused}),
		event(6, domain.EventDesiredStateChanged, "resume", domain.EventData{DesiredState: domain.DesiredRunning}),
	})
	if err != nil {
		t.Fatalf("Reduce() error = %v", err)
	}

	got := domain.Decide(state)
	if got.Type != domain.CommandRunReasoner || got.ActionID != "durable-action-id" {
		t.Fatalf("Decide(resumed pending) = type:%s action:%q, want RunReasoner/durable-action-id", got.Type, got.ActionID)
	}
}

func TestDecideReturnsNoopWhenPausedOrTerminal(t *testing.T) {
	paused, err := domain.Reduce([]domain.Event{
		event(1, domain.EventRunCreated, "created", domain.EventData{}),
		event(2, domain.EventDesiredStateChanged, "pause", domain.EventData{DesiredState: domain.DesiredPaused}),
	})
	if err != nil {
		t.Fatalf("reduce paused state: %v", err)
	}
	if got := domain.Decide(paused); got.Type != domain.CommandNoop {
		t.Fatalf("Decide(paused) = %s, want Noop", got.Type)
	}

	terminal, err := domain.Reduce(successfulEvents())
	if err != nil {
		t.Fatalf("reduce terminal state: %v", err)
	}
	if got := domain.Decide(terminal); got.Type != domain.CommandNoop {
		t.Fatalf("Decide(terminal) = %s, want Noop", got.Type)
	}
}

func TestDecideCancelIntentPreemptsNormalWork(t *testing.T) {
	state, err := domain.Reduce([]domain.Event{
		event(1, domain.EventRunCreated, "created", domain.EventData{}),
		event(2, domain.EventDesiredStateChanged, "cancel", domain.EventData{DesiredState: domain.DesiredCancelled}),
	})
	if err != nil {
		t.Fatalf("Reduce() error = %v", err)
	}

	got := domain.Decide(state)
	if got.Type != domain.CommandCancelRun {
		t.Fatalf("Decide(cancel intent) = %s, want %s", got.Type, domain.CommandCancelRun)
	}
}

func TestDecideCancelIntentPreemptsPausedNoop(t *testing.T) {
	state, err := domain.Reduce([]domain.Event{
		event(1, domain.EventRunCreated, "created", domain.EventData{}),
		event(2, domain.EventDesiredStateChanged, "pause", domain.EventData{DesiredState: domain.DesiredPaused}),
		event(3, domain.EventDesiredStateChanged, "cancel", domain.EventData{DesiredState: domain.DesiredCancelled}),
	})
	if err != nil {
		t.Fatalf("Reduce() error = %v", err)
	}

	got := domain.Decide(state)
	if got.Type != domain.CommandCancelRun {
		t.Fatalf("Decide(paused cancel intent) = %s, want %s", got.Type, domain.CommandCancelRun)
	}
}

func TestToolCallFailedPrefixDecidesStableFailRun(t *testing.T) {
	events := []domain.Event{
		event(1, domain.EventRunCreated, "created", domain.EventData{}),
		event(2, domain.EventAttemptStarted, "attempt", domain.EventData{AttemptID: "attempt-001"}),
		event(3, domain.EventWorkspaceProvisioned, "workspace", domain.EventData{}),
		event(4, domain.EventReasoningPlanned, "reason-plan", domain.EventData{ActionID: "action-reason"}),
		event(5, domain.EventToolCallFailed, "tool-failed", domain.EventData{
			ActionID: "action-reason",
			Reason:   "illegal tool",
		}),
	}
	state, err := domain.Reduce(events)
	if err != nil {
		t.Fatalf("Reduce(prefix) error = %v", err)
	}
	if state.Run.ObservedState != domain.StatusReasoning ||
		state.PendingActionID != "" ||
		state.FailureReason != "illegal tool" {
		t.Fatalf("failed prefix state = %#v", state)
	}

	first := domain.Decide(state)
	second := domain.Decide(state)
	if first.Type != domain.CommandFailRun {
		t.Fatalf("Decide(failed prefix) = %s, want %s", first.Type, domain.CommandFailRun)
	}
	if first.ActionID == "" || first.ActionID != second.ActionID {
		t.Fatalf("FailRun action ID is not stable: first=%q second=%q", first.ActionID, second.ActionID)
	}

	events = append(events, event(6, domain.EventRunFailed, "run-failed", domain.EventData{
		ActionID: first.ActionID,
		Reason:   state.FailureReason,
	}))
	failed, err := domain.Reduce(events)
	if err != nil {
		t.Fatalf("Reduce(prefix + RunFailed) error = %v", err)
	}
	if failed.Run.ObservedState != domain.StatusFailed {
		t.Fatalf("terminal state = %s, want Failed", failed.Run.ObservedState)
	}
}
