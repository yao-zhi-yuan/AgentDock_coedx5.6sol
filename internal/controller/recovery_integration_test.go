//go:build integration

package controller_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/agentdock/agentdock-verify/internal/controller"
	"github.com/agentdock/agentdock-verify/internal/domain"
	"github.com/agentdock/agentdock-verify/internal/lease"
	"github.com/agentdock/agentdock-verify/internal/reasoner"
	"github.com/agentdock/agentdock-verify/internal/store"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestCrashWindowsRecoverThroughTheSameLeasedReconcilePath(t *testing.T) {
	tests := []struct {
		name      string
		point     controller.FaultPoint
		wantCalls int
	}{
		{name: "before-planned", point: controller.FaultBeforeActionPlanned, wantCalls: 6},
		{name: "after-planned-before-execute", point: controller.FaultAfterActionPlanned, wantCalls: 6},
		{name: "after-execution-before-receipt", point: controller.FaultAfterActionExecution, wantCalls: 7},
		{name: "after-receipt-before-completed", point: controller.FaultAfterReceiptPersisted, wantCalls: 6},
		{name: "after-completed", point: controller.FaultAfterActionCompleted, wantCalls: 6},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			ctx := context.Background()
			dsn := controllerIntegrationDatabaseURL()
			eventStore, err := store.NewPostgresEventStore(ctx, dsn)
			if err != nil {
				t.Fatalf("NewPostgresEventStore() error = %v", err)
			}
			defer eventStore.Close()
			manager, err := lease.NewPostgresManager(ctx, dsn)
			if err != nil {
				t.Fatalf("NewPostgresManager() error = %v", err)
			}
			defer manager.Close()
			artifacts, err := store.NewPostgresArtifactStore(eventStore, t.TempDir())
			if err != nil {
				t.Fatalf("NewPostgresArtifactStore() error = %v", err)
			}

			runID := "run-crash-" + test.name + "-" + time.Now().UTC().Format("20060102150405.000000000")
			workerID := runID + "-worker"
			if _, err := manager.Register(ctx, workerID); err != nil {
				t.Fatalf("Register() error = %v", err)
			}
			acquired, err := manager.Acquire(ctx, runID, workerID, time.Second)
			if !errors.Is(err, store.ErrRunNotFound) {
				t.Fatalf("Acquire(before create) error = %v, want Run not found", err)
			}

			executor := &countingExecutor{
				next: controller.NewArtifactActionExecutor(reasoner.NewFakeReasoner(), artifacts),
			}
			crashOnce := sync.Once{}
			runtime := controller.NewManaged(eventStore, reasoner.NewFakeReasoner(), manager, controller.ManagedOptions{
				Executor:  executor,
				Artifacts: artifacts,
				Fault: func(point controller.FaultPoint, _ domain.Command) error {
					var fault error
					if point == test.point {
						crashOnce.Do(func() { fault = controller.ErrInjectedCrash })
					}
					return fault
				},
			})
			if _, err := runtime.CreateRun(ctx, controller.CreateRunRequest{
				RunID: runID, ScenarioID: "crash-window", SpecHash: "phase-3",
			}); err != nil {
				t.Fatalf("CreateRun() error = %v", err)
			}
			acquired, err = manager.Acquire(ctx, runID, workerID, time.Second)
			if err != nil {
				t.Fatalf("Acquire() error = %v", err)
			}
			if _, err := runtime.ReconcileLeased(ctx, runID, acquired.Lease); !errors.Is(err, controller.ErrInjectedCrash) {
				t.Fatalf("faulted ReconcileLeased() error = %v, want injected crash", err)
			}

			recovered := controller.NewManaged(eventStore, reasoner.NewFakeReasoner(), manager, controller.ManagedOptions{
				Executor:  executor,
				Artifacts: artifacts,
			})
			final := reconcileManagedToAllowedState(t, ctx, recovered, manager, acquired.Lease, runID)
			if final.Run.ObservedState != domain.StatusSucceeded {
				t.Fatalf("final state = %s, want Succeeded", final.Run.ObservedState)
			}
			if executor.Calls() != test.wantCalls {
				t.Fatalf("executor calls = %d, want %d", executor.Calls(), test.wantCalls)
			}
			assertNoDuplicateReceiptAccounting(t, ctx, manager, runID)
		})
	}
}

