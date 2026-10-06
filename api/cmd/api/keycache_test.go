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
	calls atomic.Int32
	valid map[string]bool
	err   error
}

func (c *countingValidator) ValidateKey(ctx context.Context, key string) (bool, error) {
	c.calls.Add(1)
	if c.err != nil {
		return false, c.err
	}
	return c.valid[key], nil
}

func TestCachedValidator(t *testing.T) {
	now := time.Unix(1000, 0)
	next := &countingValidator{valid: map[string]bool{"good": true}}
	c := newCachedValidator(next, 30*time.Second)
	c.now = func() time.Time { return now }
	ctx := context.Background()

	for i := 0; i < 3; i++ {
		if ok, err := c.ValidateKey(ctx, "good"); err != nil || !ok {
			t.Fatalf("good key: ok=%v err=%v", ok, err)
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
