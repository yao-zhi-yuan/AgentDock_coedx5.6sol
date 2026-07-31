package worker_test

import (
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/agentdock/agentdock-verify/internal/controller"
	"github.com/agentdock/agentdock-verify/internal/domain"
	"github.com/agentdock/agentdock-verify/internal/lease"
	"github.com/agentdock/agentdock-verify/internal/reasoner"
	"github.com/agentdock/agentdock-verify/internal/store"
	"github.com/agentdock/agentdock-verify/internal/worker"
)

func TestRunHeartbeatsIndependentlyWhileLeaseIsHeld(t *testing.T) {
	manager := newFakeManager()
	manager.held = true
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() {
		result <- worker.Run(ctx, manager, nil, worker.Config{
			WorkerID:          "waiting-worker",
			RunID:             "waiting-run",
			LeaseTTL:          100 * time.Millisecond,
			HeartbeatInterval: 10 * time.Millisecond,
			PollInterval:      time.Second,
			Output:            io.Discard,
		})
	}()
	waitForCount(t, time.Second, manager.HeartbeatCount, 2, "heartbeats")
	cancel()
	if err := <-result; !errors.Is(err, context.Canceled) {
		t.Fatalf("Run() error = %v, want context cancellation", err)
	}
}

func TestRunRenewsAndStopsAtApprovalWhenArtifactEvidenceIsUnavailable(t *testing.T) {
	manager := newFakeManager()
	eventStore := store.NewMemoryEventStore()
	executor := slowExecutor{delay: 25 * time.Millisecond}
	runtime := controller.NewManaged(eventStore, reasoner.NewFakeReasoner(), manager, controller.ManagedOptions{
		Executor: executor,
	})
	ctx := context.Background()
	if _, err := runtime.CreateRun(ctx, controller.CreateRunRequest{
		RunID: "worker-converges", ScenarioID: "worker-unit", SpecHash: "phase-3",
	}); err != nil {
		t.Fatalf("CreateRun() error = %v", err)
	}
	err := worker.Run(ctx, manager, runtime, worker.Config{
		WorkerID:          "worker-converges-incarnation",
		RunID:             "worker-converges",
		LeaseTTL:          100 * time.Millisecond,
		HeartbeatInterval: 10 * time.Millisecond,
		PollInterval:      time.Millisecond,
		Output:            io.Discard,
	})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	state, err := runtime.GetRun(ctx, "worker-converges")
	if err != nil {
		t.Fatalf("GetRun() error = %v", err)
	}
	if state.Run.ObservedState != domain.StatusWaitingApproval ||
		manager.HeartbeatCount() == 0 ||
		manager.RenewCount() == 0 {
		t.Fatalf(
			"state=%s heartbeats=%d renewals=%d",
			state.Run.ObservedState,
			manager.HeartbeatCount(),
			manager.RenewCount(),
		)
	}
}

func TestRunStopsWhenProcessHeartbeatFails(t *testing.T) {
	manager := newFakeManager()
	manager.held = true
	manager.failHeartbeatAfter = 2
	err := worker.Run(context.Background(), manager, nil, worker.Config{
		WorkerID:          "heartbeat-failure-worker",
		RunID:             "heartbeat-failure-run",
		LeaseTTL:          100 * time.Millisecond,
		HeartbeatInterval: 10 * time.Millisecond,
		PollInterval:      time.Second,
		Output:            io.Discard,
	})
	if err == nil || !strings.Contains(err.Error(), "worker heartbeat") {
		t.Fatalf("Run() error = %v, want Worker heartbeat failure", err)
	}
}

type slowExecutor struct {
	delay time.Duration
}

func (executor slowExecutor) Execute(
	ctx context.Context,
	request controller.ActionRequest,
) (lease.ActionReceipt, error) {
	timer := time.NewTimer(executor.delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return lease.ActionReceipt{}, ctx.Err()
	case <-timer.C:
		return controller.ExecuteDeterministicAction(request)
	}
}

type fakeManager struct {
	mu                 sync.Mutex
	workers            map[string]lease.Worker
	current            lease.Lease
	held               bool
	heartbeats         int
	renewals           int
	failHeartbeatAfter int
	receipts           map[string]lease.ActionReceipt
}

func newFakeManager() *fakeManager {
	return &fakeManager{
		workers:  make(map[string]lease.Worker),
		receipts: make(map[string]lease.ActionReceipt),
	}
}

func (manager *fakeManager) Register(_ context.Context, workerID string) (lease.Worker, error) {
	manager.mu.Lock()
	defer manager.mu.Unlock()
	if _, exists := manager.workers[workerID]; exists {
		return lease.Worker{}, lease.ErrWorkerRegistered
	}
	now := time.Now().UTC()
	registered := lease.Worker{ID: workerID, RegisteredAt: now, HeartbeatAt: now}
	manager.workers[workerID] = registered
	return registered, nil
}

func (manager *fakeManager) LookupWorker(_ context.Context, workerID string) (lease.Worker, error) {
	manager.mu.Lock()
	defer manager.mu.Unlock()
	registered, exists := manager.workers[workerID]
	if !exists {
		return lease.Worker{}, lease.ErrWorkerNotFound
	}
	return registered, nil
}

