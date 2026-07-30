package store

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"time"

	"github.com/agentdock/agentdock-verify/internal/domain"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const maxDatabaseVersion = uint64(1<<63 - 1)

// PostgresEventStore persists the authoritative ordered event log. It keeps no
// process-local Run state.
type PostgresEventStore struct {
	pool *pgxpool.Pool
}

// Checkpoint is a verified, disposable snapshot.
type Checkpoint struct {
	Seq       uint64
	State     domain.State
	CreatedAt string
}

// NewPostgresEventStore opens and verifies a PostgreSQL connection pool.
func NewPostgresEventStore(ctx context.Context, databaseURL string) (*PostgresEventStore, error) {
	if databaseURL == "" {
		return nil, errors.New("PostgreSQL database URL is required")
	}
	pool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		return nil, fmt.Errorf("open PostgreSQL pool: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping PostgreSQL: %w", err)
	}
	return &PostgresEventStore{pool: pool}, nil
}

// Close releases the connection pool.
func (store *PostgresEventStore) Close() {
	if store != nil && store.pool != nil {
		store.pool.Close()
	}
}

// Load returns the authoritative event sequence ordered by seq.
func (store *PostgresEventStore) Load(ctx context.Context, runID string) ([]domain.Event, error) {
	tx, err := store.pool.BeginTx(ctx, pgx.TxOptions{
		IsoLevel:   pgx.RepeatableRead,
		AccessMode: pgx.ReadOnly,
	})
	if err != nil {
		return nil, fmt.Errorf("begin Load transaction: %w", err)
	}
	defer tx.Rollback(ctx)
	events, err := loadPostgresEvents(ctx, tx, runID)
	if err != nil {
		return nil, err
	}
	if len(events) == 0 {
		var exists bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM runs WHERE run_id = $1)`, runID).Scan(&exists); err != nil {
			return nil, fmt.Errorf("check Run existence: %w", err)
		}
		if !exists {
			return nil, ErrRunNotFound
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit Load transaction: %w", err)
	}
	return events, nil
}

// Rebuild reconstructs State from the event log. A checkpoint is accepted only
// after its projection matches reduction of the authoritative event prefix;
// any inconsistency falls back to the full authoritative log.
func (store *PostgresEventStore) Rebuild(ctx context.Context, runID string) (domain.State, error) {
	tx, err := store.pool.BeginTx(ctx, pgx.TxOptions{
		IsoLevel:   pgx.RepeatableRead,
		AccessMode: pgx.ReadOnly,
	})
	if err != nil {
		return domain.State{}, fmt.Errorf("begin rebuild transaction: %w", err)
	}
	defer tx.Rollback(ctx)

	var checkpointSeq *int64
	var checkpointState []byte
	var checkpointStateDigest *string
	var checkpointEventDigest *string
	err = tx.QueryRow(ctx, `
		SELECT checkpoint_seq, checkpoint_state, checkpoint_state_digest, checkpoint_event_digest
		FROM runs
		WHERE run_id = $1`, runID).Scan(
		&checkpointSeq,
		&checkpointState,
		&checkpointStateDigest,
		&checkpointEventDigest,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.State{}, ErrRunNotFound
	}
	if err != nil {
		return domain.State{}, fmt.Errorf("load checkpoint: %w", err)
	}
	events, err := loadPostgresEvents(ctx, tx, runID)
	if err != nil {
		return domain.State{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return domain.State{}, fmt.Errorf("commit rebuild transaction: %w", err)
	}

	if checkpointSeq == nil ||
		checkpointStateDigest == nil ||
		checkpointEventDigest == nil ||
		*checkpointSeq <= 0 ||
		uint64(*checkpointSeq) > uint64(len(events)) {
		return domain.Reduce(events)
	}

	seq := uint64(*checkpointSeq)
	if events[seq-1].Seq != seq {
		return domain.Reduce(events)
	}
	var checkpoint domain.State
	if err := json.Unmarshal(checkpointState, &checkpoint); err != nil {
		return domain.Reduce(events)
	}
	canonicalSnapshot, err := json.Marshal(checkpoint)
	if err != nil {
		return domain.Reduce(events)
	}
	eventDigest, err := digestEventSequence(events[:seq])
	if err != nil ||
		digestBytes(canonicalSnapshot) != *checkpointStateDigest ||
		checkpoint.Run.ID != runID ||
		checkpoint.Run.Version != seq ||
		eventDigest != *checkpointEventDigest {
		return domain.Reduce(events)
	}
	authoritativePrefix, err := domain.Reduce(events[:seq])
	if err != nil || !reflect.DeepEqual(checkpoint, authoritativePrefix) {
		return domain.Reduce(events)
	}
	rebuilt, err := domain.ReduceFromCheckpoint(checkpoint, events[seq:])
	if err != nil {
		return domain.Reduce(events)
	}
	return rebuilt, nil
}

// Append validates expectedVersion, inserts one event, and updates the derived
// Run row in one transaction. Database time overrides caller-supplied time.
func (store *PostgresEventStore) Append(
	ctx context.Context,
	expectedVersion uint64,
	event domain.Event,
) (AppendResult, error) {
	if err := ctx.Err(); err != nil {
		return AppendResult{}, err
	}
	if expectedVersion > maxDatabaseVersion {
		return AppendResult{}, fmt.Errorf("%w: expected version exceeds PostgreSQL bigint", ErrInvalidAppend)
	}
	if err := validateAppendInput(event); err != nil {
		return AppendResult{}, err
	}
	if event.PayloadVersion == 0 {
		event.PayloadVersion = domain.CurrentEventPayloadVersion
	}

	tx, err := store.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return AppendResult{}, fmt.Errorf("begin append transaction: %w", err)
	}
	defer tx.Rollback(ctx)

	var databaseTime time.Time
	if err := tx.QueryRow(ctx, `SELECT CURRENT_TIMESTAMP`).Scan(&databaseTime); err != nil {
		return AppendResult{}, fmt.Errorf("read database time: %w", err)
	}
	databaseTime = databaseTime.UTC()

	if event.Type == domain.EventRunCreated {
		_, err := tx.Exec(ctx, `
			INSERT INTO runs (
				run_id, scenario_id, spec_hash, desired_state, observed_state,
				current_attempt, version, created_at, updated_at
			)
			VALUES ($1, $2, $3, $4, $5, 0, 0, $6, $6)
			ON CONFLICT (run_id) DO NOTHING`,
			event.RunID,
			event.Data.ScenarioID,
			event.Data.SpecHash,
			domain.DesiredRunning,
			domain.StatusQueued,
			databaseTime,
		)
		if err != nil {
			return AppendResult{}, fmt.Errorf("create Run row: %w", err)
		}
	}

	var actualVersion int64
	err = tx.QueryRow(ctx, `SELECT version FROM runs WHERE run_id = $1 FOR UPDATE`, event.RunID).Scan(&actualVersion)
	if errors.Is(err, pgx.ErrNoRows) {
		return AppendResult{}, ErrRunNotFound
	}
	if err != nil {
		return AppendResult{}, fmt.Errorf("lock Run row: %w", err)
	}

	existing, found, err := loadEventByIdempotencyKey(ctx, tx, event.RunID, event.IdempotencyKey)
	if err != nil {
		return AppendResult{}, err
	}
	if found {
		if !sameIdempotentEvent(existing, event) {
			return AppendResult{}, fmt.Errorf(
				"%w: run_id=%q idempotency_key=%q",
				ErrIdempotencyConflict,
				event.RunID,
				event.IdempotencyKey,
			)
		}
		return AppendResult{Event: existing, Appended: false}, nil
	}
	if uint64(actualVersion) != expectedVersion {
		return AppendResult{}, &VersionConflictError{
			RunID:    event.RunID,
			Expected: expectedVersion,
			Actual:   uint64(actualVersion),
		}
	}

	current, err := loadPostgresEvents(ctx, tx, event.RunID)
	if err != nil {
		return AppendResult{}, err
	}
	event.Seq = expectedVersion + 1
	event.CreatedAt = databaseTime.Format(time.RFC3339Nano)
	candidate := append(append([]domain.Event(nil), current...), event)
	state, err := domain.Reduce(candidate)
	if err != nil {
		return AppendResult{}, err
	}
	payload, err := json.Marshal(event.Data)
	if err != nil {
		return AppendResult{}, fmt.Errorf("marshal event payload: %w", err)
	}

	_, err = tx.Exec(ctx, `
		INSERT INTO events (
			run_id, seq, event_type, payload_version, payload,
			idempotency_key, causation_id, correlation_id, worker_id,
			fencing_token, created_at
		)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)`,
		event.RunID,
		int64(event.Seq),
		event.Type,
		event.PayloadVersion,
		payload,
		event.IdempotencyKey,
		nullableText(event.CausationID),
		nullableText(event.CorrelationID),
		nullableText(event.WorkerID),
		nullableUint64(event.FencingToken),
		databaseTime,
	)
	if err != nil {
		return AppendResult{}, fmt.Errorf("insert event: %w", err)
	}

	if event.Type == domain.EventAttemptStarted {
		_, err = tx.Exec(ctx, `
			INSERT INTO attempts (
				attempt_id, run_id, number, reason, started_at
			)
			VALUES ($1, $2, $3, $4, $5)`,
			event.Data.AttemptID,
			event.RunID,
			state.Run.CurrentAttempt,
			nullableText(event.Data.Reason),
			databaseTime,
		)
		if err != nil {
			return AppendResult{}, fmt.Errorf("insert attempt: %w", err)
		}
	}

	command, err := tx.Exec(ctx, `
		UPDATE runs
		SET scenario_id = $2,
			spec_hash = $3,
			desired_state = $4,
			observed_state = $5,
			current_attempt = $6,
			version = $7,
			updated_at = $8
		WHERE run_id = $1 AND version = $9`,
		event.RunID,
		state.Run.ScenarioID,
		state.Run.SpecHash,
		state.Run.DesiredState,
		state.Run.ObservedState,
		state.Run.CurrentAttempt,
		int64(state.Run.Version),
		databaseTime,
		actualVersion,
	)
	if err != nil {
		return AppendResult{}, fmt.Errorf("update Run projection: %w", err)
	}
	if command.RowsAffected() != 1 {
		return AppendResult{}, &VersionConflictError{
			RunID:    event.RunID,
			Expected: expectedVersion,
			Actual:   uint64(actualVersion),
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return AppendResult{}, fmt.Errorf("commit append transaction: %w", err)
	}
	return AppendResult{Event: event, Appended: true}, nil
}

// SaveCheckpoint validates and stores a disposable snapshot without changing
// the authoritative Run version.
func (store *PostgresEventStore) SaveCheckpoint(
	ctx context.Context,
	runID string,
	expectedVersion uint64,
) (Checkpoint, error) {
	if runID == "" || expectedVersion == 0 || expectedVersion > maxDatabaseVersion {
		return Checkpoint{}, fmt.Errorf("%w: run_id and positive expected version are required", ErrInvalidAppend)
	}
	tx, err := store.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return Checkpoint{}, fmt.Errorf("begin checkpoint transaction: %w", err)
	}
	defer tx.Rollback(ctx)

	var actualVersion int64
	err = tx.QueryRow(ctx, `SELECT version FROM runs WHERE run_id = $1 FOR UPDATE`, runID).Scan(&actualVersion)
	if errors.Is(err, pgx.ErrNoRows) {
		return Checkpoint{}, ErrRunNotFound
	}
	if err != nil {
		return Checkpoint{}, fmt.Errorf("lock Run for checkpoint: %w", err)
	}
	if uint64(actualVersion) != expectedVersion {
		return Checkpoint{}, &VersionConflictError{
			RunID:    runID,
			Expected: expectedVersion,
			Actual:   uint64(actualVersion),
		}
	}
	events, err := loadPostgresEvents(ctx, tx, runID)
	if err != nil {
		return Checkpoint{}, err
	}
	state, err := domain.Reduce(events)
	if err != nil {
		return Checkpoint{}, fmt.Errorf("reduce checkpoint prefix: %w", err)
	}
	if state.Run.Version != expectedVersion {
		return Checkpoint{}, &VersionConflictError{
			RunID:    runID,
			Expected: expectedVersion,
			Actual:   state.Run.Version,
		}
	}
	snapshot, err := json.Marshal(state)
	if err != nil {
		return Checkpoint{}, fmt.Errorf("marshal checkpoint: %w", err)
	}
	eventDigest, err := digestEventSequence(events)
	if err != nil {
		return Checkpoint{}, err
	}
	var createdAt time.Time
	err = tx.QueryRow(ctx, `
		UPDATE runs
		SET checkpoint_seq = $2,
			checkpoint_state = $3,
			checkpoint_state_digest = $4,
			checkpoint_event_digest = $5,
			checkpoint_created_at = CURRENT_TIMESTAMP
		WHERE run_id = $1 AND version = $2
		RETURNING checkpoint_created_at`,
		runID,
		int64(expectedVersion),
		snapshot,
		digestBytes(snapshot),
		eventDigest,
	).Scan(&createdAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Checkpoint{}, &VersionConflictError{
			RunID:    runID,
			Expected: expectedVersion,
			Actual:   uint64(actualVersion),
		}
	}
	if err != nil {
		return Checkpoint{}, fmt.Errorf("store checkpoint: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return Checkpoint{}, fmt.Errorf("commit checkpoint: %w", err)
	}
	return Checkpoint{
		Seq:       expectedVersion,
		State:     state,
		CreatedAt: createdAt.UTC().Format(time.RFC3339Nano),
	}, nil
}

type postgresEventQuerier interface {
	Query(context.Context, string, ...any) (pgx.Rows, error)
}

type postgresRowScanner interface {
	Scan(...any) error
}

func loadPostgresEvents(
	ctx context.Context,
	querier postgresEventQuerier,
	runID string,
) ([]domain.Event, error) {
	rows, err := querier.Query(ctx, `
		SELECT run_id, seq, event_type, payload_version, payload,
			idempotency_key, causation_id, correlation_id, worker_id,
			fencing_token, created_at
		FROM events
		WHERE run_id = $1
		ORDER BY seq`, runID)
	if err != nil {
		return nil, fmt.Errorf("query events: %w", err)
	}
	defer rows.Close()
	events := make([]domain.Event, 0)
	for rows.Next() {
		event, err := scanPostgresEvent(rows)
		if err != nil {
			return nil, err
		}
		events = append(events, event)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate events: %w", err)
	}
	return events, nil
}

func loadEventByIdempotencyKey(
	ctx context.Context,
	tx pgx.Tx,
	runID string,
	idempotencyKey string,
) (domain.Event, bool, error) {
	row := tx.QueryRow(ctx, `
		SELECT run_id, seq, event_type, payload_version, payload,
			idempotency_key, causation_id, correlation_id, worker_id,
			fencing_token, created_at
		FROM events
		WHERE run_id = $1 AND idempotency_key = $2`,
		runID,
		idempotencyKey,
	)
	event, err := scanPostgresEvent(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.Event{}, false, nil
	}
	if err != nil {
		return domain.Event{}, false, err
	}
	return event, true, nil
}

func scanPostgresEvent(scanner postgresRowScanner) (domain.Event, error) {
	var event domain.Event
	var seq int64
	var payload []byte
	var causationID *string
	var correlationID *string
	var workerID *string
	var fencingToken *int64
	var createdAt time.Time
	err := scanner.Scan(
		&event.RunID,
		&seq,
		&event.Type,
		&event.PayloadVersion,
		&payload,
		&event.IdempotencyKey,
		&causationID,
		&correlationID,
		&workerID,
		&fencingToken,
		&createdAt,
	)
	if err != nil {
		return domain.Event{}, err
	}
	if err := json.Unmarshal(payload, &event.Data); err != nil {
		return domain.Event{}, fmt.Errorf("decode event payload run=%q seq=%d: %w", event.RunID, seq, err)
	}
	event.Seq = uint64(seq)
	event.CausationID = dereferenceText(causationID)
	event.CorrelationID = dereferenceText(correlationID)
	event.WorkerID = dereferenceText(workerID)
	if fencingToken != nil {
		event.FencingToken = uint64(*fencingToken)
	}
	event.CreatedAt = createdAt.UTC().Format(time.RFC3339Nano)
	return event, nil
}

func nullableText(value string) any {
	if value == "" {
		return nil
	}
	return value
}

func nullableUint64(value uint64) any {
	if value == 0 {
		return nil
	}
	return int64(value)
}

func dereferenceText(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

func digestBytes(content []byte) string {
	digest := sha256.Sum256(content)
	return fmt.Sprintf("sha256:%x", digest)
}

func digestEventSequence(events []domain.Event) (string, error) {
	content, err := json.Marshal(events)
	if err != nil {
		return "", fmt.Errorf("marshal event digest: %w", err)
	}
	return digestBytes(content), nil
}