func TestCrashWhileExecutorIsInFlightRetriesScopedAction(t *testing.T) {
	ctx := context.Background()
	dsn := controllerIntegrationDatabaseURL()
	eventStore, err := store.NewPostgresEventStore(ctx, dsn)
	if err != nil {
		t.Fatalf("NewPostgresEventStore() error = %v", err)
	}
	defer eventStore.Close()
	manager, err := lease.NewPostgresManager(ctx, dsn)
	if err != nil {
		t.Fatalf("NewPostgresManager() error = %v", err)
	}
	defer manager.Close()
	artifacts, err := store.NewPostgresArtifactStore(eventStore, t.TempDir())
	if err != nil {
		t.Fatalf("NewPostgresArtifactStore() error = %v", err)
	}
	runID := "run-crash-in-executor-" + time.Now().UTC().Format("20060102150405.000000000")
	workerID := runID + "-worker"
	inFlight := newBlockingActionExecutor(domain.CommandStartAttempt)
	runtime := controller.NewManaged(eventStore, reasoner.NewFakeReasoner(), manager, controller.ManagedOptions{
		Executor: inFlight,
	})
	if _, err := runtime.CreateRun(ctx, controller.CreateRunRequest{
		RunID: runID, ScenarioID: "crash-in-executor", SpecHash: "phase-3",
	}); err != nil {
		t.Fatalf("CreateRun() error = %v", err)
	}
	if _, err := manager.Register(ctx, workerID); err != nil {
		t.Fatalf("Register() error = %v", err)
	}
	acquired, err := manager.Acquire(ctx, runID, workerID, time.Second)
	if err != nil {
		t.Fatalf("Acquire() error = %v", err)
	}
	executionCtx, cancelExecution := context.WithCancel(ctx)
	result := make(chan error, 1)
	go func() {
		_, reconcileErr := runtime.ReconcileLeased(executionCtx, runID, acquired.Lease)
		result <- reconcileErr
	}()
	select {
	case <-inFlight.entered:
	case <-time.After(time.Second):
		t.Fatal("Executor did not enter before crash")
	}
	cancelExecution()
	if err := <-result; !errors.Is(err, context.Canceled) {
		t.Fatalf("in-flight crash ReconcileLeased() error = %v, want context cancellation", err)
	}
	state, err := runtime.GetRun(ctx, runID)
	if err != nil {
		t.Fatalf("GetRun(after in-flight crash) error = %v", err)
	}
	if state.PendingActionID == "" || state.PendingActionType != domain.CommandStartAttempt {
		t.Fatalf("in-flight crash lost planned action: %#v", state)
	}
	receipts, err := manager.ListReceipts(ctx, runID)
	if err != nil {
		t.Fatalf("ListReceipts() error = %v", err)
	}
	if len(receipts) != 0 {
		t.Fatalf("in-flight crash persisted Receipts: %#v", receipts)
	}
	retryExecutor := &countingExecutor{
		next: controller.NewArtifactActionExecutor(reasoner.NewFakeReasoner(), artifacts),
	}
	recovered := controller.NewManaged(eventStore, reasoner.NewFakeReasoner(), manager, controller.ManagedOptions{
		Executor:  retryExecutor,
		Artifacts: artifacts,
	})
	final := reconcileManagedToAllowedState(t, ctx, recovered, manager, acquired.Lease, runID)
	if final.Run.ObservedState != domain.StatusSucceeded || retryExecutor.Calls() != 6 {
		t.Fatalf("in-flight recovery final=%s retry_calls=%d", final.Run.ObservedState, retryExecutor.Calls())
	}
}

