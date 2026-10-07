package evaluator

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// settings is a LookupFunc over a map.
func settings(kv map[string]string) LookupFunc {
	return func(key string) (string, bool) {
		v, ok := kv[key]
		return v, ok
	}
}

var testDefaults = HTTPDefaults{BaseURL: "https://vendor.example/v1", Path: "/things", Model: "m-1"}

func TestLoadHTTPConfigDefaults(t *testing.T) {
	cfg, err := LoadHTTPConfig(settings(nil), "P_", testDefaults)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.BaseURL != "https://vendor.example/v1" || cfg.Path != "/things" || cfg.Model != "m-1" ||
		cfg.Timeout != 60*time.Second || cfg.AuthHeader != "Authorization" || cfg.AuthScheme != "Bearer" ||
		cfg.APIKey != "" || cfg.Headers != nil || cfg.Query != nil || cfg.Params != nil {
		t.Fatalf("unexpected defaults: %+v", cfg)
	}
	if got := cfg.endpoint(); got != "https://vendor.example/v1/things" {
		t.Fatalf("endpoint = %q", got)
	}
}

func TestLoadHTTPConfigOverrides(t *testing.T) {
	cfg, err := LoadHTTPConfig(settings(map[string]string{
		"P_API_KEY": "k", "P_BASE_URL": "http://localhost:8000/proxy/", "P_PATH": "/v2/classify", "P_MODEL": "m-2",
		"P_TIMEOUT": "5s", "P_AUTH_HEADER": "api-key", "P_AUTH_SCHEME": "",
		"P_HEADERS": `{"OpenAI-Organization":"org-1"}`, "P_QUERY": "?api-version=2024-10-21&x=1",
		"P_PARAMS": `{"max_completion_tokens":50,"temperature":null}`,
	}), "P_", testDefaults)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.BaseURL != "http://localhost:8000/proxy" || cfg.Path != "/v2/classify" || cfg.Model != "m-2" ||
		cfg.Timeout != 5*time.Second || cfg.AuthHeader != "api-key" || cfg.AuthScheme != "" || cfg.APIKey != "k" ||
		cfg.Headers["OpenAI-Organization"] != "org-1" {
		t.Fatalf("unexpected config: %+v", cfg)
	}
	if got := cfg.endpoint(); got != "http://localhost:8000/proxy/v2/classify?api-version=2024-10-21&x=1" {
		t.Fatalf("endpoint = %q", got)
	}
}

func TestLoadHTTPConfigRejectsBadSettings(t *testing.T) {
	tests := map[string]map[string]string{
		"base url without scheme":    {"P_BASE_URL": "vendor.example/v1"},
		"base url with other scheme": {"P_BASE_URL": "ftp://vendor.example"},
		"path without slash":         {"P_PATH": "things"},
		"timeout not a duration":     {"P_TIMEOUT": "soon"},
		"zero timeout":               {"P_TIMEOUT": "0s"},
		"auth header with a space":   {"P_AUTH_HEADER": "api key"},
		"auth header host":           {"P_AUTH_HEADER": "Host"},
		"headers not json":           {"P_HEADERS": "a=b"},
		"headers not strings":        {"P_HEADERS": `{"A":1}`},
		"header content-type":        {"P_HEADERS": `{"Content-Type":"text/plain"}`},
		"header with newline":        {"P_HEADERS": `{"X\nEvil":"1"}`},
		"query malformed":            {"P_QUERY": "a=%zz"},
		"params not an object":       {"P_PARAMS": `[1]`},
		"params not json":            {"P_PARAMS": `temperature=0`},
	}
	for name, kv := range tests {
		t.Run(name, func(t *testing.T) {
			_, err := LoadHTTPConfig(settings(kv), "P_", testDefaults)
			if err == nil {
				t.Fatal("expected an error")
			}
			if !strings.Contains(err.Error(), "P_") {
				t.Errorf("the error does not say which setting is wrong: %v", err)
			}
		})
	}
	t.Run("all problems are reported at once", func(t *testing.T) {
		_, err := LoadHTTPConfig(settings(map[string]string{"P_PATH": "x", "P_TIMEOUT": "no", "P_PARAMS": "{"}), "P_", testDefaults)
		if err == nil || !strings.Contains(err.Error(), "P_PATH") || !strings.Contains(err.Error(), "P_TIMEOUT") || !strings.Contains(err.Error(), "P_PARAMS") {
			t.Fatalf("err = %v", err)
		}
	})
}

