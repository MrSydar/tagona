package client

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// The tagger's own API has no version prefix: it is internal to the stack.
func TestClientUsesUnprefixedPaths(t *testing.T) {
	var seen []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.Method+" "+r.URL.Path)
		switch r.URL.Path {
		case "/supported-types":
			json.NewEncoder(w).Encode(map[string]any{"types": []string{"txt"}})
		case "/tag":
			json.NewEncoder(w).Encode(map[string]any{"tags": map[string]bool{"golang": true}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	c := New(srv.URL, time.Second)

	types, err := c.GetSupportedTypes(context.Background())
	if err != nil || len(types) != 1 || types[0] != "txt" {
		t.Fatalf("GetSupportedTypes = %v, %v", types, err)
	}
	tags, err := c.Tag(context.Background(), "jobs", "id-1", []string{"golang"})
	if err != nil || !tags["golang"] {
		t.Fatalf("Tag = %v, %v", tags, err)
	}
	if len(seen) != 2 || seen[0] != "GET /supported-types" || seen[1] != "POST /tag" {
		t.Fatalf("requests = %v", seen)
	}
}