func TestUnsafeAmbiguousRecoveryEntersWaitingApproval(t *testing.T) {
	ctx := context.Background()
	dsn := controllerIntegrationDatabaseURL()
	eventStore, err := store.NewPostgresEventStore(ctx, dsn)
	if err != nil {
		t.Fatalf("NewPostgresEventStore() error = %v", err)
	}
	defer eventStore.Close()
	manager, err := lease.NewPostgresManager(ctx, dsn)
	if err != nil {
		t.Fatalf("NewPostgresManager() error = %v", err)
	}
	defer manager.Close()

	runID := "run-unsafe-waiting-" + time.Now().UTC().Format("20060102150405.000000000")
	workerID := runID + "-worker"
	if _, err := manager.Register(ctx, workerID); err != nil {
		t.Fatalf("Register() error = %v", err)
	}
	runtime := controller.NewManaged(eventStore, reasoner.NewFakeReasoner(), manager, controller.ManagedOptions{
		Safety: func(domain.Command) domain.IdempotencyScope { return domain.IdempotencyUnsafe },
		Fault: func(point controller.FaultPoint, _ domain.Command) error {
			if point == controller.FaultAfterActionExecution {
				return controller.ErrInjectedCrash
			}
			return nil
		},
	})
	if _, err := runtime.CreateRun(ctx, controller.CreateRunRequest{
		RunID: runID, ScenarioID: "unsafe", SpecHash: "phase-3",
	}); err != nil {
		t.Fatalf("CreateRun() error = %v", err)
	}
	acquired, err := manager.Acquire(ctx, runID, workerID, time.Second)
	if err != nil {
		t.Fatalf("Acquire() error = %v", err)
	}
	if _, err := runtime.ReconcileLeased(ctx, runID, acquired.Lease); !errors.Is(err, controller.ErrInjectedCrash) {
		t.Fatalf("faulted ReconcileLeased() error = %v", err)
	}

	recovered := controller.NewManaged(eventStore, reasoner.NewFakeReasoner(), manager, controller.ManagedOptions{
		Safety: func(domain.Command) domain.IdempotencyScope { return domain.IdempotencyUnsafe },
	})
	result, err := recovered.ReconcileLeased(ctx, runID, acquired.Lease)
	if err != nil {
		t.Fatalf("recovery ReconcileLeased() error = %v", err)
	}
	if result.State.Run.ObservedState != domain.StatusWaitingApproval {
		t.Fatalf("recovery state = %s, want WaitingApproval", result.State.Run.ObservedState)
	}
	version := result.State.Run.Version
	second, err := recovered.ReconcileLeased(ctx, runID, acquired.Lease)
	if err != nil {
		t.Fatalf("second WaitingApproval ReconcileLeased() error = %v", err)
	}
	if second.Command.Type != domain.CommandNoop ||
		second.State.Run.ObservedState != domain.StatusWaitingApproval ||
		second.State.Run.Version != version {
		t.Fatalf("second WaitingApproval reconcile advanced: %#v", second)
	}
}

func TestCancelRacingStartAttemptCompletionDoesNotInsertAttemptZero(t *testing.T) {
	ctx := context.Background()
	dsn := controllerIntegrationDatabaseURL()
	eventStore, err := store.NewPostgresEventStore(ctx, dsn)
	if err != nil {
		t.Fatalf("NewPostgresEventStore() error = %v", err)
	}
	defer eventStore.Close()
	manager, err := lease.NewPostgresManager(ctx, dsn)
	if err != nil {
		t.Fatalf("NewPostgresManager() error = %v", err)
	}
	defer manager.Close()
	runID := "run-cancel-completion-race-" + time.Now().UTC().Format("20060102150405.000000000")
	workerID := runID + "-worker"
	executor := newBlockingActionExecutor(domain.CommandStartAttempt)
	runtime := controller.NewManaged(eventStore, reasoner.NewFakeReasoner(), manager, controller.ManagedOptions{
		Executor: executor,
	})
	operator := controller.New(eventStore, reasoner.NewFakeReasoner())
	if _, err := runtime.CreateRun(ctx, controller.CreateRunRequest{
		RunID: runID, ScenarioID: "cancel-completion-race", SpecHash: "phase-3",
	}); err != nil {
		t.Fatalf("CreateRun() error = %v", err)
	}
	if _, err := manager.Register(ctx, workerID); err != nil {
		t.Fatalf("Register() error = %v", err)
	}
	acquired, err := manager.Acquire(ctx, runID, workerID, time.Second)
	if err != nil {
		t.Fatalf("Acquire() error = %v", err)
	}
	reconcileResult := make(chan error, 1)
	go func() {
		_, reconcileErr := runtime.ReconcileLeased(ctx, runID, acquired.Lease)
		reconcileResult <- reconcileErr
	}()
	select {
	case <-executor.entered:
	case <-time.After(time.Second):
		t.Fatal("StartAttempt executor was not entered")
	}
	if _, err := operator.SetDesiredState(ctx, runID, domain.DesiredCancelled); err != nil {
		t.Fatalf("SetDesiredState(Cancelled) error = %v", err)
	}
	close(executor.release)
	if err := <-reconcileResult; err != nil {
		t.Fatalf("racing completion ReconcileLeased() error = %v", err)
	}
	result, err := runtime.ReconcileLeased(ctx, runID, acquired.Lease)
	if err != nil {
		t.Fatalf("cancel ReconcileLeased() error = %v", err)
	}
	if result.State.Run.ObservedState != domain.StatusCancelled ||
		result.State.Run.CurrentAttempt != 0 {
		t.Fatalf("cancel race final state = %#v", result.State.Run)
	}
}

