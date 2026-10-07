package e2e_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// do sends a request with optional Basic admin credentials or a Bearer key and returns status and body.
func do(t *testing.T, method, path, body string, mod func(*http.Request)) (int, []byte) {
	t.Helper()
	req, err := http.NewRequest(method, storageURL+path, strings.NewReader(body))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	if mod != nil {
		mod(req)
	}
	resp, err := httpClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	return resp.StatusCode, b
}

func asAdmin(r *http.Request) { r.SetBasicAuth(adminUsername, adminPassword) }

func asKey(key string) func(*http.Request) {
	return func(r *http.Request) { setBearer(r, key) }
}

// Keys are managed through the api's /v1/admin/api-keys, which proxies to keystorage; a deleted key
// must stop working immediately, not after the gateway's verdict cache expires.
func TestKeyLifecycleThroughAdminProxy(t *testing.T) {
	status, body := do(t, http.MethodPost, "/v1/admin/api-keys", `{"name":"e2e-lifecycle"}`, asAdmin)
	require.Equal(t, http.StatusCreated, status, string(body))
	var created struct {
		ID, Name, Key string
	}
	require.NoError(t, json.Unmarshal(body, &created))
	require.True(t, strings.HasPrefix(created.Key, "tagona_"), "raw key is returned once: %s", body)
	t.Cleanup(func() { do(t, http.MethodDelete, "/v1/admin/api-keys/"+created.ID, "", asAdmin) })

	// The new key works, and listing never reveals it.
	status, body = do(t, http.MethodGet, "/v1/collections", "", asKey(created.Key))
	require.Equal(t, http.StatusOK, status, string(body))

	status, body = do(t, http.MethodGet, "/v1/admin/api-keys", "", asAdmin)
	require.Equal(t, http.StatusOK, status)
	assert.Contains(t, string(body), created.ID)
	assert.NotContains(t, string(body), created.Key, "the raw key must never be listed")

	// Deleting it takes effect at once, even though the verdict was cached by the request above.
	status, body = do(t, http.MethodDelete, "/v1/admin/api-keys/"+created.ID, "", asAdmin)
	require.Equal(t, http.StatusNoContent, status, string(body))
	status, body = do(t, http.MethodGet, "/v1/collections", "", asKey(created.Key))
	require.Equal(t, http.StatusUnauthorized, status, string(body))
	assert.Equal(t, "invalid_api_key", errorCode(t, body))

	status, body = do(t, http.MethodDelete, "/v1/admin/api-keys/"+created.ID, "", asAdmin)
	require.Equal(t, http.StatusNotFound, status, string(body))
	assert.Equal(t, "not_found", errorCode(t, body))
}

func TestAdminValidationOfRequests(t *testing.T) {
	tests := []struct {
		name, method, path, body string
		mod                      func(*http.Request)
		status                   int
		code                     string
	}{
		{"create without credentials", http.MethodPost, "/v1/admin/api-keys", `{"name":"x"}`, nil, 401, "invalid_admin_credentials"},
		{"list with wrong password", http.MethodGet, "/v1/admin/api-keys", "", func(r *http.Request) { r.SetBasicAuth(adminUsername, "wrong") }, 401, "invalid_admin_credentials"},
		{"create with invalid json", http.MethodPost, "/v1/admin/api-keys", `nope`, asAdmin, 400, "invalid_json"},
		{"create with empty name", http.MethodPost, "/v1/admin/api-keys", `{"name":""}`, asAdmin, 400, "invalid_name"},
		{"create with oversized body", http.MethodPost, "/v1/admin/api-keys", `{"name":"` + strings.Repeat("a", 5000) + `"}`, asAdmin, 413, "payload_too_large"},
		{"delete a malformed id", http.MethodDelete, "/v1/admin/api-keys/not-a-uuid", "", asAdmin, 404, "not_found"},
		{"delete an unknown id", http.MethodDelete, "/v1/admin/api-keys/00000000-0000-4000-8000-000000000000", "", asAdmin, 404, "not_found"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			status, body := do(t, tt.method, tt.path, tt.body, tt.mod)
			assert.Equal(t, tt.status, status, string(body))
			assert.Equal(t, tt.code, errorCode(t, body))
		})
	}
}

