package keystorageapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestValidateKey(t *testing.T) {
	var gotPath, gotKey string
	status := http.StatusOK
	reply := `{"id":"k1","name":"dev","expires_at":null}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.Method + " " + r.URL.Path
		var body struct{ Key string }
		_ = json.NewDecoder(r.Body).Decode(&body)
		gotKey = body.Key
		w.WriteHeader(status)
		w.Write([]byte(reply))
	}))
	defer srv.Close()
	c := New(srv.URL + "/")

	res, err := c.ValidateKey(context.Background(), "tagona_abc")
	if err != nil || res.Verdict != Valid || res.ExpiresAt != nil {
		t.Fatalf("a key that never expires: %+v, %v", res, err)
	}
	if gotPath != "POST /api-keys/validate" || gotKey != "tagona_abc" {
		t.Fatalf("request = %s with key %q", gotPath, gotKey)
	}

	exp := time.Now().Add(time.Hour).UTC().Truncate(time.Second)
	reply = `{"id":"k1","name":"dev","expires_at":"` + exp.Format(time.RFC3339) + `"}`
	if res, err := c.ValidateKey(context.Background(), "x"); err != nil || res.Verdict != Valid || res.ExpiresAt == nil || !res.ExpiresAt.Equal(exp) {
		t.Fatalf("a key with an expiry: %+v, %v", res, err)
	}

	status = http.StatusUnauthorized
	reply = `{"error":{"code":"invalid_api_key","message":"x"}}`
	if res, err := c.ValidateKey(context.Background(), "x"); err != nil || res.Verdict != Unknown {
		t.Fatalf("unknown key: %+v, %v", res, err)
	}
	reply = `{"error":{"code":"expired_api_key","message":"x"}}`
	if res, err := c.ValidateKey(context.Background(), "x"); err != nil || res.Verdict != Expired {
		t.Fatalf("expired key: %+v, %v", res, err)
	}
	reply = `not json`
	if res, err := c.ValidateKey(context.Background(), "x"); err != nil || res.Verdict != Unknown {
		t.Fatalf("a 401 that says nothing useful is still a refusal: %+v, %v", res, err)
	}

	status = http.StatusInternalServerError
	if _, err := c.ValidateKey(context.Background(), "x"); err == nil {
		t.Fatal("a 500 must be an error, not a verdict")
	}
	status, reply = http.StatusOK, `not json`
	if res, err := c.ValidateKey(context.Background(), "x"); err == nil {
		t.Fatalf("an unreadable 200 must not become a verdict: %+v", res)
	}

	srv.Close()
	if _, err := c.ValidateKey(context.Background(), "x"); err == nil {
		t.Fatal("an unreachable keystorage must be an error, not a verdict")
	}
}
