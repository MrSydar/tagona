package main

import (
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"mrsydar/tagona/api/internal/keystorageapi"
)

const (
	keyID    = "00000000-0000-4000-8000-0000000000aa"
	validKey = "tagona_" + "cd"
)

// recordingKeystorage plays keystorage: it records every request except validation and answers
// every one of them with the status it is told to return.
func recordingKeystorage(t *testing.T, status *int) (*httptest.Server, func() []seenRequest) {
	t.Helper()
	return newReplyingStorage(t, func(w http.ResponseWriter, r *http.Request) {
		if *status == http.StatusUnauthorized {
			w.Header().Set("WWW-Authenticate", `Basic realm="tagona-admin"`)
		}
		w.Header().Set("Set-Cookie", "internal=1")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(*status)
		if *status != http.StatusNoContent {
			w.Write([]byte(`{"upstream":"keystorage"}`))
		}
	})
}

func adminRequest(method, path, body string) *http.Request {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if body == "" {
		req.Body, req.ContentLength = http.NoBody, 0
	}
	req.SetBasicAuth("admin", "tagona")
	req.Header.Set("Content-Type", "application/json")
	return req
}

func TestAdminRoutesBecomeCleanKeystorageRequests(t *testing.T) {
	status := http.StatusCreated
	ks, seen := recordingKeystorage(t, &status)
	router := newTestRouter(t, ks.URL, gatewayConfig{})

	tests := []struct {
		method, path, body string
		status             int
		wantPath, wantBody string
	}{
		{http.MethodPost, "/v1/admin/api-keys", `{"name":"dev"}`, http.StatusCreated, "/api-keys", `{"name":"dev"}`},
		{http.MethodGet, "/v1/admin/api-keys", ``, http.StatusOK, "/api-keys", ``},
		{http.MethodDelete, "/v1/admin/api-keys/" + keyID, ``, http.StatusNoContent, "/api-keys/" + keyID, ``},
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
			if last.method != tt.method || last.path != tt.wantPath || last.body != tt.wantBody {
				t.Errorf("keystorage got %s %s %q", last.method, last.path, last.body)
			}
			// The admin credentials are what keystorage authenticates, so they must arrive intact.
			if last.header.Get("Authorization") != "Basic YWRtaW46dGFnb25h" {
				t.Errorf("Authorization = %q", last.header.Get("Authorization"))
			}
			if w.Header().Get("Set-Cookie") != "" {
				t.Error("an internal Set-Cookie header reached the client")
			}
		})
	}
}

func TestAdminAnswersAreRelayedIncludingTheBasicChallenge(t *testing.T) {
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
		t.Fatalf("body was not relayed: %s", w.Body)
	}
}

func TestAdminRequestsCarryOnlyWhatKeystorageNeeds(t *testing.T) {
	status := http.StatusOK
	ks, seen := recordingKeystorage(t, &status)
	router := newTestRouter(t, ks.URL, gatewayConfig{})

	req := adminRequest(http.MethodGet, "/v1/admin/api-keys", "")
	req.Header.Set("Cookie", "session=secret")
	req.Header.Set("X-Forwarded-For", "6.6.6.6")
	req.Header.Set("X-Anything", "else")
	router.ServeHTTP(httptest.NewRecorder(), req)

	got := seen()
	if len(got) != 1 {
		t.Fatalf("saw %d requests", len(got))
	}
	if want := []string{"Authorization", "User-Agent"}; !reflect.DeepEqual(headerNames(got[0].header), want) {
		t.Errorf("keystorage saw headers %v, want %v", headerNames(got[0].header), want)
	}

	// An unreasonably long credential is not passed on at all; keystorage then answers 401.
	seen2 := len(seen())
	req = adminRequest(http.MethodGet, "/v1/admin/api-keys", "")
	req.Header.Set("Authorization", "Basic "+strings.Repeat("a", 2000))
	router.ServeHTTP(httptest.NewRecorder(), req)
	if h := seen()[seen2].header; h.Get("Authorization") != "" {
		t.Errorf("oversized Authorization header was forwarded (%d bytes)", len(h.Get("Authorization")))
	}
}

