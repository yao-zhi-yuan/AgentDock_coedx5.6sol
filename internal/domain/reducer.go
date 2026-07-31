package domain

import (
	"errors"
	"fmt"
)

var (
	ErrInvalidSequence         = errors.New("invalid event sequence")
	ErrDuplicateIdempotencyKey = errors.New("duplicate idempotency key")
	ErrInvalidTransition       = errors.New("invalid state transition")
	ErrTerminalState           = errors.New("terminal state cannot advance")
	ErrUnsupportedEvent        = errors.New("unsupported event in phase 1")
	ErrInvalidEvent            = errors.New("invalid event")
)

// Reduce validates and projects an ordered event log. Its result depends only
// on the supplied event values.
func Reduce(events []Event) (State, error) {
	state := InitialState()
	idempotencyKeys := make(map[string]struct{}, len(events))

	for index, event := range events {
		expectedSeq := uint64(index + 1)
		if event.Seq != expectedSeq {
			return State{}, fmt.Errorf("%w: event index %d has seq %d, want %d", ErrInvalidSequence, index, event.Seq, expectedSeq)
		}
		if event.RunID == "" {
			return State{}, fmt.Errorf("%w: event seq %d has empty run_id", ErrInvalidEvent, event.Seq)
		}
		if event.IdempotencyKey == "" {
			return State{}, fmt.Errorf("%w: event seq %d has empty idempotency_key", ErrInvalidEvent, event.Seq)
		}
		if event.PayloadVersion != CurrentEventPayloadVersion {
			return State{}, fmt.Errorf(
				"%w: event seq %d has payload_version %d, want %d",
				ErrInvalidEvent,
				event.Seq,
				event.PayloadVersion,
				CurrentEventPayloadVersion,
			)
		}
		if _, duplicate := idempotencyKeys[event.IdempotencyKey]; duplicate {
			return State{}, fmt.Errorf("%w: %q at seq %d", ErrDuplicateIdempotencyKey, event.IdempotencyKey, event.Seq)
		}
		idempotencyKeys[event.IdempotencyKey] = struct{}{}

		if state.Exists && event.RunID != state.Run.ID {
			return State{}, fmt.Errorf("%w: event seq %d run_id %q differs from %q", ErrInvalidEvent, event.Seq, event.RunID, state.Run.ID)
		}
		if state.Exists && state.Run.ObservedState.Terminal() {
			return State{}, fmt.Errorf("%w: %s before %s at seq %d", ErrTerminalState, state.Run.ObservedState, event.Type, event.Seq)
		}

		if err := applyEvent(&state, event); err != nil {
			return State{}, fmt.Errorf("reduce %s at seq %d: %w", event.Type, event.Seq, err)
		}
		state.Run.Version = event.Seq
		if event.CreatedAt != "" {
			state.Run.UpdatedAt = event.CreatedAt
		}
		state.LastEventType = event.Type
	}

	return state, nil
}

// ReduceFromCheckpoint applies a suffix to a previously validated projection.
// Persistent stores must verify that the checkpoint matches the authoritative
// event prefix and enforce full-log idempotency uniqueness before calling it.
func ReduceFromCheckpoint(checkpoint State, events []Event) (State, error) {
	if !checkpoint.Exists || checkpoint.Run.ID == "" || checkpoint.Run.Version == 0 {
		return State{}, fmt.Errorf("%w: checkpoint must contain an existing versioned Run", ErrInvalidEvent)
	}
	state := checkpoint
	idempotencyKeys := make(map[string]struct{}, len(events))
	for index, event := range events {
		expectedSeq := checkpoint.Run.Version + uint64(index) + 1
		if event.Seq != expectedSeq {
			return State{}, fmt.Errorf(
				"%w: checkpoint suffix index %d has seq %d, want %d",
				ErrInvalidSequence,
				index,
				event.Seq,
				expectedSeq,
			)
		}
		if event.RunID != state.Run.ID {
			return State{}, fmt.Errorf(
				"%w: checkpoint suffix seq %d run_id %q differs from %q",
				ErrInvalidEvent,
				event.Seq,
				event.RunID,
				state.Run.ID,
			)
		}
		if event.IdempotencyKey == "" {
			return State{}, fmt.Errorf("%w: event seq %d has empty idempotency_key", ErrInvalidEvent, event.Seq)
		}
		if event.PayloadVersion != CurrentEventPayloadVersion {
			return State{}, fmt.Errorf(
				"%w: event seq %d has payload_version %d, want %d",
				ErrInvalidEvent,
				event.Seq,
				event.PayloadVersion,
				CurrentEventPayloadVersion,
			)
		}
		if _, duplicate := idempotencyKeys[event.IdempotencyKey]; duplicate {
			return State{}, fmt.Errorf(
				"%w: %q at seq %d",
				ErrDuplicateIdempotencyKey,
				event.IdempotencyKey,
				event.Seq,
			)
		}
		idempotencyKeys[event.IdempotencyKey] = struct{}{}
		if state.Run.ObservedState.Terminal() {
			return State{}, fmt.Errorf(
				"%w: %s before %s at seq %d",
				ErrTerminalState,
				state.Run.ObservedState,
				event.Type,
				event.Seq,
			)
		}
		if err := applyEvent(&state, event); err != nil {
			return State{}, fmt.Errorf("reduce %s at seq %d: %w", event.Type, event.Seq, err)
		}
		state.Run.Version = event.Seq
		if event.CreatedAt != "" {
			state.Run.UpdatedAt = event.CreatedAt
		}
		state.LastEventType = event.Type
	}
	return state, nil
}