func TestRecoveredReceiptRejectsTamperedArtifact(t *testing.T) {
	ctx := context.Background()
	dsn := controllerIntegrationDatabaseURL()
	eventStore, err := store.NewPostgresEventStore(ctx, dsn)
	if err != nil {
		t.Fatalf("NewPostgresEventStore() error = %v", err)
	}
	defer eventStore.Close()
	manager, err := lease.NewPostgresManager(ctx, dsn)
	if err != nil {
		t.Fatalf("NewPostgresManager() error = %v", err)
	}
	defer manager.Close()
	artifacts, err := store.NewPostgresArtifactStore(eventStore, t.TempDir())
	if err != nil {
		t.Fatalf("NewPostgresArtifactStore() error = %v", err)
	}
	runID := "run-tampered-artifact-" + time.Now().UTC().Format("20060102150405.000000000")
	workerID := runID + "-worker"
	crashed := false
	runtime := controller.NewManaged(eventStore, reasoner.NewFakeReasoner(), manager, controller.ManagedOptions{
		Executor:  controller.NewArtifactActionExecutor(reasoner.NewFakeReasoner(), artifacts),
		Artifacts: artifacts,
		Fault: func(point controller.FaultPoint, command domain.Command) error {
			if !crashed &&
				point == controller.FaultAfterReceiptPersisted &&
				command.Type == domain.CommandApplyPatch {
				crashed = true
				return controller.ErrInjectedCrash
			}
			return nil
		},
	})
	if _, err := runtime.CreateRun(ctx, controller.CreateRunRequest{
		RunID: runID, ScenarioID: "tampered-artifact", SpecHash: "phase-3",
	}); err != nil {
		t.Fatalf("CreateRun() error = %v", err)
	}
	if _, err := manager.Register(ctx, workerID); err != nil {
		t.Fatalf("Register() error = %v", err)
	}
	acquired, err := manager.Acquire(ctx, runID, workerID, time.Second)
	if err != nil {
		t.Fatalf("Acquire() error = %v", err)
	}
	for step := 0; step < 8; step++ {
		_, err = runtime.ReconcileLeased(ctx, runID, acquired.Lease)
		if errors.Is(err, controller.ErrInjectedCrash) {
			break
		}
		if err != nil {
			t.Fatalf("ReconcileLeased(%d) error = %v", step, err)
		}
	}
	if !crashed || !errors.Is(err, controller.ErrInjectedCrash) {
		t.Fatalf("ApplyPatch receipt crash was not reached: crashed=%t err=%v", crashed, err)
	}
	receipts, err := manager.ListReceipts(ctx, runID)
	if err != nil {
		t.Fatalf("ListReceipts() error = %v", err)
	}
	var applyPatchReceipt lease.ActionReceipt
	for _, receipt := range receipts {
		if receipt.ActionType == domain.CommandApplyPatch {
			applyPatchReceipt = receipt
		}
	}
	record, err := artifacts.Get(ctx, applyPatchReceipt.ArtifactID)
	if err != nil {
		t.Fatalf("Get(receipt Artifact) error = %v", err)
	}
	if err := os.WriteFile(record.Path, []byte("tampered after receipt persistence"), 0o600); err != nil {
		t.Fatalf("tamper Artifact: %v", err)
	}
	pending, err := runtime.GetRun(ctx, runID)
	if err != nil {
		t.Fatalf("GetRun(pending tampered Artifact) error = %v", err)
	}
	completionData := applyPatchReceipt.Output
	completionData.ActionID = applyPatchReceipt.ActionID
	completionData.ActionType = applyPatchReceipt.ActionType
	completionData.IdempotencyScope = applyPatchReceipt.IdempotencyScope
	completionData.ReceiptID = applyPatchReceipt.ID
	completionData.OutputDigest = applyPatchReceipt.OutputDigest
	completionData.ArtifactID = applyPatchReceipt.ArtifactID
	completionData.ArtifactDigest = applyPatchReceipt.ArtifactDigest
	if _, err := eventStore.Append(ctx, pending.Run.Version, domain.Event{
		RunID:          runID,
		Type:           domain.EventActionCompleted,
		Data:           completionData,
		IdempotencyKey: applyPatchReceipt.ActionID + ":direct-completed",
		WorkerID:       acquired.WorkerID,
		FencingToken:   acquired.FencingToken,
	}); !errors.Is(err, store.ErrArtifactIntegrity) {
		t.Fatalf("direct Append(tampered Artifact completion) error = %v, want Artifact integrity rejection", err)
	}
	stillPending, err := runtime.GetRun(ctx, runID)
	if err != nil {
		t.Fatalf("GetRun(after rejected direct completion) error = %v", err)
	}
	if stillPending.Run.Version != pending.Run.Version ||
		stillPending.PendingActionID != pending.PendingActionID {
		t.Fatalf("rejected direct completion changed durable state: before=%#v after=%#v", pending, stillPending)
	}
	recovered := controller.NewManaged(eventStore, reasoner.NewFakeReasoner(), manager, controller.ManagedOptions{
		Executor:  controller.NewArtifactActionExecutor(reasoner.NewFakeReasoner(), artifacts),
		Artifacts: artifacts,
	})
	result, err := recovered.ReconcileLeased(ctx, runID, acquired.Lease)
	if err != nil {
		t.Fatalf("recovered ReconcileLeased() error = %v", err)
	}
	if result.State.Run.ObservedState != domain.StatusWaitingApproval {
		t.Fatalf("tampered Artifact recovery state = %s, want WaitingApproval", result.State.Run.ObservedState)
	}
}

