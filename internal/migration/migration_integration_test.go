//go:build integration

package migration_test

import (
	"context"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/agentdock/agentdock-verify/internal/migration"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestUpMigratesEmptySchemaWithAllPhase2Tables(t *testing.T) {
	ctx := context.Background()
	schema := newSchemaName("phase2_empty")
	dsn := schemaDatabaseURL(t, ctx, schema)
	t.Cleanup(func() { dropSchema(t, ctx, schema) })

	if err := migration.Up(dsn, filepath.Join("..", "..", "migrations")); err != nil {
		t.Fatalf("Up(empty schema) error = %v", err)
	}

	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("pgxpool.New() error = %v", err)
	}
	defer pool.Close()
	rows, err := pool.Query(ctx, `
		SELECT table_name
		FROM information_schema.tables
		WHERE table_schema = current_schema()
		  AND table_name IN ('runs', 'events', 'attempts', 'artifacts', 'leases')
		ORDER BY table_name`)
	if err != nil {
		t.Fatalf("query migrated tables: %v", err)
	}
	defer rows.Close()
	var got []string
	for rows.Next() {
		var table string
		if err := rows.Scan(&table); err != nil {
			t.Fatalf("scan table: %v", err)
		}
		got = append(got, table)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("table rows error: %v", err)
	}
	want := []string{"artifacts", "attempts", "events", "leases", "runs"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("phase 2 tables = %v, want %v", got, want)
	}
	var ownershipConstraint bool
	if err := pool.QueryRow(ctx, `
		SELECT EXISTS(
			SELECT 1
			FROM pg_constraint
			WHERE conrelid = 'artifacts'::regclass
			  AND conname = 'artifacts_run_attempt_fk'
			  AND contype = 'f'
		)`).Scan(&ownershipConstraint); err != nil {
		t.Fatalf("query Artifact ownership constraint: %v", err)
	}
	if !ownershipConstraint {
		t.Fatal("empty-schema migration is missing artifacts_run_attempt_fk")
	}
}

