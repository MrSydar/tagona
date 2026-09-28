package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"mrsydar/tagona/api/internal/storageapi"
)

type errResp struct {
	Error struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

func decodeErrorCode(t *testing.T, body []byte) string {
	t.Helper()
	var e errResp
	if err := json.Unmarshal(body, &e); err != nil {
		t.Fatalf("decode error body: %v (%s)", err, string(body))
	}
	return e.Error.Code
}

func newRequest(t *testing.T, method, target string, setAuth func(*http.Request)) *http.Request {
	t.Helper()
	req := httptest.NewRequest(method, target, nil)
	if setAuth != nil {
		setAuth(req)
	}
	return req
}

func newStorageStub(t *testing.T, validKey string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/internal/v1/api-keys/validate":
			var req struct {
				Key string `json:"key"`
			}
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				t.Errorf("stub: decode validate body: %v", err)
				return
			}
			if req.Key == validKey {
				w.Header().Set("Content-Type", "application/json")
				json.NewEncoder(w).Encode(map[string]string{"id": "key-id", "name": "test"})
				return
			}
			writeError(w, http.StatusUnauthorized, "invalid_api_key", "invalid or unknown api key")
		case r.Method == http.MethodPost && r.URL.Path == "/internal/v1/api-keys":
			var req struct {
				Name string `json:"name"`
			}
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				t.Errorf("stub: decode create body: %v", err)
				return
			}
			if req.Name == "" {
				writeError(w, http.StatusBadRequest, "invalid_name", "api key name cannot be empty")
				return
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
			json.NewEncoder(w).Encode(map[string]any{
				"id": "new-key-id", "name": req.Name, "key": "tagona_raw", "created_at": time.Now(),
			})
		case r.Method == http.MethodGet && r.URL.Path == "/internal/v1/api-keys":
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]any{"keys": []any{}})
		case r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, "/internal/v1/api-keys/"):
			if id := strings.TrimPrefix(r.URL.Path, "/internal/v1/api-keys/"); id == "known-id" {
				w.WriteHeader(http.StatusNoContent)
				return
			}
			writeError(w, http.StatusNotFound, "not_found", "api key not found")
		case r.URL.Path == "/v1/collections":
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]any{"collections": []any{}})
		default:
			writeError(w, http.StatusNotFound, "not_found", "not found")
		}
	}))
}

func TestAdminAuth(t *testing.T) {
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("ok"))
	})

	t.Run("valid basic credentials pass", func(t *testing.T) {
		h := adminAuth("admin", "secret", next)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, newRequest(t, http.MethodGet, "/", func(r *http.Request) {
			r.SetBasicAuth("admin", "secret")
		}))
		if w.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
		}
	})

	t.Run("wrong password rejected", func(t *testing.T) {
		h := adminAuth("admin", "secret", next)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, newRequest(t, http.MethodGet, "/", func(r *http.Request) {
			r.SetBasicAuth("admin", "wrong")
		}))
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("expected 401, got %d: %s", w.Code, w.Body.String())
		}
		if got := w.Header().Get("WWW-Authenticate"); got != `Basic realm="tagona-admin"` {
			t.Errorf("expected WWW-Authenticate Basic realm, got %q", got)
		}
		if code := decodeErrorCode(t, w.Body.Bytes()); code != "invalid_admin_credentials" {
			t.Errorf("expected invalid_admin_credentials, got %q", code)
		}
	})

	t.Run("wrong username rejected", func(t *testing.T) {
		h := adminAuth("admin", "secret", next)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, newRequest(t, http.MethodGet, "/", func(r *http.Request) {
			r.SetBasicAuth("user", "secret")
		}))
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("expected 401, got %d: %s", w.Code, w.Body.String())
		}
	})

	t.Run("bearer api key rejected on admin endpoints", func(t *testing.T) {
		h := adminAuth("admin", "secret", next)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, newRequest(t, http.MethodGet, "/", func(r *http.Request) {
			r.Header.Set("Authorization", "Bearer tagona_key")
		}))
		if w.Code != http.StatusForbidden {
			t.Fatalf("expected 403, got %d: %s", w.Code, w.Body.String())
		}
		if code := decodeErrorCode(t, w.Body.Bytes()); code != "forbidden" {
			t.Errorf("expected forbidden, got %q", code)
		}
	})

	t.Run("unset credentials disable admin endpoints", func(t *testing.T) {
		h := adminAuth("", "", next)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, newRequest(t, http.MethodGet, "/", func(r *http.Request) {
			r.SetBasicAuth("admin", "secret")
		}))
		if w.Code != http.StatusForbidden {
			t.Fatalf("expected 403, got %d: %s", w.Code, w.Body.String())
		}
		if code := decodeErrorCode(t, w.Body.Bytes()); code != "admin_disabled" {
			t.Errorf("expected admin_disabled, got %q", code)
		}
	})

	t.Run("missing authorization rejected", func(t *testing.T) {
		h := adminAuth("admin", "secret", next)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, newRequest(t, http.MethodGet, "/", nil))
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("expected 401, got %d: %s", w.Code, w.Body.String())
		}
		if code := decodeErrorCode(t, w.Body.Bytes()); code != "invalid_admin_credentials" {
			t.Errorf("expected invalid_admin_credentials, got %q", code)
		}
	})
}

