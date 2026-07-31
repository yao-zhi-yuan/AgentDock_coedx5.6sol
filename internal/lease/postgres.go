package lease

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"time"

	"github.com/agentdock/agentdock-verify/internal/domain"
	"github.com/agentdock/agentdock-verify/internal/store"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

const maxFencingToken = uint64(1<<63 - 1)

// PostgresManager keeps all phase-3 coordination in the same PostgreSQL
// authority as the Event Log. It has no process-local ownership cache.
type PostgresManager struct {
	pool *pgxpool.Pool
}

var _ Manager = (*PostgresManager)(nil)

func NewPostgresManager(ctx context.Context, databaseURL string) (*PostgresManager, error) {
	if databaseURL == "" {
		return nil, fmt.Errorf("%w: PostgreSQL database URL is required", ErrInvalidLease)
	}
	pool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		return nil, fmt.Errorf("open lease PostgreSQL pool: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping lease PostgreSQL: %w", err)
	}
	return &PostgresManager{pool: pool}, nil
}

func (manager *PostgresManager) Close() {
	if manager != nil && manager.pool != nil {
		manager.pool.Close()
	}
}

func (manager *PostgresManager) Register(ctx context.Context, workerID string) (Worker, error) {
	if workerID == "" {
		return Worker{}, fmt.Errorf("%w: worker_id is required", ErrInvalidLease)
	}
	var worker Worker
	err := manager.pool.QueryRow(ctx, `
		INSERT INTO workers (worker_id, registered_at, heartbeat_at)
		VALUES ($1, CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)
		RETURNING worker_id, registered_at, heartbeat_at`,
		workerID,
	).Scan(&worker.ID, &worker.RegisteredAt, &worker.HeartbeatAt)
	var postgresError *pgconn.PgError
	if errors.As(err, &postgresError) && postgresError.Code == "23505" {
		return Worker{}, fmt.Errorf("%w: %s", ErrWorkerRegistered, workerID)
	}
	if err != nil {
		return Worker{}, fmt.Errorf("register worker: %w", err)
	}
	worker.RegisteredAt = worker.RegisteredAt.UTC()
	worker.HeartbeatAt = worker.HeartbeatAt.UTC()
	return worker, nil
}

func (manager *PostgresManager) LookupWorker(ctx context.Context, workerID string) (Worker, error) {
	if workerID == "" {
		return Worker{}, fmt.Errorf("%w: worker_id is required", ErrInvalidLease)
	}
	var worker Worker
	err := manager.pool.QueryRow(ctx, `
		SELECT worker_id, registered_at, heartbeat_at
		FROM workers
		WHERE worker_id = $1`,
		workerID,
	).Scan(&worker.ID, &worker.RegisteredAt, &worker.HeartbeatAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Worker{}, ErrWorkerNotFound
	}
	if err != nil {
		return Worker{}, fmt.Errorf("lookup worker: %w", err)
	}
	worker.RegisteredAt = worker.RegisteredAt.UTC()
	worker.HeartbeatAt = worker.HeartbeatAt.UTC()
	return worker, nil
}

func (manager *PostgresManager) Heartbeat(ctx context.Context, workerID string) (Worker, error) {
	if workerID == "" {
		return Worker{}, fmt.Errorf("%w: worker_id is required", ErrInvalidLease)
	}
	var worker Worker
	err := manager.pool.QueryRow(ctx, `
		UPDATE workers
		SET heartbeat_at = CURRENT_TIMESTAMP
		WHERE worker_id = $1
		RETURNING worker_id, registered_at, heartbeat_at`,
		workerID,
	).Scan(&worker.ID, &worker.RegisteredAt, &worker.HeartbeatAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Worker{}, ErrWorkerNotFound
	}
	if err != nil {
		return Worker{}, fmt.Errorf("heartbeat worker: %w", err)
	}
	worker.RegisteredAt = worker.RegisteredAt.UTC()
	worker.HeartbeatAt = worker.HeartbeatAt.UTC()
	return worker, nil
}