func TestStartAttemptExecutionFailureConvergesToFailed(t *testing.T) {
	ctx := context.Background()
	dsn := controllerIntegrationDatabaseURL()
	eventStore, err := store.NewPostgresEventStore(ctx, dsn)
	if err != nil {
		t.Fatalf("NewPostgresEventStore() error = %v", err)
	}
	defer eventStore.Close()
	manager, err := lease.NewPostgresManager(ctx, dsn)
	if err != nil {
		t.Fatalf("NewPostgresManager() error = %v", err)
	}
	defer manager.Close()
	runID := "run-start-failure-" + time.Now().UTC().Format("20060102150405.000000000")
	workerID := runID + "-worker"
	runtime := controller.NewManaged(eventStore, reasoner.NewFakeReasoner(), manager, controller.ManagedOptions{
		Executor: &failStartOnceExecutor{},
	})
	if _, err := runtime.CreateRun(ctx, controller.CreateRunRequest{
		RunID: runID, ScenarioID: "start-failure", SpecHash: "phase-3",
	}); err != nil {
		t.Fatalf("CreateRun() error = %v", err)
	}
	if _, err := manager.Register(ctx, workerID); err != nil {
		t.Fatalf("Register() error = %v", err)
	}
	acquired, err := manager.Acquire(ctx, runID, workerID, time.Second)
	if err != nil {
		t.Fatalf("Acquire() error = %v", err)
	}
	first, err := runtime.ReconcileLeased(ctx, runID, acquired.Lease)
	if err != nil {
		t.Fatalf("failed StartAttempt ReconcileLeased() error = %v", err)
	}
	if first.State.Run.ObservedState != domain.StatusQueued || first.State.FailureReason == "" {
		t.Fatalf("failed StartAttempt state = %#v", first.State)
	}
	second, err := runtime.ReconcileLeased(ctx, runID, acquired.Lease)
	if err != nil {
		t.Fatalf("FailRun ReconcileLeased() error = %v", err)
	}
	if second.Command.Type != domain.CommandFailRun ||
		second.State.Run.ObservedState != domain.StatusFailed {
		t.Fatalf("FailRun convergence = command:%s state:%s", second.Command.Type, second.State.Run.ObservedState)
	}
}

func TestMalformedDurableReceiptEntersWaitingApproval(t *testing.T) {
	ctx := context.Background()
	dsn := controllerIntegrationDatabaseURL()
	eventStore, err := store.NewPostgresEventStore(ctx, dsn)
	if err != nil {
		t.Fatalf("NewPostgresEventStore() error = %v", err)
	}
	defer eventStore.Close()
	manager, err := lease.NewPostgresManager(ctx, dsn)
	if err != nil {
		t.Fatalf("NewPostgresManager() error = %v", err)
	}
	defer manager.Close()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("pgxpool.New() error = %v", err)
	}
	defer pool.Close()
	runID := "run-malformed-receipt-" + time.Now().UTC().Format("20060102150405.000000000")
	workerID := runID + "-worker"
	runtime := controller.NewManaged(eventStore, reasoner.NewFakeReasoner(), manager, controller.ManagedOptions{
		Fault: func(point controller.FaultPoint, _ domain.Command) error {
			if point == controller.FaultAfterActionPlanned {
				return controller.ErrInjectedCrash
			}
			return nil
		},
	})
	if _, err := runtime.CreateRun(ctx, controller.CreateRunRequest{
		RunID: runID, ScenarioID: "malformed-receipt", SpecHash: "phase-3",
	}); err != nil {
		t.Fatalf("CreateRun() error = %v", err)
	}
	if _, err := manager.Register(ctx, workerID); err != nil {
		t.Fatalf("Register() error = %v", err)
	}
	acquired, err := manager.Acquire(ctx, runID, workerID, time.Second)
	if err != nil {
		t.Fatalf("Acquire() error = %v", err)
	}
	if _, err := runtime.ReconcileLeased(ctx, runID, acquired.Lease); !errors.Is(err, controller.ErrInjectedCrash) {
		t.Fatalf("planned ReconcileLeased() error = %v, want injected crash", err)
	}
	state, err := runtime.GetRun(ctx, runID)
	if err != nil {
		t.Fatalf("GetRun(planned) error = %v", err)
	}
	_, err = pool.Exec(ctx, `
		INSERT INTO action_receipts (
			receipt_id, run_id, action_id, action_type, idempotency_scope,
			output, cost_units, worker_id, fencing_token, created_at
		)
		VALUES ($1, $2, $3, $4, $5, '{}'::jsonb, 0, $6, $7, clock_timestamp())`,
		runID+":malformed-receipt",
		runID,
		state.PendingActionID,
		state.PendingActionType,
		state.PendingActionScope,
		workerID,
		int64(acquired.FencingToken),
	)
	if err != nil {
		t.Fatalf("insert malformed durable Receipt: %v", err)
	}
	recovered := controller.NewManaged(eventStore, reasoner.NewFakeReasoner(), manager, controller.ManagedOptions{})
	result, err := recovered.ReconcileLeased(ctx, runID, acquired.Lease)
	if err != nil {
		t.Fatalf("recover malformed Receipt error = %v", err)
	}
	if result.State.Run.ObservedState != domain.StatusWaitingApproval {
		t.Fatalf("malformed Receipt state = %s, want WaitingApproval", result.State.Run.ObservedState)
	}
}

