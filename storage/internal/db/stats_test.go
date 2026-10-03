package db_test

import (
	"context"
	"fmt"
	"math/rand"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"mrsydar/tagona/storage/internal/db"
	"mrsydar/tagona/storage/internal/dbtest"
)

// These tests exercise the trigger-maintained collection statistics against a
// real Postgres. They are skipped unless TAGONA_TEST_PG_DSN is set (see
// package dbtest). Every test runs the real migrations inside its own schema.

func newTestDB(t *testing.T, migrations ...string) (*db.DB, *pgxpool.Pool) {
	t.Helper()
	return dbtest.New(t, migrations...)
}

func allMigrations(t *testing.T) []string { return dbtest.AllMigrations(t) }

func applyMigrations(t *testing.T, pool *pgxpool.Pool, names ...string) {
	t.Helper()
	dbtest.ApplyMigrations(t, pool, names...)
}

func mustCollection(t *testing.T, d *db.DB, name string) string {
	t.Helper()
	c, err := d.CreateCollection(context.Background(), name, "txt")
	if err != nil {
		t.Fatalf("create collection: %v", err)
	}
	return c.ID
}

func mustObject(t *testing.T, d *db.DB, collID, hash string, expiresAt *time.Time) string {
	t.Helper()
	o, err := d.InsertObject(context.Background(), collID, hash, time.Now(), 1, "txt", "key/"+hash, expiresAt)
	if err != nil {
		t.Fatalf("insert object: %v", err)
	}
	return o.ID
}

func mustTag(t *testing.T, d *db.DB, collID, objID string, tags map[string]bool) {
	t.Helper()
	if err := d.UpsertTags(context.Background(), collID, objID, tags); err != nil {
		t.Fatalf("upsert tags: %v", err)
	}
}

type counts struct{ t, f int64 }

// assertStats checks the API-facing stats for a collection.
func assertStats(t *testing.T, d *db.DB, collID string, wantTotal int64, want map[string]counts) {
	t.Helper()
	total, stats, err := d.GetCollectionTagStats(context.Background(), collID, "", "", 1000)
	if err != nil {
		t.Fatalf("get stats: %v", err)
	}
	if total != wantTotal {
		t.Errorf("total_objects = %d, want %d", total, wantTotal)
	}
	got := map[string]counts{}
	for _, st := range stats {
		got[st.Tag] = counts{st.TrueCount, st.FalseCount}
		if st.UnknownCount != total-st.TrueCount-st.FalseCount {
			t.Errorf("tag %q: unknown = %d, want %d", st.Tag, st.UnknownCount, total-st.TrueCount-st.FalseCount)
		}
	}
	for tag, w := range want {
		if got[tag] != w {
			t.Errorf("tag %q = %+v, want %+v", tag, got[tag], w)
		}
	}
	for tag := range got {
		if _, ok := want[tag]; !ok {
			t.Errorf("unexpected tag %q registered", tag)
		}
	}
}