// Whatever keystorage serves besides these three routes, and above all its validation endpoint,
// must stay closed to the outside; and a request that is not exactly right is not forwarded.
func TestAdminSurfaceIsStrictlyAllowlisted(t *testing.T) {
	status := http.StatusOK
	ks, seen := recordingKeystorage(t, &status)
	router := newTestRouter(t, ks.URL, gatewayConfig{})

	closed := []struct {
		method, path, body string
		want               int
	}{
		// keystorage's validation endpoint, however it is addressed
		{http.MethodPost, "/api-keys/validate", `{"key":"` + validKey + `"}`, http.StatusNotFound},
		{http.MethodPost, "/internal/v1/api-keys/validate", `{"key":"` + validKey + `"}`, http.StatusNotFound},
		{http.MethodPost, "/v1/admin/api-keys/validate", `{"key":"` + validKey + `"}`, http.StatusMethodNotAllowed},
		{http.MethodPost, "/v1/admin/api-keys/validate/", `{"key":"` + validKey + `"}`, http.StatusNotFound},
		{http.MethodGet, "/api-keys", ``, http.StatusNotFound},
		// methods that are not part of the allowlist
		{http.MethodGet, "/v1/admin/api-keys/" + keyID, ``, http.StatusMethodNotAllowed},
		{http.MethodPut, "/v1/admin/api-keys", ``, http.StatusMethodNotAllowed},
		{http.MethodPatch, "/v1/admin/api-keys/" + keyID, ``, http.StatusMethodNotAllowed},
		{http.MethodDelete, "/v1/admin/api-keys", ``, http.StatusMethodNotAllowed},
		// other paths
		{http.MethodGet, "/v1/admin", ``, http.StatusNotFound},
		{http.MethodGet, "/v1/admin/metrics", ``, http.StatusNotFound},
		{http.MethodGet, "/v1/admin/api-keys/" + keyID + "/extra", ``, http.StatusNotFound},
		// ids that are not UUIDs are never put into a path
		{http.MethodDelete, "/v1/admin/api-keys/not-a-uuid", ``, http.StatusNotFound},
		{http.MethodDelete, "/v1/admin/api-keys/validate", ``, http.StatusNotFound},
		{http.MethodDelete, "/v1/admin/api-keys/%2e%2e", ``, http.StatusBadRequest},
		{http.MethodDelete, "/v1/admin/api-keys/..%2fapi-keys", ``, http.StatusBadRequest},
		// requests that carry more than the route takes
		{http.MethodGet, "/v1/admin/api-keys?evil=1", ``, http.StatusBadRequest},
		{http.MethodDelete, "/v1/admin/api-keys/" + keyID + "?force=1", ``, http.StatusBadRequest},
		{http.MethodGet, "/v1/admin/api-keys", `{"x":1}`, http.StatusBadRequest},
		{http.MethodPost, "/v1/admin/api-keys?x=1", `{"name":"a"}`, http.StatusBadRequest},
		{http.MethodPost, "/v1/admin/api-keys", `{"name":"a","admin":true}`, http.StatusBadRequest},
		{http.MethodPost, "/v1/admin/api-keys", `{"name":1}`, http.StatusBadRequest},
		{http.MethodPost, "/v1/admin/api-keys", `{"name":"a"} {"name":"b"}`, http.StatusBadRequest},
		{http.MethodPost, "/v1/admin/api-keys", `nope`, http.StatusBadRequest},
		{http.MethodPost, "/v1/admin/api-keys", `{"name":"` + strings.Repeat("a", 129) + `"}`, http.StatusBadRequest},
		{http.MethodPost, "/v1/admin/api-keys", `{"name":"` + strings.Repeat("a", 5000) + `"}`, http.StatusRequestEntityTooLarge},
	}
	for _, c := range closed {
		t.Run(c.method+" "+c.path+" "+truncate(c.body, 20), func(t *testing.T) {
			w := httptest.NewRecorder()
			router.ServeHTTP(w, adminRequest(c.method, c.path, c.body))
			if w.Code != c.want {
				t.Errorf("status %d, want %d: %s", w.Code, c.want, w.Body)
			}
		})
	}
	if got := seen(); len(got) != 0 {
		t.Fatalf("closed or invalid requests reached keystorage: %+v", got)
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
