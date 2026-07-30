package controller_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/agentdock/agentdock-verify/internal/controller"
	"github.com/agentdock/agentdock-verify/internal/domain"
	"github.com/agentdock/agentdock-verify/internal/reasoner"
	"github.com/agentdock/agentdock-verify/internal/store"
)

func TestReconcileCompletesFakeRunAndPreservesIntermediateInvariants(t *testing.T) {
	ctx := context.Background()
	memory := store.NewMemoryEventStore()
	fake := reasoner.NewFakeReasoner()
	reconciler := controller.New(memory, fake)

	state, err := reconciler.CreateRun(ctx, controller.CreateRunRequest{
		RunID:      "run-success",
		ScenarioID: "scenario-001",
		SpecHash:   "spec-sha256",
		CreatedAt:  "2026-07-30T10:00:00Z",
	})
	if err != nil {
		t.Fatalf("CreateRun() error = %v", err)
	}
	assertState(t, state, domain.StatusQueued, 0, false)

	want := []struct {
		command  domain.CommandType
		status   domain.Status
		attempt  int
		verified bool
	}{
		{domain.CommandStartAttempt, domain.StatusProvisioning, 1, false},
		{domain.CommandProvisionWorkspace, domain.StatusReasoning, 1, false},
		{domain.CommandRunReasoner, domain.StatusActing, 1, false},
		{domain.CommandApplyPatch, domain.StatusVerifying, 1, false},
		{domain.CommandVerify, domain.StatusVerifying, 1, true},
		{domain.CommandSucceedRun, domain.StatusSucceeded, 1, true},
	}
	for index, step := range want {
		result, reconcileErr := reconciler.Reconcile(ctx, "run-success")
		if reconcileErr != nil {
			t.Fatalf("Reconcile() step %d error = %v", index+1, reconcileErr)
		}
		if result.Command.Type != step.command {
			t.Fatalf("step %d command = %s, want %s", index+1, result.Command.Type, step.command)
		}
		assertState(t, result.State, step.status, step.attempt, step.verified)
		if len(result.Events) == 0 {
			t.Fatalf("step %d produced no auditable event", index+1)
		}
	}
	if fake.CallCount() != 1 {
		t.Fatalf("FakeReasoner calls = %d, want 1", fake.CallCount())
	}

	terminal, err := reconciler.Reconcile(ctx, "run-success")
	if err != nil {
		t.Fatalf("terminal Reconcile() error = %v", err)
	}
	if terminal.Command.Type != domain.CommandNoop || len(terminal.Events) != 0 {
		t.Fatalf("terminal reconcile produced work: command=%s events=%d", terminal.Command.Type, len(terminal.Events))
	}

	events, err := memory.Load(ctx, "run-success")
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	assertContainsEventTypes(t, events,
		domain.EventRunCreated,
		domain.EventAttemptStarted,
		domain.EventReasoningCompleted,
		domain.EventVerificationPassed,
		domain.EventRunSucceeded,
	)
}

