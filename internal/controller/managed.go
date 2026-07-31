package controller

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/agentdock/agentdock-verify/internal/domain"
	"github.com/agentdock/agentdock-verify/internal/lease"
	"github.com/agentdock/agentdock-verify/internal/reasoner"
	"github.com/agentdock/agentdock-verify/internal/store"
)

var ErrInjectedCrash = errors.New("injected worker crash")

// FaultPoint names the durable boundaries exercised by the phase-3 recovery
// matrix. A hook returns ErrInjectedCrash to model abrupt process loss.
type FaultPoint string

const (
	FaultBeforeActionPlanned   FaultPoint = "before-action-planned"
	FaultAfterActionPlanned    FaultPoint = "after-action-planned"
	FaultAfterActionExecution  FaultPoint = "after-action-execution"
	FaultAfterReceiptPersisted FaultPoint = "after-receipt-persisted"
	FaultAfterActionCompleted  FaultPoint = "after-action-completed"
)

type FaultHook func(FaultPoint, domain.Command) error
type SafetyPolicy func(domain.Command) domain.IdempotencyScope

// ActionRequest is reconstructed only from Event Log state and the stable
// command identity. It carries no process-local workflow authority.
type ActionRequest struct {
	State   domain.State
	Command domain.Command
	Scope   domain.IdempotencyScope
}

// ActionExecutor performs the narrow phase-3 fake action surface. Phase 4
// replaces neither this contract nor recovery semantics with a sandbox.
type ActionExecutor interface {
	Execute(context.Context, ActionRequest) (lease.ActionReceipt, error)
}

type ManagedOptions struct {
	Executor  ActionExecutor
	Artifacts *store.PostgresArtifactStore
	Fault     FaultHook
	Safety    SafetyPolicy
}

// NewManaged enables the leased Reconcile path used by both first execution
// and crash recovery. New remains available for the phase-1 memory demo.
func NewManaged(
	eventStore store.EventStore,
	runReasoner reasoner.Reasoner,
	manager lease.Manager,
	options ManagedOptions,
) *Controller {
	if options.Executor == nil {
		options.Executor = &reasonerActionExecutor{reasoner: runReasoner}
	}
	if options.Safety == nil {
		options.Safety = func(domain.Command) domain.IdempotencyScope {
			return domain.IdempotencyScoped
		}
	}
	return &Controller{
		store:          eventStore,
		reasoner:       runReasoner,
		leaseManager:   manager,
		managedOptions: options,
	}
}

// ReconcileLeased performs exactly one external action at most. A recovered
// Worker calls the same method; pending intent and durable receipts determine
// whether it completes, retries, cancels, or waits for approval.
func (controller *Controller) ReconcileLeased(
	ctx context.Context,
	runID string,
	presented lease.Lease,
) (ReconcileResult, error) {
	if controller.leaseManager == nil || controller.managedOptions.Executor == nil {
		return ReconcileResult{}, fmt.Errorf("%w: managed controller is not configured", ErrInvalidRequest)
	}
	if presented.RunID != runID {
		return ReconcileResult{}, fmt.Errorf("%w: lease Run %q does not match %q", ErrInvalidRequest, presented.RunID, runID)
	}
	if err := controller.leaseManager.Validate(ctx, presented); err != nil {
		return ReconcileResult{}, err
	}
	state, err := controller.GetRun(ctx, runID)
	if err != nil {
		return ReconcileResult{}, err
	}
	result := ReconcileResult{State: state}

	appendEvent := controller.managedAppender(ctx, runID, presented, &result, state.Run.Version)
	if state.PendingActionID != "" {
		return controller.recoverPendingAction(ctx, presented, state, result, appendEvent)
	}

	command := domain.Decide(state)
	result.Command = command
	if command.Type == domain.CommandNoop {
		return result, nil
	}
	if err := controller.inject(FaultBeforeActionPlanned, command); err != nil {
		return ReconcileResult{}, err
	}
	scope := controller.managedOptions.Safety(command)
	if !scope.Valid() {
		return ReconcileResult{}, fmt.Errorf("%w: invalid idempotency scope %q", ErrInvalidRequest, scope)
	}
	if err := appendEvent(domain.Event{
		Type: domain.EventActionPlanned,
		Data: domain.EventData{
			ActionID:         command.ActionID,
			ActionType:       command.Type,
			AttemptID:        command.AttemptID,
			IdempotencyScope: scope,
		},
		IdempotencyKey: command.ActionID + ":action-planned",
	}); err != nil {
		if errors.Is(err, store.ErrVersionConflict) {
			current, loadErr := controller.GetRun(ctx, runID)
			if loadErr != nil {
				return ReconcileResult{}, loadErr
			}
			result.Command = domain.Command{Type: domain.CommandNoop}
			result.State = current
			return result, nil
		}
		return ReconcileResult{}, fmt.Errorf("append ActionPlanned: %w", err)
	}
	if err := controller.inject(FaultAfterActionPlanned, command); err != nil {
		return ReconcileResult{}, err
	}
	plannedState, err := controller.GetRun(ctx, runID)
	if err != nil {
		return ReconcileResult{}, err
	}
	return controller.executeManagedAction(ctx, presented, plannedState, command, result, appendEvent)
}

