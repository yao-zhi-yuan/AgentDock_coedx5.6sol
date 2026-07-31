//go:build chaos

package controller_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"math/rand"
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

func TestWorkerKillChaos100Converges(t *testing.T) {
	ctx := context.Background()
	dsn := chaosDatabaseURL()
	workerBinary := buildChaosWorker(t)
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
	artifactRoot := t.TempDir()
	artifactReader, err := store.NewPostgresArtifactStore(eventStore, artifactRoot)
	if err != nil {
		t.Fatalf("NewPostgresArtifactStore() error = %v", err)
	}
	operator := controller.New(eventStore, reasoner.NewFakeReasoner())
	random := rand.New(rand.NewSource(303))

	const iterations = 100
	succeeded := 0
	waitingApproval := 0
	artifactPresentAtKill := 0
	for iteration := 0; iteration < iterations; iteration++ {
		runID := fmt.Sprintf(
			"run-worker-kill-%03d-%s",
			iteration,
			time.Now().UTC().Format("20060102150405.000000000"),
		)
		if _, err := operator.CreateRun(ctx, controller.CreateRunRequest{
			RunID: runID, ScenarioID: "worker-kill-chaos", SpecHash: "phase-3",
		}); err != nil {
			t.Fatalf("iteration %d CreateRun() error = %v", iteration, err)
		}

		workerA := runID + "-worker-a"
		first, firstOutput := startChaosWorker(
			t,
			workerBinary,
			dsn,
			workerA,
			runID,
			artifactRoot,
			120*time.Millisecond,
			8*time.Millisecond,
			200*time.Millisecond,
		)
		waitForLeaseOwner(t, manager, runID, workerA, 2*time.Second)
		// Six actions each have an 8 ms pre-execution delay and an 8 ms
		// post-execution/pre-Receipt delay. ApplyPatch adds a 200 ms window
		// after Artifact publication and before Receipt persistence. The
		// seeded 1..270 ms random Kill spans early actions and that uncertainty
		// window while remaining shorter than a clean run. Deterministic
		// integration tests cover the complete seven-window matrix.
		time.Sleep(time.Duration(random.Intn(270)+1) * time.Millisecond)
		if err := first.Process.Kill(); err != nil {
			t.Fatalf("iteration %d kill Worker A: %v\noutput:\n%s", iteration, err, firstOutput.String())
		}
		if err := first.Wait(); err == nil {
			t.Fatalf("iteration %d killed Worker A returned nil error", iteration)
		}
		if chaosArtifactExistsAfterKill(t, ctx, manager, artifactReader, operator, runID) {
			artifactPresentAtKill++
		}

		workerB := runID + "-worker-b"
		second, secondOutput := startChaosWorker(
			t,
			workerBinary,
			dsn,
			workerB,
			runID,
			artifactRoot,
			250*time.Millisecond,
			0,
			0,
		)
		waitCommand(t, second, 4*time.Second, func(err error) {
			if err != nil {
				t.Fatalf(
					"iteration %d replacement Worker error = %v\nfirst:\n%s\nsecond:\n%s",
					iteration,
					err,
					firstOutput.String(),
					secondOutput.String(),
				)
			}
		})

		state, err := operator.GetRun(ctx, runID)
		if err != nil {
			t.Fatalf("iteration %d GetRun() error = %v", iteration, err)
		}
		switch state.Run.ObservedState {
		case domain.StatusSucceeded:
			succeeded++
		case domain.StatusWaitingApproval:
			waitingApproval++
		default:
			t.Fatalf("iteration %d final state = %s", iteration, state.Run.ObservedState)
		}
		assertChaosReceiptUniqueness(t, manager, artifactReader, runID)
	}
	if succeeded != iterations || waitingApproval != 0 {
		t.Fatalf(
			"100-kill convergence succeeded=%d waiting_approval=%d want 100/0",
			succeeded,
			waitingApproval,
		)
	}
	if artifactPresentAtKill == 0 {
		t.Fatal("100-kill run never reached an Artifact-present crash window")
	}
	t.Logf(
		"worker-kill statistics: iterations=%d killed=100 succeeded=%d waiting_approval=%d artifact_present_at_kill=%d",
		iterations,
		succeeded,
		waitingApproval,
		artifactPresentAtKill,
	)
}

