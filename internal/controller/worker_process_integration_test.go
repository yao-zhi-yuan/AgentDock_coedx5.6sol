//go:build integration

package controller_test

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/agentdock/agentdock-verify/internal/controller"
	"github.com/agentdock/agentdock-verify/internal/domain"
	"github.com/agentdock/agentdock-verify/internal/lease"
	"github.com/agentdock/agentdock-verify/internal/reasoner"
	"github.com/agentdock/agentdock-verify/internal/store"
)

func TestPauseResumeCancelAcrossWorkerProcesses(t *testing.T) {
	ctx := context.Background()
	dsn := controllerIntegrationDatabaseURL()
	binary := buildIntegrationWorker(t)
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
	operator := controller.New(eventStore, reasoner.NewFakeReasoner())

	pauseRunID := "run-worker-pause-" + time.Now().UTC().Format("20060102150405.000000000")
	pauseWorkerID := pauseRunID + "-worker"
	if _, err := operator.CreateRun(ctx, controller.CreateRunRequest{
		RunID: pauseRunID, ScenarioID: "pause-resume", SpecHash: "phase-3",
	}); err != nil {
		t.Fatalf("CreateRun(pause) error = %v", err)
	}
	process, output := startIntegrationWorker(t, binary, dsn, pauseWorkerID, pauseRunID, 30*time.Millisecond)
	waitForIntegrationLease(t, manager, pauseRunID, pauseWorkerID, 2*time.Second)
	paused, err := operator.SetDesiredState(ctx, pauseRunID, domain.DesiredPaused)
	if err != nil {
		t.Fatalf("SetDesiredState(Paused) error = %v", err)
	}
	if paused.Run.ObservedState != domain.StatusPaused {
		t.Fatalf("paused state = %s", paused.Run.ObservedState)
	}
	time.Sleep(120 * time.Millisecond)
	stable, err := operator.GetRun(ctx, pauseRunID)
	if err != nil {
		t.Fatalf("GetRun(paused stable) error = %v", err)
	}
	stableReceipts, err := manager.ListReceipts(ctx, pauseRunID)
	if err != nil {
		t.Fatalf("ListReceipts(paused stable) error = %v", err)
	}
	time.Sleep(100 * time.Millisecond)
	unchanged, err := operator.GetRun(ctx, pauseRunID)
	if err != nil {
		t.Fatalf("GetRun(paused unchanged) error = %v", err)
	}
	unchangedReceipts, err := manager.ListReceipts(ctx, pauseRunID)
	if err != nil {
		t.Fatalf("ListReceipts(paused unchanged) error = %v", err)
	}
	if unchanged.Run.Version != stable.Run.Version ||
		len(unchangedReceipts) != len(stableReceipts) {
		t.Fatalf(
			"paused Worker advanced: version %d->%d receipts %d->%d",
			stable.Run.Version,
			unchanged.Run.Version,
			len(stableReceipts),
			len(unchangedReceipts),
		)
	}
	if _, err := operator.SetDesiredState(ctx, pauseRunID, domain.DesiredRunning); err != nil {
		t.Fatalf("SetDesiredState(Running) error = %v", err)
	}
	waitIntegrationCommand(t, process, output, 4*time.Second)
	pauseFinal, err := operator.GetRun(ctx, pauseRunID)
	if err != nil {
		t.Fatalf("GetRun(pause final) error = %v", err)
	}
	if pauseFinal.Run.ObservedState != domain.StatusSucceeded {
		t.Fatalf("pause/resume final state = %s", pauseFinal.Run.ObservedState)
	}

	cancelRunID := "run-worker-cancel-" + time.Now().UTC().Format("20060102150405.000000000")
	cancelWorkerID := cancelRunID + "-worker"
	if _, err := operator.CreateRun(ctx, controller.CreateRunRequest{
		RunID: cancelRunID, ScenarioID: "cancel", SpecHash: "phase-3",
	}); err != nil {
		t.Fatalf("CreateRun(cancel) error = %v", err)
	}
	cancelProcess, cancelOutput := startIntegrationWorker(
		t,
		binary,
		dsn,
		cancelWorkerID,
		cancelRunID,
		30*time.Millisecond,
	)
	waitForIntegrationLease(t, manager, cancelRunID, cancelWorkerID, 2*time.Second)
	if _, err := operator.SetDesiredState(ctx, cancelRunID, domain.DesiredCancelled); err != nil {
		t.Fatalf("SetDesiredState(Cancelled) error = %v", err)
	}
	waitIntegrationCommand(t, cancelProcess, cancelOutput, 4*time.Second)
	cancelFinal, err := operator.GetRun(ctx, cancelRunID)
	if err != nil {
		t.Fatalf("GetRun(cancel final) error = %v", err)
	}
	if cancelFinal.Run.ObservedState != domain.StatusCancelled {
		t.Fatalf("cancel final state = %s", cancelFinal.Run.ObservedState)
	}
}