func applyEvent(state *State, event Event) error {
	switch event.Type {
	case EventRunCreated:
		if state.Exists {
			return fmt.Errorf("%w: RunCreated requires an empty state", ErrInvalidTransition)
		}
		if err := ValidateTransition("", StatusQueued); err != nil {
			return err
		}
		*state = State{
			Exists: true,
			Run: Run{
				ID:            event.RunID,
				ScenarioID:    event.Data.ScenarioID,
				SpecHash:      event.Data.SpecHash,
				DesiredState:  DesiredRunning,
				ObservedState: StatusQueued,
				CreatedAt:     event.CreatedAt,
			},
		}
		return nil

	case EventDesiredStateChanged:
		if !state.Exists {
			return fmt.Errorf("%w: desired state requires RunCreated", ErrInvalidTransition)
		}
		return applyDesiredState(state, event.Data.DesiredState)

	case EventLeaseAcquired, EventLeaseRenewed, EventLeaseExpired:
		if !state.Exists {
			return fmt.Errorf("%w: %s requires RunCreated", ErrInvalidTransition, event.Type)
		}
		if event.WorkerID == "" || event.FencingToken == 0 {
			return fmt.Errorf("%w: %s requires worker_id and fencing_token", ErrInvalidEvent, event.Type)
		}
		return nil

	case EventActionPlanned:
		return applyActionPlanned(state, event)

	case EventActionCompleted:
		return applyActionCompleted(state, event)

	case EventActionFailed:
		return applyActionFailed(state, event)

	case EventAttemptStarted:
		if err := requireActiveState(state, StatusQueued); err != nil {
			return err
		}
		if event.Data.AttemptID == "" {
			return fmt.Errorf("%w: AttemptStarted requires attempt_id", ErrInvalidEvent)
		}
		if err := transition(state, StatusProvisioning); err != nil {
			return err
		}
		state.Run.CurrentAttempt++
		state.AttemptID = event.Data.AttemptID
		return nil

	case EventWorkspaceProvisioned:
		if err := requireActiveState(state, StatusProvisioning); err != nil {
			return err
		}
		return transition(state, StatusReasoning)

	case EventReasoningPlanned:
		if err := requireActiveState(state, StatusReasoning); err != nil {
			return err
		}
		if event.Data.ActionID == "" {
			return fmt.Errorf("%w: ReasoningPlanned requires action_id", ErrInvalidEvent)
		}
		if state.PendingActionID != "" {
			return fmt.Errorf("%w: reasoning action %q already pending", ErrInvalidTransition, state.PendingActionID)
		}
		state.PendingActionID = event.Data.ActionID
		return nil

	case EventReasoningCompleted:
		paused, err := requirePlannedResultState(state, StatusReasoning)
		if err != nil {
			return err
		}
		if err := requirePendingAction(state, event.Data.ActionID); err != nil {
			return err
		}
		if event.Data.ToolName == "" {
			return fmt.Errorf("%w: ReasoningCompleted requires tool_name", ErrInvalidEvent)
		}
		if err := advancePlannedResult(state, paused, StatusActing); err != nil {
			return err
		}
		state.PendingActionID = ""
		state.ReasoningOutput = event.Data.Output
		state.ToolName = event.Data.ToolName
		state.ToolArguments = event.Data.ToolArguments
		return nil

	case EventToolCallFailed:
		if _, err := requirePlannedResultState(state, StatusReasoning); err != nil {
			return err
		}
		if err := requirePendingAction(state, event.Data.ActionID); err != nil {
			return err
		}
		if event.Data.Reason == "" {
			return fmt.Errorf("%w: ToolCallFailed requires reason", ErrInvalidEvent)
		}
		state.PendingActionID = ""
		state.FailureReason = event.Data.Reason
		return nil

	case EventPatchProduced:
		if err := requireActiveState(state, StatusActing); err != nil {
			return err
		}
		if event.Data.ActionID == "" {
			return fmt.Errorf("%w: PatchProduced requires action_id", ErrInvalidEvent)
		}
		if err := transition(state, StatusVerifying); err != nil {
			return err
		}
		state.PatchProduced = true
		return nil

	case EventVerificationPlanned:
		if err := requireActiveState(state, StatusVerifying); err != nil {
			return err
		}
		if state.VerificationPassed {
			return fmt.Errorf("%w: verification already passed", ErrInvalidTransition)
		}
		if event.Data.ActionID == "" {
			return fmt.Errorf("%w: VerificationPlanned requires action_id", ErrInvalidEvent)
		}
		if state.PendingActionID != "" {
			return fmt.Errorf("%w: action %q already pending", ErrInvalidTransition, state.PendingActionID)
		}
		state.PendingActionID = event.Data.ActionID
		return nil

	case EventVerificationPassed:
		if _, err := requirePlannedResultState(state, StatusVerifying); err != nil {
			return err
		}
		if err := requirePendingAction(state, event.Data.ActionID); err != nil {
			return err
		}
		state.PendingActionID = ""
		state.VerificationPassed = true
		return nil

	case EventRunSucceeded:
		if err := requireActiveState(state, StatusVerifying); err != nil {
			return err
		}
		if !state.VerificationPassed || state.PendingActionID != "" {
			return fmt.Errorf("%w: success requires completed verification evidence", ErrInvalidTransition)
		}
		return transition(state, StatusSucceeded)

	case EventRunFailed:
		if !state.Exists {
			return fmt.Errorf("%w: RunFailed requires RunCreated", ErrInvalidTransition)
		}
		if state.Run.DesiredState != DesiredRunning {
			return fmt.Errorf("%w: RunFailed requires Running desired state", ErrInvalidTransition)
		}
		if event.Data.Reason != "" {
			state.FailureReason = event.Data.Reason
		}
		if state.FailureReason == "" {
			return fmt.Errorf("%w: RunFailed requires a failure reason", ErrInvalidEvent)
		}
		state.PendingActionID = ""
		return transition(state, StatusFailed)

	case EventRunCancelled:
		if !state.Exists || state.Run.DesiredState != DesiredCancelled {
			return fmt.Errorf("%w: RunCancelled requires cancelled intent", ErrInvalidTransition)
		}
		state.PendingActionID = ""
		return transition(state, StatusCancelled)

	default:
		return fmt.Errorf("%w: %s", ErrUnsupportedEvent, event.Type)
	}
}

