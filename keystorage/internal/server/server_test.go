package server

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sort"
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
	now     time.Time
}

func newMemStore() *memStore {
	return &memStore{keys: map[string]db.APIKey{}, hashes: map[string]string{}, now: time.Now()}
}

// advance moves the store's clock forward.
func (m *memStore) advance(d time.Duration) { m.now = m.now.Add(d) }

func (m *memStore) Ping(context.Context) error { return m.pingErr }

func (m *memStore) Create(_ context.Context, hash, prefix, name string, ttlSeconds int64) (*db.APIKey, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.failOn == "Create" {
		return nil, errors.New("boom")
	}
	m.next++
	k := db.APIKey{ID: fakeUUID(m.next), Name: name, KeyPrefix: prefix, CreatedAt: m.now.Truncate(time.Microsecond)}
	if ttlSeconds > 0 {
		exp := m.now.Add(time.Duration(ttlSeconds) * time.Second)
		k.ExpiresAt = &exp
	}
	m.keys[k.ID] = k
	m.hashes[hash] = k.ID
	return &k, nil
}

func (m *memStore) List(_ context.Context, limit int, after *db.Cursor) ([]db.APIKey, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.failOn == "List" {
		return nil, false, errors.New("boom")
	}
	all := make([]db.APIKey, 0, len(m.keys))
	for _, k := range m.keys {
		all = append(all, k)
	}
	// newest first, ties by id: the order the database uses
	sort.Slice(all, func(i, j int) bool {
		if !all[i].CreatedAt.Equal(all[j].CreatedAt) {
			return all[i].CreatedAt.After(all[j].CreatedAt)
		}
		return all[i].ID > all[j].ID
	})
	var out []db.APIKey
	for _, k := range all {
		if after != nil && !(k.CreatedAt.Before(after.CreatedAt) || (k.CreatedAt.Equal(after.CreatedAt) && k.ID < after.ID)) {
			continue
		}
		out = append(out, k)
	}
	if len(out) > limit {
		return out[:limit], true, nil
	}
	return out, false, nil
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
	if k.ExpiresAt != nil && !k.ExpiresAt.After(m.now) {
		return &k, db.ErrExpired
	}
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
	rec := do(h, http.MethodPost, "/api-keys", `{"name":"`+name+`"}`, admin)
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
		{http.MethodPost, "/api-keys"},
		{http.MethodGet, "/api-keys"},
		{http.MethodDelete, "/api-keys/" + fakeUUID(1)},
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
	rec := do(h, http.MethodGet, "/api-keys", "", admin)
	if rec.Code != http.StatusForbidden || errCode(t, rec) != "admin_disabled" {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	// Validation keeps working.
	store.hashes["x"] = "id"
	if rec := do(h, http.MethodPost, "/api-keys/validate", `{"key":"nope"}`, nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("validate: %d", rec.Code)
	}
}

func TestCreateListValidateDelete(t *testing.T) {
	h := newTestServer(newMemStore())

	id, raw := createKey(t, h, "dev")
	if !strings.HasPrefix(raw, "tagona_") {
		t.Fatalf("raw key = %q", raw)
	}

	rec := do(h, http.MethodGet, "/api-keys", "", admin)
	if rec.Code != 200 || strings.Contains(rec.Body.String(), raw) {
		t.Fatalf("list: %d (must never contain the raw key) %s", rec.Code, rec.Body)
	}
	var list struct{ Keys []struct{ ID, Name string } }
	_ = json.Unmarshal(rec.Body.Bytes(), &list)
	if len(list.Keys) != 1 || list.Keys[0].ID != id {
		t.Fatalf("list = %+v", list)
	}

	// Validation needs no credentials and reports id and name.
	rec = do(h, http.MethodPost, "/api-keys/validate", `{"key":"`+raw+`"}`, nil)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"name":"dev"`) || !strings.Contains(rec.Body.String(), id) {
		t.Fatalf("validate: %d %s", rec.Code, rec.Body)
	}

	if rec := do(h, http.MethodDelete, "/api-keys/"+id, "", admin); rec.Code != http.StatusNoContent {
		t.Fatalf("delete: %d %s", rec.Code, rec.Body)
	}
	rec = do(h, http.MethodPost, "/api-keys/validate", `{"key":"`+raw+`"}`, nil)
	if rec.Code != http.StatusUnauthorized || errCode(t, rec) != "invalid_api_key" {
		t.Fatalf("deleted key still validates: %d %s", rec.Code, rec.Body)
	}
}

func TestListEmptyIsArray(t *testing.T) {
	rec := do(newTestServer(newMemStore()), http.MethodGet, "/api-keys", "", admin)
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
			rec := do(h, http.MethodPost, "/api-keys", tt.body, admin)
			if rec.Code != tt.status || errCode(t, rec) != tt.code {
				t.Fatalf("%d %s", rec.Code, rec.Body)
			}
		})
	}
}

func TestValidateInput(t *testing.T) {
	h := newTestServer(newMemStore())
	if rec := do(h, http.MethodPost, "/api-keys/validate", `nope`, nil); rec.Code != 400 || errCode(t, rec) != "invalid_json" {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	if rec := do(h, http.MethodPost, "/api-keys/validate", `{"key":""}`, nil); rec.Code != 400 || errCode(t, rec) != "missing_key" {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
}

func TestDeleteUnknownAndMalformedID(t *testing.T) {
	h := newTestServer(newMemStore())
	for _, id := range []string{fakeUUID(99), "not-a-uuid", "..", "1"} {
		rec := do(h, http.MethodDelete, "/api-keys/"+id, "", admin)
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
				rec = do(h, http.MethodPost, "/api-keys", `{"name":"x"}`, admin)
			case "List":
				rec = do(h, http.MethodGet, "/api-keys", "", admin)
			case "GetByHash":
				rec = do(h, http.MethodPost, "/api-keys/validate", `{"key":"`+raw+`"}`, nil)
			case "Delete":
				rec = do(h, http.MethodDelete, "/api-keys/"+id, "", admin)
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
	if rec := do(h, http.MethodGet, "/api-keys/validate", "", nil); rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET validate: %d", rec.Code)
	}
	if rec := do(h, http.MethodGet, "/collections", "", nil); rec.Code != http.StatusNotFound {
		t.Errorf("unknown path: %d", rec.Code)
	}
	// The management paths are admin-only even for methods that are not routed.
	if rec := do(h, http.MethodPut, "/api-keys", "", admin); rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("PUT: %d", rec.Code)
	}
}

func createWith(t *testing.T, h http.Handler, body string) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	rec := do(h, http.MethodPost, "/api-keys", body, admin)
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec, out
}

func TestCreateWithTTL(t *testing.T) {
	store := newMemStore()
	h := newTestServer(store)

	t.Run("without ttl_seconds the key never expires", func(t *testing.T) {
		rec, out := createWith(t, h, `{"name":"a"}`)
		if rec.Code != http.StatusCreated {
			t.Fatalf("%d %s", rec.Code, rec.Body)
		}
		if v, present := out["expires_at"]; !present || v != nil {
			t.Fatalf("expires_at = %v (present %v), want an explicit null", v, present)
		}
	})

	t.Run("ttl_seconds sets the expiry", func(t *testing.T) {
		rec, out := createWith(t, h, `{"name":"b","ttl_seconds":14400}`)
		if rec.Code != http.StatusCreated {
			t.Fatalf("%d %s", rec.Code, rec.Body)
		}
		exp, err := time.Parse(time.RFC3339Nano, out["expires_at"].(string))
		if err != nil || !exp.Equal(store.now.Add(4*time.Hour)) {
			t.Fatalf("expires_at = %v (%v), want 4h after creation", out["expires_at"], err)
		}
	})

	for name, body := range map[string]string{
		"zero":      `{"name":"c","ttl_seconds":0}`,
		"negative":  `{"name":"c","ttl_seconds":-5}`,
		"fractions": `{"name":"c","ttl_seconds":1.5}`,
		"a string":  `{"name":"c","ttl_seconds":"60"}`,
		"absurd":    `{"name":"c","ttl_seconds":1000000000000000000}`,
		"just over": `{"name":"c","ttl_seconds":3153600001}`,
	} {
		t.Run("rejects "+name, func(t *testing.T) {
			rec, _ := createWith(t, h, body)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("%d %s", rec.Code, rec.Body)
			}
			if code := errCode(t, rec); code != "invalid_ttl" && code != "invalid_json" {
				t.Fatalf("code = %q", code)
			}
		})
	}
	if rec, _ := createWith(t, h, `{"name":"c","ttl_seconds":0}`); errCode(t, rec) != "invalid_ttl" {
		t.Fatal("ttl_seconds 0 must be invalid_ttl: it is not a way to say \"no expiry\", omit it")
	}
}

func TestKeyTTLDefaultAndCap(t *testing.T) {
	newServer := func(def, max time.Duration) (http.Handler, *memStore) {
		store := newMemStore()
		return New(store, "admin", "secret", WithKeyTTL(def, max)).Router(), store
	}
	expiryOf := func(t *testing.T, h http.Handler, store *memStore, body string) (time.Duration, int) {
		t.Helper()
		rec, out := createWith(t, h, body)
		if rec.Code != http.StatusCreated {
			return 0, rec.Code
		}
		if out["expires_at"] == nil {
			return 0, rec.Code
		}
		exp, _ := time.Parse(time.RFC3339Nano, out["expires_at"].(string))
		return exp.Sub(store.now), rec.Code
	}

	t.Run("default applies when ttl_seconds is omitted", func(t *testing.T) {
		h, store := newServer(time.Hour, 0)
		if d, code := expiryOf(t, h, store, `{"name":"a"}`); code != 201 || d != time.Hour {
			t.Fatalf("ttl %v (%d), want 1h", d, code)
		}
		if d, _ := expiryOf(t, h, store, `{"name":"a","ttl_seconds":10}`); d != 10*time.Second {
			t.Fatalf("an explicit ttl_seconds must win over the default: %v", d)
		}
	})

	t.Run("the cap rejects a longer ttl and applies when nothing else is set", func(t *testing.T) {
		h, store := newServer(0, 2*time.Hour)
		if _, code := expiryOf(t, h, store, `{"name":"a","ttl_seconds":7201}`); code != 400 {
			t.Fatalf("a ttl above the cap: %d", code)
		}
		if d, code := expiryOf(t, h, store, `{"name":"a","ttl_seconds":7200}`); code != 201 || d != 2*time.Hour {
			t.Fatalf("a ttl at the cap: %v (%d)", d, code)
		}
		if d, code := expiryOf(t, h, store, `{"name":"a"}`); code != 201 || d != 2*time.Hour {
			t.Fatalf("with a cap and no default, an omitted ttl gets the cap so no key outlives it: %v (%d)", d, code)
		}
	})

	t.Run("a default below the cap wins over the cap when omitted", func(t *testing.T) {
		h, store := newServer(time.Hour, 4*time.Hour)
		if d, _ := expiryOf(t, h, store, `{"name":"a"}`); d != time.Hour {
			t.Fatalf("ttl %v, want 1h", d)
		}
	})
}

func TestValidateDistinguishesExpiredFromUnknown(t *testing.T) {
	store := newMemStore()
	h := newTestServer(store)
	_, out := createWith(t, h, `{"name":"short","ttl_seconds":60}`)
	raw := out["key"].(string)
	_, forever := createWith(t, h, `{"name":"forever"}`)

	validate := func(key string) *httptest.ResponseRecorder {
		return do(h, http.MethodPost, "/api-keys/validate", `{"key":"`+key+`"}`, nil)
	}

	rec := validate(raw)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"expires_at":"`) {
		t.Fatalf("valid key: %d %s (the answer must carry expires_at)", rec.Code, rec.Body)
	}
	if rec := validate(forever["key"].(string)); rec.Code != 200 || !strings.Contains(rec.Body.String(), `"expires_at":null`) {
		t.Fatalf("a key without expiry: %d %s", rec.Code, rec.Body)
	}

	store.advance(59 * time.Second)
	if rec := validate(raw); rec.Code != 200 {
		t.Fatalf("one second before expiry: %d", rec.Code)
	}
	store.advance(time.Second) // exactly at expiry: expired
	rec = validate(raw)
	if rec.Code != http.StatusUnauthorized || errCode(t, rec) != "expired_api_key" {
		t.Fatalf("at expiry: %d %s", rec.Code, rec.Body)
	}
	if rec := validate("tagona_never_existed"); rec.Code != 401 || errCode(t, rec) != "invalid_api_key" {
		t.Fatalf("an unknown key keeps invalid_api_key: %d %s", rec.Code, rec.Body)
	}
	// The key that never expires is unaffected.
	store.advance(100 * 24 * time.Hour)
	if rec := validate(forever["key"].(string)); rec.Code != 200 {
		t.Fatalf("a key without expiry: %d", rec.Code)
	}
}