func TestPauseTenReconcilesProduceNoCommandsAndResumeSamePath(t *testing.T) {
	ctx := context.Background()
	memory := store.NewMemoryEventStore()
	fake := reasoner.NewFakeReasoner()
	reconciler := controller.New(memory, fake)
	mustCreate(t, ctx, reconciler, "run-paused")

	first, err := reconciler.Reconcile(ctx, "run-paused")
	if err != nil {
		t.Fatalf("first Reconcile() error = %v", err)
	}
	if first.State.Run.ObservedState != domain.StatusProvisioning {
		t.Fatalf("pre-pause state = %s, want Provisioning", first.State.Run.ObservedState)
	}

	paused, err := reconciler.SetDesiredState(ctx, "run-paused", domain.DesiredPaused)
	if err != nil {
		t.Fatalf("pause error = %v", err)
	}
	if paused.Run.ObservedState != domain.StatusPaused || paused.ResumeState != domain.StatusProvisioning {
		t.Fatalf("pause lost resume state: %#v", paused)
	}
	before, err := memory.Load(ctx, "run-paused")
	if err != nil {
		t.Fatalf("Load() before pause loop: %v", err)
	}

	for index := range 10 {
		result, reconcileErr := reconciler.Reconcile(ctx, "run-paused")
		if reconcileErr != nil {
			t.Fatalf("paused Reconcile() %d error = %v", index+1, reconcileErr)
		}
		if result.Command.Type != domain.CommandNoop || len(result.Events) != 0 {
			t.Fatalf("paused Reconcile() %d produced side effect: command=%s events=%d", index+1, result.Command.Type, len(result.Events))
		}
	}
	after, err := memory.Load(ctx, "run-paused")
	if err != nil {
		t.Fatalf("Load() after pause loop: %v", err)
	}
	if len(after) != len(before) {
		t.Fatalf("paused reconcile changed event count: before=%d after=%d", len(before), len(after))
	}
	if fake.CallCount() != 0 {
		t.Fatalf("paused reconcile called reasoner %d times", fake.CallCount())
	}

	resumed, err := reconciler.SetDesiredState(ctx, "run-paused", domain.DesiredRunning)
	if err != nil {
		t.Fatalf("resume error = %v", err)
	}
	if resumed.Run.ObservedState != domain.StatusProvisioning {
		t.Fatalf("resume state = %s, want Provisioning", resumed.Run.ObservedState)
	}
	next, err := reconciler.Reconcile(ctx, "run-paused")
	if err != nil {
		t.Fatalf("post-resume Reconcile() error = %v", err)
	}
	if next.Command.Type != domain.CommandProvisionWorkspace || next.State.Run.ObservedState != domain.StatusReasoning {
		t.Fatalf("resume did not continue path: command=%s state=%s", next.Command.Type, next.State.Run.ObservedState)
	}
}

func TestCancelStopsRun(t *testing.T) {
	ctx := context.Background()
	reconciler := controller.New(store.NewMemoryEventStore(), reasoner.NewFakeReasoner())
	mustCreate(t, ctx, reconciler, "run-cancelled")

	if _, err := reconciler.SetDesiredState(ctx, "run-cancelled", domain.DesiredCancelled); err != nil {
		t.Fatalf("cancel intent error = %v", err)
	}
	result, err := reconciler.Reconcile(ctx, "run-cancelled")
	if err != nil {
		t.Fatalf("cancel Reconcile() error = %v", err)
	}
	if result.Command.Type != domain.CommandCancelRun || result.State.Run.ObservedState != domain.StatusCancelled {
		t.Fatalf("cancel result = command:%s state:%s", result.Command.Type, result.State.Run.ObservedState)
	}
	again, err := reconciler.Reconcile(ctx, "run-cancelled")
	if err != nil {
		t.Fatalf("terminal cancel Reconcile() error = %v", err)
	}
	if again.Command.Type != domain.CommandNoop || len(again.Events) != 0 {
		t.Fatalf("cancelled run advanced: command=%s events=%d", again.Command.Type, len(again.Events))
	}
}

func TestCancelConvergesFromPausedRun(t *testing.T) {
	ctx := context.Background()
	reconciler := controller.New(store.NewMemoryEventStore(), reasoner.NewFakeReasoner())
	mustCreate(t, ctx, reconciler, "run-paused-cancel")

	if _, err := reconciler.SetDesiredState(ctx, "run-paused-cancel", domain.DesiredPaused); err != nil {
		t.Fatalf("pause error = %v", err)
	}
	if _, err := reconciler.SetDesiredState(ctx, "run-paused-cancel", domain.DesiredCancelled); err != nil {
		t.Fatalf("cancel intent error = %v", err)
	}
	result, err := reconciler.Reconcile(ctx, "run-paused-cancel")
	if err != nil {
		t.Fatalf("cancel Reconcile() error = %v", err)
	}
	if result.Command.Type != domain.CommandCancelRun || result.State.Run.ObservedState != domain.StatusCancelled {
		t.Fatalf("paused cancel result = command:%s state:%s", result.Command.Type, result.State.Run.ObservedState)
	}
}

