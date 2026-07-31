//go:build integration

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/agentdock/agentdock-verify/internal/controller"
	"github.com/agentdock/agentdock-verify/internal/domain"
	"github.com/agentdock/agentdock-verify/internal/lease"
	"github.com/agentdock/agentdock-verify/internal/reasoner"
	"github.com/agentdock/agentdock-verify/internal/store"
)

func TestCLIContinuesRunAcrossSeparateProcesses(t *testing.T) {
	databaseURL := os.Getenv("AGENTDOCK_DATABASE_URL")
	if databaseURL == "" {
		databaseURL = "postgres://agentdock:agentdock_dev_only@127.0.0.1:55433/agentdock?sslmode=disable"
	}
	runID := fmt.Sprintf("run-cli-process-%d", time.Now().UnixNano())

	var created domain.State
	runCLIProcess(
		t,
		databaseURL,
		&created,
		"run", "create", runID, "--scenario", "process-restart", "--spec-hash", "phase-2",
	)
	if created.Run.Version != 1 || created.Run.ObservedState != domain.StatusQueued {
		t.Fatalf("created State = %#v, want version 1 Queued", created)
	}

	var lastState domain.State
	for step := 0; step < 12; step++ {
		var result struct {
			State domain.State `json:"state"`
		}
		runCLIProcess(t, databaseURL, &result, "run", "step", runID)
		lastState = result.State
		if lastState.Run.ObservedState == domain.StatusSucceeded {
			break
		}
	}
	if lastState.Run.ObservedState != domain.StatusSucceeded {
		t.Fatalf("separate CLI processes ended at %#v, want Succeeded", lastState)
	}

	var events []domain.Event
	runCLIProcess(t, databaseURL, &events, "run", "events", runID)
	if len(events) != int(lastState.Run.Version) {
		t.Fatalf("events=%d State.version=%d", len(events), lastState.Run.Version)
	}
	for index, event := range events {
		if event.Seq != uint64(index+1) {
			t.Fatalf("events[%d].seq=%d, want %d", index, event.Seq, index+1)
		}
	}

	var rebuilt domain.State
	runCLIProcess(t, databaseURL, &rebuilt, "run", "get", runID)
	if rebuilt != lastState {
		t.Fatalf("fresh-process rebuilt State differs\n got: %#v\nwant: %#v", rebuilt, lastState)
	}
}