type managedAppend func(domain.Event) error

func (controller *Controller) managedAppender(
	ctx context.Context,
	runID string,
	presented lease.Lease,
	result *ReconcileResult,
	initialVersion uint64,
) managedAppend {
	expectedVersion := initialVersion
	return func(event domain.Event) error {
		event.RunID = runID
		event.CorrelationID = runID
		event.WorkerID = presented.WorkerID
		event.FencingToken = presented.FencingToken
		appendResult, err := controller.store.Append(ctx, expectedVersion, event)
		if errors.Is(err, store.ErrVersionConflict) {
			if event.Type == domain.EventActionPlanned {
				return err
			}
			current, loadErr := controller.GetRun(ctx, runID)
			if loadErr != nil {
				return loadErr
			}
			expectedVersion = current.Run.Version
			appendResult, err = controller.store.Append(ctx, expectedVersion, event)
		}
		if err != nil {
			return err
		}
		if appendResult.Appended {
			result.Events = append(result.Events, appendResult.Event)
			expectedVersion = appendResult.Event.Seq
			return nil
		}
		current, loadErr := controller.GetRun(ctx, runID)
		if loadErr != nil {
			return loadErr
		}
		expectedVersion = current.Run.Version
		return nil
	}
}

func (controller *Controller) recoverPendingAction(
	ctx context.Context,
	presented lease.Lease,
	state domain.State,
	result ReconcileResult,
	appendEvent managedAppend,
) (ReconcileResult, error) {
	command := domain.Command{
		Type:      state.PendingActionType,
		RunID:     state.Run.ID,
		ActionID:  state.PendingActionID,
		AttemptID: state.PendingAttemptID,
	}
	result.Command = command
	if state.Run.DesiredState == domain.DesiredPaused ||
		state.Run.ObservedState == domain.StatusPaused {
		result.Command = domain.Command{Type: domain.CommandNoop}
		return result, nil
	}
	if state.Run.DesiredState == domain.DesiredCancelled {
		if err := appendEvent(actionFailedEvent(command, "cancel intent superseded the pending action", domain.ActionOutcomeFailed)); err != nil {
			return ReconcileResult{}, err
		}
		var err error
		result.State, err = controller.GetRun(ctx, state.Run.ID)
		return result, err
	}

	receipt, err := controller.leaseManager.LookupReceipt(ctx, state.Run.ID, state.PendingActionID)
	if err == nil {
		if integrityErr := controller.validateReceiptEvidence(ctx, state, receipt); integrityErr != nil {
			if appendErr := appendEvent(actionFailedEvent(
				command,
				"durable action receipt evidence cannot be verified: "+integrityErr.Error(),
				domain.ActionOutcomeAmbiguous,
			)); appendErr != nil {
				return ReconcileResult{}, appendErr
			}
			result.State, err = controller.GetRun(ctx, state.Run.ID)
			return result, err
		}
		if err := appendEvent(actionCompletedEvent(receipt)); err != nil {
			return ReconcileResult{}, fmt.Errorf("append recovered ActionCompleted: %w", err)
		}
		result.State, err = controller.GetRun(ctx, state.Run.ID)
		return result, err
	}
	if !errors.Is(err, lease.ErrReceiptNotFound) {
		return ReconcileResult{}, err
	}
	if state.PendingActionScope != domain.IdempotencyScoped {
		if err := appendEvent(actionFailedEvent(
			command,
			"action outcome is ambiguous and no durable receipt proves completion",
			domain.ActionOutcomeAmbiguous,
		)); err != nil {
			return ReconcileResult{}, err
		}
		result.State, err = controller.GetRun(ctx, state.Run.ID)
		return result, err
	}
	return controller.executeManagedAction(ctx, presented, state, command, result, appendEvent)
}