func TestIllegalFakeToolCallBecomesControlledFailure(t *testing.T) {
	ctx := context.Background()
	memory := store.NewMemoryEventStore()
	fake := reasoner.NewFakeReasonerWithResult(reasoner.Result{
		ToolCall: &reasoner.ToolCall{Name: "host.shell", Arguments: `{}`},
	})
	reconciler := controller.New(memory, fake)
	mustCreate(t, ctx, reconciler, "run-illegal-tool")

	for index := 0; index < 2; index++ {
		if _, err := reconciler.Reconcile(ctx, "run-illegal-tool"); err != nil {
			t.Fatalf("setup Reconcile() %d error = %v", index+1, err)
		}
	}
	result, err := reconciler.Reconcile(ctx, "run-illegal-tool")
	if err != nil {
		t.Fatalf("illegal tool Reconcile() returned infrastructure error: %v", err)
	}
	if result.State.Run.ObservedState != domain.StatusReasoning {
		t.Fatalf("illegal tool prefix state = %s, want Reasoning", result.State.Run.ObservedState)
	}
	if result.State.FailureReason == "" {
		t.Fatal("controlled failure has no reason")
	}
	if !containsEventType(result.Events, domain.EventToolCallFailed) ||
		containsEventType(result.Events, domain.EventRunFailed) {
		t.Fatalf("controlled failure prefix events = %#v", result.Events)
	}

	converged, err := reconciler.Reconcile(ctx, "run-illegal-tool")
	if err != nil {
		t.Fatalf("failure convergence Reconcile() error = %v", err)
	}
	if converged.Command.Type != domain.CommandFailRun ||
		converged.State.Run.ObservedState != domain.StatusFailed ||
		!containsEventType(converged.Events, domain.EventRunFailed) {
		t.Fatalf("failure did not converge: command=%s state=%s events=%#v",
			converged.Command.Type,
			converged.State.Run.ObservedState,
			converged.Events,
		)
	}
	if fake.CallCount() != 1 {
		t.Fatalf("failed prefix called reasoner %d times, want 1", fake.CallCount())
	}

	terminal, err := reconciler.Reconcile(ctx, "run-illegal-tool")
	if err != nil {
		t.Fatalf("failed terminal Reconcile() error = %v", err)
	}
	if terminal.Command.Type != domain.CommandNoop {
		t.Fatalf("failed Run advanced with %s", terminal.Command.Type)
	}
}

func TestReasoningCompletionPersistsAcrossConcurrentPause(t *testing.T) {
	ctx := context.Background()
	memory := store.NewMemoryEventStore()
	blocking := newBlockingReasoner(reasoner.Result{
		Output: "blocked result",
		ToolCall: &reasoner.ToolCall{
			Name:      reasoner.Phase1PatchTool,
			Arguments: `{"patch":"fake"}`,
		},
	})
	reconciler := controller.New(memory, blocking)
	mustCreate(t, ctx, reconciler, "run-reason-pause")
	advanceToReasoning(t, ctx, reconciler, "run-reason-pause")

	resultCh := reconcileInBackground(ctx, reconciler, "run-reason-pause")
	waitSignal(t, blocking.entered, "Reasoner call")

	planned, err := memory.Load(ctx, "run-reason-pause")
	if err != nil {
		t.Fatalf("Load(planned) error = %v", err)
	}
	if !containsEventType(planned, domain.EventReasoningPlanned) {
		t.Fatalf("Reasoner was called without durable ReasoningPlanned: %#v", planned)
	}
	if _, err := reconciler.SetDesiredState(ctx, "run-reason-pause", domain.DesiredPaused); err != nil {
		t.Fatalf("pause error = %v", err)
	}
	close(blocking.release)

	result := waitReconcile(t, resultCh)
	if result.err != nil {
		t.Fatalf("in-flight reasoning completion error = %v", result.err)
	}
	if result.result.State.Run.ObservedState != domain.StatusPaused ||
		result.result.State.ResumeState != domain.StatusActing ||
		result.result.State.PendingActionID != "" ||
		!containsEventType(result.result.Events, domain.EventReasoningCompleted) {
		t.Fatalf("paused reasoning completion state/events = %#v / %#v", result.result.State, result.result.Events)
	}
	assertPausedNoop(t, ctx, reconciler, "run-reason-pause")

	resumed, err := reconciler.SetDesiredState(ctx, "run-reason-pause", domain.DesiredRunning)
	if err != nil {
		t.Fatalf("resume error = %v", err)
	}
	if resumed.Run.ObservedState != domain.StatusActing {
		t.Fatalf("resumed state = %s, want Acting", resumed.Run.ObservedState)
	}
	next, err := reconciler.Reconcile(ctx, "run-reason-pause")
	if err != nil {
		t.Fatalf("post-resume Reconcile() error = %v", err)
	}
	if next.Command.Type != domain.CommandApplyPatch ||
		next.State.Run.ObservedState != domain.StatusVerifying ||
		blocking.CallCount() != 1 {
		t.Fatalf("post-resume repeated/incorrect action: command=%s state=%s reasoner_calls=%d",
			next.Command.Type,
			next.State.Run.ObservedState,
			blocking.CallCount(),
		)
	}
}