func TestListShowsExpiry(t *testing.T) {
	h := newTestServer(newMemStore())
	createWith(t, h, `{"name":"short","ttl_seconds":60}`)
	createWith(t, h, `{"name":"forever"}`)
	rec := do(h, http.MethodGet, "/api-keys", "", admin)
	var list struct {
		Keys []struct {
			Name      string
			ExpiresAt *time.Time `json:"expires_at"`
		}
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &list)
	byName := map[string]*time.Time{}
	for _, k := range list.Keys {
		byName[k.Name] = k.ExpiresAt
	}
	if len(byName) != 2 || byName["short"] == nil || byName["forever"] != nil {
		t.Fatalf("list = %s", rec.Body)
	}
	if !strings.Contains(rec.Body.String(), `"expires_at":null`) {
		t.Fatalf("a key without expiry must show an explicit null: %s", rec.Body)
	}
}

// listPage fetches one page of keys.
func listPage(t *testing.T, h http.Handler, query string) (ids []string, next string) {
	t.Helper()
	rec := do(h, http.MethodGet, "/api-keys"+query, "", admin)
	if rec.Code != http.StatusOK {
		t.Fatalf("list %q: %d %s", query, rec.Code, rec.Body)
	}
	var page struct {
		Keys []struct{ ID string }
		Next string
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &page); err != nil {
		t.Fatal(err)
	}
	for _, k := range page.Keys {
		ids = append(ids, k.ID)
	}
	return ids, page.Next
}