func (manager *fakeManager) Heartbeat(_ context.Context, workerID string) (lease.Worker, error) {
	manager.mu.Lock()
	defer manager.mu.Unlock()
	manager.heartbeats++
	if manager.failHeartbeatAfter > 0 && manager.heartbeats >= manager.failHeartbeatAfter {
		return lease.Worker{}, errors.New("injected heartbeat failure")
	}
	registered, exists := manager.workers[workerID]
	if !exists {
		return lease.Worker{}, lease.ErrWorkerNotFound
	}
	registered.HeartbeatAt = time.Now().UTC()
	manager.workers[workerID] = registered
	return registered, nil
}

func (manager *fakeManager) Acquire(
	_ context.Context,
	runID string,
	workerID string,
	ttl time.Duration,
) (lease.AcquireResult, error) {
	manager.mu.Lock()
	defer manager.mu.Unlock()
	if manager.held {
		return lease.AcquireResult{}, lease.ErrLeaseHeld
	}
	now := time.Now().UTC()
	if manager.current.RunID == "" {
		manager.current = lease.Lease{
			RunID: runID, WorkerID: workerID, FencingToken: 1,
			ExpiresAt: now.Add(ttl), HeartbeatAt: now,
		}
		return lease.AcquireResult{Lease: manager.current, Acquired: true}, nil
	}
	return lease.AcquireResult{Lease: manager.current}, nil
}

func (manager *fakeManager) Renew(_ context.Context, presented lease.Lease, ttl time.Duration) (lease.Lease, error) {
	manager.mu.Lock()
	defer manager.mu.Unlock()
	if presented.RunID != manager.current.RunID ||
		presented.WorkerID != manager.current.WorkerID ||
		presented.FencingToken != manager.current.FencingToken {
		return lease.Lease{}, lease.ErrStaleLease
	}
	manager.renewals++
	now := time.Now().UTC()
	manager.current.ExpiresAt = now.Add(ttl)
	manager.current.HeartbeatAt = now
	return manager.current, nil
}

func (manager *fakeManager) Current(_ context.Context, _ string) (lease.Lease, error) {
	manager.mu.Lock()
	defer manager.mu.Unlock()
	if manager.current.RunID == "" {
		return lease.Lease{}, lease.ErrLeaseNotFound
	}
	return manager.current, nil
}

func (manager *fakeManager) Validate(_ context.Context, presented lease.Lease) error {
	manager.mu.Lock()
	defer manager.mu.Unlock()
	if presented.RunID != manager.current.RunID ||
		presented.WorkerID != manager.current.WorkerID ||
		presented.FencingToken != manager.current.FencingToken {
		return lease.ErrStaleLease
	}
	return nil
}

func (manager *fakeManager) RecordReceipt(
	_ context.Context,
	presented lease.Lease,
	receipt lease.ActionReceipt,
) (lease.ReceiptResult, error) {
	manager.mu.Lock()
	defer manager.mu.Unlock()
	if presented.FencingToken != manager.current.FencingToken {
		return lease.ReceiptResult{}, lease.ErrStaleLease
	}
	if existing, exists := manager.receipts[receipt.ActionID]; exists {
		return lease.ReceiptResult{Receipt: existing}, nil
	}
	if receipt.ID == "" {
		receipt.ID = receipt.RunID + ":" + receipt.ActionID
	}
	receipt.WorkerID = presented.WorkerID
	receipt.FencingToken = presented.FencingToken
	receipt.CreatedAt = time.Now().UTC()
	manager.receipts[receipt.ActionID] = receipt
	return lease.ReceiptResult{Receipt: receipt, Recorded: true}, nil
}

func (manager *fakeManager) LookupReceipt(
	_ context.Context,
	_ string,
	actionID string,
) (lease.ActionReceipt, error) {
	manager.mu.Lock()
	defer manager.mu.Unlock()
	receipt, exists := manager.receipts[actionID]
	if !exists {
		return lease.ActionReceipt{}, lease.ErrReceiptNotFound
	}
	return receipt, nil
}

func (manager *fakeManager) ListReceipts(_ context.Context, _ string) ([]lease.ActionReceipt, error) {
	manager.mu.Lock()
	defer manager.mu.Unlock()
	receipts := make([]lease.ActionReceipt, 0, len(manager.receipts))
	for _, receipt := range manager.receipts {
		receipts = append(receipts, receipt)
	}
	return receipts, nil
}

func (manager *fakeManager) HeartbeatCount() int {
	manager.mu.Lock()
	defer manager.mu.Unlock()
	return manager.heartbeats
}

func (manager *fakeManager) RenewCount() int {
	manager.mu.Lock()
	defer manager.mu.Unlock()
	return manager.renewals
}

func waitForCount(
	t *testing.T,
	timeout time.Duration,
	count func() int,
	minimum int,
	label string,
) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if count() >= minimum {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("%s = %d, want at least %d", label, count(), minimum)
}