func applyDesiredState(state *State, desired DesiredState) error {
	if !desired.Valid() {
		return fmt.Errorf("%w: unknown desired state %q", ErrInvalidEvent, desired)
	}
	if state.Run.ObservedState.Terminal() {
		return fmt.Errorf("%w: %s", ErrTerminalState, state.Run.ObservedState)
	}

	switch desired {
	case DesiredPaused:
		if state.Run.DesiredState == DesiredPaused || state.Run.ObservedState == StatusPaused {
			return fmt.Errorf("%w: Run is already paused", ErrInvalidTransition)
		}
		if state.Run.DesiredState == DesiredCancelled {
			return fmt.Errorf("%w: cancelled intent cannot be replaced by pause", ErrInvalidTransition)
		}
		state.ResumeState = state.Run.ObservedState
		if err := transition(state, StatusPaused); err != nil {
			return err
		}
		state.Run.DesiredState = DesiredPaused
		return nil

	case DesiredRunning:
		if state.Run.DesiredState != DesiredPaused ||
			state.Run.ObservedState != StatusPaused ||
			state.ResumeState == "" {
			return fmt.Errorf("%w: resume requires a paused Run", ErrInvalidTransition)
		}
		resumeState := state.ResumeState
		if err := transition(state, resumeState); err != nil {
			return err
		}
		state.ResumeState = ""
		state.Run.DesiredState = DesiredRunning
		return nil

	case DesiredCancelled:
		if state.Run.DesiredState == DesiredCancelled {
			return fmt.Errorf("%w: Run already has cancelled intent", ErrInvalidTransition)
		}
		state.Run.DesiredState = DesiredCancelled
		return nil
	}

	return fmt.Errorf("%w: unknown desired state %q", ErrInvalidEvent, desired)
}

