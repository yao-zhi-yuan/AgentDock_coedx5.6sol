package controller

import (
	"context"
	"errors"
	"fmt"

	"github.com/agentdock/agentdock-verify/internal/domain"
	"github.com/agentdock/agentdock-verify/internal/reasoner"
	"github.com/agentdock/agentdock-verify/internal/store"
)

var (
	ErrInvalidRequest = errors.New("invalid controller request")
	ErrRunExists      = errors.New("run already exists")
)

// Controller owns desired-state changes and one deterministic Reconcile path.
type Controller struct {
	store    store.EventStore
	reasoner reasoner.Reasoner
}

// CreateRunRequest contains explicit caller inputs; no ID, clock, or random
// value is generated inside the reducer or controller.
type CreateRunRequest struct {
	RunID      string
	ScenarioID string
	SpecHash   string
	CreatedAt  string
}

// ReconcileResult records the one decision plus every event persisted while
// carrying it out.
type ReconcileResult struct {
	Command domain.Command
	Events  []domain.Event
	State   domain.State
}

// New constructs a Controller over either compatible Event Store.
func New(eventStore store.EventStore, runReasoner reasoner.Reasoner) *Controller {
	return &Controller{store: eventStore, reasoner: runReasoner}
}

// CreateRun appends the authoritative RunCreated fact.
func (controller *Controller) CreateRun(ctx context.Context, request CreateRunRequest) (domain.State, error) {
	if request.RunID == "" || request.ScenarioID == "" || request.SpecHash == "" {
		return domain.State{}, fmt.Errorf("%w: run_id, scenario_id, and spec_hash are required", ErrInvalidRequest)
	}
	if _, err := controller.store.Load(ctx, request.RunID); err == nil {
		return domain.State{}, fmt.Errorf("%w: %s", ErrRunExists, request.RunID)
	} else if !errors.Is(err, store.ErrRunNotFound) {
		return domain.State{}, err
	}

	_, err := controller.store.Append(ctx, 0, domain.Event{
		RunID:          request.RunID,
		Type:           domain.EventRunCreated,
		Data:           domain.EventData{ScenarioID: request.ScenarioID, SpecHash: request.SpecHash},
		IdempotencyKey: "run-created",
		CorrelationID:  request.RunID,
		CreatedAt:      request.CreatedAt,
	})
	if err != nil {
		return domain.State{}, fmt.Errorf("append RunCreated: %w", err)
	}
	return controller.GetRun(ctx, request.RunID)
}

// GetRun rebuilds state exclusively from stored events.
func (controller *Controller) GetRun(ctx context.Context, runID string) (domain.State, error) {
	state, err := controller.store.Rebuild(ctx, runID)
	if err != nil {
		return domain.State{}, fmt.Errorf("reduce run %s: %w", runID, err)
	}
	return state, nil
}

// Events returns the auditable event log for rendering and demonstrations.
func (controller *Controller) Events(ctx context.Context, runID string) ([]domain.Event, error) {
	return controller.store.Load(ctx, runID)
}

// SetDesiredState appends operator intent. Repeating the current intent is a
// no-op; terminal intent changes are rejected.
func (controller *Controller) SetDesiredState(ctx context.Context, runID string, desired domain.DesiredState) (domain.State, error) {
	if !desired.Valid() {
		return domain.State{}, fmt.Errorf("%w: desired state %q", ErrInvalidRequest, desired)
	}
	state, err := controller.GetRun(ctx, runID)
	if err != nil {
		return domain.State{}, err
	}
	if state.Run.ObservedState.Terminal() {
		return domain.State{}, fmt.Errorf("%w: %s", domain.ErrTerminalState, state.Run.ObservedState)
	}
	if state.Run.DesiredState == desired {
		return state, nil
	}

	key := fmt.Sprintf("desired:%s:v%d", desired, state.Run.Version)
	_, err = controller.store.Append(ctx, state.Run.Version, domain.Event{
		RunID:          runID,
		Type:           domain.EventDesiredStateChanged,
		Data:           domain.EventData{DesiredState: desired},
		IdempotencyKey: key,
		CorrelationID:  runID,
	})
	if err != nil {
		return domain.State{}, fmt.Errorf("append desired state %s: %w", desired, err)
	}
	return controller.GetRun(ctx, runID)
}

