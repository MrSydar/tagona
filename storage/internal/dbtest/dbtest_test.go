package dbtest

import (
	"context"
	"fmt"
	"math/rand"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Regression test for a CI flake: test packages run in parallel, each calls
// New, and on a fresh database the concurrent CREATE EXTENSION IF NOT EXISTS
// calls raced with "duplicate key value violates unique constraint
// pg_extension_name_index". It needs a database where the extension does not
// exist yet, so it creates a throwaway one.
func TestEnsureExtensionIsSafeForConcurrentCallers(t *testing.T) {
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
	name := fmt.Sprintf("dbtest_race_%d_%d", time.Now().UnixNano(), rand.Intn(1_000_000))
	if _, err := admin.Exec(ctx, `CREATE DATABASE `+name); err != nil {
		t.Skipf("cannot create a throwaway database: %v", err)
	}
	t.Cleanup(func() { admin.Exec(context.Background(), `DROP DATABASE `+name+` WITH (FORCE)`) })

	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("parse dsn: %v", err)
	}
	cfg.ConnConfig.Database = name
	cfg.MaxConns = 24
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("connect to %s: %v", name, err)
	}
	t.Cleanup(pool.Close)

	const callers = 24
	start := make(chan struct{})
	errs := make(chan error, callers)
	var wg sync.WaitGroup
	for range callers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start // release everyone at once to maximise the overlap
			errs <- EnsureExtension(ctx, pool)
		}()
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Errorf("EnsureExtension failed under concurrency: %v", err)
		}
	}
}