func TestListIsPaged(t *testing.T) {
	h := newTestServer(newMemStore())
	var created []string
	for i := 0; i < 5; i++ {
		id, _ := createKey(t, h, "k")
		created = append(created, id)
	}

	var got []string
	query, pages := "?limit=2", 0
	for {
		ids, next := listPage(t, h, query)
		pages++
		got = append(got, ids...)
		if next == "" {
			break
		}
		if len(ids) != 2 {
			t.Fatalf("page %d has %d keys, want 2", pages, len(ids))
		}
		query = "?limit=2&cursor=" + next
	}
	if pages != 3 || len(got) != 5 {
		t.Fatalf("%d pages, %d keys: %v", pages, len(got), got)
	}
	// newest first, each key once: the creation order reversed
	for i, id := range got {
		if id != created[len(created)-1-i] {
			t.Fatalf("keys = %v, want %v reversed", got, created)
		}
	}

	// an exact fit has no next page
	if _, next := listPage(t, h, "?limit=5"); next != "" {
		t.Fatalf("next = %q after the last key", next)
	}
}

func TestListDefaultsToOneHundredKeys(t *testing.T) {
	h := newTestServer(newMemStore())
	for i := 0; i < defaultListLimit+1; i++ {
		createKey(t, h, "k")
	}
	ids, next := listPage(t, h, "")
	if len(ids) != defaultListLimit || next == "" {
		t.Fatalf("%d keys, next %q", len(ids), next)
	}
	rest, next := listPage(t, h, "?cursor="+next)
	if len(rest) != 1 || next != "" {
		t.Fatalf("second page: %d keys, next %q", len(rest), next)
	}
}