func (controller *Controller) executeManagedAction(
	ctx context.Context,
	presented lease.Lease,
	state domain.State,
	command domain.Command,
	result ReconcileResult,
	appendEvent managedAppend,
) (ReconcileResult, error) {
	receipt, err := controller.managedOptions.Executor.Execute(ctx, ActionRequest{
		State:   state,
		Command: command,
		Scope:   state.PendingActionScope,
	})
	if err != nil {
		if appendErr := appendEvent(actionFailedEvent(command, err.Error(), domain.ActionOutcomeFailed)); appendErr != nil {
			return ReconcileResult{}, fmt.Errorf("append ActionFailed after %v: %w", err, appendErr)
		}
		result.State, err = controller.GetRun(ctx, state.Run.ID)
		return result, err
	}
	if err := controller.inject(FaultAfterActionExecution, command); err != nil {
		return ReconcileResult{}, err
	}
	receipt.RunID = state.Run.ID
	receipt.ActionID = command.ActionID
	receipt.ActionType = command.Type
	receipt.IdempotencyScope = state.PendingActionScope
	recorded, err := controller.leaseManager.RecordReceipt(ctx, presented, receipt)
	if err != nil {
		return ReconcileResult{}, fmt.Errorf("record action receipt: %w", err)
	}
	if err := controller.inject(FaultAfterReceiptPersisted, command); err != nil {
		return ReconcileResult{}, err
	}
	if integrityErr := controller.validateReceiptEvidence(ctx, state, recorded.Receipt); integrityErr != nil {
		if appendErr := appendEvent(actionFailedEvent(
			command,
			"durable action receipt evidence cannot be verified: "+integrityErr.Error(),
			domain.ActionOutcomeAmbiguous,
		)); appendErr != nil {
			return ReconcileResult{}, appendErr
		}
		result.State, err = controller.GetRun(ctx, state.Run.ID)
		return result, err
	}
	if err := appendEvent(actionCompletedEvent(recorded.Receipt)); err != nil {
		return ReconcileResult{}, fmt.Errorf("append ActionCompleted: %w", err)
	}
	if err := controller.inject(FaultAfterActionCompleted, command); err != nil {
		return ReconcileResult{}, err
	}
	result.State, err = controller.GetRun(ctx, state.Run.ID)
	return result, err
}

func (controller *Controller) validateReceiptEvidence(
	ctx context.Context,
	state domain.State,
	receipt lease.ActionReceipt,
) error {
	if err := domain.ValidateManagedReceiptEvidence(
		receipt.ActionType,
		receipt.ActionID,
		state.PendingAttemptID,
		receipt.Output,
		receipt.OutputDigest,
		receipt.ArtifactID,
		receipt.ArtifactDigest,
	); err != nil {
		return err
	}
	data := actionCompletedEvent(receipt).Data
	if err := domain.ValidateActionCompletion(state, data); err != nil {
		return fmt.Errorf("Receipt completion is not reducible: %w", err)
	}
	if receipt.ArtifactID == "" {
		return nil
	}
	if controller.managedOptions.Artifacts == nil {
		return errors.New("Artifact verifier is not configured")
	}
	record, err := controller.managedOptions.Artifacts.Verify(ctx, receipt.ArtifactID)
	if err != nil {
		return err
	}
	if record.RunID != receipt.RunID ||
		record.AttemptID != state.PendingAttemptID ||
		record.ID != receipt.ArtifactID ||
		record.Type != domain.Phase3ReceiptArtifactType ||
		record.Digest != receipt.ArtifactDigest {
		return fmt.Errorf(
			"Artifact metadata mismatch: receipt run=%q attempt=%q id=%q digest=%q, Artifact run=%q attempt=%q id=%q type=%q digest=%q",
			receipt.RunID,
			state.PendingAttemptID,
			receipt.ArtifactID,
			receipt.ArtifactDigest,
			record.RunID,
			record.AttemptID,
			record.ID,
			record.Type,
			record.Digest,
		)
	}
	return nil
}

func (controller *Controller) inject(point FaultPoint, command domain.Command) error {
	if controller.managedOptions.Fault == nil {
		return nil
	}
	return controller.managedOptions.Fault(point, command)
}

func actionCompletedEvent(receipt lease.ActionReceipt) domain.Event {
	data := receipt.Output
	data.ActionID = receipt.ActionID
	data.ActionType = receipt.ActionType
	data.IdempotencyScope = receipt.IdempotencyScope
	data.ReceiptID = receipt.ID
	data.OutputDigest = receipt.OutputDigest
	data.ArtifactID = receipt.ArtifactID
	data.ArtifactDigest = receipt.ArtifactDigest
	return domain.Event{
		Type:           domain.EventActionCompleted,
		Data:           data,
		IdempotencyKey: receipt.ActionID + ":action-completed",
	}
}

func actionFailedEvent(command domain.Command, reason string, outcome domain.ActionOutcome) domain.Event {
	return domain.Event{
		Type: domain.EventActionFailed,
		Data: domain.EventData{
			ActionID:   command.ActionID,
			ActionType: command.Type,
			Reason:     reason,
			Outcome:    outcome,
		},
		IdempotencyKey: command.ActionID + ":action-failed:" + string(outcome),
	}
}

