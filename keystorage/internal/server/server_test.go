package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"mrsydar/tagona/keystorage/internal/db"
)

// memStore is an in-memory Store.
type memStore struct {
	mu      sync.Mutex
	keys    map[string]db.APIKey // by id
	hashes  map[string]string    // hash -> id
	next    int
	pingErr error
	failOn  string // method name that returns an error
}

func newMemStore() *memStore {
	return &memStore{keys: map[string]db.APIKey{}, hashes: map[string]string{}}
}

func (m *memStore) Ping(context.Context) error { return m.pingErr }

func (m *memStore) Create(_ context.Context, hash, prefix, name string) (*db.APIKey, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.failOn == "Create" {
		return nil, errors.New("boom")
	}
	m.next++
	k := db.APIKey{ID: fakeUUID(m.next), Name: name, KeyPrefix: prefix, CreatedAt: time.Now()}
	m.keys[k.ID] = k
	m.hashes[hash] = k.ID
	return &k, nil
}

func (m *memStore) List(context.Context) ([]db.APIKey, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.failOn == "List" {
		return nil, errors.New("boom")
	}
	var out []db.APIKey
	for _, k := range m.keys {
		out = append(out, k)
	}
	return out, nil
}

func (m *memStore) GetByHash(_ context.Context, hash string) (*db.APIKey, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.failOn == "GetByHash" {
		return nil, errors.New("boom")
	}
	id, ok := m.hashes[hash]
	if !ok {
		return nil, db.ErrNotFound
	}
	k := m.keys[id]
	return &k, nil
}

func (m *memStore) Delete(_ context.Context, id string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.failOn == "Delete" {
		return false, errors.New("boom")
	}
	if _, ok := m.keys[id]; !ok {
		return false, nil
	}
	delete(m.keys, id)
	for h, kid := range m.hashes {
		if kid == id {
			delete(m.hashes, h)
		}
	}
	return true, nil
}

func fakeUUID(n int) string {
	return "00000000-0000-4000-8000-" + strings.Repeat("0", 12-len(itoa(n))) + itoa(n)
}

func itoa(n int) string {
	b, _ := json.Marshal(n)
	return string(b)
}

func newTestServer(store Store) http.Handler { return New(store, "admin", "secret").Router() }

func do(h http.Handler, method, path, body string, mod func(*http.Request)) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if mod != nil {
		mod(req)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func admin(r *http.Request) { r.SetBasicAuth("admin", "secret") }

func errCode(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var e struct{ Error struct{ Code string } }
	if err := json.Unmarshal(rec.Body.Bytes(), &e); err != nil {
		t.Fatalf("body %q: %v", rec.Body.String(), err)
	}
	return e.Error.Code
}

func createKey(t *testing.T, h http.Handler, name string) (id, raw string) {
	t.Helper()
	rec := do(h, http.MethodPost, "/v1/admin/api-keys", `{"name":"`+name+`"}`, admin)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", rec.Code, rec.Body)
	}
	var out struct{ ID, Key string }
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	return out.ID, out.Key
}