func TestFailedMigrationLeavesNoPartialApplicationTables(t *testing.T) {
	ctx := context.Background()
	schema := newSchemaName("phase2_failed")
	dsn := schemaDatabaseURL(t, ctx, schema)
	t.Cleanup(func() { dropSchema(t, ctx, schema) })

	migrationDir := t.TempDir()
	content := []byte("BEGIN;\nCREATE TABLE partial_phase2_table (id bigint PRIMARY KEY);\nSELECT missing_phase2_function();\nCOMMIT;\n")
	if err := os.WriteFile(filepath.Join(migrationDir, "000001_bad.up.sql"), content, 0o600); err != nil {
		t.Fatalf("write failing migration: %v", err)
	}
	if err := migration.Up(dsn, migrationDir); err == nil {
		t.Fatal("Up(failing migration) error = nil, want failure")
	}

	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("pgxpool.New() error = %v", err)
	}
	defer pool.Close()
	var partialTable *string
	if err := pool.QueryRow(ctx, `SELECT to_regclass('partial_phase2_table')::text`).Scan(&partialTable); err != nil {
		t.Fatalf("query partial table: %v", err)
	}
	if partialTable != nil {
		t.Fatalf("failed migration left partial table %q", *partialTable)
	}
	var dirtyRows int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM schema_migrations WHERE dirty`).Scan(&dirtyRows); err != nil {
		t.Fatalf("query dirty migration rows: %v", err)
	}
	if dirtyRows != 1 {
		t.Fatalf("failed transaction left %d dirty migration rows, want one protective marker", dirtyRows)
	}
	if err := migration.Up(dsn, migrationDir); err == nil ||
		!strings.Contains(err.Error(), "Dirty database version") {
		t.Fatalf("second Up() error = %v, want protective dirty-version rejection", err)
	}
}

func TestPhase3MigrationAddsWorkersReceiptsAndFencingConstraints(t *testing.T) {
	ctx := context.Background()
	schema := newSchemaName("phase3_empty")
	dsn := schemaDatabaseURL(t, ctx, schema)
	t.Cleanup(func() { dropSchema(t, ctx, schema) })
	if err := migration.Up(dsn, filepath.Join("..", "..", "migrations")); err != nil {
		t.Fatalf("Up(empty schema) error = %v", err)
	}
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("pgxpool.New() error = %v", err)
	}
	defer pool.Close()
	var workers, receipts, workerForeignKey, receiptArtifactForeignKey, receiptArtifactDigestPair, actionUnique, artifactUnique bool
	if err := pool.QueryRow(ctx, `SELECT to_regclass('workers') IS NOT NULL`).Scan(&workers); err != nil {
		t.Fatalf("query workers table: %v", err)
	}
	if err := pool.QueryRow(ctx, `SELECT to_regclass('action_receipts') IS NOT NULL`).Scan(&receipts); err != nil {
		t.Fatalf("query action_receipts table: %v", err)
	}
	if err := pool.QueryRow(ctx, `
		SELECT EXISTS(
			SELECT 1 FROM pg_constraint
			WHERE conrelid = 'leases'::regclass
			  AND conname = 'leases_worker_fk'
			  AND contype = 'f'
		)`).Scan(&workerForeignKey); err != nil {
		t.Fatalf("query leases Worker foreign key: %v", err)
	}
	if err := pool.QueryRow(ctx, `
		SELECT EXISTS(
			SELECT 1 FROM pg_constraint
			WHERE conrelid = 'action_receipts'::regclass
			  AND conname = 'action_receipts_artifact_fk'
			  AND contype = 'f'
			  AND convalidated
		)`).Scan(&receiptArtifactForeignKey); err != nil {
		t.Fatalf("query receipt Artifact foreign key: %v", err)
	}
	if err := pool.QueryRow(ctx, `
		SELECT EXISTS(
			SELECT 1 FROM pg_constraint
			WHERE conrelid = 'action_receipts'::regclass
			  AND conname = 'action_receipts_artifact_digest_pair'
			  AND contype = 'c'
			  AND convalidated
		)`).Scan(&receiptArtifactDigestPair); err != nil {
		t.Fatalf("query receipt Artifact digest constraint: %v", err)
	}
	if err := pool.QueryRow(ctx, `
		SELECT EXISTS(
			SELECT 1 FROM pg_constraint
			WHERE conrelid = 'action_receipts'::regclass
			  AND conname = 'action_receipts_run_action_unique'
			  AND contype = 'u'
		)`).Scan(&actionUnique); err != nil {
		t.Fatalf("query receipt action uniqueness: %v", err)
	}
	if err := pool.QueryRow(ctx, `
		SELECT to_regclass('action_receipts_run_artifact_unique') IS NOT NULL`,
	).Scan(&artifactUnique); err != nil {
		t.Fatalf("query receipt Artifact uniqueness: %v", err)
	}
	if !workers || !receipts || !workerForeignKey || !receiptArtifactForeignKey ||
		!receiptArtifactDigestPair || !actionUnique || !artifactUnique {
		t.Fatalf(
			"phase 3 schema workers=%t receipts=%t worker_fk=%t receipt_artifact_fk=%t receipt_artifact_digest_pair=%t action_unique=%t artifact_unique=%t",
			workers,
			receipts,
			workerForeignKey,
			receiptArtifactForeignKey,
			receiptArtifactDigestPair,
			actionUnique,
			artifactUnique,
		)
	}
}

func TestPhase3UpgradePreservesPhase2DataAndRoundTripsDownUp(t *testing.T) {
	ctx := context.Background()
	schema := newSchemaName("phase3_upgrade")
	dsn := schemaDatabaseURL(t, ctx, schema)
	t.Cleanup(func() { dropSchema(t, ctx, schema) })
	path := filepath.Join("..", "..", "migrations")
	if err := migration.To(dsn, path, 2); err != nil {
		t.Fatalf("To(version 2) error = %v", err)
	}
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("pgxpool.New() error = %v", err)
	}
	defer pool.Close()
	runID := "phase2-upgrade-run"
	attemptID := runID + ":attempt:1"
	artifactID := runID + "-artifact"
	workerID := "phase2-legacy-worker"
	if _, err := pool.Exec(ctx, `
		INSERT INTO runs (
			run_id, scenario_id, spec_hash, desired_state, observed_state,
			current_attempt, version
		) VALUES ($1, 'phase2-upgrade', 'spec', 'Running', 'Provisioning', 1, 1)`,
		runID,
	); err != nil {
		t.Fatalf("seed phase 2 Run: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO events (
			run_id, seq, event_type, payload_version, payload,
			idempotency_key, correlation_id
		) VALUES ($1, 1, 'RunCreated', 1, '{"scenario_id":"phase2-upgrade","spec_hash":"spec"}', 'created', $1)`,
		runID,
	); err != nil {
		t.Fatalf("seed phase 2 Event: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO attempts (
			attempt_id, run_id, number, reason, started_at
		) VALUES ($2, $1, 1, 'initial', clock_timestamp())`,
		runID,
		attemptID,
	); err != nil {
		t.Fatalf("seed phase 2 Attempt: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO artifacts (
			artifact_id, run_id, attempt_id, artifact_type, digest, path, size
		) VALUES ($3, $1, $2, 'phase2-evidence',
			'sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa',
			'/tmp/phase2-upgrade-artifact', 7)`,
		runID,
		attemptID,
		artifactID,
	); err != nil {
		t.Fatalf("seed phase 2 Artifact: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO leases (
			run_id, worker_id, fencing_token, expires_at, heartbeat_at
		) VALUES ($1, $2, 7, clock_timestamp() + interval '1 hour', clock_timestamp())`,
		runID,
		workerID,
	); err != nil {
		t.Fatalf("seed phase 2 Lease: %v", err)
	}

	if err := migration.To(dsn, path, 5); err != nil {
		t.Fatalf("upgrade version 2 to 5: %v", err)
	}
	assertMigrationVersion(t, ctx, pool, 5)
	assertPhase2UpgradeData(t, ctx, pool, runID, attemptID, artifactID, workerID)
	if err := migration.To(dsn, path, 4); err != nil {
		t.Fatalf("down version 5 to 4: %v", err)
	}
	assertMigrationVersion(t, ctx, pool, 4)
	if err := migration.To(dsn, path, 3); err != nil {
		t.Fatalf("down version 4 to 3: %v", err)
	}
	assertMigrationVersion(t, ctx, pool, 3)
	if err := migration.To(dsn, path, 2); err != nil {
		t.Fatalf("down version 3 to 2: %v", err)
	}
	assertMigrationVersion(t, ctx, pool, 2)
	assertPhase2UpgradeData(t, ctx, pool, runID, attemptID, artifactID, "")
	if err := migration.To(dsn, path, 5); err != nil {
		t.Fatalf("re-up version 2 to 5: %v", err)
	}
	assertMigrationVersion(t, ctx, pool, 5)
	assertPhase2UpgradeData(t, ctx, pool, runID, attemptID, artifactID, workerID)
}

func assertMigrationVersion(t *testing.T, ctx context.Context, pool *pgxpool.Pool, want uint) {
	t.Helper()
	var version uint
	var dirty bool
	if err := pool.QueryRow(ctx, `SELECT version, dirty FROM schema_migrations`).Scan(&version, &dirty); err != nil {
		t.Fatalf("query migration version: %v", err)
	}
	if version != want || dirty {
		t.Fatalf("migration state version=%d dirty=%t, want version=%d dirty=false", version, dirty, want)
	}
}

func assertPhase2UpgradeData(
	t *testing.T,
	ctx context.Context,
	pool *pgxpool.Pool,
	runID string,
	attemptID string,
	artifactID string,
	workerID string,
) {
	t.Helper()
	var runCount, eventCount, attemptCount, artifactCount, leaseCount int
	if err := pool.QueryRow(ctx, `
		SELECT
			(SELECT count(*) FROM runs WHERE run_id = $1),
			(SELECT count(*) FROM events WHERE run_id = $1),
			(SELECT count(*) FROM attempts WHERE attempt_id = $2),
			(SELECT count(*) FROM artifacts WHERE artifact_id = $3),
			(SELECT count(*) FROM leases WHERE run_id = $1)`,
		runID,
		attemptID,
		artifactID,
	).Scan(&runCount, &eventCount, &attemptCount, &artifactCount, &leaseCount); err != nil {
		t.Fatalf("query preserved phase 2 data: %v", err)
	}
	if runCount != 1 || eventCount != 1 || attemptCount != 1 || artifactCount != 1 || leaseCount != 1 {
		t.Fatalf(
			"preserved counts run=%d event=%d attempt=%d artifact=%d lease=%d",
			runCount,
			eventCount,
			attemptCount,
			artifactCount,
			leaseCount,
		)
	}
	if workerID != "" {
		var workerCount int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM workers WHERE worker_id = $1`, workerID).Scan(&workerCount); err != nil {
			t.Fatalf("query backfilled Worker: %v", err)
		}
		if workerCount != 1 {
			t.Fatalf("backfilled Worker count = %d, want 1", workerCount)
		}
	}
}