func (manager *PostgresManager) Acquire(
	ctx context.Context,
	runID string,
	workerID string,
	ttl time.Duration,
) (AcquireResult, error) {
	if runID == "" || workerID == "" || ttl <= 0 {
		return AcquireResult{}, fmt.Errorf("%w: run_id, worker_id, and positive TTL are required", ErrInvalidLease)
	}
	tx, err := manager.pool.Begin(ctx)
	if err != nil {
		return AcquireResult{}, fmt.Errorf("begin lease acquisition: %w", err)
	}
	defer tx.Rollback(ctx)

	var workerExists bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM workers WHERE worker_id = $1)`, workerID).Scan(&workerExists); err != nil {
		return AcquireResult{}, fmt.Errorf("check worker registration: %w", err)
	}
	if !workerExists {
		return AcquireResult{}, ErrWorkerNotFound
	}
	var runExists bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM runs WHERE run_id = $1)`, runID).Scan(&runExists); err != nil {
		return AcquireResult{}, fmt.Errorf("check Run existence: %w", err)
	}
	if !runExists {
		return AcquireResult{}, store.ErrRunNotFound
	}
	if _, err := tx.Exec(
		ctx,
		`SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`,
		runID,
	); err != nil {
		return AcquireResult{}, fmt.Errorf("lock Run lease acquisition: %w", err)
	}

	current, found, err := loadLeaseForUpdate(ctx, tx, runID)
	if err != nil {
		return AcquireResult{}, err
	}
	if !found {
		created, err := insertLease(ctx, tx, runID, workerID, 1, ttl)
		if err != nil {
			return AcquireResult{}, err
		}
		if err := tx.Commit(ctx); err != nil {
			return AcquireResult{}, fmt.Errorf("commit lease acquisition: %w", err)
		}
		return AcquireResult{Lease: created, Acquired: true}, nil
	}

	var databaseNow time.Time
	if err := tx.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&databaseNow); err != nil {
		return AcquireResult{}, fmt.Errorf("read database time: %w", err)
	}
	if current.ExpiresAt.After(databaseNow) {
		if current.WorkerID == workerID {
			if err := tx.Commit(ctx); err != nil {
				return AcquireResult{}, fmt.Errorf("commit current lease observation: %w", err)
			}
			return AcquireResult{Lease: current}, nil
		}
		return AcquireResult{}, &LeaseError{
			RunID: runID, WorkerID: workerID,
			CurrentWorker: current.WorkerID, CurrentToken: current.FencingToken,
			Cause: ErrLeaseHeld,
		}
	}
	if current.FencingToken >= maxFencingToken {
		return AcquireResult{}, fmt.Errorf("%w: fencing token exhausted", ErrInvalidLease)
	}
	taken, err := updateExpiredLease(ctx, tx, runID, workerID, current.FencingToken+1, ttl)
	if err != nil {
		return AcquireResult{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return AcquireResult{}, fmt.Errorf("commit lease takeover: %w", err)
	}
	return AcquireResult{Lease: taken, Acquired: true, TookOver: true}, nil
}

func (manager *PostgresManager) Renew(ctx context.Context, presented Lease, ttl time.Duration) (Lease, error) {
	if err := validatePresentedLease(presented); err != nil || ttl <= 0 {
		if err != nil {
			return Lease{}, err
		}
		return Lease{}, fmt.Errorf("%w: positive TTL is required", ErrInvalidLease)
	}
	tx, err := manager.pool.Begin(ctx)
	if err != nil {
		return Lease{}, fmt.Errorf("begin lease renewal: %w", err)
	}
	defer tx.Rollback(ctx)
	current, found, err := loadLeaseForUpdate(ctx, tx, presented.RunID)
	if err != nil {
		return Lease{}, err
	}
	if !found {
		return Lease{}, ErrLeaseNotFound
	}
	var databaseNow time.Time
	if err := tx.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&databaseNow); err != nil {
		return Lease{}, fmt.Errorf("read database time after Lease lock: %w", err)
	}
	expired := !current.ExpiresAt.After(databaseNow)
	if current.WorkerID != presented.WorkerID ||
		current.FencingToken != presented.FencingToken ||
		expired {
		return Lease{}, leaseAuthorityError(presented, current, expired)
	}
	var renewed Lease
	err = tx.QueryRow(ctx, `
		UPDATE leases
		SET expires_at = $2::timestamptz + make_interval(secs => $3::double precision),
			heartbeat_at = $2::timestamptz
		WHERE run_id = $1
		RETURNING run_id, worker_id, fencing_token, expires_at, heartbeat_at`,
		presented.RunID,
		databaseNow,
		ttl.Seconds(),
	).Scan(
		&renewed.RunID,
		&renewed.WorkerID,
		&renewed.FencingToken,
		&renewed.ExpiresAt,
		&renewed.HeartbeatAt,
	)
	if err != nil {
		return Lease{}, fmt.Errorf("renew lease: %w", err)
	}
	renewed.ExpiresAt = renewed.ExpiresAt.UTC()
	renewed.HeartbeatAt = renewed.HeartbeatAt.UTC()
	if err := tx.Commit(ctx); err != nil {
		return Lease{}, fmt.Errorf("commit lease renewal: %w", err)
	}
	return renewed, nil
}

