//go:build integration

package store

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/agentdock/agentdock-verify/internal/domain"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestPostgresAppendUsesCASAndCommitsEventWithRunVersion(t *testing.T) {
	ctx := context.Background()
	first := openPostgresStore(t, ctx, integrationDatabaseURL())
	second := openPostgresStore(t, ctx, integrationDatabaseURL())
	runID := uniqueRunID("cas")

	created, err := first.Append(ctx, 0, createdEvent(runID))
	if err != nil {
		t.Fatalf("Append(RunCreated) error = %v", err)
	}
	if created.Event.CreatedAt == "" || created.Event.CreatedAt == "1900-01-01T00:00:00Z" {
		t.Fatalf("RunCreated did not use database time: %#v", created.Event)
	}

	candidates := []domain.Event{
		{
			RunID:          runID,
			Type:           domain.EventDesiredStateChanged,
			Data:           domain.EventData{DesiredState: domain.DesiredPaused},
			IdempotencyKey: "writer-pause",
		},
		{
			RunID:          runID,
			Type:           domain.EventDesiredStateChanged,
			Data:           domain.EventData{DesiredState: domain.DesiredCancelled},
			IdempotencyKey: "writer-cancel",
		},
	}
	stores := []*PostgresEventStore{first, second}
	start := make(chan struct{})
	errorsCh := make(chan error, len(stores))
	var wg sync.WaitGroup
	for index := range stores {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			<-start
			_, appendErr := stores[index].Append(ctx, 1, candidates[index])
			errorsCh <- appendErr
		}(index)
	}
	close(start)
	wg.Wait()
	close(errorsCh)

	successes, conflicts := 0, 0
	for appendErr := range errorsCh {
		switch {
		case appendErr == nil:
			successes++
		case errors.Is(appendErr, ErrVersionConflict):
			conflicts++
		default:
			t.Fatalf("concurrent Append() error = %v", appendErr)
		}
	}
	if successes != 1 || conflicts != 1 {
		t.Fatalf("concurrent writers = successes:%d conflicts:%d, want 1/1", successes, conflicts)
	}

	events, err := first.Load(ctx, runID)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	state, err := first.Rebuild(ctx, runID)
	if err != nil {
		t.Fatalf("Rebuild() error = %v", err)
	}
	if len(events) != 2 || state.Run.Version != 2 {
		t.Fatalf("event/run versions diverged: events=%d state.version=%d", len(events), state.Run.Version)
	}
}

func TestPostgresIdempotentReplayDoesNotDuplicateEvent(t *testing.T) {
	ctx := context.Background()
	postgres := openPostgresStore(t, ctx, integrationDatabaseURL())
	runID := uniqueRunID("idempotency")
	if _, err := postgres.Append(ctx, 0, createdEvent(runID)); err != nil {
		t.Fatalf("Append(RunCreated) error = %v", err)
	}
	pause := domain.Event{
		RunID:          runID,
		Type:           domain.EventDesiredStateChanged,
		Data:           domain.EventData{DesiredState: domain.DesiredPaused},
		IdempotencyKey: "same-key",
	}
	first, err := postgres.Append(ctx, 1, pause)
	if err != nil {
		t.Fatalf("first pause Append() error = %v", err)
	}
	second, err := postgres.Append(ctx, 1, pause)
	if err != nil {
		t.Fatalf("idempotent replay Append() error = %v", err)
	}
	if !first.Appended || second.Appended || first.Event.Seq != second.Event.Seq {
		t.Fatalf("idempotent flags/seq = first:%#v second:%#v", first, second)
	}

	conflicting := pause
	conflicting.Data.DesiredState = domain.DesiredCancelled
	if _, err := postgres.Append(ctx, 1, conflicting); !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("conflicting replay error = %v, want idempotency conflict", err)
	}
	events, err := postgres.Load(ctx, runID)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if len(events) != 2 {
		t.Fatalf("event count = %d, want 2", len(events))
	}
}