func applyActionPlanned(state *State, event Event) error {
	if !state.Exists {
		return fmt.Errorf("%w: ActionPlanned requires RunCreated", ErrInvalidTransition)
	}
	if event.Data.ActionID == "" || event.Data.ActionType == "" || !event.Data.IdempotencyScope.Valid() {
		return fmt.Errorf(
			"%w: ActionPlanned requires action_id, action_type, and valid idempotency_scope",
			ErrInvalidEvent,
		)
	}
	if state.PendingActionID != "" {
		return fmt.Errorf("%w: action %q already pending", ErrInvalidTransition, state.PendingActionID)
	}
	command := Decide(*state)
	if command.Type == CommandNoop ||
		command.Type != event.Data.ActionType ||
		command.ActionID != event.Data.ActionID {
		return fmt.Errorf(
			"%w: planned action %s/%q does not match decision %s/%q",
			ErrInvalidTransition,
			event.Data.ActionType,
			event.Data.ActionID,
			command.Type,
			command.ActionID,
		)
	}
	if command.Type == CommandStartAttempt && event.Data.AttemptID != command.AttemptID {
		return fmt.Errorf(
			"%w: attempt_id %q does not match decision %q",
			ErrInvalidTransition,
			event.Data.AttemptID,
			command.AttemptID,
		)
	}
	state.PendingActionID = event.Data.ActionID
	state.PendingActionType = event.Data.ActionType
	state.PendingAttemptID = event.Data.AttemptID
	state.PendingActionScope = event.Data.IdempotencyScope
	return nil
}

func applyActionCompleted(state *State, event Event) error {
	if event.Data.ReceiptID == "" {
		return fmt.Errorf("%w: ActionCompleted requires receipt_id", ErrInvalidEvent)
	}
	if err := requireManagedPendingAction(state, event.Data.ActionID, event.Data.ActionType); err != nil {
		return err
	}

	cancelWins := state.Run.DesiredState == DesiredCancelled &&
		event.Data.ActionType != CommandCancelRun
	if !cancelWins {
		if err := applyCompletedActionResult(state, event.Data); err != nil {
			return err
		}
	}
	state.PendingActionID = ""
	state.PendingActionType = ""
	state.PendingAttemptID = ""
	state.PendingActionScope = ""
	state.LastCompletedActionID = event.Data.ActionID
	state.LastReceiptID = event.Data.ReceiptID
	return nil
}

// ValidateActionCompletion checks whether Receipt-backed completion data can
// advance the current projection. It mutates only a copy and is used before a
// recovered Receipt is trusted.
func ValidateActionCompletion(state State, data EventData) error {
	return applyActionCompleted(&state, Event{Data: data})
}

// ValidateManagedActionOutput rejects action-specific Receipt payloads that
// could never be reduced. The planned Attempt ID is read from the durable
// ActionPlanned event by the Receipt writer.
func ValidateManagedActionOutput(
	actionType CommandType,
	plannedAttemptID string,
	output EventData,
) error {
	switch actionType {
	case CommandStartAttempt:
		if output.AttemptID == "" || output.AttemptID != plannedAttemptID {
			return fmt.Errorf(
				"%w: StartAttempt Receipt attempt_id %q does not match planned %q",
				ErrInvalidEvent,
				output.AttemptID,
				plannedAttemptID,
			)
		}
	case CommandRunReasoner:
		if output.ToolName == "" {
			return fmt.Errorf("%w: RunReasoner Receipt requires tool_name", ErrInvalidEvent)
		}
	case CommandFailRun:
		if output.Reason == "" {
			return fmt.Errorf("%w: FailRun Receipt requires a failure reason", ErrInvalidEvent)
		}
	}
	return nil
}