func TestReasoningFailurePersistsAcrossConcurrentPauseAndConverges(t *testing.T) {
	ctx := context.Background()
	memory := store.NewMemoryEventStore()
	blocking := newBlockingReasoner(reasoner.Result{
		ToolCall: &reasoner.ToolCall{Name: "host.shell", Arguments: `{}`},
	})
	reconciler := controller.New(memory, blocking)
	mustCreate(t, ctx, reconciler, "run-reason-fail-pause")
	advanceToReasoning(t, ctx, reconciler, "run-reason-fail-pause")

	resultCh := reconcileInBackground(ctx, reconciler, "run-reason-fail-pause")
	waitSignal(t, blocking.entered, "Reasoner call")
	if _, err := reconciler.SetDesiredState(ctx, "run-reason-fail-pause", domain.DesiredPaused); err != nil {
		t.Fatalf("pause error = %v", err)
	}
	close(blocking.release)

	result := waitReconcile(t, resultCh)
	if result.err != nil {
		t.Fatalf("in-flight reasoning failure error = %v", result.err)
	}
	if result.result.State.Run.ObservedState != domain.StatusPaused ||
		result.result.State.ResumeState != domain.StatusReasoning ||
		result.result.State.FailureReason == "" ||
		!containsEventType(result.result.Events, domain.EventToolCallFailed) ||
		containsEventType(result.result.Events, domain.EventRunFailed) {
		t.Fatalf("paused reasoning failure state/events = %#v / %#v", result.result.State, result.result.Events)
	}
	assertPausedNoop(t, ctx, reconciler, "run-reason-fail-pause")

	if _, err := reconciler.SetDesiredState(ctx, "run-reason-fail-pause", domain.DesiredRunning); err != nil {
		t.Fatalf("resume error = %v", err)
	}
	converged, err := reconciler.Reconcile(ctx, "run-reason-fail-pause")
	if err != nil {
		t.Fatalf("failure convergence error = %v", err)
	}
	if converged.Command.Type != domain.CommandFailRun ||
		converged.State.Run.ObservedState != domain.StatusFailed ||
		blocking.CallCount() != 1 {
		t.Fatalf("failure did not converge without retry: command=%s state=%s reasoner_calls=%d",
			converged.Command.Type,
			converged.State.Run.ObservedState,
			blocking.CallCount(),
		)
	}
}