func TestApplyPatchReceiptWithoutArtifactEntersWaitingApproval(t *testing.T) {
	ctx := context.Background()
	dsn := controllerIntegrationDatabaseURL()
	eventStore, manager, artifacts, current, state := preparePendingManagedAction(
		t,
		ctx,
		dsn,
		domain.CommandApplyPatch,
	)
	defer eventStore.Close()
	defer manager.Close()
	output := domain.EventData{}
	insertForgedReceipt(t, ctx, dsn, current, state, output, digestEventDataForTest(output), "", "")

	recovered := controller.NewManaged(eventStore, reasoner.NewFakeReasoner(), manager, controller.ManagedOptions{
		Artifacts: artifacts,
	})
	result, err := recovered.ReconcileLeased(ctx, state.Run.ID, current)
	if err != nil {
		t.Fatalf("ReconcileLeased(missing Artifact) error = %v", err)
	}
	if result.State.Run.ObservedState != domain.StatusWaitingApproval {
		t.Fatalf("missing Artifact recovery state = %s, want WaitingApproval", result.State.Run.ObservedState)
	}
}

func TestTamperedInlineReceiptDigestEntersWaitingApproval(t *testing.T) {
	ctx := context.Background()
	dsn := controllerIntegrationDatabaseURL()
	eventStore, manager, _, current, state := preparePendingManagedAction(
		t,
		ctx,
		dsn,
		domain.CommandRunReasoner,
	)
	defer eventStore.Close()
	defer manager.Close()
	output := domain.EventData{
		Output:        "tampered inline output",
		ToolName:      reasoner.Phase1PatchTool,
		ToolArguments: `{"patch":"tampered"}`,
	}
	different := domain.EventData{
		Output:        "different output",
		ToolName:      reasoner.Phase1PatchTool,
		ToolArguments: `{"patch":"different"}`,
	}
	insertForgedReceipt(t, ctx, dsn, current, state, output, digestEventDataForTest(different), "", "")

	recovered := controller.NewManaged(eventStore, reasoner.NewFakeReasoner(), manager, controller.ManagedOptions{})
	result, err := recovered.ReconcileLeased(ctx, state.Run.ID, current)
	if err != nil {
		t.Fatalf("ReconcileLeased(tampered inline digest) error = %v", err)
	}
	if result.State.Run.ObservedState != domain.StatusWaitingApproval {
		t.Fatalf("tampered inline recovery state = %s, want WaitingApproval", result.State.Run.ObservedState)
	}
}

func TestCrossRunReceiptArtifactEntersWaitingApproval(t *testing.T) {
	ctx := context.Background()
	dsn := controllerIntegrationDatabaseURL()
	eventStore, manager, artifacts, current, state := preparePendingManagedAction(
		t,
		ctx,
		dsn,
		domain.CommandApplyPatch,
	)
	defer eventStore.Close()
	defer manager.Close()
	otherRunID := state.Run.ID + "-other"
	operator := controller.New(eventStore, reasoner.NewFakeReasoner())
	if _, err := operator.CreateRun(ctx, controller.CreateRunRequest{
		RunID: otherRunID, ScenarioID: "cross-run-artifact", SpecHash: "phase-3",
	}); err != nil {
		t.Fatalf("CreateRun(other) error = %v", err)
	}
	started, err := operator.Reconcile(ctx, otherRunID)
	if err != nil {
		t.Fatalf("Reconcile(other StartAttempt) error = %v", err)
	}
	artifact, err := artifacts.Write(ctx, store.ArtifactInput{
		ID:        "action-" + state.PendingActionID,
		RunID:     otherRunID,
		AttemptID: started.State.AttemptID,
		Type:      "phase-3-action-receipt",
		Content:   strings.NewReader("cross-Run Artifact bytes"),
	})
	if err != nil {
		t.Fatalf("Write(cross-Run Artifact) error = %v", err)
	}
	output := domain.EventData{}
	insertForgedReceipt(
		t,
		ctx,
		dsn,
		current,
		state,
		output,
		digestEventDataForTest(output),
		artifact.ID,
		artifact.Digest,
	)
	recovered := controller.NewManaged(eventStore, reasoner.NewFakeReasoner(), manager, controller.ManagedOptions{
		Artifacts: artifacts,
	})
	result, err := recovered.ReconcileLeased(ctx, state.Run.ID, current)
	if err != nil {
		t.Fatalf("ReconcileLeased(cross-Run Artifact) error = %v", err)
	}
	if result.State.Run.ObservedState != domain.StatusWaitingApproval {
		t.Fatalf("cross-Run Artifact recovery state = %s, want WaitingApproval", result.State.Run.ObservedState)
	}
}

