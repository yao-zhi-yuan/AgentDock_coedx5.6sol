package domain_test

import (
	"errors"
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

func TestManagedPendingApplyPatchRejectsLegacyPatchProduced(t *testing.T) {
	runID := "run-managed-rejects-legacy-patch"
	events := []domain.Event{
		actionEvent(runID, 1, domain.EventRunCreated, "created", domain.EventData{
			ScenarioID: "scenario",
			SpecHash:   "spec",
		}),
	}
	events = completeManagedAction(t, events, domain.CommandStartAttempt)
	events = completeManagedAction(t, events, domain.CommandProvisionWorkspace)
	events = completeManagedAction(t, events, domain.CommandRunReasoner)

	state, err := domain.Reduce(events)
	if err != nil {
		t.Fatalf("Reduce(before ApplyPatch) error = %v", err)
	}
	command := domain.Decide(state)
	if command.Type != domain.CommandApplyPatch {
		t.Fatalf("Decide() = %s, want ApplyPatch", command.Type)
	}
	events = append(events, actionEvent(
		runID,
		uint64(len(events)+1),
		domain.EventActionPlanned,
		command.ActionID+":planned",
		domain.EventData{
			ActionID:         command.ActionID,
			ActionType:       command.Type,
			AttemptID:        command.AttemptID,
			IdempotencyScope: domain.IdempotencyScoped,
		},
	))
	events = append(events, actionEvent(
		runID,
		uint64(len(events)+1),
		domain.EventPatchProduced,
		command.ActionID+":legacy-patch",
		domain.EventData{ActionID: command.ActionID},
	))

	if _, err := domain.Reduce(events); !errors.Is(err, domain.ErrInvalidTransition) {
		t.Fatalf("Reduce(legacy PatchProduced during managed action) error = %v, want invalid transition", err)
	}
}

func completeManagedAction(
	t *testing.T,
	events []domain.Event,
	want domain.CommandType,
) []domain.Event {
	t.Helper()
	state, err := domain.Reduce(events)
	if err != nil {
		t.Fatalf("Reduce(before %s) error = %v", want, err)
	}
	command := domain.Decide(state)
	if command.Type != want {
		t.Fatalf("Decide() = %s, want %s", command.Type, want)
	}
	events = append(events, actionEvent(
		state.Run.ID,
		uint64(len(events)+1),
		domain.EventActionPlanned,
		command.ActionID+":planned",
		domain.EventData{
			ActionID:         command.ActionID,
			ActionType:       command.Type,
			AttemptID:        command.AttemptID,
			IdempotencyScope: domain.IdempotencyScoped,
		},
	))
	output := domain.EventData{
		ActionID:   command.ActionID,
		ActionType: command.Type,
		AttemptID:  command.AttemptID,
		ReceiptID:  command.ActionID + ":receipt",
	}
	if command.Type == domain.CommandRunReasoner {
		output.Output = "managed reasoning"
		output.ToolName = "phase1.patch"
		output.ToolArguments = `{"patch":"managed"}`
	}
	return append(events, actionEvent(
		state.Run.ID,
		uint64(len(events)+1),
		domain.EventActionCompleted,
		command.ActionID+":completed",
		output,
	))
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
