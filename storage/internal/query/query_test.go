package query_test

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"mrsydar/tagona/storage/internal/db"
	"mrsydar/tagona/storage/internal/dbtest"
	"mrsydar/tagona/storage/internal/models"
	"mrsydar/tagona/storage/internal/query"
	"mrsydar/tagona/storage/pkg/client"
)

// fakeTagger counts calls and evaluates every requested tag as true. It serves version "grep": a request that
// asks for another is refused, like the real engine does.
type fakeTagger struct {
	calls    atomic.Int32
	lastSeen atomic.Value // the tagger version of the last request
}

func (f *fakeTagger) Versions(ctx context.Context) ([]string, error) { return []string{"grep"}, nil }

func (f *fakeTagger) Tag(ctx context.Context, collection, objectID, taggerVersion string, tags []string) (map[string]bool, error) {
	f.calls.Add(1)
	f.lastSeen.Store(taggerVersion)
	if taggerVersion != "" && taggerVersion != "grep" {
		return nil, &client.VersionMismatchError{Expected: taggerVersion, Running: []string{"grep"}}
	}
	out := make(map[string]bool, len(tags))
	for _, tag := range tags {
		out[tag] = true
	}
	return out, nil
}

func ptr[T any](v T) *T { return &v }

type fixture struct {
	runner *query.Runner
	tagger *fakeTagger
	coll   *models.Collection
	ids    []string // newest first: [tagged true, tagged false, untagged]
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	d, _ := dbtest.New(t)
	ctx := context.Background()
	coll, err := d.CreateCollection(ctx, "jobs", "grep")
	if err != nil {
		t.Fatalf("create collection: %v", err)
	}
	base := time.Now().UTC().Truncate(time.Second)
	f := &fixture{tagger: &fakeTagger{}, coll: coll}
	f.runner = query.NewRunner(d, f.tagger)
	for i, h := range []string{"a", "b", "c"} {
		o, err := d.InsertObject(ctx, coll.ID, h, base.Add(-time.Duration(i)*time.Hour), 1, "key/"+h, nil, nil)
		if err != nil {
			t.Fatalf("insert object: %v", err)
		}
		f.ids = append(f.ids, o.ID)
	}
	mustUpsert(t, d, coll.ID, f.ids[0], map[string]bool{"golang": true})
	mustUpsert(t, d, coll.ID, f.ids[1], map[string]bool{"golang": false})
	return f
}

func mustUpsert(t *testing.T, d *db.DB, collID, objID string, tags map[string]bool) {
	t.Helper()
	if err := d.UpsertTags(context.Background(), collID, objID, tags); err != nil {
		t.Fatalf("upsert tags: %v", err)
	}
}

func ids(objs []models.Object) []string {
	out := make([]string, len(objs))
	for i, o := range objs {
		out[i] = o.ID
	}
	return out
}

