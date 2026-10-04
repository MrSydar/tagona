package retention

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"mrsydar/tagona/storage/internal/db"
)

// fakeBackend is an in-memory stand-in for the database and the object store.
// It lists expired rows in (ExpiresAt, ID) order with the same keyset semantics
// as db.ListExpiredObjects.
type fakeBackend struct {
	mu          sync.Mutex
	rows        []db.ExpiredObject // sorted by (ExpiresAt, ID)
	failPayload map[string]bool    // payload keys whose Delete fails
	rowErr      map[string]error   // object ids whose DeleteObject fails with this error
	ops         []string           // "payload:<key>" and "row:<id>", in call order
	listCalls   int
	onList      func() // runs at the start of every list call
}

func newBackend(n int) *fakeBackend {
	b := &fakeBackend{failPayload: map[string]bool{}, rowErr: map[string]error{}}
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for i := range n {
		id := fmt.Sprintf("id-%03d", i)
		b.rows = append(b.rows, db.ExpiredObject{ID: id, PayloadKey: "coll/" + id, ExpiresAt: base.Add(time.Duration(i) * time.Second)})
	}
	return b
}

func after(a, b db.ExpiredObject) bool { // a > b in (ExpiresAt, ID) order
	if !a.ExpiresAt.Equal(b.ExpiresAt) {
		return a.ExpiresAt.After(b.ExpiresAt)
	}
	return a.ID > b.ID
}

func (b *fakeBackend) ListExpiredObjects(ctx context.Context, cursor *db.ExpiredObject, limit int) ([]db.ExpiredObject, error) {
	if b.onList != nil {
		b.onList()
	}
	if err := ctx.Err(); err != nil { // like a real query on a cancelled context
		return nil, err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.listCalls++
	var out []db.ExpiredObject
	for _, r := range b.rows {
		if cursor != nil && !after(r, *cursor) {
			continue
		}
		out = append(out, r)
		if len(out) == limit {
			break
		}
	}
	return out, nil
}

func (b *fakeBackend) DeleteObject(ctx context.Context, id string) (string, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.ops = append(b.ops, "row:"+id)
	if err := b.rowErr[id]; err != nil {
		return "", err
	}
	for i, r := range b.rows {
		if r.ID == id {
			b.rows = append(b.rows[:i], b.rows[i+1:]...)
			return r.PayloadKey, nil
		}
	}
	return "", fmt.Errorf("object not found: %w", pgx.ErrNoRows)
}

func (b *fakeBackend) Delete(ctx context.Context, key string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.ops = append(b.ops, "payload:"+key)
	if b.failPayload[key] {
		return errors.New("s3 unavailable")
	}
	return nil
}

func (b *fakeBackend) remaining() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.rows)
}

func TestSweepDrainsTheWholeBacklogInOneRun(t *testing.T) {
	b := newBackend(25)
	s := NewSweeper(b, b, time.Minute, 10)

	res, err := s.sweep(context.Background())
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if res.deleted != 25 || res.failed != 0 {
		t.Errorf("result = %+v, want 25 deleted, 0 failed", res)
	}
	if b.remaining() != 0 {
		t.Errorf("%d objects left; a single run must drain the backlog, not just one batch", b.remaining())
	}
	if b.listCalls != 3 { // 10 + 10 + 5; the short last batch ends the run
		t.Errorf("list calls = %d, want 3", b.listCalls)
	}
}

func TestSweepWithNothingExpiredDoesNothing(t *testing.T) {
	b := newBackend(0)
	res, err := NewSweeper(b, b, time.Minute, 10).sweep(context.Background())
	if err != nil || res.deleted != 0 || res.failed != 0 || len(b.ops) != 0 {
		t.Errorf("res=%+v err=%v ops=%v, want a no-op", res, err, b.ops)
	}
}

func TestSweepBackloggedExactMultipleOfBatchSize(t *testing.T) {
	b := newBackend(20) // exactly two full batches: a third, empty list call must end the run
	res, err := NewSweeper(b, b, time.Minute, 10).sweep(context.Background())
	if err != nil || res.deleted != 20 || b.remaining() != 0 {
		t.Fatalf("res=%+v err=%v remaining=%d", res, err, b.remaining())
	}
	if b.listCalls != 3 {
		t.Errorf("list calls = %d, want 3", b.listCalls)
	}
}

func TestSweepDeletesThePayloadBeforeTheRow(t *testing.T) {
	b := newBackend(2)
	if _, err := NewSweeper(b, b, time.Minute, 10).sweep(context.Background()); err != nil {
		t.Fatal(err)
	}
	want := []string{"payload:coll/id-000", "row:id-000", "payload:coll/id-001", "row:id-001"}
	if strings.Join(b.ops, ",") != strings.Join(want, ",") {
		t.Errorf("ops = %v, want %v", b.ops, want)
	}
}