func TestPostgresSchemaEnforcesPerRunSequenceAndIdempotencyUniqueness(t *testing.T) {
	ctx := context.Background()
	postgres := openPostgresStore(t, ctx, integrationDatabaseURL())
	rows, err := postgres.pool.Query(ctx, `
		SELECT conname
		FROM pg_constraint
		WHERE conrelid = 'events'::regclass
		  AND contype IN ('p', 'u')
		ORDER BY conname`)
	if err != nil {
		t.Fatalf("query event uniqueness constraints: %v", err)
	}
	defer rows.Close()
	found := map[string]bool{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatalf("scan constraint name: %v", err)
		}
		found[name] = true
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("constraint rows error: %v", err)
	}
	for _, name := range []string{"events_run_seq_unique", "events_run_idempotency_unique"} {
		if !found[name] {
			t.Fatalf("missing database uniqueness constraint %q; found %v", name, found)
		}
	}
}

func TestPostgresTransactionFailureLeavesNoHalfEvent(t *testing.T) {
	ctx := context.Background()
	postgres := openPostgresStore(t, ctx, integrationDatabaseURL())
	runID := uniqueRunID("rollback")
	if _, err := postgres.Append(ctx, 0, createdEvent(runID)); err != nil {
		t.Fatalf("Append(RunCreated) error = %v", err)
	}

	triggerName := "fail_run_update_" + safeIdentifier(runID)
	functionName := triggerName + "_fn"
	_, err := postgres.pool.Exec(ctx, fmt.Sprintf(`
		CREATE FUNCTION %s() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN
			IF NEW.run_id = %s THEN
				RAISE EXCEPTION 'forced phase 2 transaction failure';
			END IF;
			RETURN NEW;
		END
		$$;
		CREATE TRIGGER %s BEFORE UPDATE ON runs
		FOR EACH ROW EXECUTE FUNCTION %s();`,
		quoteIdentifier(functionName),
		quoteLiteral(runID),
		quoteIdentifier(triggerName),
		quoteIdentifier(functionName),
	))
	if err != nil {
		t.Fatalf("create failure trigger: %v", err)
	}
	t.Cleanup(func() {
		_, _ = postgres.pool.Exec(context.Background(), `DROP TRIGGER IF EXISTS `+quoteIdentifier(triggerName)+` ON runs`)
		_, _ = postgres.pool.Exec(context.Background(), `DROP FUNCTION IF EXISTS `+quoteIdentifier(functionName)+`()`)
	})

	_, appendErr := postgres.Append(ctx, 1, domain.Event{
		RunID:          runID,
		Type:           domain.EventDesiredStateChanged,
		Data:           domain.EventData{DesiredState: domain.DesiredPaused},
		IdempotencyKey: "forced-failure",
	})
	if appendErr == nil {
		t.Fatal("Append() error = nil, want forced transaction failure")
	}
	events, loadErr := postgres.Load(ctx, runID)
	if loadErr != nil {
		t.Fatalf("Load() after failed transaction error = %v", loadErr)
	}
	if len(events) != 1 {
		t.Fatalf("failed transaction left %d events, want 1", len(events))
	}
	state, rebuildErr := postgres.Rebuild(ctx, runID)
	if rebuildErr != nil {
		t.Fatalf("Rebuild() error = %v", rebuildErr)
	}
	if state.Run.Version != 1 {
		t.Fatalf("failed transaction changed Run version to %d", state.Run.Version)
	}
}

func TestPostgresReconnectsAfterBackendDisconnect(t *testing.T) {
	ctx := context.Background()
	dsn := withPoolMaxConns(t, integrationDatabaseURL(), "1")
	postgres := openPostgresStore(t, ctx, dsn)
	runID := uniqueRunID("reconnect")
	if _, err := postgres.Append(ctx, 0, createdEvent(runID)); err != nil {
		t.Fatalf("Append(RunCreated) error = %v", err)
	}

	connection, err := postgres.pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("Acquire() error = %v", err)
	}
	var backendPID int32
	if err := connection.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&backendPID); err != nil {
		connection.Release()
		t.Fatalf("query backend pid: %v", err)
	}
	connection.Release()

	admin, err := pgxpool.New(ctx, integrationDatabaseURL())
	if err != nil {
		t.Fatalf("open admin pool: %v", err)
	}
	defer admin.Close()
	var terminated bool
	if err := admin.QueryRow(ctx, `SELECT pg_terminate_backend($1)`, backendPID).Scan(&terminated); err != nil {
		t.Fatalf("terminate backend: %v", err)
	}
	if !terminated {
		t.Fatalf("pg_terminate_backend(%d) = false", backendPID)
	}

	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := postgres.Load(ctx, runID); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("PostgresEventStore did not reconnect within 5s")
		}
		time.Sleep(25 * time.Millisecond)
	}
}