func TestTwoWorkerKillTakeoverRejectsRestartedStaleToken(t *testing.T) {
	ctx := context.Background()
	dsn := controllerIntegrationDatabaseURL()
	binary := buildIntegrationWorker(t)
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
	operator := controller.New(eventStore, reasoner.NewFakeReasoner())
	runID := "run-manual-takeover-" + time.Now().UTC().Format("20060102150405.000000000")
	workerA := runID + "-worker-a"
	workerB := runID + "-worker-b"
	if _, err := operator.CreateRun(ctx, controller.CreateRunRequest{
		RunID: runID, ScenarioID: "manual-takeover", SpecHash: "phase-3",
	}); err != nil {
		t.Fatalf("CreateRun() error = %v", err)
	}

	first, firstOutput := startIntegrationWorkerWithTTL(
		t,
		binary,
		dsn,
		workerA,
		runID,
		120*time.Millisecond,
		100*time.Millisecond,
	)
	oldLease := waitForIntegrationLease(t, manager, runID, workerA, 2*time.Second)
	second, secondOutput := startIntegrationWorkerWithTTL(
		t,
		binary,
		dsn,
		workerB,
		runID,
		500*time.Millisecond,
		20*time.Millisecond,
	)
	waitForWorkerRegistration(t, manager, workerB, 2*time.Second)
	if err := first.Process.Kill(); err != nil {
		t.Fatalf("kill Worker A: %v\n%s", err, firstOutput.String())
	}
	_ = first.Wait()

	newLease := waitForIntegrationLease(t, manager, runID, workerB, 3*time.Second)
	if newLease.FencingToken <= oldLease.FencingToken {
		t.Fatalf("new token=%d old token=%d", newLease.FencingToken, oldLease.FencingToken)
	}
	probe := exec.Command(
		binary,
		"--database-url", dsn,
		"--worker-id", workerA,
		"--run-id", runID,
		"--stale-token-probe", fmt.Sprintf("%d", oldLease.FencingToken),
	)
	probeOutput, err := probe.CombinedOutput()
	if err != nil {
		t.Fatalf("restart Worker A stale-token probe error = %v\n%s", err, probeOutput)
	}
	if !bytes.Contains(probeOutput, []byte("stale_probe_rejected")) {
		t.Fatalf("restart Worker A stale-token probe output = %q", probeOutput)
	}
	waitIntegrationCommand(t, second, secondOutput, 4*time.Second)
	final, err := operator.GetRun(ctx, runID)
	if err != nil {
		t.Fatalf("GetRun(final) error = %v", err)
	}
	if final.Run.ObservedState != domain.StatusSucceeded {
		t.Fatalf("final state = %s", final.Run.ObservedState)
	}
	t.Logf(
		"manual takeover: A token=%d killed; B token=%d took over; A stale append rejected; final=%s",
		oldLease.FencingToken,
		newLease.FencingToken,
		final.Run.ObservedState,
	)
}

func TestWorkerHeartbeatsWhileWaitingForHeldLease(t *testing.T) {
	ctx := context.Background()
	dsn := controllerIntegrationDatabaseURL()
	binary := buildIntegrationWorker(t)
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
	operator := controller.New(eventStore, reasoner.NewFakeReasoner())
	runID := "run-worker-wait-heartbeat-" + time.Now().UTC().Format("20060102150405.000000000")
	holderID := runID + "-holder"
	waiterID := runID + "-waiter"
	if _, err := operator.CreateRun(ctx, controller.CreateRunRequest{
		RunID: runID, ScenarioID: "waiting-heartbeat", SpecHash: "phase-3",
	}); err != nil {
		t.Fatalf("CreateRun() error = %v", err)
	}
	if _, err := manager.Register(ctx, holderID); err != nil {
		t.Fatalf("Register(holder) error = %v", err)
	}
	if _, err := manager.Acquire(ctx, runID, holderID, 2*time.Second); err != nil {
		t.Fatalf("Acquire(holder) error = %v", err)
	}
	waiter, waiterOutput := startIntegrationWorkerWithTTLAndPoll(
		t, binary, dsn, waiterID, runID, 200*time.Millisecond, 0, time.Second,
	)
	deadline := time.Now().Add(2 * time.Second)
	var registered lease.Worker
	for time.Now().Before(deadline) {
		registered, err = manager.LookupWorker(ctx, waiterID)
		if err == nil {
			break
		}
		time.Sleep(2 * time.Millisecond)
	}
	if err != nil {
		t.Fatalf("LookupWorker(waiter registration) error = %v\n%s", err, waiterOutput.String())
	}
	heartbeatDeadline := time.Now().Add(500 * time.Millisecond)
	var heartbeat lease.Worker
	for time.Now().Before(heartbeatDeadline) {
		heartbeat, err = manager.LookupWorker(ctx, waiterID)
		if err == nil && heartbeat.HeartbeatAt.After(registered.HeartbeatAt) {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if err != nil || !heartbeat.HeartbeatAt.After(registered.HeartbeatAt) {
		t.Fatalf(
			"waiting Worker heartbeat did not advance: registered=%s heartbeat=%s err=%v\n%s",
			registered.HeartbeatAt,
			heartbeat.HeartbeatAt,
			err,
			waiterOutput.String(),
		)
	}
	if err := waiter.Process.Kill(); err != nil {
		t.Fatalf("kill waiting Worker: %v", err)
	}
	_ = waiter.Wait()
}

func buildIntegrationWorker(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatalf("resolve repository root: %v", err)
	}
	target := filepath.Join(t.TempDir(), "agentdock-worker")
	command := exec.Command("go", "build", "-o", target, "./cmd/worker")
	command.Dir = root
	command.Env = append(os.Environ(), "GOCACHE=/private/tmp/agentdock-go-cache")
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("build Worker binary: %v\n%s", err, output)
	}
	return target
}

