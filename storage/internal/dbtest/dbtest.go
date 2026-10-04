// Package dbtest provides a throwaway Postgres schema for integration tests.
//
// Tests are skipped unless TAGONA_TEST_PG_DSN is set, e.g.
//
//	TAGONA_TEST_PG_DSN='postgres://tagona:tagona@localhost:5432/tagona?sslmode=disable' go test ./...
//
// Every call runs the real migrations in its own schema and drops it on cleanup.
package dbtest

import (
	"context"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"mrsydar/tagona/storage/internal/db"
)

// extensionLockKey is an arbitrary application-level advisory lock id used to
// serialize CREATE EXTENSION across test packages and processes.
const extensionLockKey = 7240001

// EnsureExtension creates the uuid-ossp extension if it is missing, safely for
// concurrent callers.
//
// CREATE EXTENSION IF NOT EXISTS is not race-free: two sessions that both see
// the extension missing both try to register it, and one fails with a unique
// violation on pg_extension_name_index. Go runs test packages in parallel and
// each calls dbtest.New, so on a fresh database (as in CI) that race is real.
// A transaction-scoped advisory lock serializes the callers, works across
// processes, and is released automatically on commit or rollback.
func EnsureExtension(ctx context.Context, pool *pgxpool.Pool) error {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, extensionLockKey); err != nil {
		return fmt.Errorf("advisory lock: %w", err)
	}
	if _, err := tx.Exec(ctx, `CREATE EXTENSION IF NOT EXISTS "uuid-ossp"`); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// MigrationsDir is the absolute path of storage/migrations.
func MigrationsDir() string {
	_, file, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(file), "..", "..", "migrations")
}

// New creates a schema, applies the given up-migration files (all of them when
// none are given) and returns a DB bound to it.
func New(t *testing.T, migrations ...string) (*db.DB, *pgxpool.Pool) {
	t.Helper()
	dsn := os.Getenv("TAGONA_TEST_PG_DSN")
	if dsn == "" {
		t.Skip("TAGONA_TEST_PG_DSN not set")
	}
	ctx := context.Background()

	admin, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(admin.Close)
	if err := EnsureExtension(ctx, admin); err != nil {
		t.Fatalf("create extension: %v", err)
	}

	schema := fmt.Sprintf("test_%d_%d", time.Now().UnixNano(), rand.Intn(1_000_000))
	if _, err := admin.Exec(ctx, `CREATE SCHEMA `+schema); err != nil {
		t.Fatalf("create schema: %v", err)
	}
	t.Cleanup(func() { admin.Exec(context.Background(), `DROP SCHEMA `+schema+` CASCADE`) })

	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("parse dsn: %v", err)
	}
	cfg.MaxConns = 16
	cfg.ConnConfig.RuntimeParams["search_path"] = schema + ",public"
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("connect to schema: %v", err)
	}
	t.Cleanup(pool.Close)

	if len(migrations) == 0 {
		migrations = AllMigrations(t)
	}
	ApplyMigrations(t, pool, migrations...)
	return db.New(pool), pool
}

// AllMigrations lists every *.up.sql file name in lexicographic order.
func AllMigrations(t *testing.T) []string {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(MigrationsDir(), "*.up.sql"))
	if err != nil || len(files) == 0 {
		t.Fatalf("find migrations: %v", err)
	}
	names := make([]string, len(files))
	for i, f := range files {
		names[i] = filepath.Base(f)
	}
	return names
}

// ApplyMigrations executes the named migration files in order.
func ApplyMigrations(t *testing.T, pool *pgxpool.Pool, names ...string) {
	t.Helper()
	for _, name := range names {
		data, err := os.ReadFile(filepath.Join(MigrationsDir(), name))
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		if _, err := pool.Exec(context.Background(), string(data)); err != nil {
			t.Fatalf("apply %s: %v", name, err)
		}
	}
}