// keystorage's validation endpoint, and the internal routes of storage and the tagger, must not be reachable from outside, with or without credentials,
// and the admin proxy serves exactly create, list and delete.
func TestKeyValidationAndOtherRoutesAreNotExposed(t *testing.T) {
	creds := createAPIKey(t)
	defer deleteAPIKey(t, creds.ID)
	probe := fmt.Sprintf(`{"key":%q}`, creds.Key)

	closed := []struct {
		method, path string
		want         int
	}{
		{http.MethodPost, "/api-keys/validate", http.StatusNotFound},
		{http.MethodPost, "/internal/v1/api-keys/validate", http.StatusNotFound},
		{http.MethodPost, "/v1/admin/api-keys/validate", http.StatusMethodNotAllowed},
		{http.MethodGet, "/api-keys", http.StatusNotFound},
		{http.MethodGet, "/collections", http.StatusNotFound}, // storage's own routes are not the public ones
		{http.MethodPost, "/tag", http.StatusNotFound},        // nor is the tagger reachable
		{http.MethodGet, "/v1/admin/api-keys/" + creds.ID, http.StatusMethodNotAllowed},
		{http.MethodPut, "/v1/admin/api-keys", http.StatusMethodNotAllowed},
		{http.MethodGet, "/v1/admin/metrics", http.StatusNotFound},
	}
	for _, c := range closed {
		for name, mod := range map[string]func(*http.Request){"anonymous": nil, "admin": asAdmin, "api key": asKey(creds.Key)} {
			t.Run(c.method+" "+c.path+" as "+name, func(t *testing.T) {
				status, body := do(t, c.method, c.path, probe, mod)
				// Unauthenticated callers may be turned away first, but never let through.
				assert.NotEqual(t, http.StatusOK, status, string(body))
				if name == "admin" {
					assert.Equal(t, c.want, status, string(body))
				}
			})
		}
	}
}

// Each service reaches the database under its own role, which can touch only its own tables: the
// keys live in a schema only keystorage's role can open, and keystorage cannot read the data.
// Needs the stack's Postgres on the host (TAGONA_E2E_PG_ADDR, default localhost:5432).
func TestDatabaseRolesAreIsolated(t *testing.T) {
	addr := envOrDefault("TAGONA_E2E_PG_ADDR", "localhost:5432")
	connect := func(role, password string) *pgx.Conn {
		conn, err := pgx.Connect(context.Background(), fmt.Sprintf("postgres://%s:%s@%s/tagona?sslmode=disable", role, password, addr))
		require.NoError(t, err, "connect as %s", role)
		t.Cleanup(func() { conn.Close(context.Background()) })
		return conn
	}
	storage := connect("tagona_storage", envOrDefault("TAGONA_STORAGE_DB_PASSWORD", "tagona_storage"))
	keystorage := connect("tagona_keys", envOrDefault("TAGONA_KEYS_DB_PASSWORD", "tagona_keys"))
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Each role works on its own tables...
	var n int
	require.NoError(t, storage.QueryRow(ctx, `SELECT count(*) FROM public.collections`).Scan(&n))
	require.NoError(t, keystorage.QueryRow(ctx, `SELECT count(*) FROM keys.api_keys`).Scan(&n))

	// ...and nothing else.
	for _, q := range []struct {
		conn *pgx.Conn
		who  string
		sql  string
	}{
		{storage, "storage", `SELECT * FROM keys.api_keys`},
		{storage, "storage", `DELETE FROM keys.api_keys`},
		{storage, "storage", `CREATE TABLE keys.shadow (i int)`},
		{keystorage, "keystorage", `SELECT * FROM public.collections`},
		{keystorage, "keystorage", `SELECT * FROM public.objects`},
		{keystorage, "keystorage", `CREATE TABLE public.shadow (i int)`},
	} {
		_, err := q.conn.Exec(ctx, q.sql)
		require.Error(t, err, "%s must not be able to run: %s", q.who, q.sql)
		assert.Contains(t, err.Error(), "permission denied", "%s: %s", q.who, q.sql)
	}
}

type expiringKey struct {
	ID        string  `json:"id"`
	Key       string  `json:"key"`
	ExpiresAt *string `json:"expires_at"`
}