func TestListRejectsBadPaging(t *testing.T) {
	h := newTestServer(newMemStore())
	createKey(t, h, "k")
	tests := []struct{ query, code string }{
		{"?limit=0", "invalid_limit"},
		{"?limit=-1", "invalid_limit"},
		{"?limit=abc", "invalid_limit"},
		{"?limit=", "invalid_limit"},
		{"?limit=1001", "invalid_limit"},
		{"?cursor=", "invalid_cursor"},
		{"?cursor=!!!", "invalid_cursor"},
		{"?cursor=" + base64.RawURLEncoding.EncodeToString([]byte("nonsense")), "invalid_cursor"},
		{"?cursor=" + base64.RawURLEncoding.EncodeToString([]byte("12|not-a-uuid")), "invalid_cursor"},
		{"?cursor=" + base64.RawURLEncoding.EncodeToString([]byte("x|00000000-0000-4000-8000-000000000001")), "invalid_cursor"},
	}
	for _, tt := range tests {
		t.Run(tt.query, func(t *testing.T) {
			rec := do(h, http.MethodGet, "/api-keys"+tt.query, "", admin)
			if rec.Code != http.StatusBadRequest || errCode(t, rec) != tt.code {
				t.Fatalf("%d %s", rec.Code, rec.Body)
			}
		})
	}
	if ids, _ := listPage(t, h, "?limit=1000"); len(ids) != 1 {
		t.Fatal("the largest page size is refused")
	}
}