func preparePendingManagedAction(
	t *testing.T,
	ctx context.Context,
	dsn string,
	target domain.CommandType,
) (*store.PostgresEventStore, *lease.PostgresManager, *store.PostgresArtifactStore, lease.Lease, domain.State) {
	t.Helper()
	eventStore, err := store.NewPostgresEventStore(ctx, dsn)
	if err != nil {
		t.Fatalf("NewPostgresEventStore() error = %v", err)
	}
	manager, err := lease.NewPostgresManager(ctx, dsn)
	if err != nil {
		eventStore.Close()
		t.Fatalf("NewPostgresManager() error = %v", err)
	}
	artifacts, err := store.NewPostgresArtifactStore(eventStore, t.TempDir())
	if err != nil {
		manager.Close()
		eventStore.Close()
		t.Fatalf("NewPostgresArtifactStore() error = %v", err)
	}
	runID := "run-forged-receipt-" + string(target) + "-" + time.Now().UTC().Format("20060102150405.000000000")
	workerID := runID + "-worker"
	runtime := controller.NewManaged(eventStore, reasoner.NewFakeReasoner(), manager, controller.ManagedOptions{
		Artifacts: artifacts,
		Fault: func(point controller.FaultPoint, command domain.Command) error {
			if point == controller.FaultAfterActionPlanned && command.Type == target {
				return controller.ErrInjectedCrash
			}
			return nil
		},
	})
	if _, err := runtime.CreateRun(ctx, controller.CreateRunRequest{
		RunID: runID, ScenarioID: "forged-receipt", SpecHash: "phase-3",
	}); err != nil {
		manager.Close()
		eventStore.Close()
		t.Fatalf("CreateRun() error = %v", err)
	}
	if _, err := manager.Register(ctx, workerID); err != nil {
		manager.Close()
		eventStore.Close()
		t.Fatalf("Register() error = %v", err)
	}
	acquired, err := manager.Acquire(ctx, runID, workerID, 2*time.Second)
	if err != nil {
		manager.Close()
		eventStore.Close()
		t.Fatalf("Acquire() error = %v", err)
	}
	for step := 0; step < 8; step++ {
		_, err = runtime.ReconcileLeased(ctx, runID, acquired.Lease)
		if errors.Is(err, controller.ErrInjectedCrash) {
			break
		}
		if err != nil {
			manager.Close()
			eventStore.Close()
			t.Fatalf("ReconcileLeased(%d) error = %v", step, err)
		}
	}
	if !errors.Is(err, controller.ErrInjectedCrash) {
		manager.Close()
		eventStore.Close()
		t.Fatalf("target %s was not planned before crash: %v", target, err)
	}
	state, err := runtime.GetRun(ctx, runID)
	if err != nil {
		manager.Close()
		eventStore.Close()
		t.Fatalf("GetRun(pending) error = %v", err)
	}
	return eventStore, manager, artifacts, acquired.Lease, state
}

func insertForgedReceipt(
	t *testing.T,
	ctx context.Context,
	dsn string,
	current lease.Lease,
	state domain.State,
	output domain.EventData,
	outputDigest string,
	artifactID string,
	artifactDigest string,
) {
	t.Helper()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("pgxpool.New() error = %v", err)
	}
	defer pool.Close()
	payload, err := json.Marshal(output)
	if err != nil {
		t.Fatalf("marshal forged Receipt: %v", err)
	}
	_, err = pool.Exec(ctx, `
		INSERT INTO action_receipts (
			receipt_id, run_id, action_id, action_type, idempotency_scope,
			output, output_digest, artifact_id, artifact_digest, cost_units, worker_id,
			fencing_token, created_at
		)
		VALUES (
			$1, $2, $3, $4, $5, $6, $7, NULLIF($8, ''),
			NULLIF($9, ''), 0, $10, $11, clock_timestamp()
		)`,
		state.Run.ID+":"+state.PendingActionID+":forged",
		state.Run.ID,
		state.PendingActionID,
		state.PendingActionType,
		state.PendingActionScope,
		payload,
		outputDigest,
		artifactID,
		artifactDigest,
		current.WorkerID,
		int64(current.FencingToken),
	)
	if err != nil {
		t.Fatalf("insert forged durable Receipt: %v", err)
	}
}

