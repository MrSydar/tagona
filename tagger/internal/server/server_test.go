package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"mrsydar/tagona/tagger/pkg/evaluator"
)

func newTestServer() *Server {
	return NewServer(nil, evaluator.NewGrepEvaluator(), "grep", "grep")
}

func TestRoutesHaveNoVersionPrefix(t *testing.T) {
	h := newTestServer().Router()
	do := func(method, path string) int {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(method, path, strings.NewReader(`{}`)))
		return rec.Code
	}

	for _, path := range []string{"/healthz", "/readyz", "/version"} {
		if code := do(http.MethodGet, path); code != http.StatusOK {
			t.Errorf("GET %s: %d", path, code)
		}
	}
	if code := do(http.MethodPost, "/tag"); code == http.StatusNotFound || code == http.StatusMethodNotAllowed {
		t.Errorf("POST /tag is not routed: %d", code)
	}
	// The old versioned paths are gone, and so are the supported data types.
	for _, p := range []struct{ method, path string }{{http.MethodGet, "/v1/version"}, {http.MethodPost, "/v1/tag"}, {http.MethodGet, "/supported-types"}} {
		if code := do(p.method, p.path); code != http.StatusNotFound {
			t.Errorf("%s %s: %d, want 404", p.method, p.path, code)
		}
	}
}

func TestVersionIsReported(t *testing.T) {
	h := NewServer(nil, evaluator.NewGrepEvaluator(), "grep", "my-grep:2").Router()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/version", nil))
	var got struct{ Version string }
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil || got.Version != "my-grep:2" {
		t.Fatalf("%s (%v)", rec.Body, err)
	}
}

// A request that asks for another version is refused before anything is fetched or evaluated (the storage
// client is nil here, so reaching it would panic).
func TestTagRefusesAnotherVersion(t *testing.T) {
	h := newTestServer().Router()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/tag",
		strings.NewReader(`{"collection":"jobs","object_id":"1","tagger_version":"decisions/openai:m","tags":["a"]}`)))
	if rec.Code != http.StatusConflict {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	var e struct {
		Error struct {
			Code    string
			Details map[string]string
		}
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &e); err != nil ||
		e.Error.Code != "tagger_version_mismatch" || e.Error.Details["expected"] != "decisions/openai:m" || e.Error.Details["running"] != "grep" {
		t.Fatalf("%s (%v)", rec.Body, err)
	}
}
