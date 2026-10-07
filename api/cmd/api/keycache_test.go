package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"mrsydar/tagona/api/internal/keystorageapi"
)

type countingValidator struct {
	calls   atomic.Int32
	results map[string]keystorageapi.Result
	err     error
}

func (c *countingValidator) ValidateKey(ctx context.Context, key string) (keystorageapi.Result, error) {
	c.calls.Add(1)
	if c.err != nil {
		return keystorageapi.Result{}, c.err
	}
	return c.results[key], nil // an absent key is Unknown
}

func valid() keystorageapi.Result { return keystorageapi.Result{Verdict: keystorageapi.Valid} }

func TestCachedValidator(t *testing.T) {
	now := time.Unix(1000, 0)
	next := &countingValidator{results: map[string]keystorageapi.Result{"good": valid()}}
	c := newCachedValidator(next, 30*time.Second)
	c.now = func() time.Time { return now }
	ctx := context.Background()

	for i := 0; i < 3; i++ {
		if res, err := c.ValidateKey(ctx, "good"); err != nil || res.Verdict != keystorageapi.Valid {
			t.Fatalf("good key: %+v err=%v", res, err)
		}
	}
	if got := next.calls.Load(); got != 1 {
		t.Errorf("expected 1 upstream call for repeated valid key, got %d", got)
	}

	now = now.Add(31 * time.Second)
	c.ValidateKey(ctx, "good")
	if got := next.calls.Load(); got != 2 {
		t.Errorf("expected re-validation after TTL, got %d calls", got)
	}

	c.ValidateKey(ctx, "bad")
	c.ValidateKey(ctx, "bad")
	if got := next.calls.Load(); got != 3 {
		t.Errorf("expected unknown key to be negatively cached, got %d calls", got)
	}
	now = now.Add(negativeKeyTTL + time.Second)
	c.ValidateKey(ctx, "bad")
	if got := next.calls.Load(); got != 4 {
		t.Errorf("negative entry should expire after %s, got %d calls", negativeKeyTTL, got)
	}

	c.Purge()
	c.ValidateKey(ctx, "good")
	if got := next.calls.Load(); got != 5 {
		t.Errorf("expected upstream call after Purge, got %d", got)
	}
}

// A cached "valid" must not outlive the key: expiry takes effect when it happens, not when the cache
// entry would have run out.
func TestCachedValidatorStopsAtTheKeysExpiry(t *testing.T) {
	now := time.Unix(1000, 0)
	expires := now.Add(3 * time.Second)
	next := &countingValidator{results: map[string]keystorageapi.Result{
		"short": {Verdict: keystorageapi.Valid, ExpiresAt: &expires},
		"long":  {Verdict: keystorageapi.Valid, ExpiresAt: ptrTime(now.Add(time.Hour))},
	}}
	c := newCachedValidator(next, 30*time.Second)
	c.now = func() time.Time { return now }
	ctx := context.Background()

	c.ValidateKey(ctx, "short")
	now = now.Add(2 * time.Second)
	c.ValidateKey(ctx, "short")
	if got := next.calls.Load(); got != 1 {
		t.Fatalf("still cached before the expiry: %d calls", got)
	}

	// The key expires at 3s. Keystorage now says so, and the cache must ask again.
	now = now.Add(time.Second)
	next.results["short"] = keystorageapi.Result{Verdict: keystorageapi.Expired}
	res, _ := c.ValidateKey(ctx, "short")
	if res.Verdict != keystorageapi.Expired || next.calls.Load() != 2 {
		t.Fatalf("at the expiry the cached verdict must be gone: %+v after %d calls", res, next.calls.Load())
	}

	// A key that lives longer than the cache is cached for the cache's ttl only.
	c.ValidateKey(ctx, "long")
	now = now.Add(31 * time.Second)
	c.ValidateKey(ctx, "long")
	if next.calls.Load() != 4 {
		t.Fatalf("a long-lived key is revalidated after the ttl: %d calls", next.calls.Load())
	}

	// A verdict whose expiry has already passed is not cached at all.
	past := now.Add(-time.Second)
	next.results["stale"] = keystorageapi.Result{Verdict: keystorageapi.Valid, ExpiresAt: &past}
	before := next.calls.Load()
	c.ValidateKey(ctx, "stale")
	c.ValidateKey(ctx, "stale")
	if next.calls.Load() != before+2 {
		t.Fatalf("a verdict past its expiry must not be cached")
	}
}

func ptrTime(t time.Time) *time.Time { return &t }

func TestCachedValidatorRemembersExpiredKeys(t *testing.T) {
	now := time.Unix(1000, 0)
	next := &countingValidator{results: map[string]keystorageapi.Result{"old": {Verdict: keystorageapi.Expired}}}
	c := newCachedValidator(next, 30*time.Second)
	c.now = func() time.Time { return now }
	for i := 0; i < 3; i++ {
		if res, _ := c.ValidateKey(context.Background(), "old"); res.Verdict != keystorageapi.Expired {
			t.Fatalf("verdict %+v", res)
		}
	}
	if next.calls.Load() != 1 {
		t.Fatalf("an expired key cannot come back, so the verdict is cached: %d calls", next.calls.Load())
	}
}

func TestCachedValidatorDoesNotCacheErrors(t *testing.T) {
	next := &countingValidator{err: errors.New("storage down")}
	c := newCachedValidator(next, time.Minute)
	for i := 0; i < 2; i++ {
		if _, err := c.ValidateKey(context.Background(), "k"); err == nil {
			t.Fatal("expected error")
		}
	}
	if got := next.calls.Load(); got != 2 {
		t.Errorf("errors must not be cached, got %d calls", got)
	}
}

func TestRouterRevocation(t *testing.T) {
	validKey := "tagona_" + "ab"
	backend := newStorageStub(t, validKey)
	defer backend.Close()
	cache := newCachedValidator(keystorageapi.New(backend.URL), time.Minute)
	router := newTestRouter(t, backend.URL, gatewayConfig{
		validator:    cache,
		onKeyDeleted: cache.Purge,
	})
	get := func() *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		router.ServeHTTP(w, newRequest(t, http.MethodGet, "/v1/collections", func(r *http.Request) {
			r.Header.Set("Authorization", "Bearer "+validKey)
		}))
		return w
	}

	if w := get(); w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	// Deleting a key through the gateway purges the validation cache.
	cache.mu.Lock()
	n := len(cache.entries)
	cache.mu.Unlock()
	if n == 0 {
		t.Fatal("expected a cached verdict")
	}
	del := httptest.NewRecorder()
	req := newRequest(t, http.MethodDelete, "/v1/admin/api-keys/"+knownKeyID, func(r *http.Request) { r.SetBasicAuth("admin", "tagona") })
	router.ServeHTTP(del, req)
	if del.Code != http.StatusNoContent {
		t.Fatalf("delete: expected 204, got %d: %s", del.Code, del.Body.String())
	}
	cache.mu.Lock()
	n = len(cache.entries)
	cache.mu.Unlock()
	if n != 0 {
		t.Errorf("expected cache purged after delete, have %d entries", n)
	}
}
