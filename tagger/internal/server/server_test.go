package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"mrsydar/tagona/tagger/pkg/evaluator"
)

func TestRoutesHaveNoVersionPrefix(t *testing.T) {
	h := NewServer(nil, evaluator.NewGrepEvaluator(), "grep").Router()
	do := func(method, path string) int {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(method, path, strings.NewReader(`{}`)))
		return rec.Code
	}

	for _, path := range []string{"/healthz", "/readyz", "/supported-types"} {
		if code := do(http.MethodGet, path); code != http.StatusOK {
			t.Errorf("GET %s: %d", path, code)
		}
	}
	if code := do(http.MethodPost, "/tag"); code == http.StatusNotFound || code == http.StatusMethodNotAllowed {
		t.Errorf("POST /tag is not routed: %d", code)
	}
	// The old versioned paths are gone.
	for _, p := range []struct{ method, path string }{{http.MethodGet, "/v1/supported-types"}, {http.MethodPost, "/v1/tag"}} {
		if code := do(p.method, p.path); code != http.StatusNotFound {
			t.Errorf("%s %s: %d, want 404", p.method, p.path, code)
		}
	}
}