func TestDurableRunStepAndLegacyResultsCannotBypassManagedLease(t *testing.T) {
	ctx := context.Background()
	databaseURL := os.Getenv("AGENTDOCK_DATABASE_URL")
	if databaseURL == "" {
		databaseURL = "postgres://agentdock:agentdock_dev_only@127.0.0.1:55433/agentdock?sslmode=disable"
	}
	eventStore, err := store.NewPostgresEventStore(ctx, databaseURL)
	if err != nil {
		t.Fatalf("NewPostgresEventStore() error = %v", err)
	}
	defer eventStore.Close()
	manager, err := lease.NewPostgresManager(ctx, databaseURL)
	if err != nil {
		t.Fatalf("NewPostgresManager() error = %v", err)
	}
	defer manager.Close()
	artifacts, err := store.NewPostgresArtifactStore(eventStore, t.TempDir())
	if err != nil {
		t.Fatalf("NewPostgresArtifactStore() error = %v", err)
	}

	runID := fmt.Sprintf("run-cli-managed-fencing-%d", time.Now().UnixNano())
	workerID := runID + "-worker"
	faulted := false
	managed := controller.NewManaged(eventStore, reasoner.NewFakeReasoner(), manager, controller.ManagedOptions{
		Executor:  controller.NewArtifactActionExecutor(reasoner.NewFakeReasoner(), artifacts),
		Artifacts: artifacts,
		Fault: func(point controller.FaultPoint, command domain.Command) error {
			if !faulted &&
				point == controller.FaultAfterActionPlanned &&
				command.Type == domain.CommandApplyPatch {
				faulted = true
				return controller.ErrInjectedCrash
			}
			return nil
		},
	})
	if _, err := managed.CreateRun(ctx, controller.CreateRunRequest{
		RunID: runID, ScenarioID: "managed-cli-fencing", SpecHash: "phase-3",
	}); err != nil {
		t.Fatalf("CreateRun() error = %v", err)
	}
	if _, err := manager.Register(ctx, workerID); err != nil {
		t.Fatalf("Register() error = %v", err)
	}
	acquired, err := manager.Acquire(ctx, runID, workerID, 30*time.Second)
	if err != nil {
		t.Fatalf("Acquire() error = %v", err)
	}
	for step := 0; step < 8; step++ {
		_, err = managed.ReconcileLeased(ctx, runID, acquired.Lease)
		if errors.Is(err, controller.ErrInjectedCrash) {
			break
		}
		if err != nil {
			t.Fatalf("ReconcileLeased(%d) error = %v", step, err)
		}
	}
	if !faulted || !errors.Is(err, controller.ErrInjectedCrash) {
		t.Fatalf("ApplyPatch ActionPlanned crash not reached: faulted=%t err=%v", faulted, err)
	}
	pending, err := managed.GetRun(ctx, runID)
	if err != nil {
		t.Fatalf("GetRun(pending) error = %v", err)
	}
	if pending.PendingActionType != domain.CommandApplyPatch ||
		pending.Run.ObservedState != domain.StatusActing {
		t.Fatalf("pending managed state = %#v", pending)
	}
	eventsBefore, err := managed.Events(ctx, runID)
	if err != nil {
		t.Fatalf("Events(before bypass attempts) error = %v", err)
	}
	assertUnchanged := func(label string) {
		t.Helper()
		current, getErr := managed.GetRun(ctx, runID)
		if getErr != nil {
			t.Fatalf("%s GetRun() error = %v", label, getErr)
		}
		events, eventsErr := managed.Events(ctx, runID)
		if eventsErr != nil {
			t.Fatalf("%s Events() error = %v", label, eventsErr)
		}
		if current != pending ||
			len(events) != len(eventsBefore) ||
			current.Run.Version != uint64(len(eventsBefore)) {
			t.Fatalf(
				"%s changed managed Run: before=%#v after=%#v events_before=%d events_after=%d",
				label,
				pending,
				current,
				len(eventsBefore),
				len(events),
			)
		}
	}

	output, stepErr := runCLIProcessRaw(databaseURL, "run", "step", runID)
	if stepErr == nil {
		t.Fatalf("durable run step unexpectedly succeeded:\n%s", output)
	}
	if !strings.Contains(string(output), controller.ErrManagedRunRequiresLease.Error()) {
		t.Fatalf("durable run step error = %s, want managed Lease requirement", output)
	}
	assertUnchanged("unleased run step")

	_, err = eventStore.Append(ctx, pending.Run.Version, domain.Event{
		RunID:          runID,
		Type:           domain.EventPatchProduced,
		Data:           domain.EventData{ActionID: pending.PendingActionID},
		IdempotencyKey: pending.PendingActionID + ":unleased-legacy-patch",
	})
	if !errors.Is(err, store.ErrStaleFencingToken) {
		t.Fatalf("unleased legacy PatchProduced error = %v, want stale fencing rejection", err)
	}
	assertUnchanged("unleased legacy PatchProduced")

	_, err = eventStore.Append(ctx, pending.Run.Version, domain.Event{
		RunID:          runID,
		Type:           domain.EventPatchProduced,
		Data:           domain.EventData{ActionID: pending.PendingActionID},
		IdempotencyKey: pending.PendingActionID + ":fenced-legacy-patch",
		WorkerID:       acquired.WorkerID,
		FencingToken:   acquired.FencingToken,
	})
	if !errors.Is(err, store.ErrManagedRunLegacyEvent) {
		t.Fatalf("fenced legacy PatchProduced error = %v, want managed legacy-event rejection", err)
	}
	assertUnchanged("fenced legacy PatchProduced")

	recovered := controller.NewManaged(eventStore, reasoner.NewFakeReasoner(), manager, controller.ManagedOptions{
		Executor:  controller.NewArtifactActionExecutor(reasoner.NewFakeReasoner(), artifacts),
		Artifacts: artifacts,
	})
	result, err := recovered.ReconcileLeased(ctx, runID, acquired.Lease)
	if err != nil {
		t.Fatalf("legal ReconcileLeased() error = %v", err)
	}
	if result.State.Run.ObservedState != domain.StatusVerifying ||
		result.State.PendingActionID != "" ||
		result.State.LastCompletedActionID != pending.PendingActionID {
		t.Fatalf("legal Worker did not complete ApplyPatch: %#v", result.State)
	}
	eventsAfter, err := managed.Events(ctx, runID)
	if err != nil {
		t.Fatalf("Events(after legal completion) error = %v", err)
	}
	completed := eventsAfter[len(eventsAfter)-1]
	if len(eventsAfter) != len(eventsBefore)+1 ||
		completed.Type != domain.EventActionCompleted ||
		completed.WorkerID != acquired.WorkerID ||
		completed.FencingToken != acquired.FencingToken {
		t.Fatalf("legal Worker completion event = %#v events=%d", completed, len(eventsAfter))
	}

	var paused domain.State
	runCLIProcess(t, databaseURL, &paused, "run", "pause", runID)
	if paused.Run.DesiredState != domain.DesiredPaused ||
		paused.Run.ObservedState != domain.StatusPaused {
		t.Fatalf("managed pause state = %#v", paused.Run)
	}
	var resumed domain.State
	runCLIProcess(t, databaseURL, &resumed, "run", "resume", runID)
	if resumed.Run.DesiredState != domain.DesiredRunning ||
		resumed.Run.ObservedState != domain.StatusVerifying {
		t.Fatalf("managed resume state = %#v", resumed.Run)
	}
	var cancelledIntent domain.State
	runCLIProcess(t, databaseURL, &cancelledIntent, "run", "cancel", runID)
	if cancelledIntent.Run.DesiredState != domain.DesiredCancelled ||
		cancelledIntent.Run.ObservedState != domain.StatusVerifying {
		t.Fatalf("managed cancel intent state = %#v", cancelledIntent.Run)
	}
	cancelled, err := recovered.ReconcileLeased(ctx, runID, acquired.Lease)
	if err != nil {
		t.Fatalf("legal Worker cancel convergence error = %v", err)
	}
	if cancelled.State.Run.ObservedState != domain.StatusCancelled {
		t.Fatalf("legal Worker cancel state = %#v", cancelled.State.Run)
	}
}