func (manager *PostgresManager) Current(ctx context.Context, runID string) (Lease, error) {
	if runID == "" {
		return Lease{}, fmt.Errorf("%w: run_id is required", ErrInvalidLease)
	}
	var current Lease
	err := manager.pool.QueryRow(ctx, `
		SELECT run_id, worker_id, fencing_token, expires_at, heartbeat_at
		FROM leases
		WHERE run_id = $1`,
		runID,
	).Scan(
		&current.RunID,
		&current.WorkerID,
		&current.FencingToken,
		&current.ExpiresAt,
		&current.HeartbeatAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return Lease{}, ErrLeaseNotFound
	}
	if err != nil {
		return Lease{}, fmt.Errorf("load current lease: %w", err)
	}
	current.ExpiresAt = current.ExpiresAt.UTC()
	current.HeartbeatAt = current.HeartbeatAt.UTC()
	return current, nil
}

func (manager *PostgresManager) Validate(ctx context.Context, presented Lease) error {
	if err := validatePresentedLease(presented); err != nil {
		return err
	}
	tx, err := manager.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin lease validation: %w", err)
	}
	defer tx.Rollback(ctx)
	current, found, err := loadLeaseForShare(ctx, tx, presented.RunID)
	if err != nil {
		return err
	}
	if !found {
		return ErrLeaseNotFound
	}
	var databaseNow time.Time
	if err := tx.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&databaseNow); err != nil {
		return fmt.Errorf("read database time after Lease lock: %w", err)
	}
	expired := !current.ExpiresAt.After(databaseNow)
	if current.WorkerID != presented.WorkerID ||
		current.FencingToken != presented.FencingToken ||
		expired {
		return leaseAuthorityError(presented, current, expired)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit lease validation: %w", err)
	}
	return nil
}

