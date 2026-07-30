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
