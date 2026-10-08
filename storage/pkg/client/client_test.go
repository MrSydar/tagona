package client

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
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

func TestListCollectionsFollowsThePages(t *testing.T) {
	var queries []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		queries = append(queries, r.URL.RawQuery)
		switch r.URL.Query().Get("cursor") {
		case "":
			w.Write([]byte(`{"collections":[{"name":"c"},{"name":"b"}],"next":"p2"}`))
		case "p2":
			w.Write([]byte(`{"collections":[{"name":"a"}]}`))
		default:
			http.Error(w, "bad cursor", http.StatusBadRequest)
		}
	}))
	defer srv.Close()
	c := New(srv.URL)

	all, err := c.ListCollections(context.Background())
	if err != nil || len(all) != 3 || all[2].Name != "a" {
		t.Fatalf("%v, %v", all, err)
	}
	if len(queries) != 2 || queries[0] != "" || queries[1] != "cursor=p2" {
		t.Fatalf("queries = %q", queries)
	}
	page, err := c.ListCollectionsPage(context.Background(), ListCollectionsOptions{Limit: 2})
	if err != nil || page.Next != "p2" || queries[2] != "limit=2" {
		t.Fatalf("%+v, %v, %q", page, err, queries)
	}
}

func TestCreateCollectionSendsTheTaggerVersionOnlyWhenGiven(t *testing.T) {
	var bodies []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		bodies = append(bodies, string(b))
		w.WriteHeader(http.StatusCreated)
		w.Write([]byte(`{"id":"1","name":"jobs","tagger_version":"grep"}`))
	}))
	defer srv.Close()
	c := New(srv.URL)
	if coll, err := c.CreateCollection(context.Background(), "jobs", ""); err != nil || coll.TaggerVersion != "grep" {
		t.Fatalf("%+v, %v", coll, err)
	}
	if _, err := c.CreateCollection(context.Background(), "jobs", "decisions/openai:m"); err != nil {
		t.Fatal(err)
	}
	if bodies[0] != `{"name":"jobs"}` || bodies[1] != `{"name":"jobs","tagger_version":"decisions/openai:m"}` {
		t.Fatalf("bodies = %q", bodies)
	}
}

func TestUploadAndMetadataRequests(t *testing.T) {
	type seen struct{ method, uri, body string }
	var got []seen
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		got = append(got, seen{r.Method, r.URL.RequestURI(), string(b)})
		if r.Method == http.MethodPost {
			w.WriteHeader(http.StatusCreated)
			w.Write([]byte(`{"id":"1","metadata":{"name":"cute-dog.png"}}`))
			return
		}
		w.Write([]byte(`{"id":"1","metadata":{"a":"b"}}`))
	}))
	defer srv.Close()
	c := New(srv.URL)
	ctx := context.Background()

	up, err := c.UploadObjectWithOptions(ctx, "jobs", []byte("hello"), UploadOptions{TTLSeconds: 60, Metadata: map[string]string{"name": "cute-dog.png"}})
	if err != nil || up.Metadata["name"] != "cute-dog.png" {
		t.Fatalf("%+v, %v", up, err)
	}
	if want := "/v1/collections/jobs/objects?metadata=%7B%22name%22%3A%22cute-dog.png%22%7D&ttl_seconds=60"; got[0].uri != want || got[0].body != "hello" {
		t.Fatalf("upload: %+v", got[0])
	}
	if _, err := c.UploadObject(ctx, "jobs", []byte("x"), time.Time{}, 0); err != nil || got[1].uri != "/v1/collections/jobs/objects" {
		t.Fatalf("a plain upload: %+v, %v", got[1], err)
	}

	if _, err := c.ReplaceObjectMetadata(ctx, "jobs", "1", map[string]string{"a": "b"}); err != nil {
		t.Fatal(err)
	}
	remove := (*string)(nil)
	keep := "v"
	if _, err := c.MergeObjectMetadata(ctx, "jobs", "1", map[string]*string{"gone": remove, "k": &keep}); err != nil {
		t.Fatal(err)
	}
	if got[2] != (seen{"PUT", "/v1/collections/jobs/objects/1/metadata", `{"a":"b"}`}) {
		t.Fatalf("replace: %+v", got[2])
	}
	if got[3] != (seen{"PATCH", "/v1/collections/jobs/objects/1/metadata", `{"gone":null,"k":"v"}`}) {
		t.Fatalf("merge: %+v", got[3])
	}
}

func TestListTaggers(t *testing.T) {
	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		w.Write([]byte(`{"taggers":["grep","decisions/openai:m"]}`))
	}))
	defer srv.Close()
	for _, c := range []*Client{New(srv.URL), NewInternal(srv.URL)} {
		got, err := c.ListTaggers(context.Background())
		if err != nil || len(got) != 2 || got[1] != "decisions/openai:m" {
			t.Fatalf("%v, %v", got, err)
		}
	}
	if paths[0] != "/v1/taggers" || paths[1] != "/taggers" {
		t.Fatalf("paths = %v", paths)
	}
}
