package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	storageclient "mrsydar/tagona/storage/pkg/client"
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
	var got struct{ Version []string }
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil || len(got.Version) != 1 || got.Version[0] != "my-grep:2" {
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
			Details struct {
				Expected string
				Running  []string
			}
		}
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &e); err != nil ||
		e.Error.Code != "tagger_version_mismatch" || e.Error.Details.Expected != "decisions/openai:m" || len(e.Error.Details.Running) != 1 || e.Error.Details.Running[0] != "grep" {
		t.Fatalf("%s (%v)", rec.Body, err)
	}
}

// recorder is an evaluator that keeps what it was given.
type recorder struct{ got evaluator.Content }

func (r *recorder) Evaluate(_ context.Context, c evaluator.Content, tags []string) (map[string]float64, error) {
	r.got = c
	return map[string]float64{tags[0]: 1}, nil
}
func (r *recorder) Version() string { return "rec" }

// The tagger hands the evaluator the object's bytes and the media type storage stored for it.
func TestTagPassesTheContentTypeToTheEvaluator(t *testing.T) {
	for _, typ := range []string{"image/png", "application/octet-stream"} {
		storage := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", typ)
			w.Write([]byte{0x89, 'P', 'N', 'G'})
		}))
		rec := &recorder{}
		h := NewServer(storageclient.NewInternal(storage.URL), rec, "rec", "rec").Router()
		out := httptest.NewRecorder()
		h.ServeHTTP(out, httptest.NewRequest(http.MethodPost, "/tag",
			strings.NewReader(`{"collection":"photos","object_id":"1","tagger_version":"rec","tags":["cat"]}`)))
		storage.Close()
		if out.Code != http.StatusOK {
			t.Fatalf("%s: %d %s", typ, out.Code, out.Body)
		}
		if rec.got.Type != typ || string(rec.got.Data) != "\x89PNG" {
			t.Errorf("%s: evaluator got %q %q", typ, rec.got.Type, rec.got.Data)
		}
	}
}
