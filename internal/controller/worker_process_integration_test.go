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
	process, _ := startIntegrationWorker(t, binary, dsn, pauseWorkerID, pauseRunID, 30*time.Millisecond)
	waitForIntegrationLease(t, manager, pauseRunID, pauseWorkerID, process, workerTestStartupTimeout)
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
	waitIntegrationCommand(t, process, 4*time.Second)
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
	cancelProcess, _ := startIntegrationWorker(
		t,
		binary,
		dsn,
		cancelWorkerID,
		cancelRunID,
		30*time.Millisecond,
	)
	waitForIntegrationLease(
		t,
		manager,
		cancelRunID,
		cancelWorkerID,
		cancelProcess,
		workerTestStartupTimeout,
	)
	if _, err := operator.SetDesiredState(ctx, cancelRunID, domain.DesiredCancelled); err != nil {
		t.Fatalf("SetDesiredState(Cancelled) error = %v", err)
	}
	waitIntegrationCommand(t, cancelProcess, 4*time.Second)
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
	oldLease := waitForIntegrationLease(
		t,
		manager,
		runID,
		workerA,
		first,
		workerTestStartupTimeout,
	)
	second, _ := startIntegrationWorkerWithTTL(
		t,
		binary,
		dsn,
		workerB,
		runID,
		500*time.Millisecond,
		20*time.Millisecond,
	)
	waitForWorkerRegistration(t, manager, workerB, second, workerTestStartupTimeout)
	if err := first.command.Process.Kill(); err != nil {
		t.Fatalf("kill Worker A: %v\n%s", err, firstOutput.String())
	}
	_ = first.wait()

	newLease := waitForIntegrationLease(t, manager, runID, workerB, second, 3*time.Second)
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
	waitIntegrationCommand(t, second, 4*time.Second)
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
	if _, err := manager.Acquire(
		ctx,
		runID,
		holderID,
		workerTestStartupTimeout+2*time.Second,
	); err != nil {
		t.Fatalf("Acquire(holder) error = %v", err)
	}
	waiter, _ := startIntegrationWorkerWithTTLAndPoll(
		t, binary, dsn, waiterID, runID, 200*time.Millisecond, 0, time.Second,
	)
	registered := waitForWorkerRegistration(
		t,
		manager,
		waiterID,
		waiter,
		workerTestStartupTimeout,
	)
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
			"waiting Worker heartbeat did not advance: registered=%s heartbeat=%s err=%v; %s",
			registered.HeartbeatAt,
			heartbeat.HeartbeatAt,
			err,
			waiter.diagnostics(err),
		)
	}
	if err := waiter.command.Process.Kill(); err != nil {
		t.Fatalf("kill waiting Worker: %v; %s", err, waiter.diagnostics(nil))
	}
	_ = waiter.wait()
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
	command.Env = os.Environ()
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
) (*testWorkerProcess, *synchronizedBuffer) {
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
) (*testWorkerProcess, *synchronizedBuffer) {
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
) (*testWorkerProcess, *synchronizedBuffer) {
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
	return startTestWorkerProcess(t, command)
}

func waitForIntegrationLease(
	t *testing.T,
	manager lease.Manager,
	runID string,
	workerID string,
	process *testWorkerProcess,
	timeout time.Duration,
) lease.Lease {
	t.Helper()
	waitCtx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()
	var lastErr error
	for {
		current, err := manager.Current(waitCtx, runID)
		if err == nil && current.WorkerID == workerID && current.ExpiresAt.After(time.Now().UTC()) {
			return current
		}
		lastErr = err
		select {
		case <-process.done:
			t.Fatalf(
				"Worker exited before acquiring Run %s: %s",
				runID,
				process.diagnostics(lastErr),
			)
		case <-waitCtx.Done():
			t.Fatalf(
				"Run %s was not leased by %s within %s: %s",
				runID,
				workerID,
				timeout,
				process.diagnostics(lastErr),
			)
		case <-ticker.C:
		}
	}
}

func waitForWorkerRegistration(
	t *testing.T,
	manager lease.Manager,
	workerID string,
	process *testWorkerProcess,
	timeout time.Duration,
) lease.Worker {
	t.Helper()
	waitCtx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()
	var lastErr error
	for {
		worker, err := manager.LookupWorker(waitCtx, workerID)
		if err == nil {
			return worker
		}
		lastErr = err
		select {
		case <-process.done:
			t.Fatalf(
				"Worker %s exited before registration: %s",
				workerID,
				process.diagnostics(lastErr),
			)
		case <-waitCtx.Done():
			t.Fatalf(
				"Worker %s was not registered within %s: %s",
				workerID,
				timeout,
				process.diagnostics(lastErr),
			)
		case <-ticker.C:
		}
	}
}

func waitIntegrationCommand(
	t *testing.T,
	process *testWorkerProcess,
	timeout time.Duration,
) {
	t.Helper()
	select {
	case <-process.done:
		err := process.wait()
		if err != nil {
			t.Fatalf("Worker process error: %s", process.diagnostics(nil))
		}
	case <-time.After(timeout):
		_ = process.command.Process.Kill()
		<-process.done
		t.Fatalf(
			"Worker process timed out after %s: %s",
			timeout,
			process.diagnostics(nil),
		)
	}
}