type reasonerActionExecutor struct {
	reasoner reasoner.Reasoner
}

type artifactActionExecutor struct {
	base      ActionExecutor
	artifacts *store.PostgresArtifactStore
}

// NewArtifactActionExecutor adds the one phase-3 allowed Artifact side effect
// to the deterministic executor. The Artifact ID and bytes are stable, and the
// Artifact store treats an identical retry as idempotent.
func NewArtifactActionExecutor(
	runReasoner reasoner.Reasoner,
	artifacts *store.PostgresArtifactStore,
) ActionExecutor {
	return &artifactActionExecutor{
		base:      &reasonerActionExecutor{reasoner: runReasoner},
		artifacts: artifacts,
	}
}

func (executor *artifactActionExecutor) Execute(
	ctx context.Context,
	request ActionRequest,
) (lease.ActionReceipt, error) {
	receipt, err := executor.base.Execute(ctx, request)
	if err != nil || request.Command.Type != domain.CommandApplyPatch {
		return receipt, err
	}
	if executor.artifacts == nil {
		return lease.ActionReceipt{}, errors.New("PostgreSQL Artifact store is required")
	}
	artifactID := domain.ExpectedActionArtifactID(request.Command.ActionID)
	content := fmt.Sprintf(
		"phase-3 receipt artifact\nrun_id=%s\naction_id=%s\n",
		request.State.Run.ID,
		request.Command.ActionID,
	)
	record, err := executor.artifacts.Write(ctx, store.ArtifactInput{
		ID:        artifactID,
		RunID:     request.State.Run.ID,
		AttemptID: request.State.AttemptID,
		Type:      domain.Phase3ReceiptArtifactType,
		Content:   strings.NewReader(content),
	})
	if err != nil {
		return lease.ActionReceipt{}, fmt.Errorf("write action Artifact: %w", err)
	}
	receipt.ArtifactID = record.ID
	receipt.ArtifactDigest = record.Digest
	return receipt, nil
}

func (executor *reasonerActionExecutor) Execute(
	ctx context.Context,
	request ActionRequest,
) (lease.ActionReceipt, error) {
	if request.Command.Type != domain.CommandRunReasoner {
		return ExecuteDeterministicAction(request)
	}
	result, err := executor.reasoner.Reason(ctx, reasoner.Request{
		RunID:      request.State.Run.ID,
		ScenarioID: request.State.Run.ScenarioID,
		AttemptID:  request.State.AttemptID,
	})
	if err != nil {
		return lease.ActionReceipt{}, fmt.Errorf("reasoner error: %w", err)
	}
	if err := reasoner.ValidatePhase1Result(result); err != nil {
		return lease.ActionReceipt{}, err
	}
	return receiptForOutput(request, domain.EventData{
		Output:        result.Output,
		ToolName:      result.ToolCall.Name,
		ToolArguments: result.ToolCall.Arguments,
	}, 1, ""), nil
}

// ExecuteDeterministicAction is the minimal no-business-side-effect executor
// used by integration and Chaos tests.
func ExecuteDeterministicAction(request ActionRequest) (lease.ActionReceipt, error) {
	var output domain.EventData
	var cost int64
	switch request.Command.Type {
	case domain.CommandStartAttempt:
		output.AttemptID = request.Command.AttemptID
		output.Reason = "initial"
	case domain.CommandProvisionWorkspace:
	case domain.CommandRunReasoner:
		output.Output = "phase-3 deterministic reasoning"
		output.ToolName = reasoner.Phase1PatchTool
		output.ToolArguments = `{"patch":"phase-3"}`
		cost = 1
	case domain.CommandApplyPatch:
	case domain.CommandVerify:
	case domain.CommandSucceedRun:
	case domain.CommandFailRun:
		output.Reason = request.State.FailureReason
	case domain.CommandCancelRun:
	default:
		return lease.ActionReceipt{}, fmt.Errorf("unsupported managed action %s", request.Command.Type)
	}
	return receiptForOutput(request, output, cost, ""), nil
}

func receiptForOutput(
	request ActionRequest,
	output domain.EventData,
	cost int64,
	artifactID string,
) lease.ActionReceipt {
	digest, err := domain.DigestEventData(output)
	if err != nil {
		panic(err)
	}
	return lease.ActionReceipt{
		RunID:            request.State.Run.ID,
		ActionID:         request.Command.ActionID,
		ActionType:       request.Command.Type,
		IdempotencyScope: request.Scope,
		Output:           output,
		OutputDigest:     digest,
		ArtifactID:       artifactID,
		CostUnits:        cost,
	}
}