func TestInitialLeaseWaitsForInFlightLegacyReconcile(t *testing.T) {
	ctx := context.Background()
	databaseURL := os.Getenv("AGENTDOCK_DATABASE_URL")
	if databaseURL == "" {
		databaseURL = "postgres://agentdock:agentdock_dev_only@127.0.0.1:55433/agentdock?sslmode=disable"
	}
	eventStore, err := store.NewPostgresEventStore(ctx, databaseURL)
	if err != nil {
		t.Fatalf("NewPostgresEventStore() error = %v", err)
	}
	defer eventStore.Close()
	manager, err := lease.NewPostgresManager(ctx, databaseURL)
	if err != nil {
		t.Fatalf("NewPostgresManager() error = %v", err)
	}
	defer manager.Close()

	runID := fmt.Sprintf("run-cli-inflight-legacy-%d", time.Now().UnixNano())
	workerID := runID + "-worker"
	reasoningStarted := make(chan struct{})
	releaseReasoning := make(chan struct{})
	legacy := controller.New(eventStore, blockingReasoner{
		started: reasoningStarted,
		release: releaseReasoning,
	})
	if _, err := legacy.CreateRun(ctx, controller.CreateRunRequest{
		RunID: runID, ScenarioID: "legacy-acquire-race", SpecHash: "phase-2",
	}); err != nil {
		t.Fatalf("CreateRun() error = %v", err)
	}
	for step := 0; step < 2; step++ {
		if _, err := legacy.Reconcile(ctx, runID); err != nil {
			t.Fatalf("Reconcile(%d) error = %v", step, err)
		}
	}
	if _, err := manager.Register(ctx, workerID); err != nil {
		t.Fatalf("Register() error = %v", err)
	}

	reconcileResult := make(chan error, 1)
	go func() {
		_, reconcileErr := legacy.Reconcile(ctx, runID)
		reconcileResult <- reconcileErr
	}()
	select {
	case <-reasoningStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("legacy Reconcile did not reach the Reasoner")
	}

	_, acquireErr := manager.Acquire(ctx, runID, workerID, 30*time.Second)
	if !errors.Is(acquireErr, lease.ErrLegacyExecutionInProgress) {
		close(releaseReasoning)
		reconcileErr := <-reconcileResult
		t.Fatalf(
			"Acquire() during legacy plan error = %v, want legacy execution in progress; Reconcile error after release = %v",
			acquireErr,
			reconcileErr,
		)
	}
	if !errors.Is(acquireErr, lease.ErrLeaseHeld) {
		close(releaseReasoning)
		<-reconcileResult
		t.Fatalf("legacy execution error = %v, want a retryable held Lease", acquireErr)
	}
	if _, err := manager.Current(ctx, runID); !errors.Is(err, lease.ErrLeaseNotFound) {
		close(releaseReasoning)
		<-reconcileResult
		t.Fatalf("Current() after rejected initial Acquire error = %v, want no Lease", err)
	}

	close(releaseReasoning)
	if err := <-reconcileResult; err != nil {
		t.Fatalf("legacy Reconcile completion error = %v", err)
	}
	completed, err := legacy.GetRun(ctx, runID)
	if err != nil {
		t.Fatalf("GetRun(after legacy completion) error = %v", err)
	}
	if completed.Run.ObservedState != domain.StatusActing ||
		completed.PendingActionID != "" {
		t.Fatalf("legacy completion state = %#v, want Acting without pending action", completed)
	}

	acquired, err := manager.Acquire(ctx, runID, workerID, 30*time.Second)
	if err != nil {
		t.Fatalf("Acquire(after legacy completion) error = %v", err)
	}
	if !acquired.Acquired || acquired.FencingToken != 1 {
		t.Fatalf("Acquire(after legacy completion) = %#v, want initial token 1", acquired)
	}
}