func TestAdminAuth(t *testing.T) {
	h := newTestServer(newMemStore())
	routes := []struct{ method, path string }{
		{http.MethodPost, "/v1/admin/api-keys"},
		{http.MethodGet, "/v1/admin/api-keys"},
		{http.MethodDelete, "/v1/admin/api-keys/" + fakeUUID(1)},
	}
	for _, rt := range routes {
		t.Run(rt.method+" "+rt.path, func(t *testing.T) {
			rec := do(h, rt.method, rt.path, `{"name":"x"}`, nil)
			if rec.Code != http.StatusUnauthorized || errCode(t, rec) != "invalid_admin_credentials" {
				t.Fatalf("no credentials: %d %s", rec.Code, rec.Body)
			}
			if got := rec.Header().Get("WWW-Authenticate"); got != `Basic realm="tagona-admin"` {
				t.Errorf("WWW-Authenticate = %q", got)
			}
			for name, mod := range map[string]func(*http.Request){
				"wrong password": func(r *http.Request) { r.SetBasicAuth("admin", "nope") },
				"wrong username": func(r *http.Request) { r.SetBasicAuth("root", "secret") },
				"empty":          func(r *http.Request) { r.SetBasicAuth("", "") },
			} {
				if rec := do(h, rt.method, rt.path, `{"name":"x"}`, mod); rec.Code != http.StatusUnauthorized {
					t.Errorf("%s: %d", name, rec.Code)
				}
			}
			// A valid API key is no substitute for admin credentials.
			rec = do(h, rt.method, rt.path, `{"name":"x"}`, func(r *http.Request) { r.Header.Set("Authorization", "Bearer tagona_key") })
			if rec.Code != http.StatusForbidden || errCode(t, rec) != "forbidden" {
				t.Fatalf("bearer: %d %s", rec.Code, rec.Body)
			}
		})
	}
}

