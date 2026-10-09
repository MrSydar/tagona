package client

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

type slowTagger struct {
	inflight, maxSeen atomic.Int32
	delay             time.Duration
}

func (s *slowTagger) Versions(context.Context) ([]string, error) { return []string{"grep"}, nil }
func (s *slowTagger) Tag(ctx context.Context, _, _, _ string, _ []string) (map[string]bool, error) {
	now := s.inflight.Add(1)
	defer s.inflight.Add(-1)
	for {
		seen := s.maxSeen.Load()
		if now <= seen || s.maxSeen.CompareAndSwap(seen, now) {
			break
		}
	}
	select {
	case <-time.After(s.delay):
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return map[string]bool{"a": true}, nil
}

func TestLimitedTaggerKeepsTheCallsInFlightUnderTheLimit(t *testing.T) {
	inner := &slowTagger{delay: 15 * time.Millisecond}
	limited := NewLimitedTagger(inner, 3)
	done := make(chan error, 20)
	for i := 0; i < 20; i++ {
		go func() {
			_, err := limited.Tag(context.Background(), "c", "o", "grep", []string{"a"})
			done <- err
		}()
	}
	for i := 0; i < 20; i++ {
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}
	if got := inner.maxSeen.Load(); got != 3 {
		t.Fatalf("%d calls were in flight at the most, want exactly the limit of 3", got)
	}
}

func TestLimitedTaggerWaitingCallsGiveUpWithTheirContext(t *testing.T) {
	inner := &slowTagger{delay: time.Second}
	limited := NewLimitedTagger(inner, 1)
	go limited.Tag(context.Background(), "c", "o", "grep", nil) // takes the only slot
	time.Sleep(20 * time.Millisecond)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if _, err := limited.Tag(ctx, "c", "o", "grep", nil); err != context.DeadlineExceeded {
		t.Fatalf("err = %v", err)
	}
	if v, err := limited.Versions(context.Background()); err != nil || len(v) != 1 {
		t.Fatalf("Versions must not wait for a slot: %v, %v", v, err)
	}
	if limited := NewLimitedTagger(inner, 0); cap(limited.slots) != 1 {
		t.Fatal("a limit below 1 must still allow one call")
	}
}