func (manager *PostgresManager) RecordReceipt(
	ctx context.Context,
	presented Lease,
	receipt ActionReceipt,
) (ReceiptResult, error) {
	if err := validatePresentedLease(presented); err != nil {
		return ReceiptResult{}, err
	}
	if receipt.RunID == "" ||
		receipt.RunID != presented.RunID ||
		receipt.ActionID == "" ||
		receipt.ActionType == "" ||
		!receipt.IdempotencyScope.Valid() ||
		receipt.CostUnits < 0 {
		return ReceiptResult{}, fmt.Errorf("%w: invalid action receipt", ErrInvalidLease)
	}
	if err := store.ValidateEventData(receipt.Output); err != nil {
		return ReceiptResult{}, err
	}
	if receipt.ID == "" {
		receipt.ID = receipt.RunID + ":" + receipt.ActionID
	}
	tx, err := manager.pool.Begin(ctx)
	if err != nil {
		return ReceiptResult{}, fmt.Errorf("begin receipt transaction: %w", err)
	}
	defer tx.Rollback(ctx)
	if err := validateLeaseInTx(ctx, tx, presented); err != nil {
		return ReceiptResult{}, err
	}
	existing, found, err := lookupReceiptWithQuerier(ctx, tx, receipt.RunID, receipt.ActionID)
	if err != nil {
		return ReceiptResult{}, err
	}
	if found {
		if !sameReceipt(existing, receipt) {
			return ReceiptResult{}, fmt.Errorf(
				"%w: run_id=%q action_id=%q",
				ErrReceiptConflict,
				receipt.RunID,
				receipt.ActionID,
			)
		}
		if err := tx.Commit(ctx); err != nil {
			return ReceiptResult{}, fmt.Errorf("commit receipt replay: %w", err)
		}
		return ReceiptResult{Receipt: existing}, nil
	}
	var plannedAttemptID string
	err = tx.QueryRow(ctx, `
		SELECT COALESCE(planned.payload->>'attempt_id', '')
		FROM events planned
		WHERE planned.run_id = $1
		  AND planned.event_type = $2
		  AND planned.payload->>'action_id' = $3
		  AND planned.payload->>'action_type' = $4
		  AND planned.payload->>'idempotency_scope' = $5
		  AND NOT EXISTS(
			SELECT 1
			FROM events result
			WHERE result.run_id = $1
			  AND result.event_type IN ($6, $7)
			  AND result.payload->>'action_id' = $3
		  )
		ORDER BY planned.seq DESC
		LIMIT 1`,
		receipt.RunID,
		domain.EventActionPlanned,
		receipt.ActionID,
		receipt.ActionType,
		receipt.IdempotencyScope,
		domain.EventActionCompleted,
		domain.EventActionFailed,
	).Scan(&plannedAttemptID)
	if errors.Is(err, pgx.ErrNoRows) {
		return ReceiptResult{}, fmt.Errorf(
			"%w: run_id=%q action_id=%q",
			ErrReceiptWithoutPlan,
			receipt.RunID,
			receipt.ActionID,
		)
	}
	if err != nil {
		return ReceiptResult{}, fmt.Errorf("validate receipt ActionPlanned event: %w", err)
	}
	if err := domain.ValidateManagedReceiptEvidence(
		receipt.ActionType,
		receipt.ActionID,
		plannedAttemptID,
		receipt.Output,
		receipt.OutputDigest,
		receipt.ArtifactID,
		receipt.ArtifactDigest,
	); err != nil {
		return ReceiptResult{}, err
	}
	output, err := json.Marshal(receipt.Output)
	if err != nil {
		return ReceiptResult{}, fmt.Errorf("marshal receipt output: %w", err)
	}
	err = tx.QueryRow(ctx, `
		INSERT INTO action_receipts (
			receipt_id, run_id, action_id, action_type, idempotency_scope,
			output, output_digest, artifact_id, artifact_digest, cost_units, worker_id,
			fencing_token, created_at
		)
		VALUES (
			$1, $2, $3, $4, $5, $6, NULLIF($7, ''), NULLIF($8, ''),
			NULLIF($9, ''), $10, $11, $12, clock_timestamp()
		)
		RETURNING created_at`,
		receipt.ID,
		receipt.RunID,
		receipt.ActionID,
		receipt.ActionType,
		receipt.IdempotencyScope,
		output,
		receipt.OutputDigest,
		receipt.ArtifactID,
		receipt.ArtifactDigest,
		receipt.CostUnits,
		presented.WorkerID,
		int64(presented.FencingToken),
	).Scan(&receipt.CreatedAt)
	if err != nil {
		return ReceiptResult{}, fmt.Errorf("insert action receipt: %w", err)
	}
	receipt.WorkerID = presented.WorkerID
	receipt.FencingToken = presented.FencingToken
	receipt.CreatedAt = receipt.CreatedAt.UTC()
	if err := tx.Commit(ctx); err != nil {
		return ReceiptResult{}, fmt.Errorf("commit action receipt: %w", err)
	}
	return ReceiptResult{Receipt: receipt, Recorded: true}, nil
}

func (manager *PostgresManager) LookupReceipt(
	ctx context.Context,
	runID string,
	actionID string,
) (ActionReceipt, error) {
	receipt, found, err := lookupReceiptWithQuerier(ctx, manager.pool, runID, actionID)
	if err != nil {
		return ActionReceipt{}, err
	}
	if !found {
		return ActionReceipt{}, ErrReceiptNotFound
	}
	return receipt, nil
}

