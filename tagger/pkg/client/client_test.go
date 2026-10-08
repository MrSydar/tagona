package client

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"mrsydar/tagona/storage/pkg/client"
)

// The tagger's own API has no version prefix: it is internal to the stack.
func TestClientUsesUnprefixedPaths(t *testing.T) {
	var seen []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.Method+" "+r.URL.Path)
		switch r.URL.Path {
		case "/version":
			json.NewEncoder(w).Encode(map[string]any{"version": "grep"})
		case "/tag":
			json.NewEncoder(w).Encode(map[string]any{"tags": map[string]bool{"golang": true}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	c := New(srv.URL, time.Second)

	version, err := c.Version(context.Background())
	if err != nil || version != "grep" {
		t.Fatalf("Version = %q, %v", version, err)
	}
	tags, err := c.Tag(context.Background(), "jobs", "id-1", "grep", []string{"golang"})
	if err != nil || !tags["golang"] {
		t.Fatalf("Tag = %v, %v", tags, err)
	}
	if len(seen) != 2 || seen[0] != "GET /version" || seen[1] != "POST /tag" {
		t.Fatalf("requests = %v", seen)
	}
}

func TestClientReportsAVersionMismatch(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		var req struct {
			TaggerVersion string `json:"tagger_version"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		if req.TaggerVersion != "grep" {
			w.WriteHeader(http.StatusConflict)
			json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{
				"code": "tagger_version_mismatch", "details": map[string]string{"expected": req.TaggerVersion, "running": "grep"},
			}})
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"tags": map[string]bool{"a": true}})
	}))
	defer srv.Close()
	c := New(srv.URL, time.Second)

	_, err := c.Tag(context.Background(), "jobs", "id-1", "decisions/openai:m", []string{"a"})
	var mismatch *client.VersionMismatchError
	if !errors.As(err, &mismatch) || mismatch.Expected != "decisions/openai:m" || mismatch.Running != "grep" {
		t.Fatalf("err = %v", err)
	}
	if calls != 1 {
		t.Fatalf("a mismatch was retried: %d calls", calls)
	}
	if tags, err := c.Tag(context.Background(), "jobs", "id-1", "grep", []string{"a"}); err != nil || !tags["a"] {
		t.Fatalf("a matching version: %v, %v", tags, err)
	}
}