func TestVerificationCompletionPersistsAcrossConcurrentPause(t *testing.T) {
	ctx := context.Background()
	blockingStore := newBlockingVerificationStore()
	fake := reasoner.NewFakeReasoner()
	reconciler := controller.New(blockingStore, fake)
	mustCreate(t, ctx, reconciler, "run-verify-pause")
	advanceToVerifying(t, ctx, reconciler, "run-verify-pause")

	resultCh := reconcileInBackground(ctx, reconciler, "run-verify-pause")
	waitSignal(t, blockingStore.entered, "VerificationPassed append")

	planned, err := blockingStore.Load(ctx, "run-verify-pause")
	if err != nil {
		t.Fatalf("Load(planned) error = %v", err)
	}
	if !containsEventType(planned, domain.EventVerificationPlanned) {
		t.Fatalf("VerificationPassed started without durable VerificationPlanned: %#v", planned)
	}
	if _, err := reconciler.SetDesiredState(ctx, "run-verify-pause", domain.DesiredPaused); err != nil {
		t.Fatalf("pause error = %v", err)
	}
	close(blockingStore.release)

	result := waitReconcile(t, resultCh)
	if result.err != nil {
		t.Fatalf("in-flight verification completion error = %v", result.err)
	}
	if result.result.State.Run.ObservedState != domain.StatusPaused ||
		result.result.State.ResumeState != domain.StatusVerifying ||
		!result.result.State.VerificationPassed ||
		result.result.State.PendingActionID != "" ||
		!containsEventType(result.result.Events, domain.EventVerificationPassed) {
		t.Fatalf("paused verification completion state/events = %#v / %#v", result.result.State, result.result.Events)
	}
	assertPausedNoop(t, ctx, reconciler, "run-verify-pause")

	resumed, err := reconciler.SetDesiredState(ctx, "run-verify-pause", domain.DesiredRunning)
	if err != nil {
		t.Fatalf("resume error = %v", err)
	}
	if resumed.Run.ObservedState != domain.StatusVerifying || !resumed.VerificationPassed {
		t.Fatalf("resumed verification state = %#v", resumed)
	}
	converged, err := reconciler.Reconcile(ctx, "run-verify-pause")
	if err != nil {
		t.Fatalf("success convergence error = %v", err)
	}
	if converged.Command.Type != domain.CommandSucceedRun ||
		converged.State.Run.ObservedState != domain.StatusSucceeded ||
		fake.CallCount() != 1 {
		t.Fatalf("verification repeated/failed to converge: command=%s state=%s reasoner_calls=%d",
			converged.Command.Type,
			converged.State.Run.ObservedState,
			fake.CallCount(),
		)
	}
}

func TestSetDesiredStateRejectsTerminalRun(t *testing.T) {
	ctx := context.Background()
	reconciler := controller.New(store.NewMemoryEventStore(), reasoner.NewFakeReasoner())
	mustCreate(t, ctx, reconciler, "run-terminal")
	for {
		state, err := reconciler.GetRun(ctx, "run-terminal")
		if err != nil {
			t.Fatalf("GetRun() error = %v", err)
		}
		if state.Run.ObservedState.Terminal() {
			break
		}
		if _, err := reconciler.Reconcile(ctx, "run-terminal"); err != nil {
			t.Fatalf("Reconcile() error = %v", err)
		}
	}

	_, err := reconciler.SetDesiredState(ctx, "run-terminal", domain.DesiredPaused)
	if !errors.Is(err, domain.ErrTerminalState) {
		t.Fatalf("pause terminal error = %v, want terminal-state error", err)
	}
}

func mustCreate(t *testing.T, ctx context.Context, reconciler *controller.Controller, runID string) {
	t.Helper()
	if _, err := reconciler.CreateRun(ctx, controller.CreateRunRequest{RunID: runID, ScenarioID: "scenario", SpecHash: "spec"}); err != nil {
		t.Fatalf("CreateRun() error = %v", err)
	}
}

func assertState(t *testing.T, state domain.State, status domain.Status, attempt int, verified bool) {
	t.Helper()
	if state.Run.ObservedState != status ||
		state.Run.CurrentAttempt != attempt ||
		state.VerificationPassed != verified {
		t.Fatalf("state = status:%s attempt:%d verified:%t, want status:%s attempt:%d verified:%t",
			state.Run.ObservedState,
			state.Run.CurrentAttempt,
			state.VerificationPassed,
			status,
			attempt,
			verified,
		)
	}
}

func assertContainsEventTypes(t *testing.T, events []domain.Event, expected ...domain.EventType) {
	t.Helper()
	for _, eventType := range expected {
		if !containsEventType(events, eventType) {
			t.Fatalf("events do not contain %s: %#v", eventType, events)
		}
	}
}

func containsEventType(events []domain.Event, target domain.EventType) bool {
	for _, event := range events {
		if event.Type == target {
			return true
		}
	}
	return false
}

