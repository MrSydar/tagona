package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"mrsydar/tagona/api/internal/keystorageapi"
)

const (
	keyID    = "00000000-0000-4000-8000-0000000000aa"
	validKey = "tagona_" + "cd"
)

// recordingKeystorage plays keystorage: it records every request except validation and answers
// the way the real service does for the status it is told to return.
func recordingKeystorage(t *testing.T, status *int) (*httptest.Server, func() []seenRequest) {
	t.Helper()
	var mu sync.Mutex
	var seen []seenRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/internal/v1/api-keys/validate" {
			w.Write([]byte(`{"id":"k","name":"n"}`))
			return
		}
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		seen = append(seen, seenRequest{r.Method, r.URL.RequestURI(), r.Header.Clone(), string(body)})
		mu.Unlock()
		if *status == http.StatusUnauthorized {
			w.Header().Set("WWW-Authenticate", `Basic realm="tagona-admin"`)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(*status)
		if *status != http.StatusNoContent {
			w.Write([]byte(`{"upstream":"keystorage"}`))
		}
	}))
	t.Cleanup(srv.Close)
	return srv, func() []seenRequest {
		mu.Lock()
		defer mu.Unlock()
		return append([]seenRequest(nil), seen...)
	}
}

func adminRequest(method, path, body string) *http.Request {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.SetBasicAuth("admin", "tagona")
	req.Header.Set("Content-Type", "application/json")
	return req
}

func TestAdminRoutesAreForwardedToKeystorage(t *testing.T) {
	status := http.StatusCreated
	ks, seen := recordingKeystorage(t, &status)
	router := newTestRouter(t, ks.URL, gatewayConfig{})

	tests := []struct {
		method, path, body string
		status             int
	}{
		{http.MethodPost, "/v1/admin/api-keys", `{"name":"dev"}`, http.StatusCreated},
		{http.MethodGet, "/v1/admin/api-keys", ``, http.StatusOK},
		{http.MethodDelete, "/v1/admin/api-keys/" + keyID, ``, http.StatusNoContent},
	}
	for _, tt := range tests {
		t.Run(tt.method+" "+tt.path, func(t *testing.T) {
			status = tt.status
			before := len(seen())
			w := httptest.NewRecorder()
			router.ServeHTTP(w, adminRequest(tt.method, tt.path, tt.body))
			if w.Code != tt.status {
				t.Fatalf("status %d, want %d: %s", w.Code, tt.status, w.Body)
			}
			got := seen()
			if len(got) != before+1 {
				t.Fatalf("keystorage saw %d new requests, want 1", len(got)-before)
			}
			last := got[len(got)-1]
			if last.method != tt.method || last.path != tt.path || last.body != tt.body {
				t.Errorf("forwarded %s %s %q", last.method, last.path, last.body)
			}
			// The admin credentials are what keystorage authenticates, so they must arrive intact.
			if last.header.Get("Authorization") != "Basic YWRtaW46dGFnb25h" {
				t.Errorf("Authorization = %q", last.header.Get("Authorization"))
			}
		})
	}
}

func TestAdminProxyPassesKeystorageAnswersThrough(t *testing.T) {
	status := http.StatusUnauthorized
	ks, _ := recordingKeystorage(t, &status)
	router := newTestRouter(t, ks.URL, gatewayConfig{})

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/admin/api-keys", nil) // no credentials at all
	router.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized || w.Header().Get("WWW-Authenticate") != `Basic realm="tagona-admin"` {
		t.Fatalf("got %d, WWW-Authenticate %q", w.Code, w.Header().Get("WWW-Authenticate"))
	}
	if !strings.Contains(w.Body.String(), "keystorage") {
		t.Fatalf("body was not passed through: %s", w.Body)
	}
}

func TestAdminProxyOnlyForwardsWhatKeystorageNeeds(t *testing.T) {
	status := http.StatusOK
	ks, seen := recordingKeystorage(t, &status)
	router := newTestRouter(t, ks.URL, gatewayConfig{})

	req := adminRequest(http.MethodGet, "/v1/admin/api-keys?evil=1&x=y", "")
	req.Header.Set("Cookie", "session=secret")
	req.Header.Set("X-Forwarded-For", "6.6.6.6")
	req.Header.Set("X-Anything", "else")
	router.ServeHTTP(httptest.NewRecorder(), req)

	got := seen()
	if len(got) != 1 {
		t.Fatalf("saw %d requests", len(got))
	}
	if got[0].path != "/v1/admin/api-keys" {
		t.Errorf("query string forwarded: %q", got[0].path)
	}
	for _, h := range []string{"Cookie", "X-Anything", "Forwarded", "X-Forwarded-Host"} {
		if v := got[0].header.Get(h); v != "" {
			t.Errorf("%s forwarded: %q", h, v)
		}
	}
	if v := got[0].header.Get("X-Forwarded-For"); strings.Contains(v, "6.6.6.6") {
		t.Errorf("spoofed X-Forwarded-For forwarded: %q", v)
	}
}