// Reconcile rebuilds, decides one command, and persists its current
// intent/result facts through the same path after a clean start or restart.
func (controller *Controller) Reconcile(ctx context.Context, runID string) (ReconcileResult, error) {
	state, err := controller.GetRun(ctx, runID)
	if err != nil {
		return ReconcileResult{}, err
	}
	command := domain.Decide(state)
	result := ReconcileResult{Command: command, State: state}
	if command.Type == domain.CommandNoop {
		return result, nil
	}

	expectedVersion := state.Run.Version
	appendEvent := func(event domain.Event) error {
		event.RunID = runID
		event.CorrelationID = runID
		appendResult, appendErr := controller.store.Append(ctx, expectedVersion, event)
		if errors.Is(appendErr, store.ErrVersionConflict) {
			current, loadErr := controller.GetRun(ctx, runID)
			if loadErr != nil {
				return loadErr
			}
			expectedVersion = current.Run.Version
			appendResult, appendErr = controller.store.Append(ctx, expectedVersion, event)
		}
		if appendErr != nil {
			return appendErr
		}
		if appendResult.Appended {
			result.Events = append(result.Events, appendResult.Event)
			expectedVersion = appendResult.Event.Seq
		} else {
			current, loadErr := controller.GetRun(ctx, runID)
			if loadErr != nil {
				return loadErr
			}
			expectedVersion = current.Run.Version
		}
		return nil
	}

	switch command.Type {
	case domain.CommandStartAttempt:
		err = appendEvent(domain.Event{
			Type: domain.EventAttemptStarted,
			Data: domain.EventData{
				AttemptID: command.AttemptID,
				ActionID:  command.ActionID,
				Reason:    "initial",
			},
			IdempotencyKey: command.ActionID + ":attempt-started",
		})

	case domain.CommandProvisionWorkspace:
		err = appendEvent(domain.Event{
			Type:           domain.EventWorkspaceProvisioned,
			Data:           domain.EventData{ActionID: command.ActionID},
			IdempotencyKey: command.ActionID + ":workspace-provisioned",
		})

	case domain.CommandRunReasoner:
		err = controller.runReasoner(ctx, state, command, appendEvent)

	case domain.CommandApplyPatch:
		err = appendEvent(domain.Event{
			Type:           domain.EventPatchProduced,
			Data:           domain.EventData{ActionID: command.ActionID},
			IdempotencyKey: command.ActionID + ":patch-produced",
		})

	case domain.CommandVerify:
		if state.PendingActionID == "" {
			err = appendEvent(domain.Event{
				Type:           domain.EventVerificationPlanned,
				Data:           domain.EventData{ActionID: command.ActionID},
				IdempotencyKey: command.ActionID + ":verification-planned",
			})
		}
		if err == nil {
			err = appendEvent(domain.Event{
				Type:           domain.EventVerificationPassed,
				Data:           domain.EventData{ActionID: command.ActionID},
				IdempotencyKey: command.ActionID + ":verification-passed",
			})
		}

	case domain.CommandSucceedRun:
		err = appendEvent(domain.Event{
			Type:           domain.EventRunSucceeded,
			Data:           domain.EventData{ActionID: command.ActionID},
			IdempotencyKey: command.ActionID + ":run-succeeded",
		})

	case domain.CommandFailRun:
		err = appendEvent(domain.Event{
			Type:           domain.EventRunFailed,
			Data:           domain.EventData{ActionID: command.ActionID, Reason: state.FailureReason},
			IdempotencyKey: command.ActionID + ":run-failed",
		})

	case domain.CommandCancelRun:
		err = appendEvent(domain.Event{
			Type:           domain.EventRunCancelled,
			Data:           domain.EventData{ActionID: command.ActionID},
			IdempotencyKey: command.ActionID + ":run-cancelled",
		})

	default:
		err = fmt.Errorf("%w: command %s", ErrInvalidRequest, command.Type)
	}
	if err != nil {
		return ReconcileResult{}, fmt.Errorf("execute %s: %w", command.Type, err)
	}

	result.State, err = controller.GetRun(ctx, runID)
	if err != nil {
		return ReconcileResult{}, err
	}
	return result, nil
}

func (controller *Controller) runReasoner(
	ctx context.Context,
	state domain.State,
	command domain.Command,
	appendEvent func(domain.Event) error,
) error {
	if state.PendingActionID == "" {
		if err := appendEvent(domain.Event{
			Type:           domain.EventReasoningPlanned,
			Data:           domain.EventData{ActionID: command.ActionID},
			IdempotencyKey: command.ActionID + ":reasoning-planned",
		}); err != nil {
			return err
		}
	}

	result, err := controller.reasoner.Reason(ctx, reasoner.Request{
		RunID:      state.Run.ID,
		ScenarioID: state.Run.ScenarioID,
		AttemptID:  state.AttemptID,
	})
	if err != nil {
		return controller.appendControlledReasonerFailure(command, fmt.Errorf("reasoner error: %w", err), appendEvent)
	}
	if err := reasoner.ValidatePhase1Result(result); err != nil {
		return controller.appendControlledReasonerFailure(command, err, appendEvent)
	}

	return appendEvent(domain.Event{
		Type: domain.EventReasoningCompleted,
		Data: domain.EventData{
			ActionID:      command.ActionID,
			Output:        result.Output,
			ToolName:      result.ToolCall.Name,
			ToolArguments: result.ToolCall.Arguments,
		},
		IdempotencyKey: command.ActionID + ":reasoning-completed",
	})
}

func (controller *Controller) appendControlledReasonerFailure(
	command domain.Command,
	reason error,
	appendEvent func(domain.Event) error,
) error {
	reasonText := reason.Error()
	return appendEvent(domain.Event{
		Type: domain.EventToolCallFailed,
		Data: domain.EventData{
			ActionID: command.ActionID,
			Reason:   reasonText,
		},
		IdempotencyKey: command.ActionID + ":tool-call-failed",
	})
}