func digestEventDataForTest(data domain.EventData) string {
	digest, _ := domain.DigestEventData(data)
	return digest
}

type countingExecutor struct {
	mu    sync.Mutex
	calls int
	next  controller.ActionExecutor
}

type failStartOnceExecutor struct {
	mu     sync.Mutex
	failed bool
}

func (executor *failStartOnceExecutor) Execute(
	_ context.Context,
	request controller.ActionRequest,
) (lease.ActionReceipt, error) {
	executor.mu.Lock()
	if request.Command.Type == domain.CommandStartAttempt && !executor.failed {
		executor.failed = true
		executor.mu.Unlock()
		return lease.ActionReceipt{}, errors.New("injected StartAttempt failure")
	}
	executor.mu.Unlock()
	return controller.ExecuteDeterministicAction(request)
}

type blockingActionExecutor struct {
	action  domain.CommandType
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func newBlockingActionExecutor(action domain.CommandType) *blockingActionExecutor {
	return &blockingActionExecutor{
		action:  action,
		entered: make(chan struct{}),
		release: make(chan struct{}),
	}
}

func (executor *blockingActionExecutor) Execute(
	ctx context.Context,
	request controller.ActionRequest,
) (lease.ActionReceipt, error) {
	if request.Command.Type == executor.action {
		executor.once.Do(func() { close(executor.entered) })
		select {
		case <-executor.release:
		case <-ctx.Done():
			return lease.ActionReceipt{}, ctx.Err()
		}
	}
	return controller.ExecuteDeterministicAction(request)
}

func (executor *countingExecutor) Execute(
	ctx context.Context,
	request controller.ActionRequest,
) (lease.ActionReceipt, error) {
	executor.mu.Lock()
	executor.calls++
	executor.mu.Unlock()
	if executor.next != nil {
		return executor.next.Execute(ctx, request)
	}
	return controller.ExecuteDeterministicAction(request)
}

func (executor *countingExecutor) Calls() int {
	executor.mu.Lock()
	defer executor.mu.Unlock()
	return executor.calls
}

func reconcileManagedToAllowedState(
	t *testing.T,
	ctx context.Context,
	runtime *controller.Controller,
	manager lease.Manager,
	current lease.Lease,
	runID string,
) domain.State {
	t.Helper()
	for step := 0; step < 32; step++ {
		state, err := runtime.GetRun(ctx, runID)
		if err != nil {
			t.Fatalf("GetRun(%d) error = %v", step, err)
		}
		if state.Run.ObservedState.Terminal() || state.Run.ObservedState == domain.StatusWaitingApproval {
			return state
		}
		renewed, err := manager.Renew(ctx, current, time.Second)
		if err != nil {
			t.Fatalf("Renew(%d) error = %v", step, err)
		}
		current = renewed
		if _, err := runtime.ReconcileLeased(ctx, runID, current); err != nil {
			t.Fatalf("ReconcileLeased(%d) error = %v", step, err)
		}
	}
	t.Fatal("Run did not converge in 32 managed reconciles")
	return domain.State{}
}

func assertNoDuplicateReceiptAccounting(t *testing.T, ctx context.Context, manager lease.Manager, runID string) {
	t.Helper()
	receipts, err := manager.ListReceipts(ctx, runID)
	if err != nil {
		t.Fatalf("ListReceipts() error = %v", err)
	}
	seenActions := make(map[string]struct{}, len(receipts))
	seenArtifacts := make(map[string]struct{}, len(receipts))
	for _, receipt := range receipts {
		if _, duplicate := seenActions[receipt.ActionID]; duplicate {
			t.Fatalf("duplicate receipt accounting for action %s", receipt.ActionID)
		}
		seenActions[receipt.ActionID] = struct{}{}
		if receipt.ArtifactID != "" {
			if _, duplicate := seenArtifacts[receipt.ArtifactID]; duplicate {
				t.Fatalf("duplicate Artifact accounting for %s", receipt.ArtifactID)
			}
			seenArtifacts[receipt.ArtifactID] = struct{}{}
		}
	}
}