// The validation endpoint and everything else keystorage serves must stay closed to the outside.
func TestAdminSurfaceIsStrictlyAllowlisted(t *testing.T) {
	status := http.StatusOK
	ks, seen := recordingKeystorage(t, &status)
	router := newTestRouter(t, ks.URL, gatewayConfig{})

	closed := []struct {
		method, path string
		want         int
	}{
		// keystorage's validation endpoint, however it is addressed
		{http.MethodPost, "/internal/v1/api-keys/validate", http.StatusNotFound},
		{http.MethodPost, "/v1/admin/api-keys/validate", http.StatusMethodNotAllowed},
		{http.MethodPost, "/v1/admin/internal/v1/api-keys/validate", http.StatusNotFound},
		{http.MethodGet, "/internal/v1/api-keys", http.StatusNotFound},
		// methods that are not part of the allowlist
		{http.MethodGet, "/v1/admin/api-keys/" + keyID, http.StatusMethodNotAllowed},
		{http.MethodPut, "/v1/admin/api-keys", http.StatusMethodNotAllowed},
		{http.MethodPatch, "/v1/admin/api-keys/" + keyID, http.StatusMethodNotAllowed},
		{http.MethodDelete, "/v1/admin/api-keys", http.StatusMethodNotAllowed},
		// other paths under the admin prefix and anything keystorage serves for operators
		{http.MethodGet, "/v1/admin", http.StatusNotFound},
		{http.MethodGet, "/v1/admin/metrics", http.StatusNotFound},
		{http.MethodGet, "/v1/admin/api-keys/" + keyID + "/extra", http.StatusNotFound},
		{http.MethodGet, "/readyz-keys", http.StatusNotFound},
		// ids that are not UUIDs are never interpolated into the forwarded path
		{http.MethodDelete, "/v1/admin/api-keys/not-a-uuid", http.StatusNotFound},
		{http.MethodDelete, "/v1/admin/api-keys/validate", http.StatusNotFound},
		{http.MethodDelete, "/v1/admin/api-keys/%2e%2e", http.StatusBadRequest},
		{http.MethodDelete, "/v1/admin/api-keys/..%2fapi-keys", http.StatusBadRequest},
	}
	for _, c := range closed {
		t.Run(c.method+" "+c.path, func(t *testing.T) {
			w := httptest.NewRecorder()
			router.ServeHTTP(w, adminRequest(c.method, c.path, `{"key":"`+validKey+`"}`))
			if w.Code != c.want {
				t.Errorf("status %d, want %d: %s", w.Code, c.want, w.Body)
			}
		})
	}
	if got := seen(); len(got) != 0 {
		t.Fatalf("closed routes reached keystorage: %+v", got)
	}
}

func TestAdminBodyIsLimited(t *testing.T) {
	status := http.StatusCreated
	ks, seen := recordingKeystorage(t, &status)
	router := newTestRouter(t, ks.URL, gatewayConfig{})

	w := httptest.NewRecorder()
	router.ServeHTTP(w, adminRequest(http.MethodPost, "/v1/admin/api-keys", `{"name":"`+strings.Repeat("a", 5000)+`"}`))
	if w.Code != http.StatusRequestEntityTooLarge || decodeErrorCode(t, w.Body.Bytes()) != "payload_too_large" {
		t.Fatalf("got %d: %s", w.Code, w.Body)
	}
	if len(seen()) != 0 {
		t.Fatal("oversized body reached keystorage")
	}
}

func TestAdminKeystorageDownIsNotReady(t *testing.T) {
	status := http.StatusOK
	ks, _ := recordingKeystorage(t, &status)
	router := newTestRouter(t, ks.URL, gatewayConfig{})
	ks.Close()

	w := httptest.NewRecorder()
	router.ServeHTTP(w, adminRequest(http.MethodGet, "/v1/admin/api-keys", ""))
	if w.Code != http.StatusServiceUnavailable || decodeErrorCode(t, w.Body.Bytes()) != "not_ready" {
		t.Fatalf("got %d: %s", w.Code, w.Body)
	}
}

// A deleted key must stop working at once, not when the cached verdict expires; but only a
// delete that actually happened may purge the cache.
func TestAdminDeletePurgesVerdictCache(t *testing.T) {
	status := http.StatusNoContent
	ks, _ := recordingKeystorage(t, &status)
	cache := newCachedValidator(keystorageapi.New(ks.URL), time.Minute)
	router := newTestRouter(t, ks.URL, gatewayConfig{validator: cache, onKeyDeleted: cache.Purge})

	size := func() int {
		cache.mu.Lock()
		defer cache.mu.Unlock()
		return len(cache.entries)
	}
	if _, err := cache.ValidateKey(t.Context(), validKey); err != nil || size() != 1 {
		t.Fatalf("setup: err %v, %d entries", err, size())
	}

	status = http.StatusNotFound // keystorage found no such key
	router.ServeHTTP(httptest.NewRecorder(), adminRequest(http.MethodDelete, "/v1/admin/api-keys/"+keyID, ""))
	status = http.StatusUnauthorized // admin credentials rejected
	router.ServeHTTP(httptest.NewRecorder(), adminRequest(http.MethodDelete, "/v1/admin/api-keys/"+keyID, ""))
	if size() != 1 {
		t.Fatal("a failed delete purged the cache")
	}

	status = http.StatusNoContent
	router.ServeHTTP(httptest.NewRecorder(), adminRequest(http.MethodDelete, "/v1/admin/api-keys/"+keyID, ""))
	if size() != 0 {
		t.Fatal("a successful delete did not purge the cache")
	}
}