// A failed payload delete must leave the row so the object is retried; the row
// must not be deleted first, which would orphan the payload forever.
func TestSweepKeepsTheRowWhenThePayloadDeleteFailsAndRetriesNextRun(t *testing.T) {
	b := newBackend(3)
	b.failPayload["coll/id-001"] = true
	s := NewSweeper(b, b, time.Minute, 10)

	res, err := s.sweep(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if res.deleted != 2 || res.failed != 1 {
		t.Errorf("first run = %+v, want 2 deleted, 1 failed", res)
	}
	if b.remaining() != 1 || b.rows[0].ID != "id-001" {
		t.Fatalf("remaining rows = %+v, want only id-001", b.rows)
	}
	for _, op := range b.ops {
		if op == "row:id-001" {
			t.Error("the row of an object whose payload failed to delete must not be touched")
		}
	}

	delete(b.failPayload, "coll/id-001") // S3 recovers
	res, err = s.sweep(context.Background())
	if err != nil || res.deleted != 1 || res.failed != 0 || b.remaining() != 0 {
		t.Errorf("second run = %+v err=%v remaining=%d, want the retry to succeed", res, err, b.remaining())
	}
}

// A whole batch of objects that cannot be removed must not stall the sweep:
// the keyset cursor moves past them so the objects behind them still go.
func TestSweepFailuresDoNotBlockLaterObjects(t *testing.T) {
	b := newBackend(25)
	for i := range 10 { // the first full batch fails
		b.failPayload[fmt.Sprintf("coll/id-%03d", i)] = true
	}
	res, err := NewSweeper(b, b, time.Minute, 10).sweep(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if res.deleted != 15 || res.failed != 10 {
		t.Errorf("result = %+v, want 15 deleted and 10 failed", res)
	}
	if b.remaining() != 10 {
		t.Errorf("remaining = %d, want the 10 failed objects", b.remaining())
	}
}

func TestSweepTreatsAnAlreadyDeletedRowAsDone(t *testing.T) {
	b := newBackend(2)
	b.rowErr["id-000"] = fmt.Errorf("object not found: %w", pgx.ErrNoRows) // removed concurrently
	res, err := NewSweeper(b, b, time.Minute, 10).sweep(context.Background())
	if err != nil || res.failed != 0 || res.deleted != 2 {
		t.Errorf("res=%+v err=%v, want a vanished row to count as removed", res, err)
	}
}

func TestSweepCountsRowDeleteErrorsAsFailures(t *testing.T) {
	b := newBackend(2)
	b.rowErr["id-000"] = errors.New("database unavailable")
	res, err := NewSweeper(b, b, time.Minute, 10).sweep(context.Background())
	if err != nil || res.failed != 1 || res.deleted != 1 {
		t.Errorf("res=%+v err=%v, want 1 failed and 1 deleted", res, err)
	}
}

func TestSweepStopsWhenTheContextIsCancelled(t *testing.T) {
	b := newBackend(50)
	ctx, cancel := context.WithCancel(context.Background())
	b.onList = func() {
		if b.listCalls == 1 { // cancel while the second batch is being requested
			cancel()
		}
	}
	res, err := NewSweeper(b, b, time.Minute, 10).sweep(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if res.deleted != 10 || b.remaining() != 40 {
		t.Errorf("res=%+v remaining=%d, want it to stop after the first batch", res, b.remaining())
	}
}

func TestSweepReportsListErrors(t *testing.T) {
	s := NewSweeper(erroringStore{}, newBackend(0), time.Minute, 10)
	if _, err := s.sweep(context.Background()); err == nil || !strings.Contains(err.Error(), "list expired objects") {
		t.Errorf("err = %v, want a list error", err)
	}
}

type erroringStore struct{}

func (erroringStore) ListExpiredObjects(context.Context, *db.ExpiredObject, int) ([]db.ExpiredObject, error) {
	return nil, errors.New("db down")
}
func (erroringStore) DeleteObject(context.Context, string) (string, error) { return "", nil }

func TestNewSweeperClampsABatchSizeBelowOne(t *testing.T) {
	b := newBackend(3)
	res, err := NewSweeper(b, b, time.Minute, 0).sweep(context.Background())
	if err != nil || res.deleted != 3 {
		t.Errorf("res=%+v err=%v, want all 3 removed with a clamped batch size of 1", res, err)
	}
}

func TestStartSweepsOnEveryTickAndStopsOnCancel(t *testing.T) {
	b := newBackend(30)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { NewSweeper(b, b, 10*time.Millisecond, 7).Start(ctx); close(done) }()

	deadline := time.Now().Add(3 * time.Second)
	for b.remaining() > 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if b.remaining() != 0 {
		t.Errorf("%d objects left after 3s of ticks", b.remaining())
	}
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Start did not return after the context was cancelled")
	}
}