// assertConsistent recomputes the statistics from the source tables and
// compares them with the trigger-maintained counters (ignoring expiry).
func assertConsistent(t *testing.T, pool *pgxpool.Pool, collID string) {
	t.Helper()
	ctx := context.Background()

	var counter, actual int64
	if err := pool.QueryRow(ctx, `SELECT object_count FROM collections WHERE id = $1`, collID).Scan(&counter); err != nil {
		t.Fatalf("read object_count: %v", err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM objects WHERE collection_id = $1`, collID).Scan(&actual); err != nil {
		t.Fatalf("count objects: %v", err)
	}
	if counter != actual {
		t.Errorf("object_count = %d, actual %d", counter, actual)
	}

	truth := map[string]counts{}
	rows, err := pool.Query(ctx,
		`SELECT tag, count(*) FILTER (WHERE value), count(*) FILTER (WHERE NOT value)
		 FROM object_tags WHERE collection_id = $1 GROUP BY tag`, collID)
	if err != nil {
		t.Fatalf("recompute: %v", err)
	}
	for rows.Next() {
		var tag string
		var c counts
		rows.Scan(&tag, &c.t, &c.f)
		truth[tag] = c
	}
	rows.Close()

	registered := map[string]counts{}
	rows, err = pool.Query(ctx, `SELECT tag, true_count, false_count FROM collection_tags WHERE collection_id = $1`, collID)
	if err != nil {
		t.Fatalf("read registry: %v", err)
	}
	for rows.Next() {
		var tag string
		var c counts
		rows.Scan(&tag, &c.t, &c.f)
		registered[tag] = c
	}
	rows.Close()

	for tag, want := range truth {
		if got, ok := registered[tag]; !ok || got != want {
			t.Errorf("tag %q: registry %+v (present=%v), actual %+v", tag, got, ok, want)
		}
	}
	for tag, got := range registered {
		if _, ok := truth[tag]; !ok && (got.t != 0 || got.f != 0) {
			t.Errorf("tag %q: registry %+v but no object_tags rows", tag, got)
		}
	}
}

func TestCollectionStatsTriggers(t *testing.T) {
	d, pool := newTestDB(t)
	ctx := context.Background()
	coll := mustCollection(t, d, "jobs")

	assertStats(t, d, coll, 0, map[string]counts{})

	o1 := mustObject(t, d, coll, "h1", nil)
	o2 := mustObject(t, d, coll, "h2", nil)
	o3 := mustObject(t, d, coll, "h3", nil)
	assertStats(t, d, coll, 3, map[string]counts{})

	mustTag(t, d, coll, o1, map[string]bool{"golang": true, "qa": false})
	mustTag(t, d, coll, o2, map[string]bool{"golang": true})
	mustTag(t, d, coll, o3, map[string]bool{"golang": false, "remote": true})
	assertStats(t, d, coll, 3, map[string]counts{
		"golang": {2, 1},
		"qa":     {0, 1},
		"remote": {1, 0},
	})
	assertConsistent(t, pool, coll)

	_, stats, _ := d.GetCollectionTagStats(ctx, coll, "", "", 100)
	for _, st := range stats {
		if st.Tag == "golang" && st.UnknownCount != 0 {
			t.Errorf("golang unknown = %d, want 0", st.UnknownCount)
		}
		if st.Tag == "qa" && st.UnknownCount != 2 {
			t.Errorf("qa unknown = %d, want 2", st.UnknownCount)
		}
	}

	// Re-upserting the same value is a no-op for the counters; flipping moves
	// the object between true and false.
	mustTag(t, d, coll, o1, map[string]bool{"golang": true})
	mustTag(t, d, coll, o1, map[string]bool{"golang": false, "qa": true})
	assertStats(t, d, coll, 3, map[string]counts{
		"golang": {1, 2},
		"qa":     {1, 0},
		"remote": {1, 0},
	})
	assertConsistent(t, pool, coll)

	// Deleting an object cascades to its tags and decrements everything.
	if _, err := d.DeleteObject(ctx, o3); err != nil {
		t.Fatalf("delete object: %v", err)
	}
	assertStats(t, d, coll, 2, map[string]counts{
		"golang": {1, 1},
		"qa":     {1, 0},
		"remote": {0, 0}, // stays registered, with no objects
	})
	assertConsistent(t, pool, coll)

	// Collections are isolated.
	other := mustCollection(t, d, "other")
	oo := mustObject(t, d, other, "h1", nil)
	mustTag(t, d, other, oo, map[string]bool{"golang": true})
	assertStats(t, d, other, 1, map[string]counts{"golang": {1, 0}})
	assertStats(t, d, coll, 2, map[string]counts{"golang": {1, 1}, "qa": {1, 0}, "remote": {0, 0}})

	// Deleting a collection (cascade) must not fail and removes its registry.
	if err := d.DeleteCollection(ctx, coll); err != nil {
		t.Fatalf("delete collection: %v", err)
	}
	var n int
	pool.QueryRow(ctx, `SELECT count(*) FROM collection_tags WHERE collection_id = $1`, coll).Scan(&n)
	if n != 0 {
		t.Errorf("registry rows left after collection delete: %d", n)
	}
	assertConsistent(t, pool, other)
}

func TestCollectionStatsExcludeExpired(t *testing.T) {
	d, pool := newTestDB(t)
	coll := mustCollection(t, d, "jobs")
	past := time.Now().Add(-time.Hour)
	future := time.Now().Add(time.Hour)

	live := mustObject(t, d, coll, "live", &future)
	forever := mustObject(t, d, coll, "forever", nil)
	expired := mustObject(t, d, coll, "expired", &past) // not swept yet
	mustTag(t, d, coll, live, map[string]bool{"a": true})
	mustTag(t, d, coll, forever, map[string]bool{"a": false})
	mustTag(t, d, coll, expired, map[string]bool{"a": true, "only-expired": true})

	// The counters still include the unswept object...
	assertConsistent(t, pool, coll)
	var raw int64
	pool.QueryRow(context.Background(), `SELECT object_count FROM collections WHERE id = $1`, coll).Scan(&raw)
	if raw != 3 {
		t.Fatalf("raw object_count = %d, want 3", raw)
	}
	// ...but the stats match what reads and queries see.
	assertStats(t, d, coll, 2, map[string]counts{
		"a":            {1, 1},
		"only-expired": {0, 0},
	})

	// The retention sweep removes it; the corrected numbers do not change.
	if _, err := d.DeleteObject(context.Background(), expired); err != nil {
		t.Fatalf("delete expired: %v", err)
	}
	assertStats(t, d, coll, 2, map[string]counts{
		"a":            {1, 1},
		"only-expired": {0, 0},
	})
	assertConsistent(t, pool, coll)
}

func TestCollectionStatsBackfillAndIdempotentMigration(t *testing.T) {
	// Start from the pre-stats schema and create data with no triggers.
	d, pool := newTestDB(t, "000001_initial_schema.up.sql", "000002_api_keys.up.sql")
	coll := mustCollection(t, d, "jobs")
	o1 := mustObject(t, d, coll, "h1", nil)
	o2 := mustObject(t, d, coll, "h2", nil)
	// Seed with raw SQL: before migration 000003 there is no collection_tags
	// table, so the regular UpsertTags path cannot be used yet.
	for _, row := range []struct {
		obj, tag string
		value    bool
	}{{o1, "golang", true}, {o1, "qa", false}, {o2, "golang", false}} {
		if _, err := pool.Exec(context.Background(),
			`INSERT INTO object_tags (object_id, collection_id, tag, value) VALUES ($1, $2, $3, $4)`,
			row.obj, coll, row.tag, row.value); err != nil {
			t.Fatalf("seed tag: %v", err)
		}
	}

	applyMigrations(t, pool, "000003_collection_tag_stats.up.sql")
	assertStats(t, d, coll, 2, map[string]counts{"golang": {1, 1}, "qa": {0, 1}})
	assertConsistent(t, pool, coll)

	// Migrations run on every startup: re-running must not change anything,
	// and in particular must not re-run the backfill on top of live counters.
	o3 := mustObject(t, d, coll, "h3", nil)
	mustTag(t, d, coll, o3, map[string]bool{"golang": true})
	applyMigrations(t, pool, "000003_collection_tag_stats.up.sql")
	applyMigrations(t, pool, allMigrations(t)...)
	assertStats(t, d, coll, 3, map[string]counts{"golang": {2, 1}, "qa": {0, 1}})
	assertConsistent(t, pool, coll)
}

func TestCollectionStatsConcurrentWriters(t *testing.T) {
	d, pool := newTestDB(t)
	ctx := context.Background()
	coll := mustCollection(t, d, "jobs")

	const objects = 30
	ids := make([]string, objects)
	for i := range ids {
		ids[i] = mustObject(t, d, coll, fmt.Sprintf("h%d", i), nil)
	}
	tagNames := []string{"a", "b", "c", "d", "e", "f"}

	var wg sync.WaitGroup
	errs := make(chan error, 64)
	for w := 0; w < 8; w++ {
		wg.Add(1)
		go func(seed int64) {
			defer wg.Done()
			rng := rand.New(rand.NewSource(seed))
			for i := 0; i < 60; i++ {
				id := ids[rng.Intn(len(ids))]
				tags := map[string]bool{}
				for _, name := range tagNames {
					if rng.Intn(2) == 0 {
						tags[name] = rng.Intn(2) == 0
					}
				}
				// Another worker may have deleted the object meanwhile; that is a
				// legitimate foreign key failure, not a counter problem.
				if err := d.UpsertTags(ctx, coll, id, tags); err != nil && !isFKViolation(err) {
					errs <- fmt.Errorf("upsert: %w", err)
					return
				}
				if rng.Intn(10) == 0 {
					if _, err := d.DeleteObject(ctx, id); err != nil && !isNotFound(err) {
						errs <- fmt.Errorf("delete: %w", err)
						return
					}
				}
			}
		}(int64(w))
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	assertConsistent(t, pool, coll)
}

// isFKViolation reports that the object was deleted by another worker.
func isFKViolation(err error) bool {
	return err != nil && strings.Contains(err.Error(), "object not found")
}

func isNotFound(err error) bool {
	return err != nil && strings.Contains(err.Error(), "not found")
}

func TestCollectionTagStatsPaging(t *testing.T) {
	d, _ := newTestDB(t)
	ctx := context.Background()
	coll := mustCollection(t, d, "jobs")
	o := mustObject(t, d, coll, "h1", nil)
	mustTag(t, d, coll, o, map[string]bool{
		"lang:go": true, "lang:rust": false, "lang:zig": true, "team_a": true, "team%b": false,
	})

	_, page1, err := d.GetCollectionTagStats(ctx, coll, "", "", 2)
	if err != nil || len(page1) != 2 || page1[0].Tag != "lang:go" || page1[1].Tag != "lang:rust" {
		t.Fatalf("page1 = %+v, err %v", page1, err)
	}
	_, page2, _ := d.GetCollectionTagStats(ctx, coll, "", page1[1].Tag, 2)
	if len(page2) != 2 || page2[0].Tag != "lang:zig" || page2[1].Tag != "team%b" {
		t.Fatalf("page2 = %+v", page2)
	}

	_, byPrefix, _ := d.GetCollectionTagStats(ctx, coll, "lang:", "", 100)
	if len(byPrefix) != 3 {
		t.Errorf("prefix lang: matched %d tags, want 3", len(byPrefix))
	}
	// LIKE wildcards in the prefix are literal.
	_, wild, _ := d.GetCollectionTagStats(ctx, coll, "team%", "", 100)
	if len(wild) != 1 || wild[0].Tag != "team%b" {
		t.Errorf("prefix team%% = %+v, want only team%%b", wild)
	}
	_, wild, _ = d.GetCollectionTagStats(ctx, coll, "team_", "", 100)
	if len(wild) != 1 || wild[0].Tag != "team_a" {
		t.Errorf("prefix team_ = %+v, want only team_a", wild)
	}
}
