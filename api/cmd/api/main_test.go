package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"mrsydar/tagona/api/internal/keystorageapi"
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

// knownKeyID is the id of the key the stub's admin delete route knows.
const knownKeyID = "00000000-0000-4000-8000-000000000001"

func newStorageStub(t *testing.T, validKey string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api-keys/validate":
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
			if req.Key == "tagona_expired" {
				writeError(w, http.StatusUnauthorized, "expired_api_key", "api key has expired")
				return
			}
			writeError(w, http.StatusUnauthorized, "invalid_api_key", "invalid or unknown api key")
		case r.Method == http.MethodDelete && r.URL.Path == "/api-keys/"+knownKeyID:
			w.WriteHeader(http.StatusNoContent)
		case r.URL.Path == "/collections":
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]any{"collections": []any{}})
		default:
			writeError(w, http.StatusNotFound, "not_found", "not found")
		}
	}))
}

func TestAPIKeyAuth(t *testing.T) {
	validKey := "tagona_" + strings.Repeat("ab", 32)
	backend := newStorageStub(t, validKey)
	defer backend.Close()

	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"collections": []any{}})
	})
	h := apiKeyAuth(keystorageapi.New(backend.URL), next)

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

	t.Run("expired key gets its own code", func(t *testing.T) {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, newRequest(t, http.MethodGet, "/v1/collections", func(r *http.Request) {
			r.Header.Set("Authorization", "Bearer tagona_expired")
		}))
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("expected 401, got %d: %s", w.Code, w.Body.String())
		}
		if code := decodeErrorCode(t, w.Body.Bytes()); code != "expired_api_key" {
			t.Errorf("expected expired_api_key, got %q", code)
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
		h := apiKeyAuth(keystorageapi.New(dead.URL), http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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

func TestRouter(t *testing.T) {
	validKey := "tagona_" + strings.Repeat("ab", 32)
	backend := newStorageStub(t, validKey)
	defer backend.Close()
	router := newTestRouter(t, backend.URL, gatewayConfig{})

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

	t.Run("unknown bearer key rejected", func(t *testing.T) {
		w := httptest.NewRecorder()
		router.ServeHTTP(w, newRequest(t, http.MethodGet, "/v1/collections", func(r *http.Request) {
			r.Header.Set("Authorization", "Bearer tagona_unknown")
		}))
		if w.Code != http.StatusUnauthorized || decodeErrorCode(t, w.Body.Bytes()) != "invalid_api_key" {
			t.Fatalf("got %d: %s", w.Code, w.Body.String())
		}
	})

	t.Run("proxied path without key rejected by router", func(t *testing.T) {
		w := httptest.NewRecorder()
		router.ServeHTTP(w, newRequest(t, http.MethodGet, "/v1/collections", nil))
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

func TestReadyzNeedsEveryUpstream(t *testing.T) {
	status := func(code *int) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(*code) }))
	}
	storageCode, keyCode := http.StatusOK, http.StatusOK
	storage, keystorage := status(&storageCode), status(&keyCode)
	defer storage.Close()
	defer keystorage.Close()

	h := readyz([]string{storage.URL, keystorage.URL}, http.DefaultClient)
	probe := func() int {
		w := httptest.NewRecorder()
		h(w, newRequest(t, http.MethodGet, "/readyz", nil))
		return w.Code
	}
	if got := probe(); got != http.StatusOK {
		t.Fatalf("both ready: %d", got)
	}
	keyCode = http.StatusServiceUnavailable
	if got := probe(); got != http.StatusServiceUnavailable {
		t.Fatalf("keystorage down must make the api not ready: %d", got)
	}
	keyCode, storageCode = http.StatusOK, http.StatusServiceUnavailable
	if got := probe(); got != http.StatusServiceUnavailable {
		t.Fatalf("storage down must make the api not ready: %d", got)
	}
	storage.Close()
	if got := probe(); got != http.StatusServiceUnavailable {
		t.Fatalf("unreachable upstream must make the api not ready: %d", got)
	}
}
