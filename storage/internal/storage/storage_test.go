package storage

import (
	"context"
	"net/url"
	"testing"
	"time"
)

// A download URL is signed for the address clients use, not the one the service uses, and for as long
// as asked. Signing is local, so no object store is needed.
func TestPresignGet(t *testing.T) {
	for _, tc := range []struct{ name, endpoint, public, wantHost string }{
		{"public endpoint", "http://garage:3900", "https://s3.example.com", "s3.example.com"},
		{"defaults to the endpoint", "http://garage:3900", "", "garage:3900"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, err := NewS3Store(tc.endpoint, tc.public, "us-east-1", "tagona", "key", "secret", true)
			if err != nil {
				t.Fatal(err)
			}
			raw, expires, err := s.PresignGet(context.Background(), "ab/cdef", 90*time.Second)
			if err != nil {
				t.Fatal(err)
			}
			u, err := url.Parse(raw)
			if err != nil {
				t.Fatal(err)
			}
			q := u.Query()
			if u.Host != tc.wantHost || u.Path != "/tagona/ab/cdef" {
				t.Errorf("URL = %s", raw)
			}
			if q.Get("X-Amz-Expires") != "90" || q.Get("X-Amz-Signature") == "" || q.Get("X-Amz-Credential") == "" {
				t.Errorf("query = %v", q)
			}
			if q.Get("X-Amz-Credential") == "" || u.User != nil || q.Get("X-Amz-Security-Token") != "" {
				t.Errorf("credentials leaked or missing: %s", raw)
			}
			if d := time.Until(expires); d < 80*time.Second || d > 90*time.Second {
				t.Errorf("expires in %s", d)
			}
		})
	}
}
