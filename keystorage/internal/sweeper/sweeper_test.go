package sweeper

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

type fakeStore struct {
	calls     atomic.Int32
	retention atomic.Int64
	err       error
}

func (f *fakeStore) DeleteExpired(_ context.Context, retention time.Duration) (int64, error) {
	f.calls.Add(1)
	f.retention.Store(int64(retention))
	return 1, f.err
}

func TestRunSweepsImmediatelyAndPeriodicallyUntilCancelled(t *testing.T) {
	store := &fakeStore{}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { Run(ctx, store, 90*time.Minute, 10*time.Millisecond); close(done) }()

	deadline := time.After(2 * time.Second)
	for store.calls.Load() < 3 {
		select {
		case <-deadline:
			t.Fatalf("only %d sweeps: it must sweep at start and then every interval", store.calls.Load())
		case <-time.After(5 * time.Millisecond):
		}
	}
	if got := time.Duration(store.retention.Load()); got != 90*time.Minute {
		t.Fatalf("retention passed on = %v", got)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Run did not stop when its context was cancelled")
	}
}

func TestRunSurvivesStoreErrors(t *testing.T) {
	store := &fakeStore{err: errors.New("db down")}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go Run(ctx, store, 0, 5*time.Millisecond)
	deadline := time.After(2 * time.Second)
	for store.calls.Load() < 3 {
		select {
		case <-deadline:
			t.Fatal("a failing sweep must not stop the sweeper")
		case <-time.After(5 * time.Millisecond):
		}
	}
}
