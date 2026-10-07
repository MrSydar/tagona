package client

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// New speaks the public API of the gateway (under /v1); NewInternal speaks the storage service's
// own API, which has no version prefix.
func TestPathPrefix(t *testing.T) {
	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"collections":[]}`))
	}))
	defer srv.Close()

	tests := []struct {
		name   string
		client *Client
		want   string
	}{
		{"public", New(srv.URL), "/v1/collections"},
		{"public with token", NewWithToken(srv.URL, "tagona_x"), "/v1/collections"},
		{"internal", NewInternal(srv.URL), "/collections"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			paths = nil
			if _, err := tt.client.ListCollections(context.Background()); err != nil {
				t.Fatal(err)
			}
			if len(paths) != 1 || paths[0] != tt.want {
				t.Fatalf("requested %v, want %s", paths, tt.want)
			}
		})
	}
}