func startIntegrationWorker(
	t *testing.T,
	binary string,
	dsn string,
	workerID string,
	runID string,
	actionDelay time.Duration,
) (*exec.Cmd, *bytes.Buffer) {
	t.Helper()
	return startIntegrationWorkerWithTTL(
		t,
		binary,
		dsn,
		workerID,
		runID,
		500*time.Millisecond,
		actionDelay,
	)
}

func startIntegrationWorkerWithTTL(
	t *testing.T,
	binary string,
	dsn string,
	workerID string,
	runID string,
	ttl time.Duration,
	actionDelay time.Duration,
) (*exec.Cmd, *bytes.Buffer) {
	return startIntegrationWorkerWithTTLAndPoll(
		t,
		binary,
		dsn,
		workerID,
		runID,
		ttl,
		actionDelay,
		2*time.Millisecond,
	)
}

func startIntegrationWorkerWithTTLAndPoll(
	t *testing.T,
	binary string,
	dsn string,
	workerID string,
	runID string,
	ttl time.Duration,
	actionDelay time.Duration,
	poll time.Duration,
) (*exec.Cmd, *bytes.Buffer) {
	t.Helper()
	command := exec.Command(
		binary,
		"--database-url", dsn,
		"--worker-id", workerID,
		"--run-id", runID,
		"--lease-ttl", ttl.String(),
		"--heartbeat", (ttl / 4).String(),
		"--poll", poll.String(),
		"--action-delay", actionDelay.String(),
	)
	output := &bytes.Buffer{}
	command.Stdout = output
	command.Stderr = output
	if err := command.Start(); err != nil {
		t.Fatalf("start Worker %s: %v", workerID, err)
	}
	t.Cleanup(func() {
		if command.ProcessState == nil {
			_ = command.Process.Kill()
			_, _ = command.Process.Wait()
		}
	})
	return command, output
}

func waitForIntegrationLease(
	t *testing.T,
	manager lease.Manager,
	runID string,
	workerID string,
	timeout time.Duration,
) lease.Lease {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		current, err := manager.Current(context.Background(), runID)
		if err == nil && current.WorkerID == workerID && current.ExpiresAt.After(time.Now().UTC()) {
			return current
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("Run %s was not leased by %s within %s", runID, workerID, timeout)
	return lease.Lease{}
}

func waitForWorkerRegistration(
	t *testing.T,
	manager lease.Manager,
	workerID string,
	timeout time.Duration,
) lease.Worker {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		worker, err := manager.LookupWorker(context.Background(), workerID)
		if err == nil {
			return worker
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("Worker %s was not registered within %s", workerID, timeout)
	return lease.Worker{}
}

func waitIntegrationCommand(
	t *testing.T,
	command *exec.Cmd,
	output *bytes.Buffer,
	timeout time.Duration,
) {
	t.Helper()
	result := make(chan error, 1)
	go func() { result <- command.Wait() }()
	select {
	case err := <-result:
		if err != nil {
			t.Fatalf("Worker process error = %v\n%s", err, output.String())
		}
	case <-time.After(timeout):
		_ = command.Process.Kill()
		<-result
		t.Fatalf("Worker process timed out after %s\n%s", timeout, output.String())
	}
}