type blockingReasoner struct {
	started chan<- struct{}
	release <-chan struct{}
}

func (blocker blockingReasoner) Reason(
	ctx context.Context,
	request reasoner.Request,
) (reasoner.Result, error) {
	close(blocker.started)
	select {
	case <-blocker.release:
		return reasoner.NewFakeReasoner().Reason(ctx, request)
	case <-ctx.Done():
		return reasoner.Result{}, ctx.Err()
	}
}

func TestCLIHelperProcess(t *testing.T) {
	if os.Getenv("AGENTDOCK_CLI_HELPER_PROCESS") != "1" {
		return
	}
	separator := -1
	for index, argument := range os.Args {
		if argument == "--" {
			separator = index
			break
		}
	}
	if separator == -1 || separator+1 >= len(os.Args) {
		fmt.Fprintln(os.Stderr, "helper process is missing CLI arguments")
		os.Exit(2)
	}
	os.Exit(run(os.Args[separator+1:], os.Stdin, os.Stdout, os.Stderr))
}

func runCLIProcess(
	t *testing.T,
	databaseURL string,
	target any,
	arguments ...string,
) {
	t.Helper()
	output, err := runCLIProcessRaw(databaseURL, arguments...)
	if err != nil {
		t.Fatalf(
			"separate process agentdock %s error = %v\n%s",
			strings.Join(arguments, " "),
			err,
			output,
		)
	}
	if err := json.Unmarshal(output, target); err != nil {
		t.Fatalf(
			"decode separate process agentdock %s output: %v\n%s",
			strings.Join(arguments, " "),
			err,
			output,
		)
	}
}

func runCLIProcessRaw(databaseURL string, arguments ...string) ([]byte, error) {
	helperArguments := append(
		[]string{"-test.run=^TestCLIHelperProcess$", "--"},
		arguments...,
	)
	command := exec.Command(os.Args[0], helperArguments...)
	command.Env = append(
		os.Environ(),
		"AGENTDOCK_CLI_HELPER_PROCESS=1",
		"AGENTDOCK_DATABASE_URL="+databaseURL,
	)
	return command.CombinedOutput()
}
