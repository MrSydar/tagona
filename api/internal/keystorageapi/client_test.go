package keystorageapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestValidateKey(t *testing.T) {
	var gotPath, gotKey string
	status := http.StatusOK
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.Method + " " + r.URL.Path
		var body struct{ Key string }
		_ = json.NewDecoder(r.Body).Decode(&body)
		gotKey = body.Key
		w.WriteHeader(status)
	}))
	defer srv.Close()
	c := New(srv.URL + "/")

	valid, err := c.ValidateKey(context.Background(), "tagona_abc")
	if err != nil || !valid {
		t.Fatalf("valid key: %v, %v", valid, err)
	}
	if gotPath != "POST /api-keys/validate" || gotKey != "tagona_abc" {
		t.Fatalf("request = %s with key %q", gotPath, gotKey)
	}

	status = http.StatusUnauthorized
	if valid, err := c.ValidateKey(context.Background(), "x"); err != nil || valid {
		t.Fatalf("unknown key: %v, %v", valid, err)
	}

	status = http.StatusInternalServerError
	if _, err := c.ValidateKey(context.Background(), "x"); err == nil {
		t.Fatal("a 500 must be an error, not a verdict")
	}

	srv.Close()
	if _, err := c.ValidateKey(context.Background(), "x"); err == nil {
		t.Fatal("an unreachable keystorage must be an error, not a verdict")
	}
}
