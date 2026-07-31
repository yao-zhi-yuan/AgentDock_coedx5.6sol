//go:build integration

package lease_test

import (
	"context"
	"errors"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/agentdock/agentdock-verify/internal/domain"
	"github.com/agentdock/agentdock-verify/internal/lease"
	"github.com/agentdock/agentdock-verify/internal/store"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestOneValidLeaseMonotonicTakeoverAndStaleAppendRejection(t *testing.T) {
	ctx := context.Background()
	dsn := leaseIntegrationDatabaseURL()
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

	runID := "run-lease-takeover-" + time.Now().UTC().Format("20060102150405.000000000")
	workerA := runID + "-worker-a"
	workerB := runID + "-worker-b"
	if _, err := eventStore.Append(ctx, 0, domain.Event{
		RunID:          runID,
		Type:           domain.EventRunCreated,
		IdempotencyKey: "run-created",
		Data:           domain.EventData{ScenarioID: "lease", SpecHash: "lease-spec"},
	}); err != nil {
		t.Fatalf("Append(RunCreated) error = %v", err)
	}
	for _, workerID := range []string{workerA, workerB} {
		if _, err := manager.Register(ctx, workerID); err != nil {
			t.Fatalf("Register(%s) error = %v", workerID, err)
		}
	}

	start := make(chan struct{})
	type acquireResult struct {
		result lease.AcquireResult
		err    error
	}
	results := make(chan acquireResult, 2)
	var wait sync.WaitGroup
	for _, workerID := range []string{workerA, workerB} {
		workerID := workerID
		wait.Add(1)
		go func() {
			defer wait.Done()
			<-start
			result, acquireErr := manager.Acquire(ctx, runID, workerID, 80*time.Millisecond)
			results <- acquireResult{result: result, err: acquireErr}
		}()
	}
	close(start)
	wait.Wait()
	close(results)

	var first lease.Lease
	successes, held := 0, 0
	for result := range results {
		switch {
		case result.err == nil:
			successes++
			first = result.result.Lease
		case errors.Is(result.err, lease.ErrLeaseHeld):
			held++
		default:
			t.Fatalf("Acquire() error = %v", result.err)
		}
	}
	if successes != 1 || held != 1 || first.FencingToken != 1 {
		t.Fatalf("concurrent acquire successes=%d held=%d lease=%#v", successes, held, first)
	}

	time.Sleep(100 * time.Millisecond)
	nextWorker := workerA
	if first.WorkerID == nextWorker {
		nextWorker = workerB
	}
	takeover, err := manager.Acquire(ctx, runID, nextWorker, time.Second)
	if err != nil {
		t.Fatalf("takeover Acquire() error = %v", err)
	}
	if !takeover.TookOver || takeover.FencingToken <= first.FencingToken {
		t.Fatalf("takeover = %#v, first = %#v", takeover, first)
	}
	state, err := eventStore.Rebuild(ctx, runID)
	if err != nil {
		t.Fatalf("Rebuild() error = %v", err)
	}
	command := domain.Decide(state)

	staleEvent := domain.Event{
		RunID:          runID,
		Type:           domain.EventActionPlanned,
		IdempotencyKey: command.ActionID + ":planned",
		WorkerID:       first.WorkerID,
		FencingToken:   first.FencingToken,
		Data: domain.EventData{
			ActionID:         command.ActionID,
			ActionType:       domain.CommandStartAttempt,
			AttemptID:        command.AttemptID,
			IdempotencyScope: domain.IdempotencyScoped,
		},
	}
	if _, err := eventStore.Append(ctx, 1, staleEvent); !errors.Is(err, store.ErrStaleFencingToken) {
		t.Fatalf("stale Append() error = %v, want stale fencing rejection", err)
	}
	if _, err := manager.RecordReceipt(ctx, first, lease.ActionReceipt{
		RunID: runID, ActionID: "stale-receipt",
		ActionType: domain.CommandStartAttempt, IdempotencyScope: domain.IdempotencyScoped,
	}); !errors.Is(err, lease.ErrStaleLease) {
		t.Fatalf("stale RecordReceipt() error = %v, want stale lease rejection", err)
	}

	currentEvent := staleEvent
	currentEvent.WorkerID = takeover.WorkerID
	currentEvent.FencingToken = takeover.FencingToken
	if _, err := eventStore.Append(ctx, 1, currentEvent); err != nil {
		t.Fatalf("current fenced Append() error = %v", err)
	}
}

func TestReceiptIsFencedAndIdempotentByStableActionID(t *testing.T) {
	ctx := context.Background()
	dsn := leaseIntegrationDatabaseURL()
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

	runID := "run-receipt-" + time.Now().UTC().Format("20060102150405.000000000")
	workerID := runID + "-worker"
	if _, err := eventStore.Append(ctx, 0, domain.Event{
		RunID:          runID,
		Type:           domain.EventRunCreated,
		IdempotencyKey: "run-created",
		Data:           domain.EventData{ScenarioID: "receipt", SpecHash: "receipt-spec"},
	}); err != nil {
		t.Fatalf("Append(RunCreated) error = %v", err)
	}
	if _, err := manager.Register(ctx, workerID); err != nil {
		t.Fatalf("Register() error = %v", err)
	}
	acquired, err := manager.Acquire(ctx, runID, workerID, time.Second)
	if err != nil {
		t.Fatalf("Acquire() error = %v", err)
	}
	state, err := eventStore.Rebuild(ctx, runID)
	if err != nil {
		t.Fatalf("Rebuild() error = %v", err)
	}
	command := domain.Decide(state)
	if _, err := eventStore.Append(ctx, state.Run.Version, domain.Event{
		RunID: runID, Type: domain.EventActionPlanned,
		IdempotencyKey: command.ActionID + ":planned",
		WorkerID:       acquired.WorkerID,
		FencingToken:   acquired.FencingToken,
		Data: domain.EventData{
			ActionID: command.ActionID, ActionType: command.Type,
			AttemptID: command.AttemptID, IdempotencyScope: domain.IdempotencyScoped,
		},
	}); err != nil {
		t.Fatalf("Append(ActionPlanned) error = %v", err)
	}
	output := domain.EventData{AttemptID: command.AttemptID, Reason: "initial"}
	outputDigest, err := domain.DigestEventData(output)
	if err != nil {
		t.Fatalf("DigestEventData() error = %v", err)
	}
	receipt := lease.ActionReceipt{
		RunID:            runID,
		ActionID:         command.ActionID,
		ActionType:       command.Type,
		IdempotencyScope: domain.IdempotencyScoped,
		Output:           output,
		OutputDigest:     outputDigest,
		CostUnits:        1,
	}
	malformed := receipt
	malformed.Output.AttemptID = ""
	if _, err := manager.RecordReceipt(ctx, acquired.Lease, malformed); !errors.Is(err, domain.ErrInvalidEvent) {
		t.Fatalf("RecordReceipt(malformed StartAttempt) error = %v, want invalid event", err)
	}
	first, err := manager.RecordReceipt(ctx, acquired.Lease, receipt)
	if err != nil {
		t.Fatalf("RecordReceipt(first) error = %v", err)
	}
	second, err := manager.RecordReceipt(ctx, acquired.Lease, receipt)
	if err != nil {
		t.Fatalf("RecordReceipt(replay) error = %v", err)
	}
	if !first.Recorded || second.Recorded || first.Receipt.ID != second.Receipt.ID {
		t.Fatalf("receipt results first=%#v second=%#v", first, second)
	}
	if got, err := manager.LookupReceipt(ctx, runID, receipt.ActionID); err != nil || got.CostUnits != 1 || got.OutputDigest != receipt.OutputDigest {
		t.Fatalf("LookupReceipt() = %#v, %v", got, err)
	}
	unplanned := receipt
	unplanned.ID = ""
	unplanned.ActionID = "unplanned-action"
	unplanned.ArtifactID = ""
	if _, err := manager.RecordReceipt(ctx, acquired.Lease, unplanned); !errors.Is(err, lease.ErrReceiptWithoutPlan) {
		t.Fatalf("RecordReceipt(unplanned) error = %v, want missing ActionPlanned rejection", err)
	}
	sensitive := receipt
	sensitive.ID = ""
	sensitive.ActionID = "sensitive-action"
	sensitive.ArtifactID = ""
	sensitive.Output = domain.EventData{Reason: `{"api_key":"opaque-credential"}`}
	if _, err := manager.RecordReceipt(ctx, acquired.Lease, sensitive); !errors.Is(err, store.ErrSensitivePayload) {
		t.Fatalf("RecordReceipt(sensitive) error = %v, want sensitive payload rejection", err)
	}
}

func TestTwoWorkersSimultaneousExpiredTakeoverHasOneWinner(t *testing.T) {
	ctx := context.Background()
	dsn := leaseIntegrationDatabaseURL()
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
	runID := "run-simultaneous-takeover-" + time.Now().UTC().Format("20060102150405.000000000")
	seedWorker := runID + "-seed"
	firstWorker := runID + "-one"
	secondWorker := runID + "-two"
	if _, err := eventStore.Append(ctx, 0, domain.Event{
		RunID: runID, Type: domain.EventRunCreated, IdempotencyKey: "run-created",
		Data: domain.EventData{ScenarioID: "takeover", SpecHash: "phase-3"},
	}); err != nil {
		t.Fatalf("Append(RunCreated) error = %v", err)
	}
	for _, workerID := range []string{seedWorker, firstWorker, secondWorker} {
		if _, err := manager.Register(ctx, workerID); err != nil {
			t.Fatalf("Register(%s) error = %v", workerID, err)
		}
	}
	seed, err := manager.Acquire(ctx, runID, seedWorker, 40*time.Millisecond)
	if err != nil {
		t.Fatalf("seed Acquire() error = %v", err)
	}
	time.Sleep(60 * time.Millisecond)

	start := make(chan struct{})
	results := make(chan error, 2)
	for _, workerID := range []string{firstWorker, secondWorker} {
		workerID := workerID
		go func() {
			<-start
			_, acquireErr := manager.Acquire(ctx, runID, workerID, time.Second)
			results <- acquireErr
		}()
	}
	close(start)
	successes, held := 0, 0
	for count := 0; count < 2; count++ {
		switch err := <-results; {
		case err == nil:
			successes++
		case errors.Is(err, lease.ErrLeaseHeld):
			held++
		default:
			t.Fatalf("simultaneous takeover error = %v", err)
		}
	}
	current, err := manager.Current(ctx, runID)
	if err != nil {
		t.Fatalf("Current() error = %v", err)
	}
	if successes != 1 || held != 1 || current.FencingToken != seed.FencingToken+1 {
		t.Fatalf(
			"takeover successes=%d held=%d current=%#v seed=%#v",
			successes,
			held,
			current,
			seed,
		)
	}
}

func TestHeartbeatRenewsWithoutChangingFencingToken(t *testing.T) {
	ctx := context.Background()
	dsn := leaseIntegrationDatabaseURL()
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
	runID := "run-heartbeat-" + time.Now().UTC().Format("20060102150405.000000000")
	workerID := runID + "-worker"
	if _, err := eventStore.Append(ctx, 0, domain.Event{
		RunID: runID, Type: domain.EventRunCreated, IdempotencyKey: "run-created",
		Data: domain.EventData{ScenarioID: "heartbeat", SpecHash: "phase-3"},
	}); err != nil {
		t.Fatalf("Append(RunCreated) error = %v", err)
	}
	registered, err := manager.Register(ctx, workerID)
	if err != nil {
		t.Fatalf("Register() error = %v", err)
	}
	if _, err := manager.Register(ctx, workerID); !errors.Is(err, lease.ErrWorkerRegistered) {
		t.Fatalf("duplicate Register() error = %v, want incarnation rejection", err)
	}
	acquired, err := manager.Acquire(ctx, runID, registered.ID, 80*time.Millisecond)
	if err != nil {
		t.Fatalf("Acquire() error = %v", err)
	}
	time.Sleep(15 * time.Millisecond)
	heartbeat, err := manager.Heartbeat(ctx, registered.ID)
	if err != nil {
		t.Fatalf("Heartbeat() error = %v", err)
	}
	renewed, err := manager.Renew(ctx, acquired.Lease, 200*time.Millisecond)
	if err != nil {
		t.Fatalf("Renew() error = %v", err)
	}
	if !heartbeat.HeartbeatAt.After(registered.HeartbeatAt) {
		t.Fatalf("worker heartbeat did not advance: registered=%s heartbeat=%s", registered.HeartbeatAt, heartbeat.HeartbeatAt)
	}
	if renewed.FencingToken != acquired.FencingToken ||
		!renewed.ExpiresAt.After(acquired.ExpiresAt) {
		t.Fatalf("renewed=%#v acquired=%#v", renewed, acquired)
	}
}

func TestStaleWorkerCannotReplayPreviouslyAppendedEvent(t *testing.T) {
	ctx := context.Background()
	dsn := leaseIntegrationDatabaseURL()
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
	runID := "run-stale-replay-" + time.Now().UTC().Format("20060102150405.000000000")
	workerA := runID + "-worker-a"
	workerB := runID + "-worker-b"
	if _, err := eventStore.Append(ctx, 0, domain.Event{
		RunID: runID, Type: domain.EventRunCreated, IdempotencyKey: "run-created",
		Data: domain.EventData{ScenarioID: "stale-replay", SpecHash: "phase-3"},
	}); err != nil {
		t.Fatalf("Append(RunCreated) error = %v", err)
	}
	for _, workerID := range []string{workerA, workerB} {
		if _, err := manager.Register(ctx, workerID); err != nil {
			t.Fatalf("Register(%s) error = %v", workerID, err)
		}
	}
	first, err := manager.Acquire(ctx, runID, workerA, 40*time.Millisecond)
	if err != nil {
		t.Fatalf("first Acquire() error = %v", err)
	}
	state, err := eventStore.Rebuild(ctx, runID)
	if err != nil {
		t.Fatalf("Rebuild() error = %v", err)
	}
	command := domain.Decide(state)
	planned := domain.Event{
		RunID: runID, Type: domain.EventActionPlanned,
		IdempotencyKey: command.ActionID + ":action-planned",
		WorkerID:       first.WorkerID,
		FencingToken:   first.FencingToken,
		Data: domain.EventData{
			ActionID:         command.ActionID,
			ActionType:       command.Type,
			AttemptID:        command.AttemptID,
			IdempotencyScope: domain.IdempotencyScoped,
		},
	}
	if _, err := eventStore.Append(ctx, 1, planned); err != nil {
		t.Fatalf("first Append(ActionPlanned) error = %v", err)
	}
	time.Sleep(60 * time.Millisecond)
	if _, err := manager.Acquire(ctx, runID, workerB, time.Second); err != nil {
		t.Fatalf("takeover Acquire() error = %v", err)
	}
	if _, err := eventStore.Append(ctx, 1, planned); !errors.Is(err, store.ErrStaleFencingToken) {
		t.Fatalf("stale idempotent replay error = %v, want stale fencing rejection", err)
	}
	events, err := eventStore.Load(ctx, runID)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if len(events) != 2 {
		t.Fatalf("stale replay changed event count to %d, want 2", len(events))
	}
}

func TestActionCompletedMustMatchDurableReceiptPayload(t *testing.T) {
	ctx := context.Background()
	dsn := leaseIntegrationDatabaseURL()
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
	runID := "run-receipt-match-" + time.Now().UTC().Format("20060102150405.000000000")
	workerID := runID + "-worker"
	if _, err := eventStore.Append(ctx, 0, domain.Event{
		RunID: runID, Type: domain.EventRunCreated, IdempotencyKey: "run-created",
		Data: domain.EventData{ScenarioID: "receipt-match", SpecHash: "phase-3"},
	}); err != nil {
		t.Fatalf("Append(RunCreated) error = %v", err)
	}
	if _, err := manager.Register(ctx, workerID); err != nil {
		t.Fatalf("Register() error = %v", err)
	}
	acquired, err := manager.Acquire(ctx, runID, workerID, time.Second)
	if err != nil {
		t.Fatalf("Acquire() error = %v", err)
	}
	state, err := eventStore.Rebuild(ctx, runID)
	if err != nil {
		t.Fatalf("Rebuild() error = %v", err)
	}
	command := domain.Decide(state)
	planned := domain.Event{
		RunID: runID, Type: domain.EventActionPlanned,
		IdempotencyKey: command.ActionID + ":planned",
		WorkerID:       acquired.WorkerID,
		FencingToken:   acquired.FencingToken,
		Data: domain.EventData{
			ActionID: command.ActionID, ActionType: command.Type,
			AttemptID: command.AttemptID, IdempotencyScope: domain.IdempotencyScoped,
		},
	}
	if _, err := eventStore.Append(ctx, 1, planned); err != nil {
		t.Fatalf("Append(ActionPlanned) error = %v", err)
	}
	output := domain.EventData{AttemptID: command.AttemptID, Reason: "initial"}
	outputDigest, err := domain.DigestEventData(output)
	if err != nil {
		t.Fatalf("DigestEventData() error = %v", err)
	}
	recorded, err := manager.RecordReceipt(ctx, acquired.Lease, lease.ActionReceipt{
		RunID: runID, ActionID: command.ActionID, ActionType: command.Type,
		IdempotencyScope: domain.IdempotencyScoped,
		Output:           output,
		OutputDigest:     outputDigest,
	})
	if err != nil {
		t.Fatalf("RecordReceipt() error = %v", err)
	}
	tampered := domain.Event{
		RunID: runID, Type: domain.EventActionCompleted,
		IdempotencyKey: command.ActionID + ":completed",
		WorkerID:       acquired.WorkerID,
		FencingToken:   acquired.FencingToken,
		Data: domain.EventData{
			ActionID: command.ActionID, ActionType: command.Type,
			AttemptID: command.AttemptID, Reason: "tampered",
			IdempotencyScope: domain.IdempotencyScoped,
			ReceiptID:        recorded.Receipt.ID,
			OutputDigest:     recorded.Receipt.OutputDigest,
		},
	}
	if _, err := eventStore.Append(ctx, 2, tampered); !errors.Is(err, store.ErrActionReceiptMismatch) {
		t.Fatalf("Append(tampered ActionCompleted) error = %v, want receipt mismatch", err)
	}
	events, err := eventStore.Load(ctx, runID)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if len(events) != 2 {
		t.Fatalf("tampered completion changed event count to %d, want 2", len(events))
	}
}

func TestLeaseSensitiveOperationsUseClockAfterLockWait(t *testing.T) {
	ctx := context.Background()
	dsn := leaseIntegrationDatabaseURL()
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

	const ttl = 100 * time.Millisecond
	t.Run("EventAppend", func(t *testing.T) {
		runID, workerID, current := createLeasedRun(t, ctx, eventStore, manager, "clock-append", ttl)
		state, err := eventStore.Rebuild(ctx, runID)
		if err != nil {
			t.Fatalf("Rebuild() error = %v", err)
		}
		command := domain.Decide(state)
		lock, err := pool.Begin(ctx)
		if err != nil {
			t.Fatalf("begin Run lock: %v", err)
		}
		defer lock.Rollback(ctx)
		if _, err := lock.Exec(ctx, `SELECT 1 FROM runs WHERE run_id = $1 FOR UPDATE`, runID); err != nil {
			t.Fatalf("lock Run row: %v", err)
		}
		result := make(chan error, 1)
		go func() {
			_, appendErr := eventStore.Append(ctx, state.Run.Version, domain.Event{
				RunID: runID, Type: domain.EventActionPlanned,
				IdempotencyKey: command.ActionID + ":planned-after-lock",
				WorkerID:       workerID,
				FencingToken:   current.FencingToken,
				Data: domain.EventData{
					ActionID: command.ActionID, ActionType: command.Type,
					AttemptID: command.AttemptID, IdempotencyScope: domain.IdempotencyScoped,
				},
			})
			result <- appendErr
		}()
		time.Sleep(ttl + 50*time.Millisecond)
		if err := lock.Commit(ctx); err != nil {
			t.Fatalf("release Run lock: %v", err)
		}
		if err := <-result; !errors.Is(err, store.ErrStaleFencingToken) {
			t.Fatalf("Append after lock crossed TTL error = %v, want stale fencing", err)
		}
	})

	t.Run("ReceiptWrite", func(t *testing.T) {
		runID, _, current := createLeasedRun(t, ctx, eventStore, manager, "clock-receipt", ttl)
		state, err := eventStore.Rebuild(ctx, runID)
		if err != nil {
			t.Fatalf("Rebuild() error = %v", err)
		}
		command := domain.Decide(state)
		if _, err := eventStore.Append(ctx, state.Run.Version, domain.Event{
			RunID: runID, Type: domain.EventActionPlanned,
			IdempotencyKey: command.ActionID + ":planned",
			WorkerID:       current.WorkerID,
			FencingToken:   current.FencingToken,
			Data: domain.EventData{
				ActionID: command.ActionID, ActionType: command.Type,
				AttemptID: command.AttemptID, IdempotencyScope: domain.IdempotencyScoped,
			},
		}); err != nil {
			t.Fatalf("Append(ActionPlanned) error = %v", err)
		}
		lock, err := pool.Begin(ctx)
		if err != nil {
			t.Fatalf("begin Lease lock: %v", err)
		}
		defer lock.Rollback(ctx)
		if _, err := lock.Exec(ctx, `SELECT 1 FROM leases WHERE run_id = $1 FOR UPDATE`, runID); err != nil {
			t.Fatalf("lock Lease row: %v", err)
		}
		result := make(chan error, 1)
		go func() {
			_, receiptErr := manager.RecordReceipt(ctx, current, lease.ActionReceipt{
				RunID: runID, ActionID: command.ActionID, ActionType: command.Type,
				IdempotencyScope: domain.IdempotencyScoped,
				Output:           domain.EventData{AttemptID: command.AttemptID, Reason: "initial"},
			})
			result <- receiptErr
		}()
		time.Sleep(ttl + 50*time.Millisecond)
		if err := lock.Commit(ctx); err != nil {
			t.Fatalf("release Lease lock: %v", err)
		}
		if err := <-result; !errors.Is(err, lease.ErrLeaseExpired) {
			t.Fatalf("RecordReceipt after lock crossed TTL error = %v, want expired Lease", err)
		}
	})

	t.Run("TakeoverExpiry", func(t *testing.T) {
		runID, _, current := createLeasedRun(t, ctx, eventStore, manager, "clock-takeover", ttl)
		nextWorker := runID + "-worker-next"
		if _, err := manager.Register(ctx, nextWorker); err != nil {
			t.Fatalf("Register(next) error = %v", err)
		}
		lock, err := pool.Begin(ctx)
		if err != nil {
			t.Fatalf("begin Lease lock: %v", err)
		}
		defer lock.Rollback(ctx)
		if _, err := lock.Exec(ctx, `SELECT 1 FROM leases WHERE run_id = $1 FOR UPDATE`, runID); err != nil {
			t.Fatalf("lock Lease row: %v", err)
		}
		result := make(chan struct {
			acquired lease.AcquireResult
			err      error
		}, 1)
		go func() {
			acquired, acquireErr := manager.Acquire(ctx, runID, nextWorker, ttl)
			result <- struct {
				acquired lease.AcquireResult
				err      error
			}{acquired: acquired, err: acquireErr}
		}()
		time.Sleep(ttl + 50*time.Millisecond)
		if err := lock.Commit(ctx); err != nil {
			t.Fatalf("release Lease lock: %v", err)
		}
		takeover := <-result
		if takeover.err != nil {
			t.Fatalf("Acquire after lock crossed TTL error = %v", takeover.err)
		}
		var databaseNow time.Time
		if err := pool.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&databaseNow); err != nil {
			t.Fatalf("read database clock: %v", err)
		}
		if !takeover.acquired.TookOver ||
			takeover.acquired.FencingToken != current.FencingToken+1 ||
			takeover.acquired.ExpiresAt.Sub(databaseNow) < ttl/2 {
			t.Fatalf("takeover used stale transaction time: current=%#v takeover=%#v now=%s", current, takeover.acquired, databaseNow)
		}
	})
}