func buildChaosWorker(t *testing.T) string {
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

func startChaosWorker(
	t *testing.T,
	binary string,
	dsn string,
	workerID string,
	runID string,
	artifactRoot string,
	ttl time.Duration,
	actionDelay time.Duration,
	artifactReceiptDelay time.Duration,
) (*exec.Cmd, *bytes.Buffer) {
	t.Helper()
	command := exec.Command(
		binary,
		"--database-url", dsn,
		"--worker-id", workerID,
		"--run-id", runID,
		"--artifact-root", artifactRoot,
		"--lease-ttl", ttl.String(),
		"--heartbeat", (ttl / 4).String(),
		"--poll", "2ms",
		"--action-delay", actionDelay.String(),
		"--post-action-delay", actionDelay.String(),
		"--artifact-receipt-delay", artifactReceiptDelay.String(),
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

func chaosArtifactExistsAfterKill(
	t *testing.T,
	ctx context.Context,
	manager lease.Manager,
	artifacts *store.PostgresArtifactStore,
	operator *controller.Controller,
	runID string,
) bool {
	t.Helper()
	state, err := operator.GetRun(ctx, runID)
	if err != nil {
		t.Fatalf("GetRun(after kill) error = %v", err)
	}
	actionID := ""
	if state.Run.ObservedState == domain.StatusActing {
		actionID = state.PendingActionID
		if actionID == "" {
			actionID = domain.Decide(state).ActionID
		}
	}
	if actionID != "" {
		if _, err := artifacts.Get(ctx, "action-"+actionID); err == nil {
			return true
		} else if !errors.Is(err, store.ErrArtifactNotFound) {
			t.Fatalf("Get(Artifact after kill) error = %v", err)
		}
	}
	receipts, err := manager.ListReceipts(ctx, runID)
	if err != nil {
		t.Fatalf("ListReceipts(after kill) error = %v", err)
	}
	for _, receipt := range receipts {
		if receipt.ActionType == domain.CommandApplyPatch && receipt.ArtifactID != "" {
			return true
		}
	}
	return false
}

func waitForLeaseOwner(
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

func waitCommand(t *testing.T, command *exec.Cmd, timeout time.Duration, check func(error)) {
	t.Helper()
	result := make(chan error, 1)
	go func() { result <- command.Wait() }()
	select {
	case err := <-result:
		check(err)
	case <-time.After(timeout):
		_ = command.Process.Kill()
		<-result
		t.Fatalf("command timed out after %s", timeout)
	}
}

func assertChaosReceiptUniqueness(
	t *testing.T,
	manager lease.Manager,
	artifacts *store.PostgresArtifactStore,
	runID string,
) {
	t.Helper()
	receipts, err := manager.ListReceipts(context.Background(), runID)
	if err != nil {
		t.Fatalf("ListReceipts() error = %v", err)
	}
	actions := make(map[string]struct{}, len(receipts))
	artifactIDs := make(map[string]struct{}, len(receipts))
	for _, receipt := range receipts {
		if _, duplicate := actions[receipt.ActionID]; duplicate {
			t.Fatalf("duplicate receipt accounting for %s", receipt.ActionID)
		}
		actions[receipt.ActionID] = struct{}{}
		if receipt.ArtifactID != "" {
			if _, duplicate := artifactIDs[receipt.ArtifactID]; duplicate {
				t.Fatalf("duplicate Artifact accounting for %s", receipt.ArtifactID)
			}
			artifactIDs[receipt.ArtifactID] = struct{}{}
			record, err := artifacts.Get(context.Background(), receipt.ArtifactID)
			if err != nil {
				t.Fatalf("Get(Artifact %s) error = %v", receipt.ArtifactID, err)
			}
			content, err := os.ReadFile(record.Path)
			if err != nil {
				t.Fatalf("ReadFile(Artifact %s) error = %v", receipt.ArtifactID, err)
			}
			digest := fmt.Sprintf("sha256:%x", sha256.Sum256(content))
			if record.RunID != runID || record.Digest != digest {
				t.Fatalf("Artifact %s record=%#v digest=%s", receipt.ArtifactID, record, digest)
			}
		}
	}
}

func chaosDatabaseURL() string {
	if value := os.Getenv("AGENTDOCK_DATABASE_URL"); value != "" {
		return value
	}
	return "postgres://agentdock:agentdock_dev_only@127.0.0.1:55433/agentdock?sslmode=disable"
}