func applyActionFailed(state *State, event Event) error {
	if err := requireManagedPendingAction(state, event.Data.ActionID, event.Data.ActionType); err != nil {
		return err
	}
	if event.Data.Reason == "" {
		return fmt.Errorf("%w: ActionFailed requires reason", ErrInvalidEvent)
	}
	if event.Data.Outcome != ActionOutcomeFailed && event.Data.Outcome != ActionOutcomeAmbiguous {
		return fmt.Errorf("%w: ActionFailed has invalid outcome %q", ErrInvalidEvent, event.Data.Outcome)
	}
	state.PendingActionID = ""
	state.PendingActionType = ""
	state.PendingAttemptID = ""
	state.PendingActionScope = ""
	state.FailureReason = event.Data.Reason
	if event.Data.Outcome == ActionOutcomeAmbiguous &&
		state.Run.DesiredState != DesiredCancelled {
		return advanceManagedState(state, StatusWaitingApproval)
	}
	return nil
}

func requireManagedPendingAction(state *State, actionID string, actionType CommandType) error {
	if actionID == "" ||
		actionID != state.PendingActionID ||
		actionType == "" ||
		actionType != state.PendingActionType {
		return fmt.Errorf(
			"%w: action %s/%q does not match pending %s/%q",
			ErrInvalidTransition,
			actionType,
			actionID,
			state.PendingActionType,
			state.PendingActionID,
		)
	}
	return nil
}

func applyCompletedActionResult(state *State, data EventData) error {
	switch data.ActionType {
	case CommandStartAttempt:
		if data.AttemptID == "" {
			return fmt.Errorf("%w: StartAttempt completion requires attempt_id", ErrInvalidEvent)
		}
		if err := requireManagedLogicalState(state, StatusQueued); err != nil {
			return err
		}
		if err := advanceManagedState(state, StatusProvisioning); err != nil {
			return err
		}
		state.Run.CurrentAttempt++
		state.AttemptID = data.AttemptID
		return nil
	case CommandProvisionWorkspace:
		if err := requireManagedLogicalState(state, StatusProvisioning); err != nil {
			return err
		}
		return advanceManagedState(state, StatusReasoning)
	case CommandRunReasoner:
		if err := requireManagedLogicalState(state, StatusReasoning); err != nil {
			return err
		}
		if data.ToolName == "" {
			return fmt.Errorf("%w: RunReasoner completion requires tool_name", ErrInvalidEvent)
		}
		if err := advanceManagedState(state, StatusActing); err != nil {
			return err
		}
		state.ReasoningOutput = data.Output
		state.ToolName = data.ToolName
		state.ToolArguments = data.ToolArguments
		return nil
	case CommandApplyPatch:
		if err := requireManagedLogicalState(state, StatusActing); err != nil {
			return err
		}
		if err := advanceManagedState(state, StatusVerifying); err != nil {
			return err
		}
		state.PatchProduced = true
		return nil
	case CommandVerify:
		if err := requireManagedLogicalState(state, StatusVerifying); err != nil {
			return err
		}
		state.VerificationPassed = true
		return nil
	case CommandSucceedRun:
		if err := requireManagedLogicalState(state, StatusVerifying); err != nil {
			return err
		}
		if !state.VerificationPassed {
			return fmt.Errorf("%w: success requires completed verification evidence", ErrInvalidTransition)
		}
		return advanceManagedState(state, StatusSucceeded)
	case CommandFailRun:
		if state.FailureReason == "" && data.Reason == "" {
			return fmt.Errorf("%w: FailRun completion requires a failure reason", ErrInvalidEvent)
		}
		if data.Reason != "" {
			state.FailureReason = data.Reason
		}
		return advanceManagedState(state, StatusFailed)
	case CommandCancelRun:
		if state.Run.DesiredState != DesiredCancelled {
			return fmt.Errorf("%w: CancelRun completion requires cancelled intent", ErrInvalidTransition)
		}
		return advanceManagedState(state, StatusCancelled)
	default:
		return fmt.Errorf("%w: completed action %s", ErrInvalidEvent, data.ActionType)
	}
}

func requireManagedLogicalState(state *State, expected Status) error {
	logical := state.Run.ObservedState
	if logical == StatusPaused {
		logical = state.ResumeState
	}
	if logical != expected {
		return fmt.Errorf(
			"%w: managed action requires logical state %s, got %s",
			ErrInvalidTransition,
			expected,
			logical,
		)
	}
	return nil
}

func advanceManagedState(state *State, next Status) error {
	if state.Run.ObservedState == StatusPaused {
		if err := ValidateTransition(state.ResumeState, next); err != nil {
			return err
		}
		state.ResumeState = next
		return nil
	}
	return transition(state, next)
}