func same(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestEvaluateFalseNeverCallsTagger(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	resp, err := f.runner.Query(ctx, f.coll, models.TagsQueryRequest{
		Tags: map[string]bool{"golang": true}, Limit: 10, Evaluate: ptr(false),
	})
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	// Only the object already known true matches; the untagged one is not
	// returned because nothing confirms it.
	if !same(ids(resp.Objects), f.ids[0:1]) {
		t.Errorf("results = %v, want %v", ids(resp.Objects), f.ids[0:1])
	}
	if got := resp.Objects[0].Tags; len(got) != 1 || !got["golang"] {
		t.Errorf("result tags = %v, want the requested filter", got)
	}
	if n := f.tagger.calls.Load(); n != 0 {
		t.Errorf("tagger was called %d times with evaluate=false", n)
	}

	// Known false is matched by a false filter; unknown still is not.
	resp, err = f.runner.Query(ctx, f.coll, models.TagsQueryRequest{
		Tags: map[string]bool{"golang": false}, Limit: 10, Evaluate: ptr(false),
	})
	if err != nil || !same(ids(resp.Objects), f.ids[1:2]) {
		t.Errorf("golang=false: results %v err %v, want %v", ids(resp.Objects), err, f.ids[1:2])
	}

	// A tag nobody has evaluated yet matches nothing.
	resp, err = f.runner.Query(ctx, f.coll, models.TagsQueryRequest{
		Tags: map[string]bool{"rust": true}, Limit: 10, Evaluate: ptr(false),
	})
	if err != nil || len(resp.Objects) != 0 {
		t.Errorf("rust: results %v err %v, want none", ids(resp.Objects), err)
	}
	if resp != nil && resp.Objects == nil {
		t.Errorf("empty result must be an empty list (JSON []), not nil (JSON null)")
	}

	// No tags: every object, by date, still without the tagger.
	resp, err = f.runner.Query(ctx, f.coll, models.TagsQueryRequest{Limit: 10, Evaluate: ptr(false)})
	if err != nil || !same(ids(resp.Objects), f.ids) {
		t.Errorf("no tags: results %v err %v, want %v", ids(resp.Objects), err, f.ids)
	}
	if n := f.tagger.calls.Load(); n != 0 {
		t.Errorf("tagger was called %d times with evaluate=false", n)
	}
}

func TestEvaluateFalsePagination(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	page1, err := f.runner.Query(ctx, f.coll, models.TagsQueryRequest{Limit: 2, Evaluate: ptr(false)})
	if err != nil {
		t.Fatalf("page 1: %v", err)
	}
	if !same(ids(page1.Objects), f.ids[0:2]) || page1.Next == "" {
		t.Fatalf("page 1: %v next=%q, want %v and a cursor", ids(page1.Objects), page1.Next, f.ids[0:2])
	}
	page2, err := f.runner.Query(ctx, f.coll, models.TagsQueryRequest{Limit: 2, Cursor: page1.Next, Evaluate: ptr(false)})
	if err != nil {
		t.Fatalf("page 2: %v", err)
	}
	if !same(ids(page2.Objects), f.ids[2:3]) || page2.Next != "" {
		t.Fatalf("page 2: %v next=%q, want %v and no cursor", ids(page2.Objects), page2.Next, f.ids[2:3])
	}

	// With a tag filter and a date filter on a later page.
	filter := &models.DateFilter{GTE: ptr(time.Now().Add(-90 * time.Minute))}
	resp, err := f.runner.Query(ctx, f.coll, models.TagsQueryRequest{
		Tags: map[string]bool{"golang": false}, Date: filter, Limit: 1, Evaluate: ptr(false),
	})
	if err != nil {
		t.Fatalf("filtered query: %v", err)
	}
	if !same(ids(resp.Objects), f.ids[1:2]) {
		t.Errorf("filtered: %v, want %v", ids(resp.Objects), f.ids[1:2])
	}
}

// The default (evaluate omitted or true) keeps the original behavior: missing
// tags are evaluated by the tagger and then stored.
func TestEvaluateDefaultStillCallsTagger(t *testing.T) {
	for name, evaluate := range map[string]*bool{"omitted": nil, "true": ptr(true)} {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			ctx := context.Background()

			resp, err := f.runner.Query(ctx, f.coll, models.TagsQueryRequest{
				Tags: map[string]bool{"golang": true}, Limit: 10, Evaluate: evaluate,
			})
			if err != nil {
				t.Fatalf("query: %v", err)
			}
			// Known true + the untagged one, which the fake evaluates to true.
			if !same(ids(resp.Objects), []string{f.ids[0], f.ids[2]}) {
				t.Errorf("results = %v, want %v", ids(resp.Objects), []string{f.ids[0], f.ids[2]})
			}
			if n := f.tagger.calls.Load(); n != 1 {
				t.Errorf("tagger calls = %d, want 1 (only the untagged object)", n)
			}

			// The evaluation was stored, so an evaluate=false query now sees it.
			resp, err = f.runner.Query(ctx, f.coll, models.TagsQueryRequest{
				Tags: map[string]bool{"golang": true}, Limit: 10, Evaluate: ptr(false),
			})
			if err != nil || !same(ids(resp.Objects), []string{f.ids[0], f.ids[2]}) {
				t.Errorf("after evaluation, known-only results = %v err %v", ids(resp.Objects), err)
			}
		})
	}
}

// The tagger is asked for the version the collection is tagged with, and a tagger that runs another version
// is not allowed to answer for it.
func TestEvaluationUsesTheCollectionsTaggerVersion(t *testing.T) {
	f := newFixture(t) // the collection is tagged with "grep", which is what the fake tagger runs
	ctx := context.Background()

	if _, err := f.runner.Query(ctx, f.coll, models.TagsQueryRequest{Tags: map[string]bool{"rust": true}, Limit: 10}); err != nil {
		t.Fatalf("query: %v", err)
	}
	if got := f.tagger.lastSeen.Load(); got != "grep" {
		t.Fatalf("the tagger was asked for version %v", got)
	}

	other := *f.coll
	other.TaggerVersion = "decisions/openai:m"
	f.tagger.calls.Store(0)
	_, err := f.runner.Query(ctx, &other, models.TagsQueryRequest{Tags: map[string]bool{"java": true}, Limit: 10}) // not known yet
	var mismatch *client.VersionMismatchError
	if !errors.As(err, &mismatch) || mismatch.Expected != "decisions/openai:m" {
		t.Fatalf("err = %v", err)
	}

	// what is already known still answers, and nothing reaches the tagger
	f.tagger.calls.Store(0)
	resp, err := f.runner.Query(ctx, &other, models.TagsQueryRequest{Tags: map[string]bool{"golang": true}, Limit: 10, Evaluate: ptr(false)})
	if err != nil || !same(ids(resp.Objects), f.ids[0:1]) || f.tagger.calls.Load() != 0 {
		t.Fatalf("evaluate=false under a mismatch: %v, %v, %d tagger calls", ids(resp.Objects), err, f.tagger.calls.Load())
	}
}
