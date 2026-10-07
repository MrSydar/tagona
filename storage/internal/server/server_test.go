package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// TestListCollectionTagsValidation covers the request checks that run before
// any database access, so no database is needed.
func TestListCollectionTagsValidation(t *testing.T) {
	router := (&Server{}).Router()

	cases := []struct {
		name     string
		target   string
		wantCode string
	}{
		{"bad collection name", "/collections/Bad_Name/tags", "invalid_collection_name"},
		{"zero limit", "/collections/jobs/tags?limit=0", "invalid_limit"},
		{"negative limit", "/collections/jobs/tags?limit=-1", "invalid_limit"},
		{"non-numeric limit", "/collections/jobs/tags?limit=abc", "invalid_limit"},
		{"limit over max", "/collections/jobs/tags?limit=1001", "invalid_limit"},
		{"prefix too long", "/collections/jobs/tags?prefix=" + strings.Repeat("a", 129), "invalid_prefix"},
		{"prefix not utf-8", "/collections/jobs/tags?prefix=" + url.QueryEscape("\xff"), "invalid_prefix"},
		{"cursor not base64", "/collections/jobs/tags?cursor=!!!", "invalid_cursor"},
		{"cursor not utf-8", "/collections/jobs/tags?cursor=_w", "invalid_cursor"}, // base64url of 0xff
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			router.ServeHTTP(w, httptest.NewRequest(http.MethodGet, tc.target, nil))
			if w.Code != http.StatusBadRequest {
				t.Fatalf("expected 400, got %d: %s", w.Code, w.Body.String())
			}
			var resp struct {
				Error struct {
					Code string `json:"code"`
				} `json:"error"`
			}
			if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
				t.Fatalf("decode: %v (%s)", err, w.Body.String())
			}
			if resp.Error.Code != tc.wantCode {
				t.Errorf("code = %q, want %q", resp.Error.Code, tc.wantCode)
			}
		})
	}
}

// evaluate is validated before any database access.
func TestGetObjectTagsEvaluateValidation(t *testing.T) {
	router := (&Server{}).Router()
	w := httptest.NewRecorder()
	router.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/collections/jobs/objects/abc/tags?evaluate=maybe", nil))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", w.Code, w.Body.String())
	}
	var resp struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil || resp.Error.Code != "invalid_evaluate" {
		t.Errorf("body = %s, want invalid_evaluate", w.Body.String())
	}
}