func TestAdminDisabledWithoutCredentials(t *testing.T) {
	store := newMemStore()
	h := New(store, "", "").Router()
	rec := do(h, http.MethodGet, "/v1/admin/api-keys", "", admin)
	if rec.Code != http.StatusForbidden || errCode(t, rec) != "admin_disabled" {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	// Validation keeps working.
	store.hashes["x"] = "id"
	if rec := do(h, http.MethodPost, "/internal/v1/api-keys/validate", `{"key":"nope"}`, nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("validate: %d", rec.Code)
	}
}

func TestCreateListValidateDelete(t *testing.T) {
	h := newTestServer(newMemStore())

	id, raw := createKey(t, h, "dev")
	if !strings.HasPrefix(raw, "tagona_") {
		t.Fatalf("raw key = %q", raw)
	}

	rec := do(h, http.MethodGet, "/v1/admin/api-keys", "", admin)
	if rec.Code != 200 || strings.Contains(rec.Body.String(), raw) {
		t.Fatalf("list: %d (must never contain the raw key) %s", rec.Code, rec.Body)
	}
	var list struct{ Keys []struct{ ID, Name string } }
	_ = json.Unmarshal(rec.Body.Bytes(), &list)
	if len(list.Keys) != 1 || list.Keys[0].ID != id {
		t.Fatalf("list = %+v", list)
	}

	// Validation needs no credentials and reports id and name.
	rec = do(h, http.MethodPost, "/internal/v1/api-keys/validate", `{"key":"`+raw+`"}`, nil)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"name":"dev"`) || !strings.Contains(rec.Body.String(), id) {
		t.Fatalf("validate: %d %s", rec.Code, rec.Body)
	}

	if rec := do(h, http.MethodDelete, "/v1/admin/api-keys/"+id, "", admin); rec.Code != http.StatusNoContent {
		t.Fatalf("delete: %d %s", rec.Code, rec.Body)
	}
	rec = do(h, http.MethodPost, "/internal/v1/api-keys/validate", `{"key":"`+raw+`"}`, nil)
	if rec.Code != http.StatusUnauthorized || errCode(t, rec) != "invalid_api_key" {
		t.Fatalf("deleted key still validates: %d %s", rec.Code, rec.Body)
	}
}

func TestListEmptyIsArray(t *testing.T) {
	rec := do(newTestServer(newMemStore()), http.MethodGet, "/v1/admin/api-keys", "", admin)
	if strings.TrimSpace(rec.Body.String()) != `{"keys":[]}` {
		t.Fatalf("body = %s", rec.Body)
	}
}

func TestCreateValidation(t *testing.T) {
	h := newTestServer(newMemStore())
	tests := []struct {
		name, body, code string
		status           int
	}{
		{"not json", `nope`, "invalid_json", 400},
		{"empty name", `{"name":""}`, "invalid_name", 400},
		{"missing name", `{}`, "invalid_name", 400},
		{"name too long", `{"name":"` + strings.Repeat("a", 129) + `"}`, "invalid_name", 400},
		{"body too large", `{"name":"` + strings.Repeat("a", 5000) + `"}`, "payload_too_large", 413},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := do(h, http.MethodPost, "/v1/admin/api-keys", tt.body, admin)
			if rec.Code != tt.status || errCode(t, rec) != tt.code {
				t.Fatalf("%d %s", rec.Code, rec.Body)
			}
		})
	}
}

func TestValidateInput(t *testing.T) {
	h := newTestServer(newMemStore())
	if rec := do(h, http.MethodPost, "/internal/v1/api-keys/validate", `nope`, nil); rec.Code != 400 || errCode(t, rec) != "invalid_json" {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	if rec := do(h, http.MethodPost, "/internal/v1/api-keys/validate", `{"key":""}`, nil); rec.Code != 400 || errCode(t, rec) != "missing_key" {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
}

func TestDeleteUnknownAndMalformedID(t *testing.T) {
	h := newTestServer(newMemStore())
	for _, id := range []string{fakeUUID(99), "not-a-uuid", "..", "1"} {
		rec := do(h, http.MethodDelete, "/v1/admin/api-keys/"+id, "", admin)
		if rec.Code != http.StatusNotFound || errCode(t, rec) != "not_found" {
			t.Errorf("id %q: %d %s", id, rec.Code, rec.Body)
		}
	}
}

func TestStoreFailuresAre500(t *testing.T) {
	for _, method := range []string{"Create", "List", "GetByHash", "Delete"} {
		t.Run(method, func(t *testing.T) {
			store := newMemStore()
			h := newTestServer(store)
			id, raw := createKey(t, h, "dev")
			store.failOn = method
			var rec *httptest.ResponseRecorder
			switch method {
			case "Create":
				rec = do(h, http.MethodPost, "/v1/admin/api-keys", `{"name":"x"}`, admin)
			case "List":
				rec = do(h, http.MethodGet, "/v1/admin/api-keys", "", admin)
			case "GetByHash":
				rec = do(h, http.MethodPost, "/internal/v1/api-keys/validate", `{"key":"`+raw+`"}`, nil)
			case "Delete":
				rec = do(h, http.MethodDelete, "/v1/admin/api-keys/"+id, "", admin)
			}
			if rec.Code != http.StatusInternalServerError || errCode(t, rec) != "internal_error" {
				t.Fatalf("%d %s", rec.Code, rec.Body)
			}
			if strings.Contains(rec.Body.String(), "boom") {
				t.Fatal("internal error details leaked to the client")
			}
		})
	}
}

func TestHealthAndReadiness(t *testing.T) {
	store := newMemStore()
	h := newTestServer(store)
	for _, path := range []string{"/healthz", "/readyz", "/metrics"} {
		if rec := do(h, http.MethodGet, path, "", nil); rec.Code != 200 {
			t.Errorf("%s: %d", path, rec.Code)
		}
	}
	store.pingErr = errors.New("down")
	rec := do(h, http.MethodGet, "/readyz", "", nil)
	if rec.Code != http.StatusServiceUnavailable || errCode(t, rec) != "not_ready" {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
}

func TestUnknownRoutes(t *testing.T) {
	h := newTestServer(newMemStore())
	if rec := do(h, http.MethodGet, "/internal/v1/api-keys/validate", "", nil); rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET validate: %d", rec.Code)
	}
	if rec := do(h, http.MethodGet, "/v1/collections", "", nil); rec.Code != http.StatusNotFound {
		t.Errorf("unknown path: %d", rec.Code)
	}
	// The management paths are admin-only even for methods that are not routed.
	if rec := do(h, http.MethodPut, "/v1/admin/api-keys", "", admin); rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("PUT: %d", rec.Code)
	}
}