func (manager *PostgresManager) ListReceipts(ctx context.Context, runID string) ([]ActionReceipt, error) {
	rows, err := manager.pool.Query(ctx, `
		SELECT receipt_id, run_id, action_id, action_type, idempotency_scope,
			output, COALESCE(output_digest, ''), COALESCE(artifact_id, ''),
			COALESCE(artifact_digest, ''), cost_units, worker_id, fencing_token, created_at
		FROM action_receipts
		WHERE run_id = $1
		ORDER BY created_at, action_id`,
		runID,
	)
	if err != nil {
		return nil, fmt.Errorf("list action receipts: %w", err)
	}
	defer rows.Close()
	var receipts []ActionReceipt
	for rows.Next() {
		receipt, err := scanReceipt(rows)
		if err != nil {
			return nil, err
		}
		receipts = append(receipts, receipt)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate action receipts: %w", err)
	}
	return receipts, nil
}

func validatePresentedLease(presented Lease) error {
	if presented.RunID == "" ||
		presented.WorkerID == "" ||
		presented.FencingToken == 0 ||
		presented.FencingToken > maxFencingToken {
		return fmt.Errorf("%w: run_id, worker_id, and positive fencing_token are required", ErrInvalidLease)
	}
	return nil
}

func loadLeaseForUpdate(ctx context.Context, tx pgx.Tx, runID string) (Lease, bool, error) {
	var current Lease
	err := tx.QueryRow(ctx, `
		SELECT run_id, worker_id, fencing_token, expires_at, heartbeat_at
		FROM leases
		WHERE run_id = $1
		FOR UPDATE`,
		runID,
	).Scan(
		&current.RunID,
		&current.WorkerID,
		&current.FencingToken,
		&current.ExpiresAt,
		&current.HeartbeatAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return Lease{}, false, nil
	}
	if err != nil {
		return Lease{}, false, fmt.Errorf("lock lease: %w", err)
	}
	current.ExpiresAt = current.ExpiresAt.UTC()
	current.HeartbeatAt = current.HeartbeatAt.UTC()
	return current, true, nil
}

func loadLeaseForShare(ctx context.Context, tx pgx.Tx, runID string) (Lease, bool, error) {
	var current Lease
	err := tx.QueryRow(ctx, `
		SELECT run_id, worker_id, fencing_token, expires_at, heartbeat_at
		FROM leases
		WHERE run_id = $1
		FOR SHARE`,
		runID,
	).Scan(
		&current.RunID,
		&current.WorkerID,
		&current.FencingToken,
		&current.ExpiresAt,
		&current.HeartbeatAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return Lease{}, false, nil
	}
	if err != nil {
		return Lease{}, false, fmt.Errorf("lock Lease for share: %w", err)
	}
	current.ExpiresAt = current.ExpiresAt.UTC()
	current.HeartbeatAt = current.HeartbeatAt.UTC()
	return current, true, nil
}

func insertLease(
	ctx context.Context,
	tx pgx.Tx,
	runID string,
	workerID string,
	token uint64,
	ttl time.Duration,
) (Lease, error) {
	var created Lease
	err := tx.QueryRow(ctx, `
		INSERT INTO leases (run_id, worker_id, fencing_token, expires_at, heartbeat_at)
		VALUES ($1, $2, $3, clock_timestamp() + make_interval(secs => $4::double precision), clock_timestamp())
		RETURNING run_id, worker_id, fencing_token, expires_at, heartbeat_at`,
		runID,
		workerID,
		int64(token),
		ttl.Seconds(),
	).Scan(
		&created.RunID,
		&created.WorkerID,
		&created.FencingToken,
		&created.ExpiresAt,
		&created.HeartbeatAt,
	)
	if err != nil {
		return Lease{}, fmt.Errorf("insert lease: %w", err)
	}
	created.ExpiresAt = created.ExpiresAt.UTC()
	created.HeartbeatAt = created.HeartbeatAt.UTC()
	return created, nil
}