func requireActiveState(state *State, expected Status) error {
	if !state.Exists {
		return fmt.Errorf("%w: %s requires RunCreated", ErrInvalidTransition, expected)
	}
	if state.Run.DesiredState != DesiredRunning || state.Run.ObservedState != expected {
		return fmt.Errorf(
			"%w: requires desired=%s observed=%s, got desired=%s observed=%s",
			ErrInvalidTransition,
			DesiredRunning,
			expected,
			state.Run.DesiredState,
			state.Run.ObservedState,
		)
	}
	return nil
}

func requirePlannedResultState(state *State, expected Status) (bool, error) {
	if !state.Exists {
		return false, fmt.Errorf("%w: %s result requires RunCreated", ErrInvalidTransition, expected)
	}
	if state.Run.DesiredState == DesiredRunning && state.Run.ObservedState == expected {
		return false, nil
	}
	if state.Run.DesiredState == DesiredPaused &&
		state.Run.ObservedState == StatusPaused &&
		state.ResumeState == expected {
		return true, nil
	}
	return false, fmt.Errorf(
		"%w: result requires active %s or Paused with resume=%s, got desired=%s observed=%s resume=%s",
		ErrInvalidTransition,
		expected,
		expected,
		state.Run.DesiredState,
		state.Run.ObservedState,
		state.ResumeState,
	)
}

func advancePlannedResult(state *State, paused bool, next Status) error {
	if !paused {
		return transition(state, next)
	}
	if err := ValidateTransition(state.ResumeState, next); err != nil {
		return err
	}
	state.ResumeState = next
	return nil
}

func requirePendingAction(state *State, actionID string) error {
	if actionID == "" || state.PendingActionID == "" || actionID != state.PendingActionID {
		return fmt.Errorf("%w: action_id %q does not match pending action %q", ErrInvalidTransition, actionID, state.PendingActionID)
	}
	return nil
}

func transition(state *State, next Status) error {
	if err := ValidateTransition(state.Run.ObservedState, next); err != nil {
		return err
	}
	state.Run.ObservedState = next
	return nil
}

// ValidateTransition is the program-owned transition guard. Models and
// reasoners never provide a target state.
func ValidateTransition(from, to Status) error {
	if from.Terminal() {
		return fmt.Errorf("%w: %w: %s -> %s", ErrTerminalState, ErrInvalidTransition, from, to)
	}

	allowed := map[Status]map[Status]struct{}{
		"": {
			StatusQueued: {},
		},
		StatusQueued: {
			StatusProvisioning:    {},
			StatusPaused:          {},
			StatusWaitingApproval: {},
			StatusFailed:          {},
			StatusCancelled:       {},
		},
		StatusProvisioning: {
			StatusReasoning:       {},
			StatusPaused:          {},
			StatusWaitingApproval: {},
			StatusFailed:          {},
			StatusCancelled:       {},
		},
		StatusReasoning: {
			StatusActing:          {},
			StatusPaused:          {},
			StatusWaitingApproval: {},
			StatusFailed:          {},
			StatusCancelled:       {},
		},
		StatusActing: {
			StatusVerifying:       {},
			StatusPaused:          {},
			StatusWaitingApproval: {},
			StatusFailed:          {},
			StatusCancelled:       {},
		},
		StatusVerifying: {
			StatusSucceeded:       {},
			StatusRepairing:       {},
			StatusPaused:          {},
			StatusWaitingApproval: {},
			StatusFailed:          {},
			StatusCancelled:       {},
		},
		StatusRepairing: {
			StatusReasoning:       {},
			StatusWaitingApproval: {},
			StatusPaused:          {},
			StatusFailed:          {},
			StatusCancelled:       {},
		},
		StatusWaitingApproval: {
			StatusReasoning: {},
			StatusPaused:    {},
			StatusFailed:    {},
			StatusCancelled: {},
		},
		StatusPaused: {
			StatusQueued:          {},
			StatusProvisioning:    {},
			StatusReasoning:       {},
			StatusActing:          {},
			StatusVerifying:       {},
			StatusRepairing:       {},
			StatusWaitingApproval: {},
			StatusSucceeded:       {},
			StatusFailed:          {},
			StatusCancelled:       {},
		},
	}

	if _, ok := allowed[from][to]; !ok {
		return fmt.Errorf("%w: %s -> %s", ErrInvalidTransition, from, to)
	}
	return nil
}
