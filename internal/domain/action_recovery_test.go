package domain_test

import (
	"testing"

	"github.com/agentdock/agentdock-verify/internal/domain"
)

func TestActionLifecycleUsesStableIntentAndReceiptToAdvance(t *testing.T) {
	runID := "run-action-lifecycle"
	created := actionEvent(runID, 1, domain.EventRunCreated, "created", domain.EventData{
		ScenarioID: "scenario",
		SpecHash:   "spec",
	})
	initial, err := domain.Reduce([]domain.Event{created})
	if err != nil {
		t.Fatalf("Reduce(RunCreated) error = %v", err)
	}
	command := domain.Decide(initial)
	actionID := command.ActionID
	events := []domain.Event{
		created,
		actionEvent(runID, 2, domain.EventActionPlanned, actionID+":planned", domain.EventData{
			ActionID:         actionID,
			ActionType:       domain.CommandStartAttempt,
			AttemptID:        command.AttemptID,
			IdempotencyScope: domain.IdempotencyScoped,
		}),
		actionEvent(runID, 3, domain.EventActionCompleted, actionID+":completed", domain.EventData{
			ActionID:   actionID,
			ActionType: domain.CommandStartAttempt,
			AttemptID:  command.AttemptID,
			ReceiptID:  actionID,
		}),
	}

	state, err := domain.Reduce(events)
	if err != nil {
		t.Fatalf("Reduce() error = %v", err)
	}
	if state.Run.ObservedState != domain.StatusProvisioning {
		t.Fatalf("observed state = %s, want Provisioning", state.Run.ObservedState)
	}
	if state.PendingActionID != "" || state.LastCompletedActionID != actionID {
		t.Fatalf("action projection = %#v", state)
	}
}

func TestAmbiguousUnsafeActionEntersWaitingApproval(t *testing.T) {
	runID := "run-unsafe-action"
	created := actionEvent(runID, 1, domain.EventRunCreated, "created", domain.EventData{
		ScenarioID: "scenario",
		SpecHash:   "spec",
	})
	initial, err := domain.Reduce([]domain.Event{created})
	if err != nil {
		t.Fatalf("Reduce(RunCreated) error = %v", err)
	}
	command := domain.Decide(initial)
	actionID := command.ActionID
	events := []domain.Event{
		created,
		actionEvent(runID, 2, domain.EventActionPlanned, actionID+":planned", domain.EventData{
			ActionID:         actionID,
			ActionType:       domain.CommandStartAttempt,
			AttemptID:        command.AttemptID,
			IdempotencyScope: domain.IdempotencyUnsafe,
		}),
		actionEvent(runID, 3, domain.EventActionFailed, actionID+":failed", domain.EventData{
			ActionID:   actionID,
			ActionType: domain.CommandStartAttempt,
			Reason:     "outcome is ambiguous and has no durable receipt",
			Outcome:    domain.ActionOutcomeAmbiguous,
		}),
	}

	state, err := domain.Reduce(events)
	if err != nil {
		t.Fatalf("Reduce() error = %v", err)
	}
	if state.Run.ObservedState != domain.StatusWaitingApproval {
		t.Fatalf("observed state = %s, want WaitingApproval", state.Run.ObservedState)
	}
	if state.PendingActionID != "" || state.FailureReason == "" {
		t.Fatalf("ambiguous action projection = %#v", state)
	}
	if command := domain.Decide(state); command.Type != domain.CommandNoop {
		t.Fatalf("Decide(WaitingApproval) = %s, want Noop", command.Type)
	}
}

func actionEvent(runID string, seq uint64, eventType domain.EventType, key string, data domain.EventData) domain.Event {
	return domain.Event{
		RunID:          runID,
		Seq:            seq,
		Type:           eventType,
		PayloadVersion: domain.CurrentEventPayloadVersion,
		Data:           data,
		IdempotencyKey: key,
		CorrelationID:  runID,
	}
}