func updateExpiredLease(
	ctx context.Context,
	tx pgx.Tx,
	runID string,
	workerID string,
	token uint64,
	ttl time.Duration,
) (Lease, error) {
	var updated Lease
	err := tx.QueryRow(ctx, `
		UPDATE leases
		SET worker_id = $2,
			fencing_token = $3,
			expires_at = clock_timestamp() + make_interval(secs => $4::double precision),
			heartbeat_at = clock_timestamp()
		WHERE run_id = $1
		RETURNING run_id, worker_id, fencing_token, expires_at, heartbeat_at`,
		runID,
		workerID,
		int64(token),
		ttl.Seconds(),
	).Scan(
		&updated.RunID,
		&updated.WorkerID,
		&updated.FencingToken,
		&updated.ExpiresAt,
		&updated.HeartbeatAt,
	)
	if err != nil {
		return Lease{}, fmt.Errorf("take over lease: %w", err)
	}
	updated.ExpiresAt = updated.ExpiresAt.UTC()
	updated.HeartbeatAt = updated.HeartbeatAt.UTC()
	return updated, nil
}

func validateLeaseInTx(ctx context.Context, tx pgx.Tx, presented Lease) error {
	current, found, err := loadLeaseForUpdate(ctx, tx, presented.RunID)
	if err != nil {
		return fmt.Errorf("lock Lease for Receipt: %w", err)
	}
	if !found {
		return ErrLeaseNotFound
	}
	var databaseNow time.Time
	if err := tx.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&databaseNow); err != nil {
		return fmt.Errorf("read database time after Lease lock: %w", err)
	}
	expired := !current.ExpiresAt.After(databaseNow)
	if current.WorkerID != presented.WorkerID ||
		current.FencingToken != presented.FencingToken ||
		expired {
		return leaseAuthorityError(presented, current, expired)
	}
	return nil
}

func leaseAuthorityError(presented Lease, current Lease, expired bool) error {
	cause := ErrStaleLease
	if current.WorkerID == presented.WorkerID &&
		current.FencingToken == presented.FencingToken &&
		expired {
		cause = ErrLeaseExpired
	}
	return &LeaseError{
		RunID: presented.RunID, WorkerID: presented.WorkerID,
		FencingToken:  presented.FencingToken,
		CurrentWorker: current.WorkerID, CurrentToken: current.FencingToken,
		Cause: cause,
	}
}

type receiptQuerier interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}

func lookupReceiptWithQuerier(
	ctx context.Context,
	querier receiptQuerier,
	runID string,
	actionID string,
) (ActionReceipt, bool, error) {
	row := querier.QueryRow(ctx, `
		SELECT receipt_id, run_id, action_id, action_type, idempotency_scope,
			output, COALESCE(output_digest, ''), COALESCE(artifact_id, ''),
			COALESCE(artifact_digest, ''), cost_units, worker_id, fencing_token, created_at
		FROM action_receipts
		WHERE run_id = $1 AND action_id = $2`,
		runID,
		actionID,
	)
	receipt, err := scanReceipt(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return ActionReceipt{}, false, nil
	}
	if err != nil {
		return ActionReceipt{}, false, err
	}
	return receipt, true, nil
}

type rowScanner interface {
	Scan(...any) error
}

func scanReceipt(scanner rowScanner) (ActionReceipt, error) {
	var receipt ActionReceipt
	var output []byte
	err := scanner.Scan(
		&receipt.ID,
		&receipt.RunID,
		&receipt.ActionID,
		&receipt.ActionType,
		&receipt.IdempotencyScope,
		&output,
		&receipt.OutputDigest,
		&receipt.ArtifactID,
		&receipt.ArtifactDigest,
		&receipt.CostUnits,
		&receipt.WorkerID,
		&receipt.FencingToken,
		&receipt.CreatedAt,
	)
	if err != nil {
		return ActionReceipt{}, err
	}
	if err := json.Unmarshal(output, &receipt.Output); err != nil {
		return ActionReceipt{}, fmt.Errorf("decode action receipt output: %w", err)
	}
	receipt.CreatedAt = receipt.CreatedAt.UTC()
	return receipt, nil
}

func sameReceipt(existing, replay ActionReceipt) bool {
	replay.WorkerID = existing.WorkerID
	replay.FencingToken = existing.FencingToken
	replay.CreatedAt = existing.CreatedAt
	return reflect.DeepEqual(existing, replay)
}
