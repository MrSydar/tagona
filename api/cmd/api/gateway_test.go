package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"

	"mrsydar/tagona/api/internal/keystorageapi"
)

type seenRequest struct {
	method string
	path   string
	header http.Header
	body   string
}

// newRecordingStorage is a storage stub that accepts any key and records the
// requests that reach it beyond key validation.
func newRecordingStorage(t *testing.T) (*httptest.Server, func() []seenRequest) {
	t.Helper()
	var mu sync.Mutex
	var seen []seenRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/internal/v1/api-keys/validate" {
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"id":"key-id","name":"test"}`))
			return
		}
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		seen = append(seen, seenRequest{r.Method, r.URL.EscapedPath(), r.Header.Clone(), string(body)})
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	return srv, func() []seenRequest {
		mu.Lock()
		defer mu.Unlock()
		return append([]seenRequest(nil), seen...)
	}
}

func newGatewayRouter(t *testing.T, backendURL string, cfg gatewayConfig) http.Handler {
	t.Helper()
	return newTestRouter(t, backendURL, cfg)
}

// newTestRouter wires the gateway to a single stub that plays both storage and keystorage.
func newTestRouter(t *testing.T, backendURL string, cfg gatewayConfig) http.Handler {
	t.Helper()
	target, err := url.Parse(backendURL)
	if err != nil {
		t.Fatalf("parse backend url: %v", err)
	}
	if cfg.validator == nil {
		cfg.validator = keystorageapi.New(backendURL)
	}
	return newRouter([]string{backendURL}, newProxy(target, nil), newKeystorageProxy(target, nil, cfg.onKeyDeleted), cfg)
}

func bearer(r *http.Request) { r.Header.Set("Authorization", "Bearer tagona_anykey") }

func TestProxyAllowlist(t *testing.T) {
	backend, seen := newRecordingStorage(t)
	router := newGatewayRouter(t, backend.URL, gatewayConfig{})

	t.Run("allowlisted routes are forwarded", func(t *testing.T) {
		for _, route := range proxiedRoutes {
			path := strings.NewReplacer("{collection}", "jobs", "{id}", "abc").Replace(route.pattern)
			w := httptest.NewRecorder()
			router.ServeHTTP(w, newRequest(t, route.method, path, bearer))
			if w.Code != http.StatusOK {
				t.Errorf("%s %s: expected 200, got %d: %s", route.method, path, w.Code, w.Body.String())
			}
		}
		if got := len(seen()); got != len(proxiedRoutes) {
			t.Errorf("expected %d forwarded requests, got %d", len(proxiedRoutes), got)
		}
	})

	t.Run("unlisted paths are 404 and never forwarded", func(t *testing.T) {
		before := len(seen())
		for _, path := range []string{
			"/v1/debug",
			"/v1/collections/jobs/objects/abc/extra",
			"/internal/v1/api-keys",
			"/internal/v1/api-keys/validate",
			"/v1/internal/v1/api-keys",
		} {
			w := httptest.NewRecorder()
			router.ServeHTTP(w, newRequest(t, http.MethodGet, path, bearer))
			if w.Code != http.StatusNotFound {
				t.Errorf("GET %s: expected 404, got %d", path, w.Code)
			}
		}
		if got := len(seen()); got != before {
			t.Errorf("unlisted paths reached storage (%d new requests)", got-before)
		}
	})

	t.Run("wrong method on a listed path is 405", func(t *testing.T) {
		w := httptest.NewRecorder()
		router.ServeHTTP(w, newRequest(t, http.MethodPut, "/v1/collections", bearer))
		if w.Code != http.StatusMethodNotAllowed {
			t.Fatalf("expected 405, got %d", w.Code)
		}
		if code := decodeErrorCode(t, w.Body.Bytes()); code != "method_not_allowed" {
			t.Errorf("expected method_not_allowed, got %q", code)
		}
	})
}

func TestSafePath(t *testing.T) {
	backend, seen := newRecordingStorage(t)
	router := newGatewayRouter(t, backend.URL, gatewayConfig{})

	for _, path := range []string{
		"/v1/../internal/v1/api-keys",
		"/v1/collections/../objects",
		"/v1/collections/./objects",
		"/v1/%2e%2e/internal/v1/api-keys",
		"/v1/collections/%2E%2E/objects",
		"/v1/collections/a%2fb/objects",
		"/v1/collections/a%5cb/objects",
		"/v1/collections//objects",
		"/v1//collections",
		"/v1/collections/a%00b",
	} {
		w := httptest.NewRecorder()
		router.ServeHTTP(w, newRequest(t, http.MethodPost, path, bearer))
		if w.Code != http.StatusBadRequest {
			t.Errorf("%s: expected 400, got %d", path, w.Code)
			continue
		}
		if code := decodeErrorCode(t, w.Body.Bytes()); code != "invalid_path" {
			t.Errorf("%s: expected invalid_path, got %q", path, code)
		}
	}
	if got := len(seen()); got != 0 {
		t.Errorf("unsafe paths reached storage (%d requests)", got)
	}
}

func TestBodyLimit(t *testing.T) {
	backend, seen := newRecordingStorage(t)
	router := newGatewayRouter(t, backend.URL, gatewayConfig{maxBodyBytes: 10})
	path := "/v1/collections/jobs/objects"

	t.Run("within limit is forwarded", func(t *testing.T) {
		w := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader("0123456789"))
		bearer(req)
		router.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
		}
	})

	t.Run("declared length over limit is rejected up front", func(t *testing.T) {
		before := len(seen())
		w := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader("0123456789x"))
		bearer(req)
		router.ServeHTTP(w, req)
		if w.Code != http.StatusRequestEntityTooLarge {
			t.Fatalf("expected 413, got %d: %s", w.Code, w.Body.String())
		}
		if code := decodeErrorCode(t, w.Body.Bytes()); code != "payload_too_large" {
			t.Errorf("expected payload_too_large, got %q", code)
		}
		if len(seen()) != before {
			t.Errorf("oversized request reached storage")
		}
	})

	t.Run("chunked body over limit is cut off", func(t *testing.T) {
		front := httptest.NewServer(router)
		defer front.Close()
		req, err := http.NewRequest(http.MethodPost, front.URL+path, io.NopCloser(strings.NewReader(strings.Repeat("x", 1024))))
		if err != nil {
			t.Fatal(err)
		}
		req.ContentLength = -1 // force chunked transfer encoding
		bearer(req)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("do request: %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusRequestEntityTooLarge {
			body, _ := io.ReadAll(resp.Body)
			t.Fatalf("expected 413, got %d: %s", resp.StatusCode, body)
		}
	})
}

func TestProxyScrubsHeaders(t *testing.T) {
	backend, seen := newRecordingStorage(t)
	router := newGatewayRouter(t, backend.URL, gatewayConfig{})

	w := httptest.NewRecorder()
	req := newRequest(t, http.MethodGet, "/v1/collections", func(r *http.Request) {
		bearer(r)
		r.Header.Set("X-Forwarded-For", "6.6.6.6")
		r.Header.Set("X-Forwarded-Host", "evil.example")
		r.Header.Set("Forwarded", "for=6.6.6.6")
		r.Header.Set("X-Request-Id", "keep-me")
	})
	router.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	got := seen()
	if len(got) != 1 {
		t.Fatalf("expected 1 forwarded request, got %d", len(got))
	}
	h := got[0].header
	if v := h.Get("Authorization"); v != "" {
		t.Errorf("Authorization leaked to storage: %q", v)
	}
	for _, name := range []string{"X-Forwarded-Host", "Forwarded"} {
		if v := h.Get(name); v != "" {
			t.Errorf("%s leaked to storage: %q", name, v)
		}
	}
	if v := h.Get("X-Forwarded-For"); strings.Contains(v, "6.6.6.6") {
		t.Errorf("spoofed X-Forwarded-For reached storage: %q", v)
	}
	if v := h.Get("X-Request-Id"); v != "keep-me" {
		t.Errorf("ordinary headers should pass through, X-Request-Id = %q", v)
	}
}