type blockingReasoner struct {
	mu      sync.Mutex
	result  reasoner.Result
	calls   int
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func newBlockingReasoner(result reasoner.Result) *blockingReasoner {
	return &blockingReasoner{
		result:  result,
		entered: make(chan struct{}),
		release: make(chan struct{}),
	}
}

func (blocking *blockingReasoner) Reason(ctx context.Context, _ reasoner.Request) (reasoner.Result, error) {
	blocking.mu.Lock()
	blocking.calls++
	blocking.mu.Unlock()
	blocking.once.Do(func() { close(blocking.entered) })

	select {
	case <-blocking.release:
		return blocking.result, nil
	case <-ctx.Done():
		return reasoner.Result{}, ctx.Err()
	}
}

func (blocking *blockingReasoner) CallCount() int {
	blocking.mu.Lock()
	defer blocking.mu.Unlock()
	return blocking.calls
}

type blockingVerificationStore struct {
	inner   *store.MemoryEventStore
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func newBlockingVerificationStore() *blockingVerificationStore {
	return &blockingVerificationStore{
		inner:   store.NewMemoryEventStore(),
		entered: make(chan struct{}),
		release: make(chan struct{}),
	}
}

func (blocking *blockingVerificationStore) Load(ctx context.Context, runID string) ([]domain.Event, error) {
	return blocking.inner.Load(ctx, runID)
}

func (blocking *blockingVerificationStore) Append(
	ctx context.Context,
	expectedVersion uint64,
	event domain.Event,
) (store.AppendResult, error) {
	if event.Type == domain.EventVerificationPassed {
		blocking.once.Do(func() { close(blocking.entered) })
		select {
		case <-blocking.release:
		case <-ctx.Done():
			return store.AppendResult{}, ctx.Err()
		}
	}
	return blocking.inner.Append(ctx, expectedVersion, event)
}

func (blocking *blockingVerificationStore) Rebuild(ctx context.Context, runID string) (domain.State, error) {
	return blocking.inner.Rebuild(ctx, runID)
}

type backgroundReconcileResult struct {
	result controller.ReconcileResult
	err    error
}

func reconcileInBackground(
	ctx context.Context,
	reconciler *controller.Controller,
	runID string,
) <-chan backgroundReconcileResult {
	results := make(chan backgroundReconcileResult, 1)
	go func() {
		result, err := reconciler.Reconcile(ctx, runID)
		results <- backgroundReconcileResult{result: result, err: err}
	}()
	return results
}

func waitSignal(t *testing.T, signal <-chan struct{}, description string) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(5 * time.Second):
		t.Fatalf("timed out waiting for %s", description)
	}
}

func waitReconcile(t *testing.T, results <-chan backgroundReconcileResult) backgroundReconcileResult {
	t.Helper()
	select {
	case result := <-results:
		return result
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for Reconcile")
		return backgroundReconcileResult{}
	}
}

func advanceToReasoning(t *testing.T, ctx context.Context, reconciler *controller.Controller, runID string) {
	t.Helper()
	for _, expected := range []domain.CommandType{domain.CommandStartAttempt, domain.CommandProvisionWorkspace} {
		result, err := reconciler.Reconcile(ctx, runID)
		if err != nil {
			t.Fatalf("advance to Reasoning with %s: %v", expected, err)
		}
		if result.Command.Type != expected {
			t.Fatalf("advance command = %s, want %s", result.Command.Type, expected)
		}
	}
}

func advanceToVerifying(t *testing.T, ctx context.Context, reconciler *controller.Controller, runID string) {
	t.Helper()
	advanceToReasoning(t, ctx, reconciler, runID)
	for _, expected := range []domain.CommandType{domain.CommandRunReasoner, domain.CommandApplyPatch} {
		result, err := reconciler.Reconcile(ctx, runID)
		if err != nil {
			t.Fatalf("advance to Verifying with %s: %v", expected, err)
		}
		if result.Command.Type != expected {
			t.Fatalf("advance command = %s, want %s", result.Command.Type, expected)
		}
	}
}

func assertPausedNoop(t *testing.T, ctx context.Context, reconciler *controller.Controller, runID string) {
	t.Helper()
	result, err := reconciler.Reconcile(ctx, runID)
	if err != nil {
		t.Fatalf("paused Reconcile() error = %v", err)
	}
	if result.Command.Type != domain.CommandNoop || len(result.Events) != 0 {
		t.Fatalf("paused Reconcile() produced command=%s events=%d", result.Command.Type, len(result.Events))
	}
}