func TestPostgresLoadUsesOneConsistentSnapshot(t *testing.T) {
	ctx := context.Background()
	runID := uniqueRunID("load-snapshot")
	entered := make(chan struct{})
	release := make(chan struct{})
	config, err := pgxpool.ParseConfig(integrationDatabaseURL())
	if err != nil {
		t.Fatalf("ParseConfig() error = %v", err)
	}
	config.ConnConfig.Tracer = &blockingLoadTracer{
		runID:   runID,
		entered: entered,
		release: release,
	}
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		t.Fatalf("NewWithConfig() error = %v", err)
	}
	reader := &PostgresEventStore{pool: pool}
	t.Cleanup(reader.Close)
	writer := openPostgresStore(t, ctx, integrationDatabaseURL())

	loadResult := make(chan error, 1)
	go func() {
		events, loadErr := reader.Load(ctx, runID)
		if loadErr == nil {
			loadErr = fmt.Errorf("Load() returned an impossible mixed snapshot with %d events", len(events))
		}
		loadResult <- loadErr
	}()

	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("Load() did not reach the controlled post-events boundary")
	}
	if _, err := writer.Append(ctx, 0, createdEvent(runID)); err != nil {
		t.Fatalf("concurrent Append(RunCreated) error = %v", err)
	}
	close(release)

	select {
	case err := <-loadResult:
		if !errors.Is(err, ErrRunNotFound) {
			t.Fatalf("Load() snapshot error = %v, want Run not found from its pre-create snapshot", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Load() did not complete after releasing the controlled query")
	}
	events, err := reader.Load(ctx, runID)
	if err != nil {
		t.Fatalf("subsequent Load() error = %v", err)
	}
	if len(events) != 1 || events[0].Seq != 1 {
		t.Fatalf("subsequent Load() events = %#v, want the committed RunCreated event", events)
	}
}

func TestPostgresRebuilds1000EventsAndCheckpointMatchesFullLog(t *testing.T) {
	ctx := context.Background()
	postgres := openPostgresStore(t, ctx, integrationDatabaseURL())
	runID := uniqueRunID("thousand")
	events := thousandAppendEvents(runID)
	for index, event := range events {
		if _, err := postgres.Append(ctx, uint64(index), event); err != nil {
			t.Fatalf("Append(event %d %s) error = %v", index+1, event.Type, err)
		}
		if index == 500 {
			checkpoint, err := postgres.SaveCheckpoint(ctx, runID, 501)
			if err != nil {
				t.Fatalf("SaveCheckpoint() error = %v", err)
			}
			if checkpoint.Seq != 501 || checkpoint.State.Run.Version != 501 {
				t.Fatalf("checkpoint = %#v, want seq/version 501", checkpoint)
			}
		}
	}

	loaded, err := postgres.Load(ctx, runID)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if len(loaded) != 1000 {
		t.Fatalf("loaded event count = %d, want 1000", len(loaded))
	}
	want, err := domain.Reduce(loaded)
	if err != nil {
		t.Fatalf("Reduce(full log) error = %v", err)
	}
	got, err := postgres.Rebuild(ctx, runID)
	if err != nil {
		t.Fatalf("Rebuild(checkpoint + suffix) error = %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("checkpoint/full rebuild differ field-by-field\n got: %#v\nwant: %#v", got, want)
	}

	if _, err := postgres.pool.Exec(ctx, `
		UPDATE runs
		SET checkpoint_state = '{"exists":false}'::jsonb
		WHERE run_id = $1`, runID); err != nil {
		t.Fatalf("corrupt disposable checkpoint: %v", err)
	}
	afterCorruption, err := postgres.Rebuild(ctx, runID)
	if err != nil {
		t.Fatalf("Rebuild(corrupt checkpoint) error = %v", err)
	}
	if !reflect.DeepEqual(afterCorruption, want) {
		t.Fatalf("corrupt checkpoint overrode Event Log\n got: %#v\nwant: %#v", afterCorruption, want)
	}
}

func TestPostgresRebuildRejectsSemanticallyForgedCheckpointState(t *testing.T) {
	ctx := context.Background()
	for _, withSuffix := range []bool{false, true} {
		name := "without-suffix"
		if withSuffix {
			name = "with-suffix"
		}
		t.Run(name, func(t *testing.T) {
			postgres := openPostgresStore(t, ctx, integrationDatabaseURL())
			runID := uniqueRunID("forged-checkpoint-" + name)
			if _, err := postgres.Append(ctx, 0, createdEvent(runID)); err != nil {
				t.Fatalf("Append(RunCreated) error = %v", err)
			}
			if _, err := postgres.Append(ctx, 1, domain.Event{
				RunID:          runID,
				Type:           domain.EventDesiredStateChanged,
				Data:           domain.EventData{DesiredState: domain.DesiredPaused},
				IdempotencyKey: "pause",
			}); err != nil {
				t.Fatalf("Append(Pause) error = %v", err)
			}
			checkpoint, err := postgres.SaveCheckpoint(ctx, runID, 2)
			if err != nil {
				t.Fatalf("SaveCheckpoint() error = %v", err)
			}
			var eventDigestBefore string
			if err := postgres.pool.QueryRow(ctx, `
				SELECT checkpoint_event_digest
				FROM runs
				WHERE run_id = $1`, runID).Scan(&eventDigestBefore); err != nil {
				t.Fatalf("load checkpoint event digest: %v", err)
			}
			checkpoint.State.Run.ScenarioID = "forged-scenario"
			forgedState, err := json.Marshal(checkpoint.State)
			if err != nil {
				t.Fatalf("marshal forged checkpoint State: %v", err)
			}
			if _, err := postgres.pool.Exec(ctx, `
				UPDATE runs
				SET checkpoint_state = $2::jsonb,
				    checkpoint_state_digest = $3
				WHERE run_id = $1`,
				runID,
				forgedState,
				digestBytes(forgedState),
			); err != nil {
				t.Fatalf("write self-consistent forged checkpoint: %v", err)
			}
			if withSuffix {
				if _, err := postgres.Append(ctx, 2, domain.Event{
					RunID:          runID,
					Type:           domain.EventDesiredStateChanged,
					Data:           domain.EventData{DesiredState: domain.DesiredRunning},
					IdempotencyKey: "resume",
				}); err != nil {
					t.Fatalf("Append(Resume suffix) error = %v", err)
				}
			}
			var eventDigestAfter string
			if err := postgres.pool.QueryRow(ctx, `
				SELECT checkpoint_event_digest
				FROM runs
				WHERE run_id = $1`, runID).Scan(&eventDigestAfter); err != nil {
				t.Fatalf("reload checkpoint event digest: %v", err)
			}
			if eventDigestAfter != eventDigestBefore {
				t.Fatalf(
					"checkpoint event-prefix digest changed: before=%s after=%s",
					eventDigestBefore,
					eventDigestAfter,
				)
			}

			events, err := postgres.Load(ctx, runID)
			if err != nil {
				t.Fatalf("Load() error = %v", err)
			}
			want, err := domain.Reduce(events)
			if err != nil {
				t.Fatalf("Reduce(authoritative log) error = %v", err)
			}
			got, err := postgres.Rebuild(ctx, runID)
			if err != nil {
				t.Fatalf("Rebuild() error = %v", err)
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf(
					"forged checkpoint overrode authoritative Event Log\n got: %#v\nwant: %#v",
					got,
					want,
				)
			}
		})
	}
}

func TestPostgresRejectsNestedCredentialInReasonWithoutAppending(t *testing.T) {
	ctx := context.Background()
	postgres := openPostgresStore(t, ctx, integrationDatabaseURL())
	runID := uniqueRunID("nested-credential")
	if _, err := postgres.Append(ctx, 0, createdEvent(runID)); err != nil {
		t.Fatalf("Append(RunCreated) error = %v", err)
	}
	_, err := postgres.Append(ctx, 1, domain.Event{
		RunID:          runID,
		Type:           domain.EventDesiredStateChanged,
		IdempotencyKey: "secret-reason",
		Data: domain.EventData{
			DesiredState: domain.DesiredPaused,
			Reason:       `[{"AWS.Secret-Access-Key":"opaque-credential"}]`,
		},
	})
	if !errors.Is(err, ErrSensitivePayload) {
		t.Fatalf("Append(nested credential Reason) error = %v, want sensitive payload rejection", err)
	}
	events, loadErr := postgres.Load(ctx, runID)
	if loadErr != nil {
		t.Fatalf("Load() error = %v", loadErr)
	}
	if len(events) != 1 {
		t.Fatalf("credential-bearing append changed event count to %d, want 1", len(events))
	}
	if _, err := postgres.Append(ctx, 1, domain.Event{
		RunID:          runID,
		Type:           domain.EventDesiredStateChanged,
		IdempotencyKey: "normal-reason",
		Data: domain.EventData{
			DesiredState: domain.DesiredPaused,
			Reason:       `[{"message":"ordinary non-credential text","token_count":42}]`,
		},
	}); err != nil {
		t.Fatalf("Append(normal nested Reason) error = %v, want success", err)
	}
	events, loadErr = postgres.Load(ctx, runID)
	if loadErr != nil {
		t.Fatalf("Load() after normal Reason error = %v", loadErr)
	}
	if len(events) != 2 {
		t.Fatalf("normal nested Reason event count = %d, want 2", len(events))
	}
}

func TestArtifactAttemptMustBelongToSameRun(t *testing.T) {
	ctx := context.Background()
	postgres := openPostgresStore(t, ctx, integrationDatabaseURL())
	firstRunID := uniqueRunID("artifact-owner")
	secondRunID := uniqueRunID("artifact-wrong-run")
	for _, runID := range []string{firstRunID, secondRunID} {
		if _, err := postgres.Append(ctx, 0, createdEvent(runID)); err != nil {
			t.Fatalf("Append(RunCreated %s) error = %v", runID, err)
		}
	}
	attemptID := firstRunID + ":attempt:1"
	if _, err := postgres.Append(ctx, 1, domain.Event{
		RunID:          firstRunID,
		Type:           domain.EventAttemptStarted,
		IdempotencyKey: "attempt-started",
		Data: domain.EventData{
			AttemptID: attemptID,
			ActionID:  "start",
			Reason:    "initial",
		},
	}); err != nil {
		t.Fatalf("Append(AttemptStarted) error = %v", err)
	}
	artifacts, err := NewPostgresArtifactStore(postgres, t.TempDir())
	if err != nil {
		t.Fatalf("NewPostgresArtifactStore() error = %v", err)
	}
	artifactID := secondRunID + "-cross-run"
	if _, err := artifacts.Write(ctx, ArtifactInput{
		ID:        artifactID,
		RunID:     secondRunID,
		AttemptID: attemptID,
		Type:      "test-log",
		Content:   strings.NewReader("complete but owned by another Run"),
	}); err == nil {
		t.Fatal("Write(cross-Run attempt) error = nil, want database ownership rejection")
	}
	var count int
	if err := postgres.pool.QueryRow(
		ctx,
		`SELECT count(*) FROM artifacts WHERE artifact_id = $1`,
		artifactID,
	).Scan(&count); err != nil {
		t.Fatalf("query cross-Run artifact: %v", err)
	}
	if count != 0 {
		t.Fatalf("cross-Run artifact metadata count = %d, want 0", count)
	}
}

func TestArtifactRegisteredOnlyAfterCompleteDigestWrite(t *testing.T) {
	ctx := context.Background()
	postgres := openPostgresStore(t, ctx, integrationDatabaseURL())
	runID := uniqueRunID("artifact")
	if _, err := postgres.Append(ctx, 0, createdEvent(runID)); err != nil {
		t.Fatalf("Append(RunCreated) error = %v", err)
	}
	if _, err := postgres.Append(ctx, 1, domain.Event{
		RunID:          runID,
		Type:           domain.EventAttemptStarted,
		IdempotencyKey: "attempt-started",
		Data: domain.EventData{
			AttemptID: runID + ":attempt:1",
			ActionID:  "start",
			Reason:    "initial",
		},
	}); err != nil {
		t.Fatalf("Append(AttemptStarted) error = %v", err)
	}

	artifacts, err := NewPostgresArtifactStore(postgres, t.TempDir())
	if err != nil {
		t.Fatalf("NewPostgresArtifactStore() error = %v", err)
	}
	failedArtifactID := runID + "-failed"
	_, err = artifacts.Write(ctx, ArtifactInput{
		ID:        failedArtifactID,
		RunID:     runID,
		AttemptID: runID + ":attempt:1",
		Type:      "test-log",
		Content:   &failingReader{content: []byte("partial")},
	})
	if err == nil {
		t.Fatal("Write(failing reader) error = nil")
	}
	var failedCount int
	if err := postgres.pool.QueryRow(ctx, `SELECT count(*) FROM artifacts WHERE artifact_id = $1`, failedArtifactID).Scan(&failedCount); err != nil {
		t.Fatalf("query failed artifact: %v", err)
	}
	if failedCount != 0 {
		t.Fatalf("incomplete artifact registration count = %d, want 0", failedCount)
	}
	entries, err := os.ReadDir(artifacts.root)
	if err != nil {
		t.Fatalf("ReadDir(artifact root) error = %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("failed artifact left partial files: %v", entries)
	}

	content := []byte("complete artifact bytes")
	completeArtifactID := runID + "-complete"
	record, err := artifacts.Write(ctx, ArtifactInput{
		ID:        completeArtifactID,
		RunID:     runID,
		AttemptID: runID + ":attempt:1",
		Type:      "test-log",
		Content:   bytes.NewReader(content),
	})
	if err != nil {
		t.Fatalf("Write(complete artifact) error = %v", err)
	}
	wantDigest := fmt.Sprintf("sha256:%x", sha256.Sum256(content))
	if record.Digest != wantDigest || record.Size != int64(len(content)) || record.CreatedAt == "" {
		t.Fatalf("artifact record = %#v, want digest=%s size=%d and database time", record, wantDigest, len(content))
	}
	persisted, err := os.ReadFile(record.Path)
	if err != nil {
		t.Fatalf("ReadFile(%s) error = %v", record.Path, err)
	}
	if !bytes.Equal(persisted, content) {
		t.Fatalf("artifact bytes = %q, want %q", persisted, content)
	}
	if filepath.Dir(record.Path) == "" {
		t.Fatalf("artifact path is not durable: %q", record.Path)
	}
	replayed, err := artifacts.Write(ctx, ArtifactInput{
		ID:        completeArtifactID,
		RunID:     runID,
		AttemptID: runID + ":attempt:1",
		Type:      "test-log",
		Content:   bytes.NewReader(content),
	})
	if err != nil {
		t.Fatalf("Write(idempotent Artifact replay) error = %v", err)
	}
	if !reflect.DeepEqual(replayed, record) {
		t.Fatalf("idempotent Artifact replay = %#v, want %#v", replayed, record)
	}
	var completeRows int
	if err := postgres.pool.QueryRow(
		ctx,
		`SELECT count(*) FROM artifacts WHERE artifact_id = $1`,
		completeArtifactID,
	).Scan(&completeRows); err != nil {
		t.Fatalf("query idempotent Artifact rows: %v", err)
	}
	if completeRows != 1 {
		t.Fatalf("idempotent Artifact row count = %d, want 1", completeRows)
	}
}

type failingReader struct {
	content []byte
	read    bool
}

type loadTraceKey struct{}

type blockingLoadTracer struct {
	runID   string
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (tracer *blockingLoadTracer) TraceQueryStart(
	ctx context.Context,
	_ *pgx.Conn,
	data pgx.TraceQueryStartData,
) context.Context {
	target := strings.Contains(data.SQL, "FROM events") &&
		len(data.Args) > 0 &&
		fmt.Sprint(data.Args[0]) == tracer.runID
	return context.WithValue(ctx, loadTraceKey{}, target)
}

func (tracer *blockingLoadTracer) TraceQueryEnd(
	ctx context.Context,
	_ *pgx.Conn,
	_ pgx.TraceQueryEndData,
) {
	target, _ := ctx.Value(loadTraceKey{}).(bool)
	if !target {
		return
	}
	tracer.once.Do(func() {
		close(tracer.entered)
		<-tracer.release
	})
}

func (reader *failingReader) Read(target []byte) (int, error) {
	if reader.read {
		return 0, errors.New("forced reader failure")
	}
	reader.read = true
	count := copy(target, reader.content)
	return count, io.ErrUnexpectedEOF
}

func openPostgresStore(t *testing.T, ctx context.Context, dsn string) *PostgresEventStore {
	t.Helper()
	postgres, err := NewPostgresEventStore(ctx, dsn)
	if err != nil {
		t.Fatalf("NewPostgresEventStore() error = %v", err)
	}
	t.Cleanup(postgres.Close)
	return postgres
}

func createdEvent(runID string) domain.Event {
	return domain.Event{
		RunID:          runID,
		Type:           domain.EventRunCreated,
		IdempotencyKey: "run-created",
		Data:           domain.EventData{ScenarioID: "scenario", SpecHash: "spec"},
		CorrelationID:  runID,
		CreatedAt:      "1900-01-01T00:00:00Z",
	}
}

func thousandAppendEvents(runID string) []domain.Event {
	events := make([]domain.Event, 0, 1000)
	events = append(events, createdEvent(runID))
	for pair := 0; pair < 499; pair++ {
		events = append(events,
			domain.Event{
				RunID:          runID,
				Type:           domain.EventDesiredStateChanged,
				Data:           domain.EventData{DesiredState: domain.DesiredPaused},
				IdempotencyKey: fmt.Sprintf("pause-%d", pair),
				CorrelationID:  runID,
			},
			domain.Event{
				RunID:          runID,
				Type:           domain.EventDesiredStateChanged,
				Data:           domain.EventData{DesiredState: domain.DesiredRunning},
				IdempotencyKey: fmt.Sprintf("resume-%d", pair),
				CorrelationID:  runID,
			},
		)
	}
	events = append(events, domain.Event{
		RunID:          runID,
		Type:           domain.EventAttemptStarted,
		IdempotencyKey: "attempt-started",
		Data: domain.EventData{
			AttemptID: runID + ":attempt:1",
			ActionID:  "start",
			Reason:    "initial",
		},
		CorrelationID: runID,
	})
	return events
}

func integrationDatabaseURL() string {
	if value := os.Getenv("AGENTDOCK_DATABASE_URL"); value != "" {
		return value
	}
	return "postgres://agentdock:agentdock_dev_only@127.0.0.1:55433/agentdock?sslmode=disable"
}

func uniqueRunID(prefix string) string {
	return fmt.Sprintf("run-%s-%d", prefix, time.Now().UnixNano())
}

func withPoolMaxConns(t *testing.T, dsn, count string) string {
	t.Helper()
	parsed, err := url.Parse(dsn)
	if err != nil {
		t.Fatalf("parse DSN: %v", err)
	}
	query := parsed.Query()
	query.Set("pool_max_conns", count)
	parsed.RawQuery = query.Encode()
	return parsed.String()
}

func safeIdentifier(value string) string {
	result := make([]byte, 0, len(value))
	for index := range len(value) {
		character := value[index]
		if (character >= 'a' && character <= 'z') ||
			(character >= 'A' && character <= 'Z') ||
			(character >= '0' && character <= '9') ||
			character == '_' {
			result = append(result, character)
		} else {
			result = append(result, '_')
		}
	}
	return string(result)
}

func quoteIdentifier(value string) string {
	return `"` + value + `"`
}

func quoteLiteral(value string) string {
	return `'` + value + `'`
}
