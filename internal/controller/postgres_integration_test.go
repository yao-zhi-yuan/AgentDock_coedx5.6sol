//go:build integration

package controller_test

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/agentdock/agentdock-verify/internal/controller"
	"github.com/agentdock/agentdock-verify/internal/domain"
	"github.com/agentdock/agentdock-verify/internal/reasoner"
	"github.com/agentdock/agentdock-verify/internal/store"
)

func TestPostgresReasoningResultSurvivesConcurrentPause(t *testing.T) {
	ctx := context.Background()
	dsn := controllerIntegrationDatabaseURL()
	runID := "run-postgres-reason-pause-" + time.Now().UTC().Format("20060102150405.000000000")
	firstStore, err := store.NewPostgresEventStore(ctx, dsn)
	if err != nil {
		t.Fatalf("NewPostgresEventStore(first) error = %v", err)
	}
	defer firstStore.Close()
	secondStore, err := store.NewPostgresEventStore(ctx, dsn)
	if err != nil {
		t.Fatalf("NewPostgresEventStore(second) error = %v", err)
	}
	defer secondStore.Close()

	blocking := newBlockingReasoner(reasoner.Result{
		Output: "durable concurrent result",
		ToolCall: &reasoner.ToolCall{
			Name:      reasoner.Phase1PatchTool,
			Arguments: `{"patch":"postgres"}`,
		},
	})
	reconciler := controller.New(firstStore, blocking)
	operator := controller.New(secondStore, reasoner.NewFakeReasoner())
	mustCreate(t, ctx, reconciler, runID)
	advanceToReasoning(t, ctx, reconciler, runID)

	resultCh := reconcileInBackground(ctx, reconciler, runID)
	waitSignal(t, blocking.entered, "PostgreSQL Reasoner call")
	paused, err := operator.SetDesiredState(ctx, runID, domain.DesiredPaused)
	if err != nil {
		t.Fatalf("concurrent pause error = %v", err)
	}
	if paused.Run.ObservedState != domain.StatusPaused || paused.ResumeState != domain.StatusReasoning {
		t.Fatalf("pause state = %#v", paused)
	}
	close(blocking.release)

	result := waitReconcile(t, resultCh)
	if result.err != nil {
		t.Fatalf("PostgreSQL Reasoning Reconcile() error = %v", result.err)
	}
	if result.result.State.Run.ObservedState != domain.StatusPaused ||
		result.result.State.ResumeState != domain.StatusActing ||
		result.result.State.ReasoningOutput != "durable concurrent result" {
		t.Fatalf("concurrent result was not durably reduced after Pause: %#v", result.result.State)
	}
	assertContainsEventTypes(t, result.result.Events, domain.EventReasoningCompleted)
}

func TestControllerContinuesSameRunAfterAllProcessObjectsAreDiscarded(t *testing.T) {
	ctx := context.Background()
	dsn := controllerIntegrationDatabaseURL()
	runID := "run-controller-restart-" + time.Now().UTC().Format("20060102150405.000000000")

	firstStore, err := store.NewPostgresEventStore(ctx, dsn)
	if err != nil {
		t.Fatalf("NewPostgresEventStore(first) error = %v", err)
	}
	firstController := controller.New(firstStore, reasoner.NewFakeReasoner())
	if _, err := firstController.CreateRun(ctx, controller.CreateRunRequest{
		RunID:      runID,
		ScenarioID: "restart",
		SpecHash:   "restart-spec",
	}); err != nil {
		firstStore.Close()
		t.Fatalf("CreateRun() error = %v", err)
	}
	for step := 0; step < 2; step++ {
		if _, err := firstController.Reconcile(ctx, runID); err != nil {
			firstStore.Close()
			t.Fatalf("first process Reconcile(%d) error = %v", step+1, err)
		}
	}
	before, err := firstController.GetRun(ctx, runID)
	if err != nil {
		firstStore.Close()
		t.Fatalf("first process GetRun() error = %v", err)
	}
	if _, err := firstStore.SaveCheckpoint(ctx, runID, before.Run.Version); err != nil {
		firstStore.Close()
		t.Fatalf("SaveCheckpoint() error = %v", err)
	}
	firstStore.Close()
	firstController = nil
	firstStore = nil

	secondStore, err := store.NewPostgresEventStore(ctx, dsn)
	if err != nil {
		t.Fatalf("NewPostgresEventStore(second) error = %v", err)
	}
	defer secondStore.Close()
	secondController := controller.New(secondStore, reasoner.NewFakeReasoner())
	for step := 0; step < 10; step++ {
		state, getErr := secondController.GetRun(ctx, runID)
		if getErr != nil {
			t.Fatalf("second process GetRun(%d) error = %v", step+1, getErr)
		}
		if state.Run.ObservedState.Terminal() {
			break
		}
		if _, reconcileErr := secondController.Reconcile(ctx, runID); reconcileErr != nil {
			t.Fatalf("second process Reconcile(%d) error = %v", step+1, reconcileErr)
		}
	}

	finalState, err := secondController.GetRun(ctx, runID)
	if err != nil {
		t.Fatalf("final GetRun() error = %v", err)
	}
	if finalState.Run.ObservedState != domain.StatusSucceeded {
		t.Fatalf("final state = %s, want Succeeded", finalState.Run.ObservedState)
	}
	events, err := secondController.Events(ctx, runID)
	if err != nil {
		t.Fatalf("Events() error = %v", err)
	}
	for index, event := range events {
		if event.Seq != uint64(index+1) {
			t.Fatalf("event index %d seq = %d, want %d", index, event.Seq, index+1)
		}
	}
}

func controllerIntegrationDatabaseURL() string {
	if value := os.Getenv("AGENTDOCK_DATABASE_URL"); value != "" {
		return value
	}
	return "postgres://agentdock:agentdock_dev_only@127.0.0.1:55433/agentdock?sslmode=disable"
}
