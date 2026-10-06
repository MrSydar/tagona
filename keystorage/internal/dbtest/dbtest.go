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
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"mrsydar/tagona/keystorage/internal/db"
	"mrsydar/tagona/keystorage/migrations"
)

// New creates a schema, applies the migrations in it and returns a DB (and its pool) bound to it.
func New(t *testing.T) (*db.DB, *pgxpool.Pool) {
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

	schema := fmt.Sprintf("test_keys_%d_%d", time.Now().UnixNano(), rand.Intn(1_000_000))
	if _, err := admin.Exec(ctx, `CREATE SCHEMA `+schema); err != nil {
		t.Fatalf("create schema: %v", err)
	}
	t.Cleanup(func() { admin.Exec(context.Background(), `DROP SCHEMA `+schema+` CASCADE`) })

	pool, err := db.Connect(ctx, dsn, schema)
	if err != nil {
		t.Fatalf("connect to schema: %v", err)
	}
	t.Cleanup(pool.Close)
	if err := db.Migrate(ctx, pool, migrations.FS); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return db.New(pool), pool
}