func TestAPIKeyAuth(t *testing.T) {
	validKey := "tagona_" + strings.Repeat("ab", 32)
	backend := newStorageStub(t, validKey)
	defer backend.Close()

	target, err := url.Parse(backend.URL)
	if err != nil {
		t.Fatalf("parse stub url: %v", err)
	}
	proxy := httputil.NewSingleHostReverseProxy(target)
	h := apiKeyAuth(storageapi.New(backend.URL), proxy)

	t.Run("no token rejected", func(t *testing.T) {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, newRequest(t, http.MethodGet, "/v1/collections", nil))
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("expected 401, got %d: %s", w.Code, w.Body.String())
		}
		if code := decodeErrorCode(t, w.Body.Bytes()); code != "missing_api_key" {
			t.Errorf("expected missing_api_key, got %q", code)
		}
	})

	t.Run("malformed scheme rejected", func(t *testing.T) {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, newRequest(t, http.MethodGet, "/v1/collections", func(r *http.Request) {
			r.Header.Set("Authorization", "Basic dXNlcjpwYXNz")
		}))
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("expected 401, got %d: %s", w.Code, w.Body.String())
		}
		if code := decodeErrorCode(t, w.Body.Bytes()); code != "missing_api_key" {
			t.Errorf("expected missing_api_key, got %q", code)
		}
	})

	t.Run("invalid key rejected", func(t *testing.T) {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, newRequest(t, http.MethodGet, "/v1/collections", func(r *http.Request) {
			r.Header.Set("Authorization", "Bearer tagona_unknown")
		}))
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("expected 401, got %d: %s", w.Code, w.Body.String())
		}
		if code := decodeErrorCode(t, w.Body.Bytes()); code != "invalid_api_key" {
			t.Errorf("expected invalid_api_key, got %q", code)
		}
	})

	t.Run("valid key proxied through", func(t *testing.T) {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, newRequest(t, http.MethodGet, "/v1/collections", func(r *http.Request) {
			r.Header.Set("Authorization", "Bearer "+validKey)
		}))
		if w.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
		}
		var result struct {
			Collections []any `json:"collections"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
			t.Fatalf("decode proxied response: %v (%s)", err, w.Body.String())
		}
	})

	t.Run("storage unreachable returns not_ready", func(t *testing.T) {
		dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
		dead.Close()
		h := apiKeyAuth(storageapi.New(dead.URL), http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			t.Error("next handler should not be called")
		}))
		w := httptest.NewRecorder()
		h.ServeHTTP(w, newRequest(t, http.MethodGet, "/v1/collections", func(r *http.Request) {
			r.Header.Set("Authorization", "Bearer "+validKey)
		}))
		if w.Code != http.StatusServiceUnavailable {
			t.Fatalf("expected 503, got %d: %s", w.Code, w.Body.String())
		}
		if code := decodeErrorCode(t, w.Body.Bytes()); code != "not_ready" {
			t.Errorf("expected not_ready, got %q", code)
		}
	})
}

func TestAdminHandlers(t *testing.T) {
	validKey := "tagona_" + strings.Repeat("ab", 32)
	backend := newStorageStub(t, validKey)
	defer backend.Close()

	keyClient := storageapi.New(backend.URL)

	t.Run("create forwards storage response", func(t *testing.T) {
		w := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"name":"dev"}`))
		createAPIKey(keyClient).ServeHTTP(w, req)
		if w.Code != http.StatusCreated {
			t.Fatalf("expected 201, got %d: %s", w.Code, w.Body.String())
		}
		var created storageapi.APIKeyCreated
		if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil {
			t.Fatalf("decode created key: %v (%s)", err, w.Body.String())
		}
		if created.ID != "new-key-id" || created.Key != "tagona_raw" || created.Name != "dev" {
			t.Errorf("unexpected created key: %+v", created)
		}
	})

	t.Run("create invalid body rejected", func(t *testing.T) {
		w := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`not-json`))
		createAPIKey(keyClient).ServeHTTP(w, req)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("expected 400, got %d: %s", w.Code, w.Body.String())
		}
		if code := decodeErrorCode(t, w.Body.Bytes()); code != "invalid_json" {
			t.Errorf("expected invalid_json, got %q", code)
		}
	})

	t.Run("create empty name forwards storage 400", func(t *testing.T) {
		w := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"name":""}`))
		createAPIKey(keyClient).ServeHTTP(w, req)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("expected 400, got %d: %s", w.Code, w.Body.String())
		}
		if code := decodeErrorCode(t, w.Body.Bytes()); code != "invalid_name" {
			t.Errorf("expected invalid_name, got %q", code)
		}
	})

	t.Run("create with storage unreachable returns not_ready", func(t *testing.T) {
		w := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"name":"dev"}`))
		h := createAPIKey(storageapi.New("http://127.0.0.1:1"))
		h.ServeHTTP(w, req)
		if w.Code != http.StatusServiceUnavailable {
			t.Fatalf("expected 503, got %d: %s", w.Code, w.Body.String())
		}
		if code := decodeErrorCode(t, w.Body.Bytes()); code != "not_ready" {
			t.Errorf("expected not_ready, got %q", code)
		}
	})

	t.Run("list returns keys", func(t *testing.T) {
		w := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		listAPIKeys(keyClient).ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
		}
		var result struct {
			Keys []storageapi.APIKeyInfo `json:"keys"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
			t.Fatalf("decode keys: %v (%s)", err, w.Body.String())
		}
		if result.Keys == nil || len(result.Keys) != 0 {
			t.Errorf("expected empty non-nil keys, got %+v", result.Keys)
		}
	})

	t.Run("delete known id returns 204", func(t *testing.T) {
		w := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodDelete, "/", nil)
		h := deleteAPIKeyWithID(keyClient, "known-id")
		h.ServeHTTP(w, req)
		if w.Code != http.StatusNoContent {
			t.Fatalf("expected 204, got %d: %s", w.Code, w.Body.String())
		}
	})

	t.Run("delete unknown id forwards 404", func(t *testing.T) {
		w := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodDelete, "/", nil)
		h := deleteAPIKeyWithID(keyClient, "unknown-id")
		h.ServeHTTP(w, req)
		if w.Code != http.StatusNotFound {
			t.Fatalf("expected 404, got %d: %s", w.Code, w.Body.String())
		}
		if code := decodeErrorCode(t, w.Body.Bytes()); code != "not_found" {
			t.Errorf("expected not_found, got %q", code)
		}
	})
}

func deleteAPIKeyWithID(keyClient *storageapi.Client, id string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rctx := chi.NewRouteContext()
		rctx.URLParams.Add("id", id)
		r = r.WithContext(context.WithValue(r.Context(), chi.RouteCtxKey, rctx))
		deleteAPIKey(keyClient).ServeHTTP(w, r)
	})
}

func TestRouterAdminRoutesTakePrecedence(t *testing.T) {
	validKey := "tagona_" + strings.Repeat("ab", 32)
	backend := newStorageStub(t, validKey)
	defer backend.Close()

	target, err := url.Parse(backend.URL)
	if err != nil {
		t.Fatalf("parse stub url: %v", err)
	}
	proxy := httputil.NewSingleHostReverseProxy(target)
	router := newRouter(backend.URL, proxy, storageapi.New(backend.URL), "admin", "tagona")

	t.Run("bearer on proxied path passes key enforcement", func(t *testing.T) {
		w := httptest.NewRecorder()
		req := newRequest(t, http.MethodGet, "/v1/collections", func(r *http.Request) {
			r.Header.Set("Authorization", "Bearer "+validKey)
		})
		router.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
		}
	})

	t.Run("admin list with basic auth hits admin handler", func(t *testing.T) {
		w := httptest.NewRecorder()
		req := newRequest(t, http.MethodGet, "/v1/admin/api-keys", func(r *http.Request) {
			r.SetBasicAuth("admin", "tagona")
		})
		router.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
		}
		var result struct {
			Keys []any `json:"keys"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
			t.Fatalf("decode keys: %v (%s)", err, w.Body.String())
		}
		if result.Keys == nil {
			t.Errorf("expected non-nil keys array")
		}
	})

	t.Run("admin create with basic auth hits admin handler", func(t *testing.T) {
		w := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/v1/admin/api-keys", strings.NewReader(`{"name":"dev"}`))
		req.SetBasicAuth("admin", "tagona")
		router.ServeHTTP(w, req)
		if w.Code != http.StatusCreated {
			t.Fatalf("expected 201, got %d: %s", w.Code, w.Body.String())
		}
	})

	t.Run("admin delete with basic auth hits admin handler", func(t *testing.T) {
		w := httptest.NewRecorder()
		req := newRequest(t, http.MethodDelete, "/v1/admin/api-keys/known-id", func(r *http.Request) {
			r.SetBasicAuth("admin", "tagona")
		})
		router.ServeHTTP(w, req)
		if w.Code != http.StatusNoContent {
			t.Fatalf("expected 204, got %d: %s", w.Code, w.Body.String())
		}
	})

	t.Run("admin endpoint with bearer key rejected by router", func(t *testing.T) {
		w := httptest.NewRecorder()
		req := newRequest(t, http.MethodGet, "/v1/admin/api-keys", func(r *http.Request) {
			r.Header.Set("Authorization", "Bearer "+validKey)
		})
		router.ServeHTTP(w, req)
		if w.Code != http.StatusForbidden {
			t.Fatalf("expected 403, got %d: %s", w.Code, w.Body.String())
		}
		if code := decodeErrorCode(t, w.Body.Bytes()); code != "forbidden" {
			t.Errorf("expected forbidden, got %q", code)
		}
	})

	t.Run("admin endpoint without auth rejected by router", func(t *testing.T) {
		w := httptest.NewRecorder()
		req := newRequest(t, http.MethodGet, "/v1/admin/api-keys", nil)
		router.ServeHTTP(w, req)
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("expected 401, got %d: %s", w.Code, w.Body.String())
		}
	})

	t.Run("proxied path without key rejected by router", func(t *testing.T) {
		w := httptest.NewRecorder()
		req := newRequest(t, http.MethodGet, "/v1/collections", nil)
		router.ServeHTTP(w, req)
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("expected 401, got %d: %s", w.Code, w.Body.String())
		}
		if code := decodeErrorCode(t, w.Body.Bytes()); code != "missing_api_key" {
			t.Errorf("expected missing_api_key, got %q", code)
		}
	})

	t.Run("health endpoints stay unauthenticated", func(t *testing.T) {
		for _, path := range []string{"/healthz", "/metrics"} {
			w := httptest.NewRecorder()
			router.ServeHTTP(w, newRequest(t, http.MethodGet, path, nil))
			if w.Code != http.StatusOK {
				t.Errorf("expected 200 for %s, got %d", path, w.Code)
			}
		}
	})
}