func schemaDatabaseURL(t *testing.T, ctx context.Context, schema string) string {
	t.Helper()
	base := integrationDatabaseURL()
	admin, err := pgxpool.New(ctx, base)
	if err != nil {
		t.Fatalf("open admin database: %v", err)
	}
	defer admin.Close()
	if _, err := admin.Exec(ctx, `CREATE SCHEMA `+quoteIdentifier(schema)); err != nil {
		t.Fatalf("create schema %s: %v", schema, err)
	}

	parsed, err := url.Parse(base)
	if err != nil {
		t.Fatalf("parse database URL: %v", err)
	}
	query := parsed.Query()
	query.Set("search_path", schema)
	parsed.RawQuery = query.Encode()
	return parsed.String()
}

func dropSchema(t *testing.T, ctx context.Context, schema string) {
	t.Helper()
	pool, err := pgxpool.New(ctx, integrationDatabaseURL())
	if err != nil {
		t.Errorf("open admin database for cleanup: %v", err)
		return
	}
	defer pool.Close()
	if _, err := pool.Exec(ctx, `DROP SCHEMA IF EXISTS `+quoteIdentifier(schema)+` CASCADE`); err != nil {
		t.Errorf("drop schema %s: %v", schema, err)
	}
}

func quoteIdentifier(value string) string {
	return `"` + strings.ReplaceAll(value, `"`, `""`) + `"`
}

func newSchemaName(prefix string) string {
	timestamp := time.Now().UTC().Format("20060102_150405.000000000")
	return strings.ReplaceAll(prefix+"_"+timestamp, ".", "_")
}

func integrationDatabaseURL() string {
	if value := os.Getenv("AGENTDOCK_DATABASE_URL"); value != "" {
		return value
	}
	return "postgres://agentdock:agentdock_dev_only@127.0.0.1:55433/agentdock?sslmode=disable"
}
