package db_test

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"mrsydar/tagona/storage/internal/db"
	"mrsydar/tagona/storage/internal/dbtest"
	"mrsydar/tagona/storage/internal/models"
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
	c, err := d.CreateCollection(context.Background(), name, "grep")
	if err != nil {
		t.Fatalf("create collection: %v", err)
	}
	return c.ID
}

func mustObject(t *testing.T, d *db.DB, collID, hash string, expiresAt *time.Time) string {
	t.Helper()
	o, err := d.InsertObject(context.Background(), collID, hash, time.Now(), 1, "key/"+hash, nil, expiresAt)
	if err != nil {
		t.Fatalf("insert object: %v", err)
	}
	return o.ID
}

func mustTag(t *testing.T, d *db.DB, collID, objID string, tags map[string]bool) {
	t.Helper()
	if err := d.UpsertTags(context.Background(), collID, objID, models.Forced(tags)); err != nil {
		t.Fatalf("upsert tags: %v", err)
	}
}

// counts is how many objects score at least 0.5 (t) and below it (f); the tests that only force tags, or
// store scores far from the middle, read the histogram through it.
type counts struct{ t, f int64 }

func countsOf(histogram []int64) counts {
	var c counts
	for b, n := range histogram {
		if b >= 11 { // the bins from 0.5 up, and the bucket of exactly 1
			c.t += n
		} else {
			c.f += n
		}
	}
	return c
}

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
		got[st.Tag] = countsOf(st.Histogram)
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

	truth := map[string]map[int]int64{}
	rows, err := pool.Query(ctx,
		`SELECT tag, tagona_score_bucket(score), count(*)
		 FROM object_tags WHERE collection_id = $1 GROUP BY tag, tagona_score_bucket(score)`, collID)
	if err != nil {
		t.Fatalf("recompute: %v", err)
	}
	for rows.Next() {
		var tag string
		var bucket int
		var n int64
		rows.Scan(&tag, &bucket, &n)
		if truth[tag] == nil {
			truth[tag] = map[int]int64{}
		}
		truth[tag][bucket] = n
	}
	rows.Close()

	registered := map[string]map[int]int64{}
	rows, err = pool.Query(ctx,
		`SELECT ct.tag, b.bucket, b.count FROM collection_tags ct
		 LEFT JOIN collection_tag_buckets b ON b.collection_id = ct.collection_id AND b.tag = ct.tag
		 WHERE ct.collection_id = $1`, collID)
	if err != nil {
		t.Fatalf("read registry: %v", err)
	}
	for rows.Next() {
		var tag string
		var bucket *int
		var n *int64
		rows.Scan(&tag, &bucket, &n)
		if registered[tag] == nil {
			registered[tag] = map[int]int64{}
		}
		if bucket != nil && *n != 0 { // a bucket that was emptied stays as a row with 0
			registered[tag][*bucket] = *n
		}
	}
	rows.Close()

	for tag, want := range truth {
		got, ok := registered[tag]
		if !ok || fmt.Sprint(got) != fmt.Sprint(want) {
			t.Errorf("tag %q: buckets %v (present=%v), actual %v", tag, got, ok, want)
		}
	}
	for tag, got := range registered {
		if _, ok := truth[tag]; !ok && len(got) != 0 {
			t.Errorf("tag %q: buckets %v but no object_tags rows", tag, got)
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
	d, pool := newTestDB(t, "000001_initial_schema.up.sql")
	coll, mustObjectRaw := preStatsFixtures(t, pool)
	o1 := mustObjectRaw("h1")
	o2 := mustObjectRaw("h2")
	// Seed with raw SQL: before migration 000003 there is no collection_tags
	// table, so the regular UpsertTags path cannot be used yet.
	for _, row := range []struct {
		obj, tag string
		score    float64
	}{{o1, "golang", 0.9}, {o1, "qa", 0.1}, {o2, "golang", 0.4}} {
		if _, err := pool.Exec(context.Background(),
			`INSERT INTO object_tags (object_id, collection_id, tag, score) VALUES ($1, $2, $3, $4)`,
			row.obj, coll, row.tag, row.score); err != nil {
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
				if err := d.UpsertTags(ctx, coll, id, models.Forced(tags)); err != nil && !isFKViolation(err) {
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

// preStatsFixtures creates a collection and returns its id and a function that adds objects to it, with
// raw SQL, which works before migration 000003 has created the statistics tables.
func preStatsFixtures(t *testing.T, pool *pgxpool.Pool) (string, func(hash string) string) {
	t.Helper()
	ctx := context.Background()
	var coll string
	if err := pool.QueryRow(ctx, `INSERT INTO collections (name, tagger_version) VALUES ('jobs', 'grep') RETURNING id`).Scan(&coll); err != nil {
		t.Fatalf("seed collection: %v", err)
	}
	return coll, func(hash string) string {
		var id string
		if err := pool.QueryRow(ctx,
			`INSERT INTO objects (collection_id, content_hash, size_bytes, payload_key) VALUES ($1, $2, 1, $3) RETURNING id`,
			coll, hash, "key/"+hash).Scan(&id); err != nil {
			t.Fatalf("seed object: %v", err)
		}
		return id
	}
}

func TestForcedTagsKeepCountersAndBeatEvaluation(t *testing.T) {
	d, pool := newTestDB(t)
	ctx := context.Background()
	coll := mustCollection(t, d, "jobs")
	o1 := mustObject(t, d, coll, "h1", nil)
	o2 := mustObject(t, d, coll, "h2", nil)

	// add, flip and delete in one change
	mustTag(t, d, coll, o1, map[string]bool{"golang": true, "qa": true})
	mustTag(t, d, coll, o2, map[string]bool{"golang": true})
	if err := d.ChangeTags(ctx, coll, o1, models.Forced(map[string]bool{"golang": false, "remote": true}), []string{"qa", "never-there"}); err != nil {
		t.Fatalf("change tags: %v", err)
	}
	known, _ := d.GetTagsForObject(ctx, o1)
	if len(known) != 2 || known["golang"] != 0 || known["remote"] != 1 {
		t.Errorf("tags of o1 = %v", known)
	}
	assertStats(t, d, coll, 2, map[string]counts{
		"golang":      {1, 1},
		"qa":          {0, 0}, // deleted: stays registered, with no objects
		"remote":      {1, 0},
		"never-there": {0, 0},
	})
	assertConsistent(t, pool, coll)

	// a tag both set and removed is set
	if err := d.ChangeTags(ctx, coll, o2, models.Forced(map[string]bool{"golang": false}), []string{"golang"}); err != nil {
		t.Fatalf("change tags: %v", err)
	}
	if known, _ := d.GetTagsForObject(ctx, o2); len(known) != 1 || known["golang"] != 0 {
		t.Errorf("tags of o2 = %v", known)
	}
	assertConsistent(t, pool, coll)

	// an evaluator only fills what is unknown: it never overwrites a forced value
	if err := d.FillTags(ctx, coll, o1, models.Forced(map[string]bool{"golang": true, "qa": true})); err != nil {
		t.Fatalf("fill tags: %v", err)
	}
	known, _ = d.GetTagsForObject(ctx, o1)
	if known["golang"] != 0 || known["qa"] != 1 || known["remote"] != 1 {
		t.Errorf("tags of o1 after evaluation = %v", known)
	}
	assertStats(t, d, coll, 2, map[string]counts{"golang": {0, 2}, "qa": {1, 0}, "remote": {1, 0}})
	assertConsistent(t, pool, coll)

	// an unknown object is reported as such
	if err := d.ChangeTags(ctx, coll, "00000000-0000-0000-0000-000000000000", map[string]float64{"a": 1}, nil); !errors.Is(err, db.ErrObjectNotFound) {
		t.Errorf("unknown object: err = %v", err)
	}
}

// The scores are kept as a distribution: the database's bucket function agrees with models.ScoreBucket, and the
// stats of a tag carry its histogram, the number of evaluated objects and the percentiles.
func TestCollectionStatsHistogram(t *testing.T) {
	d, pool := newTestDB(t)
	ctx := context.Background()
	coll := mustCollection(t, d, "jobs")

	for _, score := range []float64{0, 1e-9, 0.04999, 0.05, 0.5, 0.7, 0.95, 0.999999, 1} {
		var got int
		if err := pool.QueryRow(ctx, `SELECT tagona_score_bucket($1::float8)`, score).Scan(&got); err != nil {
			t.Fatal(err)
		}
		if want := models.ScoreBucket(score); got != want {
			t.Errorf("tagona_score_bucket(%v) = %d, Go says %d", score, got, want)
		}
	}

	scores := []float64{0, 0.12, 0.12, 0.55, 0.93, 1}
	var ids []string
	for i := range scores {
		ids = append(ids, mustObject(t, d, coll, fmt.Sprintf("h%d", i), nil))
	}
	for i, id := range ids {
		if err := d.FillTags(ctx, coll, id, map[string]float64{"golang": scores[i]}); err != nil {
			t.Fatal(err)
		}
	}
	assertConsistent(t, pool, coll)

	stat := func() models.TagStat {
		t.Helper()
		_, stats, err := d.GetCollectionTagStats(ctx, coll, "", "", 10)
		if err != nil || len(stats) != 1 {
			t.Fatalf("stats = %v, %v", stats, err)
		}
		return stats[0]
	}
	st := stat()
	want := make([]int64, models.HistogramBuckets)
	want[0], want[3], want[12], want[19], want[21] = 1, 2, 1, 1, 1 // 0; 0.12 twice; 0.55; 0.93; 1
	if st.Evaluated != 6 || fmt.Sprint(st.Histogram) != fmt.Sprint(want) {
		t.Fatalf("evaluated %d, histogram %v, want 6 and %v", st.Evaluated, st.Histogram, want)
	}
	if st.P50 == nil || *st.P50 <= 0.1 || *st.P50 >= 0.2 {
		t.Errorf("median = %v, want a value in the bin of 0.12", st.P50)
	}

	// a forced value moves an object from one bucket to another, and a deleted tag leaves the distribution
	if err := d.ChangeTags(ctx, coll, ids[1], map[string]float64{"golang": 1}, nil); err != nil {
		t.Fatal(err)
	}
	if err := d.ChangeTags(ctx, coll, ids[0], nil, []string{"golang"}); err != nil {
		t.Fatal(err)
	}
	assertConsistent(t, pool, coll)
	st = stat()
	if st.Evaluated != 5 || st.Histogram[0] != 0 || st.Histogram[3] != 1 || st.Histogram[21] != 2 {
		t.Errorf("after the changes: evaluated %d, histogram %v", st.Evaluated, st.Histogram)
	}
}