func createLeasedRun(
	t *testing.T,
	ctx context.Context,
	eventStore *store.PostgresEventStore,
	manager lease.Manager,
	scenario string,
	ttl time.Duration,
) (string, string, lease.Lease) {
	t.Helper()
	runID := "run-" + scenario + "-" + time.Now().UTC().Format("20060102150405.000000000")
	workerID := runID + "-worker"
	if _, err := eventStore.Append(ctx, 0, domain.Event{
		RunID: runID, Type: domain.EventRunCreated, IdempotencyKey: "run-created",
		Data: domain.EventData{ScenarioID: scenario, SpecHash: "phase-3"},
	}); err != nil {
		t.Fatalf("Append(RunCreated) error = %v", err)
	}
	if _, err := manager.Register(ctx, workerID); err != nil {
		t.Fatalf("Register() error = %v", err)
	}
	acquired, err := manager.Acquire(ctx, runID, workerID, ttl)
	if err != nil {
		t.Fatalf("Acquire() error = %v", err)
	}
	return runID, workerID, acquired.Lease
}

func leaseIntegrationDatabaseURL() string {
	if value := os.Getenv("AGENTDOCK_DATABASE_URL"); value != "" {
		return value
	}
	return "postgres://agentdock:agentdock_dev_only@127.0.0.1:55433/agentdock?sslmode=disable"
}
