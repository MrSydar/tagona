package server

import (
	"context"
	"encoding/json"
	"errors"
	"mrsydar/tagona/storage/pkg/client"
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

type fakeVersions struct {
	versions []string
	err      error
}

func (f fakeVersions) Versions(context.Context) ([]string, error) { return f.versions, f.err }
func (f fakeVersions) Tag(context.Context, string, string, string, []string) (map[string]bool, error) {
	return nil, errors.New("not used")
}

func serveWith(tagger client.Tagger, method, path, body string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	(&Server{tagClient: tagger}).Router().ServeHTTP(rec, httptest.NewRequest(method, path, strings.NewReader(body)))
	return rec
}

func TestListTaggersAnswersTheVersionsOfTheTaggingEngine(t *testing.T) {
	rec := serveWith(fakeVersions{versions: []string{"grep", "decisions/openai:m"}}, http.MethodGet, "/taggers", "")
	if rec.Code != http.StatusOK || strings.TrimSpace(rec.Body.String()) != `{"taggers":["grep","decisions/openai:m"]}` {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	rec = serveWith(fakeVersions{err: errors.New("down")}, http.MethodGet, "/taggers", "")
	if rec.Code != http.StatusBadGateway || !strings.Contains(rec.Body.String(), "tag_engine_error") {
		t.Fatalf("a tagger that is down: %d %s", rec.Code, rec.Body)
	}
}

// Without a tagger_version, a tagging engine that serves several versions cannot say which one is meant.
func TestCreateCollectionNeedsAVersionWhenSeveralAreServed(t *testing.T) {
	rec := serveWith(fakeVersions{versions: []string{"grep", "false"}}, http.MethodPost, "/collections", `{"name":"jobs"}`)
	var e struct {
		Error struct {
			Code    string
			Details struct{ Available []string }
		}
	}
	if rec.Code != http.StatusBadRequest || json.Unmarshal(rec.Body.Bytes(), &e) != nil ||
		e.Error.Code != "tagger_version_required" || len(e.Error.Details.Available) != 2 || e.Error.Details.Available[1] != "false" {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	rec = serveWith(fakeVersions{err: errors.New("down")}, http.MethodPost, "/collections", `{"name":"jobs"}`)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("a tagger that is down: %d %s", rec.Code, rec.Body)
	}
}
