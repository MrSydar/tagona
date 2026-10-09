package query_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
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

	// for the tests of concurrency: how long an answer takes, what it is, and how many ran at once
	delay    time.Duration
	answer   func(objectID string, tags []string) (map[string]bool, error)
	inflight atomic.Int32
	maxSeen  atomic.Int32
	mu       sync.Mutex
	asked    []string // object ids, in the order the calls started
}

func (f *fakeTagger) Versions(ctx context.Context) ([]string, error) { return []string{"grep"}, nil }

func (f *fakeTagger) Tag(ctx context.Context, collection, objectID, taggerVersion string, tags []string) (map[string]bool, error) {
	f.calls.Add(1)
	f.lastSeen.Store(taggerVersion)
	f.mu.Lock()
	f.asked = append(f.asked, objectID)
	f.mu.Unlock()
	now := f.inflight.Add(1)
	defer f.inflight.Add(-1)
	for {
		seen := f.maxSeen.Load()
		if now <= seen || f.maxSeen.CompareAndSwap(seen, now) {
			break
		}
	}
	if f.delay > 0 {
		select {
		case <-time.After(f.delay):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if taggerVersion != "" && taggerVersion != "grep" {
		return nil, &client.VersionMismatchError{Expected: taggerVersion, Running: []string{"grep"}}
	}
	if f.answer != nil {
		return f.answer(objectID, tags)
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
	f.runner = query.NewRunner(d, f.tagger, 1)
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

// ---- concurrency: the tagger is asked about several objects at once, and the answer is the sequential one

// bulk is a collection of n untagged objects, newest first; ids[i] has date base - i hours.
type bulk struct {
	d    *db.DB
	coll *models.Collection
	ids  []string
	idx  map[string]int
}

func newBulk(t *testing.T, n int) *bulk {
	t.Helper()
	d, _ := dbtest.New(t)
	ctx := context.Background()
	coll, err := d.CreateCollection(ctx, "bulk", "grep")
	if err != nil {
		t.Fatal(err)
	}
	b := &bulk{d: d, coll: coll, idx: map[string]int{}}
	base := time.Now().UTC().Truncate(time.Second)
	for i := 0; i < n; i++ {
		h := fmt.Sprintf("h%03d", i)
		o, err := d.InsertObject(ctx, coll.ID, h, base.Add(-time.Duration(i)*time.Hour), 1, "key/"+h, nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		b.ids = append(b.ids, o.ID)
		b.idx[o.ID] = i
	}
	return b
}

// evenGolang answers golang=true for the objects with an even index.
func (b *bulk) evenGolang(objectID string, tags []string) (map[string]bool, error) {
	out := map[string]bool{}
	for _, tag := range tags {
		out[tag] = b.idx[objectID]%2 == 0
	}
	return out, nil
}

func (b *bulk) ask(t *testing.T, concurrency int, tagger *fakeTagger, req models.TagsQueryRequest) (*models.TagsQueryResponse, error) {
	t.Helper()
	return query.NewRunner(b.d, tagger, concurrency).Query(context.Background(), b.coll, req)
}

func TestConcurrentQueryGivesTheSequentialAnswer(t *testing.T) {
	b := newBulk(t, 30)
	req := models.TagsQueryRequest{Tags: map[string]bool{"golang": true}, Limit: 5}

	seq := &fakeTagger{answer: b.evenGolang}
	want, err := b.ask(t, 1, seq, req)
	if err != nil || len(want.Objects) != 5 || want.Next == "" {
		t.Fatalf("sequential: %+v, %v", want, err)
	}
	// start again from nothing known, with eight at a time
	b2 := newBulk(t, 30)
	par := &fakeTagger{answer: b2.evenGolang, delay: 5 * time.Millisecond}
	got, err := b2.ask(t, 8, par, req)
	if err != nil {
		t.Fatal(err)
	}
	for i := range want.Objects { // the same objects, by position, in both collections
		if got, sequential := b2.idx[got.Objects[i].ID], b.idx[want.Objects[i].ID]; got != sequential {
			t.Fatalf("object %d is #%d, sequentially #%d", i, got, sequential)
		}
	}
	if len(got.Objects) != 5 || got.Next == "" {
		t.Fatalf("%d objects, next %q", len(got.Objects), got.Next)
	}
	for i, o := range got.Objects { // the even objects, newest first
		if b2.idx[o.ID] != 2*i {
			t.Fatalf("result %d is object %d, want %d", i, b2.idx[o.ID], 2*i)
		}
	}
}

func TestQueryEvaluatesAtMostConcurrencyObjectsAtOnce(t *testing.T) {
	for _, k := range []int{1, 3} {
		b := newBulk(t, 24)
		tagger := &fakeTagger{answer: b.evenGolang, delay: 20 * time.Millisecond}
		if _, err := b.ask(t, k, tagger, models.TagsQueryRequest{Tags: map[string]bool{"golang": true}, Limit: 10}); err != nil {
			t.Fatal(err)
		}
		if got := int(tagger.maxSeen.Load()); got > k || (k > 1 && got < 2) {
			t.Errorf("concurrency %d: %d calls in flight at the most", k, got)
		}
	}
}

func TestConcurrentQueryIsFaster(t *testing.T) {
	const n, delay = 20, 50 * time.Millisecond
	b := newBulk(t, n)
	all := models.TagsQueryRequest{Tags: map[string]bool{"golang": true}, Limit: 100}
	start := time.Now()
	if _, err := b.ask(t, 5, &fakeTagger{answer: b.evenGolang, delay: delay}, all); err != nil {
		t.Fatal(err)
	}
	took := time.Since(start)
	// 20 evaluations of 50ms: a second one after the other, a quarter of that five at a time
	if took > 600*time.Millisecond {
		t.Fatalf("20 evaluations took %v with 5 at a time", took)
	}
}

// What the query does not need is evaluated only up to the window ahead of the last match.
func TestConcurrentQueryEvaluatesLittleMoreThanItNeeds(t *testing.T) {
	const k = 4
	b := newBulk(t, 60)
	tagger := &fakeTagger{answer: func(string, []string) (map[string]bool, error) { return map[string]bool{"golang": true}, nil }, delay: 10 * time.Millisecond}
	got, err := b.ask(t, k, tagger, models.TagsQueryRequest{Tags: map[string]bool{"golang": true}, Limit: 5})
	if err != nil || len(got.Objects) != 5 {
		t.Fatalf("%v, %v", got, err)
	}
	// 6 matches are needed (limit+1); at most k-1 more may have been started
	if calls := int(tagger.calls.Load()); calls < 6 || calls > 6+k-1 {
		t.Fatalf("%d evaluations for 6 needed objects with a window of %d", calls, k)
	}
}

// An evaluation that finished is kept even when the query did not need it.
func TestFinishedEvaluationsAreStoredEvenIfUnused(t *testing.T) {
	b := newBulk(t, 40)
	tagger := &fakeTagger{answer: func(string, []string) (map[string]bool, error) { return map[string]bool{"golang": true}, nil }}
	if _, err := b.ask(t, 6, tagger, models.TagsQueryRequest{Tags: map[string]bool{"golang": true}, Limit: 2}); err != nil {
		t.Fatal(err)
	}
	asked := int(tagger.calls.Load())
	known, err := b.d.GetKnownTagsForObjects(context.Background(), b.ids)
	if err != nil {
		t.Fatal(err)
	}
	stored := 0
	for _, tags := range known {
		if _, ok := tags["golang"]; ok {
			stored++
		}
	}
	if stored < 3 || stored > asked {
		t.Fatalf("%d objects have the tag stored, %d were asked about", stored, asked)
	}
}

func TestTaggerErrorFailsTheQueryAndStopsTheRest(t *testing.T) {
	b := newBulk(t, 30)
	boom := errors.New("boom")
	tagger := &fakeTagger{delay: 5 * time.Millisecond, answer: func(id string, tags []string) (map[string]bool, error) {
		if b.idx[id] == 2 {
			return nil, boom
		}
		return map[string]bool{"golang": true}, nil
	}}
	_, err := b.ask(t, 4, tagger, models.TagsQueryRequest{Tags: map[string]bool{"golang": true}, Limit: 20})
	if !errors.Is(err, boom) || !strings.Contains(err.Error(), "tag engine error") {
		t.Fatalf("err = %v", err)
	}
	if calls := tagger.calls.Load(); calls > 8 {
		t.Fatalf("%d evaluations after an error at the third object", calls)
	}
}

func TestVersionMismatchSurfacesFromAConcurrentQuery(t *testing.T) {
	b := newBulk(t, 10)
	other := *b.coll
	other.TaggerVersion = "decisions/openai:m"
	_, err := query.NewRunner(b.d, &fakeTagger{}, 4).Query(context.Background(), &other, models.TagsQueryRequest{Tags: map[string]bool{"golang": true}, Limit: 3})
	var mismatch *client.VersionMismatchError
	if !errors.As(err, &mismatch) {
		t.Fatalf("err = %v", err)
	}
}

// A timeout with best_effort answers with what is resolved, in order, and a cursor after the last resolved
// object: the next page evaluates the object that was in progress, it is not skipped.
func TestBestEffortTimeoutCursorSkipsNothing(t *testing.T) {
	b := newBulk(t, 40)
	tagger := &fakeTagger{answer: func(string, []string) (map[string]bool, error) { return map[string]bool{"golang": true}, nil }, delay: 40 * time.Millisecond}
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	req := models.TagsQueryRequest{Tags: map[string]bool{"golang": true}, Limit: 30, BestEffort: true}
	part, err := query.NewRunner(b.d, tagger, 2).Query(ctx, b.coll, req)
	if err != nil || part.Next == "" || len(part.Objects) == 0 || len(part.Objects) >= 30 {
		t.Fatalf("%v, %v", part, err)
	}
	for i, o := range part.Objects {
		if b.idx[o.ID] != i {
			t.Fatalf("partial result %d is object %d", i, b.idx[o.ID])
		}
	}
	// the rest, from the cursor, starts right after the last returned object
	req.Cursor = part.Next
	rest, err := query.NewRunner(b.d, &fakeTagger{answer: tagger.answer}, 4).Query(context.Background(), b.coll, req)
	if err != nil || len(rest.Objects) == 0 {
		t.Fatalf("%v, %v", rest, err)
	}
	if got, want := b.idx[rest.Objects[0].ID], len(part.Objects); got != want {
		t.Fatalf("the next page starts at object %d, want %d", got, want)
	}

	// without best_effort the same timeout is an error (on objects that nothing has evaluated yet)
	fresh := newBulk(t, 40)
	ctx2, cancel2 := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel2()
	req.BestEffort, req.Cursor = false, ""
	slow := &fakeTagger{answer: tagger.answer, delay: 40 * time.Millisecond}
	if _, err := query.NewRunner(fresh.d, slow, 2).Query(ctx2, fresh.coll, req); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v", err)
	}
}

// Workers that evaluate objects with the same tags at once write the same counters.
func TestConcurrentEvaluationKeepsTheCollectionStatsConsistent(t *testing.T) {
	b := newBulk(t, 60)
	tagger := &fakeTagger{answer: b.evenGolang, delay: time.Millisecond}
	if _, err := b.ask(t, 16, tagger, models.TagsQueryRequest{Tags: map[string]bool{"golang": true, "senior": true}, Limit: 100}); err != nil {
		t.Fatal(err)
	}
	total, stats, err := b.d.GetCollectionTagStats(context.Background(), b.coll.ID, "", "", 10)
	if err != nil || total != 60 {
		t.Fatalf("%d objects, %v", total, err)
	}
	var wantTrue int64
	known, _ := b.d.GetKnownTagsForObjects(context.Background(), b.ids)
	for _, tags := range known {
		if tags["golang"] {
			wantTrue++
		}
	}
	for _, st := range stats {
		if st.Tag == "golang" && (st.TrueCount != wantTrue || st.TrueCount+st.FalseCount != int64(len(known))) {
			t.Fatalf("golang counters %+v, want %d true of %d known", st, wantTrue, len(known))
		}
	}
}