// A key created with ttl_seconds works until it expires, and then gets its own error code at once:
// the gateway does not keep believing a cached verdict past the key's expiry.
func TestKeyExpiry(t *testing.T) {
	status, body := do(t, http.MethodPost, "/v1/admin/api-keys", `{"name":"e2e-expiry","ttl_seconds":3}`, asAdmin)
	require.Equal(t, http.StatusCreated, status, string(body))
	var k expiringKey
	require.NoError(t, json.Unmarshal(body, &k))
	t.Cleanup(func() { do(t, http.MethodDelete, "/v1/admin/api-keys/"+k.ID, "", asAdmin) })
	require.NotNil(t, k.ExpiresAt, "a key created with a ttl reports when it expires: %s", body)
	expires, err := time.Parse(time.RFC3339Nano, *k.ExpiresAt)
	require.NoError(t, err)
	assert.WithinDuration(t, time.Now().Add(3*time.Second), expires, 2*time.Second)

	// It works, which also caches a verdict in the gateway.
	status, body = do(t, http.MethodGet, "/v1/collections", "", asKey(k.Key))
	require.Equal(t, http.StatusOK, status, string(body))

	// The list shows the expiry too.
	status, body = do(t, http.MethodGet, "/v1/admin/api-keys", "", asAdmin)
	require.Equal(t, http.StatusOK, status)
	var list struct {
		Keys []expiringKey `json:"keys"`
	}
	require.NoError(t, json.Unmarshal(body, &list))
	found := false
	for _, listed := range list.Keys {
		if listed.ID == k.ID {
			found = true
			require.NotNil(t, listed.ExpiresAt)
			assert.Equal(t, expires.UTC().Truncate(time.Millisecond), mustParse(t, *listed.ExpiresAt).UTC().Truncate(time.Millisecond))
		}
	}
	assert.True(t, found, "the key is listed")

	// Just after the expiry it is refused with the dedicated code, not after the cache would run out.
	time.Sleep(time.Until(expires) + 300*time.Millisecond)
	status, body = do(t, http.MethodGet, "/v1/collections", "", asKey(k.Key))
	require.Equal(t, http.StatusUnauthorized, status, string(body))
	assert.Equal(t, "expired_api_key", errorCode(t, body))

	// An expired key is not an unknown one: an unknown key keeps invalid_api_key.
	status, body = do(t, http.MethodGet, "/v1/collections", "", asKey("tagona_"+strings.Repeat("0", 64)))
	require.Equal(t, http.StatusUnauthorized, status)
	assert.Equal(t, "invalid_api_key", errorCode(t, body))

	// It is still listed (until swept), and can be deleted like any key.
	status, _ = do(t, http.MethodDelete, "/v1/admin/api-keys/"+k.ID, "", asAdmin)
	assert.Equal(t, http.StatusNoContent, status)
}

func mustParse(t *testing.T, s string) time.Time {
	t.Helper()
	v, err := time.Parse(time.RFC3339Nano, s)
	require.NoError(t, err)
	return v
}

func TestKeyWithoutTTLNeverExpires(t *testing.T) {
	status, body := do(t, http.MethodPost, "/v1/admin/api-keys", `{"name":"e2e-forever"}`, asAdmin)
	require.Equal(t, http.StatusCreated, status, string(body))
	var k expiringKey
	require.NoError(t, json.Unmarshal(body, &k))
	t.Cleanup(func() { do(t, http.MethodDelete, "/v1/admin/api-keys/"+k.ID, "", asAdmin) })
	assert.Nil(t, k.ExpiresAt, "no ttl_seconds, no expiry (the default configuration): %s", body)
	assert.Contains(t, string(body), `"expires_at":null`, "an explicit null, not a missing field")
}

func TestKeyTTLValidation(t *testing.T) {
	tests := []struct {
		name, body, code string
	}{
		{"zero is not no-expiry", `{"name":"x","ttl_seconds":0}`, "invalid_ttl"},
		{"negative", `{"name":"x","ttl_seconds":-1}`, "invalid_ttl"},
		{"absurdly large", `{"name":"x","ttl_seconds":1000000000000000000}`, "invalid_ttl"},
		{"a string", `{"name":"x","ttl_seconds":"60"}`, "invalid_json"},
		{"a fraction", `{"name":"x","ttl_seconds":1.5}`, "invalid_json"},
		{"misspelled", `{"name":"x","ttl":60}`, "invalid_json"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			status, body := do(t, http.MethodPost, "/v1/admin/api-keys", tt.body, asAdmin)
			assert.Equal(t, http.StatusBadRequest, status, string(body))
			assert.Equal(t, tt.code, errorCode(t, body))
		})
	}
}