func TestMergeParams(t *testing.T) {
	cfg := HTTPConfig{Params: map[string]any{"temperature": nil, "seed": float64(7), "model": "override"}}
	got := cfg.mergeParams(map[string]any{"model": "m", "temperature": 0, "messages": []string{"x"}})
	if _, ok := got["temperature"]; ok {
		t.Error("a null param must remove the field")
	}
	if got["seed"] != float64(7) || got["model"] != "override" || got["messages"] == nil {
		t.Errorf("merged = %v", got)
	}
}

func TestPostSendsConfiguredRequest(t *testing.T) {
	var gotReq *http.Request
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotReq = r
		json.NewDecoder(r.Body).Decode(&gotBody)
		w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	tests := []struct {
		name       string
		cfg        HTTPConfig
		wantHeader string
		wantValue  string
	}{
		{"bearer by default", HTTPConfig{APIKey: "k", AuthHeader: "Authorization", AuthScheme: "Bearer"}, "Authorization", "Bearer k"},
		{"custom scheme", HTTPConfig{APIKey: "k", AuthHeader: "Authorization", AuthScheme: "Token"}, "Authorization", "Token k"},
		{"bare key in a custom header", HTTPConfig{APIKey: "k", AuthHeader: "api-key", AuthScheme: ""}, "Api-Key", "k"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := tt.cfg
			cfg.BaseURL, cfg.Path = srv.URL+"/base", "/ep"
			cfg.Headers = map[string]string{"X-Org": "o1"}
			cfg.Query = map[string][]string{"api-version": {"1"}}
			cfg.Params = map[string]any{"extra": "yes", "drop": nil}
			body, err := cfg.post(context.Background(), srv.Client(), map[string]any{"model": "m", "drop": 1})
			if err != nil || string(body) != `{"ok":true}` {
				t.Fatalf("post = %q, %v", body, err)
			}
			if gotReq.Method != "POST" || gotReq.URL.Path != "/base/ep" || gotReq.URL.Query().Get("api-version") != "1" {
				t.Errorf("request = %s %s", gotReq.Method, gotReq.URL)
			}
			if got := gotReq.Header.Get(tt.wantHeader); got != tt.wantValue {
				t.Errorf("%s = %q, want %q", tt.wantHeader, got, tt.wantValue)
			}
			if gotReq.Header.Get("X-Org") != "o1" || gotReq.Header.Get("Content-Type") != "application/json" {
				t.Errorf("headers = %v", gotReq.Header)
			}
			if gotBody["model"] != "m" || gotBody["extra"] != "yes" || gotBody["drop"] != nil {
				t.Errorf("body = %v", gotBody)
			}
		})
	}

	t.Run("no key sends no credentials", func(t *testing.T) {
		cfg := HTTPConfig{BaseURL: srv.URL, Path: "/ep", AuthHeader: "Authorization", AuthScheme: "Bearer"}
		if _, err := cfg.post(context.Background(), srv.Client(), map[string]any{}); err != nil {
			t.Fatal(err)
		}
		if gotReq.Header.Get("Authorization") != "" {
			t.Error("credentials were sent without a key")
		}
	})
}

func TestPostErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		w.Write([]byte(strings.Repeat("x", 5000)))
	}))
	cfg := HTTPConfig{BaseURL: srv.URL, Path: "/ep"}
	_, err := cfg.post(context.Background(), srv.Client(), map[string]any{})
	if err == nil || !strings.Contains(err.Error(), "429") || len(err.Error()) > 1200 {
		t.Fatalf("err = %v (a status error must say the status and keep the vendor's body short)", err)
	}
	srv.Close()
	if _, err := cfg.post(context.Background(), srv.Client(), map[string]any{}); err == nil {
		t.Fatal("expected an error for an unreachable vendor")
	}
}
