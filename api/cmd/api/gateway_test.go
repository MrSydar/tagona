package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"

	"mrsydar/tagona/api/internal/keystorageapi"
)

type seenRequest struct {
	method string
	path   string // path and query as sent: Request-URI
	header http.Header
	body   string
}

// newReplyingStorage is a stub for both internal services. It accepts any key on the validation
// endpoint, records every other request, and answers those with reply (200 and `{}` when nil).
func newReplyingStorage(t *testing.T, reply http.HandlerFunc) (*httptest.Server, func() []seenRequest) {
	t.Helper()
	var mu sync.Mutex
	var seen []seenRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api-keys/validate" {
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"id":"key-id","name":"test"}`))
			return
		}
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		seen = append(seen, seenRequest{r.Method, r.URL.RequestURI(), r.Header.Clone(), string(body)})
		mu.Unlock()
		if reply != nil {
			reply(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{}`))
	}))
	t.Cleanup(srv.Close)
	return srv, func() []seenRequest {
		mu.Lock()
		defer mu.Unlock()
		return append([]seenRequest(nil), seen...)
	}
}

func newRecordingStorage(t *testing.T) (*httptest.Server, func() []seenRequest) {
	return newReplyingStorage(t, nil)
}

// newTestGateway wires a gateway to a single stub that plays both storage and keystorage.
func newTestGateway(t *testing.T, backendURL string) *gateway {
	t.Helper()
	rt := newStorageTransport() // the production transport, which adds no headers of its own
	storage, err := newUpstream("storage", backendURL, rt, http.StatusBadGateway, "bad_gateway", "storage service not available")
	if err != nil {
		t.Fatal(err)
	}
	keystorage, err := newUpstream("keystorage", backendURL, rt, http.StatusServiceUnavailable, "not_ready", "key service not available", "WWW-Authenticate")
	if err != nil {
		t.Fatal(err)
	}
	return &gateway{storage: storage, keystorage: keystorage, maxBodyBytes: defaultMaxBodyBytes}
}

func newGatewayRouter(t *testing.T, backendURL string, cfg gatewayConfig) http.Handler {
	t.Helper()
	return newRouterFor(t, newTestGateway(t, backendURL), backendURL, cfg)
}

func newRouterFor(t *testing.T, g *gateway, backendURL string, cfg gatewayConfig) http.Handler {
	t.Helper()
	if cfg.validator == nil {
		cfg.validator = keystorageapi.New(backendURL)
	}
	g.onKeyDeleted = cfg.onKeyDeleted
	return newRouter([]string{backendURL}, g, cfg)
}

// newTestRouter is newGatewayRouter under its other name, kept for the tests that predate it.
func newTestRouter(t *testing.T, backendURL string, cfg gatewayConfig) http.Handler {
	return newGatewayRouter(t, backendURL, cfg)
}

func bearer(r *http.Request) { r.Header.Set("Authorization", "Bearer tagona_anykey") }

const objID = "00000000-0000-4000-8000-0000000000bb"

type call struct {
	name, method, target, body string
	headers                    map[string]string
}

func (c call) request() *http.Request {
	req := httptest.NewRequest(c.method, c.target, strings.NewReader(c.body))
	if c.body == "" {
		req.Body, req.ContentLength = http.NoBody, 0
	}
	bearer(req)
	for k, v := range c.headers {
		req.Header.Set(k, v)
	}
	return req
}

func sameJSON(t *testing.T, got, want string) {
	t.Helper()
	var g, w any
	if err := json.Unmarshal([]byte(got), &g); err != nil {
		t.Fatalf("upstream body is not JSON: %q", got)
	}
	if err := json.Unmarshal([]byte(want), &w); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(g, w) {
		t.Errorf("upstream body = %s, want %s", got, want)
	}
}

// Every public route turns into a request the gateway built itself, from validated and
// normalized values.
func TestRoutesBuildCleanRequests(t *testing.T) {
	tests := []struct {
		call
		wantMethod, wantURI, wantBody string // wantBody "" means none; JSON compared structurally
	}{
		{call{name: "list collections", method: "GET", target: "/v1/collections"}, "GET", "/collections", ""},
		{call{name: "list taggers", method: "GET", target: "/v1/taggers"}, "GET", "/taggers", ""},
		{call{name: "list collections, paged", method: "GET", target: "/v1/collections?limit=007&cursor=abc_-"},
			"GET", "/collections?cursor=abc_-&limit=7", ""},
		{call{name: "list collections, empty params dropped", method: "GET", target: "/v1/collections?limit=&cursor="}, "GET", "/collections", ""},
		{call{name: "create collection", method: "POST", target: "/v1/collections", body: `{"name":"jobs"}`},
			"POST", "/collections", `{"name":"jobs"}`},
		{call{name: "create collection with a tagger version", method: "POST", target: "/v1/collections", body: `{"name":"jobs","tagger_version":"decisions/openai:zai-org/GLM-5.3-Flash"}`},
			"POST", "/collections", `{"name":"jobs","tagger_version":"decisions/openai:zai-org/GLM-5.3-Flash"}`},
		{call{name: "delete collection", method: "DELETE", target: "/v1/collections/jobs"}, "DELETE", "/collections/jobs", ""},
		{call{name: "collection tags", method: "GET", target: "/v1/collections/jobs/tags?limit=007&prefix=go&cursor=abc_-"},
			"GET", "/collections/jobs/tags?cursor=abc_-&limit=7&prefix=go", ""},
		{call{name: "collection tags, empty params dropped", method: "GET", target: "/v1/collections/jobs/tags?limit=&prefix="},
			"GET", "/collections/jobs/tags", ""},
		{call{name: "upload", method: "POST", target: "/v1/collections/jobs/objects?ttl_seconds=0060&date=2026-01-02T03:04:05%2B02:00", body: "hello"},
			"POST", "/collections/jobs/objects?date=2026-01-02T01%3A04%3A05Z&ttl_seconds=60", "hello"},
		{call{name: "upload with metadata, re-encoded", method: "POST", target: "/v1/collections/jobs/objects?metadata=%7B%20%22name%22%3A%22cute-dog.png%22%2C%22a%22%3A%22b%22%20%7D", body: "hello"},
			"POST", "/collections/jobs/objects?metadata=%7B%22a%22%3A%22b%22%2C%22name%22%3A%22cute-dog.png%22%7D", "hello"},
		{call{name: "replace metadata", method: "PUT", target: "/v1/collections/jobs/objects/" + objID + "/metadata", body: `{"name":"cute-dog.png"}`},
			"PUT", "/collections/jobs/objects/" + objID + "/metadata", `{"name":"cute-dog.png"}`},
		{call{name: "upload with forced tags, re-encoded", method: "POST", target: "/v1/collections/jobs/objects?tags=%7B%20%22qa%22%3Afalse%2C%22go%22%3Atrue%20%7D", body: "hello"},
			"POST", "/collections/jobs/objects?tags=%7B%22go%22%3Atrue%2C%22qa%22%3Afalse%7D", "hello"},
		{call{name: "force tags of an object", method: "PATCH", target: "/v1/collections/jobs/objects/" + objID + "/tags", body: `{"go":true,"qa":false,"old":null}`},
			"PATCH", "/collections/jobs/objects/" + objID + "/tags", `{"go":true,"old":null,"qa":false}`},
		{call{name: "merge metadata", method: "PATCH", target: "/v1/collections/jobs/objects/" + objID + "/metadata", body: `{"name":"dog.png","old":null}`},
			"PATCH", "/collections/jobs/objects/" + objID + "/metadata", `{"name":"dog.png","old":null}`},
		{call{name: "get object", method: "GET", target: "/v1/collections/jobs/objects/" + objID}, "GET", "/collections/jobs/objects/" + objID, ""},
		{call{name: "get data", method: "GET", target: "/v1/collections/jobs/objects/" + objID + "/data"}, "GET", "/collections/jobs/objects/" + objID + "/data", ""},
		{call{name: "object tags", method: "GET", target: "/v1/collections/jobs/objects/" + objID + "/tags?tags=golang,qa&evaluate=1"},
			"GET", "/collections/jobs/objects/" + objID + "/tags?evaluate=true&tags=golang%2Cqa", ""},
		{call{name: "delete object", method: "DELETE", target: "/v1/collections/jobs/objects/" + objID}, "DELETE", "/collections/jobs/objects/" + objID, ""},
		{call{name: "query", method: "POST", target: "/v1/collections/jobs/objects/query",
			body: `{ "tags": {"golang": true, "qa": false}, "limit": 3, "evaluate": false, "date": {"gte": "2026-01-02T03:04:05+02:00"}, "cursor": "abc", "timeout_ms": 5000, "best_effort": true }`},
			"POST", "/collections/jobs/objects/query",
			`{"tags":{"golang":true,"qa":false},"limit":3,"evaluate":false,"date":{"gte":"2026-01-02T01:04:05Z"},"cursor":"abc","timeout_ms":5000,"best_effort":true}`},
		{call{name: "query with nothing", method: "POST", target: "/v1/collections/jobs/objects/query", body: `{}`}, "POST", "/collections/jobs/objects/query", `{}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			backend, seen := newRecordingStorage(t)
			router := newGatewayRouter(t, backend.URL, gatewayConfig{})
			w := httptest.NewRecorder()
			router.ServeHTTP(w, tt.request())
			if w.Code != http.StatusOK {
				t.Fatalf("status %d: %s", w.Code, w.Body)
			}
			got := seen()
			if len(got) != 1 {
				t.Fatalf("upstream saw %d requests, want 1", len(got))
			}
			if got[0].method != tt.wantMethod || got[0].path != tt.wantURI {
				t.Errorf("upstream got %s %s, want %s %s", got[0].method, got[0].path, tt.wantMethod, tt.wantURI)
			}
			switch {
			case tt.wantBody == "":
				if got[0].body != "" {
					t.Errorf("upstream got a body: %q", got[0].body)
				}
			case strings.HasPrefix(tt.wantBody, "{"):
				sameJSON(t, got[0].body, tt.wantBody)
			default:
				if got[0].body != tt.wantBody {
					t.Errorf("upstream body = %q, want %q", got[0].body, tt.wantBody)
				}
			}
		})
	}
}

// Anything the route does not take is refused, and nothing is forwarded.
func TestUnexpectedInputIsRejected(t *testing.T) {
	tests := []struct {
		call
		status int
		code   string
	}{
		// query parameters
		{call{name: "unknown parameter", method: "GET", target: "/v1/collections?admin=true"}, 400, "invalid_parameter"},
		{call{name: "parameter on taggers", method: "GET", target: "/v1/taggers?all=1"}, 400, "invalid_parameter"},
		{call{name: "body on taggers", method: "GET", target: "/v1/taggers", body: `{"x":1}`}, 400, "unexpected_body"},
		{call{name: "parameter on delete", method: "DELETE", target: "/v1/collections/jobs?force=1"}, 400, "invalid_parameter"},
		{call{name: "unknown parameter on tags", method: "GET", target: "/v1/collections/jobs/tags?limit=1&sort=desc"}, 400, "invalid_parameter"},
		{call{name: "repeated parameter", method: "GET", target: "/v1/collections/jobs/tags?limit=1&limit=2"}, 400, "invalid_parameter"},
		{call{name: "parameter on object", method: "GET", target: "/v1/collections/jobs/objects/" + objID + "?x=1"}, 400, "invalid_parameter"},
		{call{name: "parameter on query", method: "POST", target: "/v1/collections/jobs/objects/query?limit=1", body: `{}`}, 400, "invalid_parameter"},
		{call{name: "parameter on create", method: "POST", target: "/v1/collections?x=1", body: `{"name":"a"}`}, 400, "invalid_parameter"},
		{call{name: "unknown parameter on upload", method: "POST", target: "/v1/collections/jobs/objects?owner=x", body: "x"}, 400, "invalid_parameter"},
		{call{name: "malformed query string", method: "GET", target: "/v1/collections/jobs/tags?limit=%zz"}, 400, "invalid_parameter"},
		// values
		{call{name: "bad limit", method: "GET", target: "/v1/collections/jobs/tags?limit=-1"}, 400, "invalid_limit"},
		{call{name: "limit not a number", method: "GET", target: "/v1/collections/jobs/tags?limit=ten"}, 400, "invalid_limit"},
		{call{name: "huge limit", method: "GET", target: "/v1/collections/jobs/tags?limit=99999999999"}, 400, "invalid_limit"},
		{call{name: "bad cursor", method: "GET", target: "/v1/collections/jobs/tags?cursor=a%2Fb"}, 400, "invalid_cursor"},
		{call{name: "long prefix", method: "GET", target: "/v1/collections/jobs/tags?prefix=" + strings.Repeat("a", 129)}, 400, "invalid_prefix"},
		{call{name: "invalid utf-8 prefix", method: "GET", target: "/v1/collections/jobs/tags?prefix=%ff"}, 400, "invalid_prefix"},
		{call{name: "bad evaluate", method: "GET", target: "/v1/collections/jobs/objects/" + objID + "/tags?evaluate=maybe"}, 400, "invalid_evaluate"},
		{call{name: "control characters in tags", method: "GET", target: "/v1/collections/jobs/objects/" + objID + "/tags?tags=a%0d%0ab"}, 400, "invalid_tags"},
		{call{name: "bad ttl", method: "POST", target: "/v1/collections/jobs/objects?ttl_seconds=-1", body: "x"}, 400, "invalid_ttl"},
		{call{name: "bad date", method: "POST", target: "/v1/collections/jobs/objects?date=yesterday", body: "x"}, 400, "invalid_date"},
		{call{name: "the data_type parameter is gone", method: "POST", target: "/v1/collections/jobs/objects?data_type=txt", body: "x"}, 400, "invalid_parameter"},
		{call{name: "metadata not json", method: "POST", target: "/v1/collections/jobs/objects?metadata=nope", body: "x"}, 400, "invalid_metadata"},
		{call{name: "metadata not an object", method: "POST", target: "/v1/collections/jobs/objects?metadata=%5B1%5D", body: "x"}, 400, "invalid_metadata"},
		{call{name: "metadata null", method: "POST", target: "/v1/collections/jobs/objects?metadata=null", body: "x"}, 400, "invalid_metadata"},
		{call{name: "metadata value not a string", method: "POST", target: "/v1/collections/jobs/objects?metadata=%7B%22a%22%3A1%7D", body: "x"}, 400, "invalid_metadata"},
		{call{name: "metadata with trailing data", method: "POST", target: "/v1/collections/jobs/objects?metadata=%7B%7D%7B%7D", body: "x"}, 400, "invalid_metadata"},
		{call{name: "metadata too large", method: "POST", target: "/v1/collections/jobs/objects?metadata=" + strings.Repeat("a", maxMetadataBytes+1), body: "x"}, 400, "invalid_metadata"},
		{call{name: "parameter on a metadata request", method: "PUT", target: "/v1/collections/jobs/objects/" + objID + "/metadata?x=1", body: `{}`}, 400, "invalid_parameter"},
		{call{name: "metadata request on a bad id", method: "PUT", target: "/v1/collections/jobs/objects/nope/metadata", body: `{}`}, 404, "not_found"},
		{call{name: "replace with a non-string", method: "PUT", target: "/v1/collections/jobs/objects/" + objID + "/metadata", body: `{"a":1}`}, 400, "invalid_json"},
		{call{name: "replace with null", method: "PUT", target: "/v1/collections/jobs/objects/" + objID + "/metadata", body: `null`}, 400, "invalid_json"},
		{call{name: "replace with null values", method: "PUT", target: "/v1/collections/jobs/objects/" + objID + "/metadata", body: `{"a":null}`}, 400, "invalid_json"},
		{call{name: "replace with an array", method: "PUT", target: "/v1/collections/jobs/objects/" + objID + "/metadata", body: `["a"]`}, 400, "invalid_json"},
		{call{name: "merge with a number", method: "PATCH", target: "/v1/collections/jobs/objects/" + objID + "/metadata", body: `{"a":2}`}, 400, "invalid_json"},
		{call{name: "metadata body too large", method: "PUT", target: "/v1/collections/jobs/objects/" + objID + "/metadata", body: `{"a":"` + strings.Repeat("x", maxMetadataBytes) + `"}`}, 413, "payload_too_large"},
		{call{name: "bad limit on collections", method: "GET", target: "/v1/collections?limit=x"}, 400, "invalid_limit"},
		{call{name: "bad cursor on collections", method: "GET", target: "/v1/collections?cursor=a%20b"}, 400, "invalid_cursor"},
		{call{name: "unknown parameter on collections", method: "GET", target: "/v1/collections?sort=name"}, 400, "invalid_parameter"},
		{call{name: "tags not json", method: "POST", target: "/v1/collections/jobs/objects?tags=nope", body: "x"}, 400, "invalid_tags"},
		{call{name: "tags null", method: "POST", target: "/v1/collections/jobs/objects?tags=null", body: "x"}, 400, "invalid_tags"},
		{call{name: "tags with a null value", method: "POST", target: "/v1/collections/jobs/objects?tags=%7B%22a%22%3Anull%7D", body: "x"}, 400, "invalid_tags"},
		{call{name: "tags value not a boolean", method: "POST", target: "/v1/collections/jobs/objects?tags=%7B%22a%22%3A%22yes%22%7D", body: "x"}, 400, "invalid_tags"},
		{call{name: "tags with trailing data", method: "POST", target: "/v1/collections/jobs/objects?tags=%7B%7D%7B%7D", body: "x"}, 400, "invalid_tags"},
		{call{name: "force tags with a string", method: "PATCH", target: "/v1/collections/jobs/objects/" + objID + "/tags", body: `{"a":"yes"}`}, 400, "invalid_json"},
		{call{name: "force tags with null body", method: "PATCH", target: "/v1/collections/jobs/objects/" + objID + "/tags", body: `null`}, 400, "invalid_json"},
		{call{name: "parameter on a tags request", method: "PATCH", target: "/v1/collections/jobs/objects/" + objID + "/tags?x=1", body: `{}`}, 400, "invalid_parameter"},
		{call{name: "force tags on a bad id", method: "PATCH", target: "/v1/collections/jobs/objects/nope/tags", body: `{}`}, 404, "not_found"},
		{call{name: "bad collection name", method: "GET", target: "/v1/collections/Jobs/tags"}, 400, "invalid_collection_name"},
		{call{name: "collection name with dots", method: "DELETE", target: "/v1/collections/a.b"}, 400, "invalid_collection_name"},
		{call{name: "object id not a uuid", method: "GET", target: "/v1/collections/jobs/objects/not-a-uuid"}, 404, "not_found"},
		{call{name: "object id traversal", method: "GET", target: "/v1/collections/jobs/objects/abc/data"}, 404, "not_found"},
		// bodies
		{call{name: "body on get", method: "GET", target: "/v1/collections", body: `{"x":1}`}, 400, "unexpected_body"},
		{call{name: "body on delete", method: "DELETE", target: "/v1/collections/jobs", body: "x"}, 400, "unexpected_body"},
		{call{name: "body on object get", method: "GET", target: "/v1/collections/jobs/objects/" + objID, body: "x"}, 400, "unexpected_body"},
		// JSON
		{call{name: "unknown field in query", method: "POST", target: "/v1/collections/jobs/objects/query", body: `{"limit":1,"sql":"drop"}`}, 400, "invalid_json"},
		{call{name: "unknown field in date filter", method: "POST", target: "/v1/collections/jobs/objects/query", body: `{"date":{"gt":"2026-01-01T00:00:00Z","x":1}}`}, 400, "invalid_json"},
		{call{name: "wrong type", method: "POST", target: "/v1/collections/jobs/objects/query", body: `{"limit":"3"}`}, 400, "invalid_json"},
		{call{name: "wrong tag value type", method: "POST", target: "/v1/collections/jobs/objects/query", body: `{"tags":{"a":"yes"}}`}, 400, "invalid_json"},
		{call{name: "bad date in filter", method: "POST", target: "/v1/collections/jobs/objects/query", body: `{"date":{"gt":"soon"}}`}, 400, "invalid_json"},
		{call{name: "trailing data", method: "POST", target: "/v1/collections/jobs/objects/query", body: `{} {"limit":1}`}, 400, "invalid_json"},
		{call{name: "trailing garbage", method: "POST", target: "/v1/collections/jobs/objects/query", body: `{}x`}, 400, "invalid_json"},
		{call{name: "not an object", method: "POST", target: "/v1/collections/jobs/objects/query", body: `[1]`}, 400, "invalid_json"},
		{call{name: "empty body", method: "POST", target: "/v1/collections/jobs/objects/query"}, 400, "invalid_json"},
		{call{name: "bad cursor in body", method: "POST", target: "/v1/collections/jobs/objects/query", body: `{"cursor":"a b"}`}, 400, "invalid_cursor"},
		{call{name: "unknown field in create", method: "POST", target: "/v1/collections", body: `{"name":"a","owner":"x"}`}, 400, "invalid_json"},
		{call{name: "the data_type field is gone", method: "POST", target: "/v1/collections", body: `{"name":"a","data_type":"txt"}`}, 400, "invalid_json"},
		{call{name: "bad collection name in body", method: "POST", target: "/v1/collections", body: `{"name":"A b"}`}, 400, "invalid_name"},
		{call{name: "empty tagger version", method: "POST", target: "/v1/collections", body: `{"name":"a","tagger_version":""}`}, 400, "invalid_tagger_version"},
		{call{name: "long tagger version", method: "POST", target: "/v1/collections", body: `{"name":"a","tagger_version":"` + strings.Repeat("x", maxVersionBytes+1) + `"}`}, 400, "invalid_tagger_version"},
		{call{name: "tagger version with a control character", method: "POST", target: "/v1/collections", body: `{"name":"a","tagger_version":"a\nb"}`}, 400, "invalid_tagger_version"},
		{call{name: "tagger version not a string", method: "POST", target: "/v1/collections", body: `{"name":"a","tagger_version":1}`}, 400, "invalid_json"},
		{call{name: "oversized json", method: "POST", target: "/v1/collections/jobs/objects/query", body: `{"cursor":"` + strings.Repeat("a", maxJSONBodyBytes) + `"}`}, 413, "payload_too_large"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			backend, seen := newRecordingStorage(t)
			router := newGatewayRouter(t, backend.URL, gatewayConfig{})
			w := httptest.NewRecorder()
			router.ServeHTTP(w, tt.request())
			if w.Code != tt.status || decodeErrorCode(t, w.Body.Bytes()) != tt.code {
				t.Fatalf("got %d %s, want %d %s", w.Code, w.Body, tt.status, tt.code)
			}
			if got := seen(); len(got) != 0 {
				t.Fatalf("a rejected request reached storage: %+v", got)
			}
		})
	}
}

func headerNames(h http.Header) []string {
	var names []string
	for k := range h {
		names = append(names, k)
	}
	sort.Strings(names)
	return names
}

// The client's headers are never copied: storage sees the headers the gateway chose, nothing else.
func TestInboundHeadersNeverReachStorage(t *testing.T) {
	hostile := map[string]string{
		"Cookie": "session=secret", "X-Forwarded-For": "6.6.6.6", "X-Forwarded-Host": "evil.example",
		"Forwarded": "for=6.6.6.6", "X-Request-Id": "abc", "X-Anything": "else", "Accept-Encoding": "br",
		"Accept": "text/html", "Origin": "https://evil.example", "Referer": "https://evil.example/",
		"X-Original-URL": "/admin", "X-HTTP-Method-Override": "DELETE", "Range": "bytes=0-1", "If-None-Match": `"x"`,
		"Content-Encoding": "gzip", "Transfer-Encoding": "chunked", "Te": "trailers", "Upgrade": "websocket",
	}
	tests := []struct {
		call
		want []string // header names storage may see
	}{
		{call{name: "get", method: "GET", target: "/v1/collections", headers: hostile}, []string{"User-Agent"}},
		{call{name: "delete", method: "DELETE", target: "/v1/collections/jobs", headers: hostile}, []string{"User-Agent"}},
		{call{name: "json", method: "POST", target: "/v1/collections", body: `{"name":"a"}`, headers: hostile},
			[]string{"Content-Length", "Content-Type", "User-Agent"}},
		{call{name: "upload", method: "POST", target: "/v1/collections/jobs/objects", body: "payload", headers: hostile},
			[]string{"Content-Length", "Content-Type", "User-Agent"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			backend, seen := newRecordingStorage(t)
			router := newGatewayRouter(t, backend.URL, gatewayConfig{})
			router.ServeHTTP(httptest.NewRecorder(), tt.request())
			got := seen()
			if len(got) != 1 {
				t.Fatalf("upstream saw %d requests", len(got))
			}
			if names := headerNames(got[0].header); !reflect.DeepEqual(names, tt.want) {
				t.Errorf("storage saw headers %v, want %v", names, tt.want)
			}
			if v := got[0].header.Get("Authorization"); v != "" {
				t.Errorf("the API key reached storage: %q", v)
			}
		})
	}

	t.Run("upload content type is fixed", func(t *testing.T) {
		backend, seen := newRecordingStorage(t)
		router := newGatewayRouter(t, backend.URL, gatewayConfig{})
		router.ServeHTTP(httptest.NewRecorder(), call{method: "POST", target: "/v1/collections/jobs/objects", body: "x",
			headers: map[string]string{"Content-Type": "text/html; charset=evil"}}.request())
		if ct := seen()[0].header.Get("Content-Type"); ct != "application/octet-stream" {
			t.Errorf("storage was told Content-Type %q", ct)
		}
	})
}

// Only the status, body and the headers of the contract come back; internal details stay inside.
func TestResponseHeadersAreFiltered(t *testing.T) {
	backend, _ := newReplyingStorage(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Set-Cookie", "internal=1")
		w.Header().Set("X-Internal-Host", "storage-7")
		w.Header().Set("Server", "storage/1.0")
		w.Header().Set("Location", "http://storage:8082/other")
		w.Header().Set("WWW-Authenticate", "Basic realm=internal")
		w.WriteHeader(http.StatusCreated)
		w.Write([]byte(`{"name":"jobs"}`))
	})
	router := newGatewayRouter(t, backend.URL, gatewayConfig{})
	w := httptest.NewRecorder()
	router.ServeHTTP(w, call{method: "POST", target: "/v1/collections", body: `{"name":"jobs"}`}.request())

	if w.Code != http.StatusCreated || w.Body.String() != `{"name":"jobs"}` {
		t.Fatalf("status %d, body %s", w.Code, w.Body)
	}
	for _, h := range []string{"Set-Cookie", "X-Internal-Host", "Server", "Location", "Www-Authenticate"} {
		if v := w.Header().Get(h); v != "" {
			t.Errorf("internal header %s leaked: %q", h, v)
		}
	}
	if w.Header().Get("Content-Type") != "application/json" || w.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Errorf("headers = %v", w.Header())
	}
}

func TestStorageErrorsAndOutageAreRelayed(t *testing.T) {
	backend, _ := newReplyingStorage(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		w.Write([]byte(`{"error":{"code":"not_found","message":"collection not found"}}`))
	})
	router := newGatewayRouter(t, backend.URL, gatewayConfig{})
	w := httptest.NewRecorder()
	router.ServeHTTP(w, call{method: "GET", target: "/v1/collections/jobs/tags"}.request())
	if w.Code != http.StatusNotFound || decodeErrorCode(t, w.Body.Bytes()) != "not_found" {
		t.Fatalf("storage's answer was not relayed: %d %s", w.Code, w.Body)
	}

	backend.Close()
	w = httptest.NewRecorder()
	router.ServeHTTP(w, call{method: "GET", target: "/v1/collections"}.request())
	// the validator cannot reach the backend either, so this is a key-service outage
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("outage: %d %s", w.Code, w.Body)
	}
}

func TestStorageUnreachableIsBadGateway(t *testing.T) {
	backend, _ := newRecordingStorage(t)
	g := newTestGateway(t, "http://127.0.0.1:1") // storage is down...
	g.keystorage = nil
	router := newRouter([]string{}, g, gatewayConfig{validator: keystorageapi.New(backend.URL)}) // ...but keys validate
	w := httptest.NewRecorder()
	router.ServeHTTP(w, call{method: "GET", target: "/v1/collections"}.request())
	if w.Code != http.StatusBadGateway || decodeErrorCode(t, w.Body.Bytes()) != "bad_gateway" {
		t.Fatalf("got %d %s", w.Code, w.Body)
	}
}

func TestUnlistedRoutesAreNotServed(t *testing.T) {
	backend, seen := newRecordingStorage(t)
	router := newGatewayRouter(t, backend.URL, gatewayConfig{})

	for _, path := range []string{
		"/v1/debug", "/v1/collections/jobs/objects/" + objID + "/extra", "/collections", "/collections/jobs",
		"/internal/v1/api-keys", "/api-keys/validate", "/api-keys", "/v1/api-keys", "/tag", "/supported-types",
		"/v1/internal/v1/api-keys",
	} {
		w := httptest.NewRecorder()
		router.ServeHTTP(w, newRequest(t, http.MethodGet, path, bearer))
		if w.Code != http.StatusNotFound {
			t.Errorf("GET %s: expected 404, got %d", path, w.Code)
		}
	}
	if got := len(seen()); got != 0 {
		t.Errorf("unlisted paths reached storage (%d requests)", got)
	}

	w := httptest.NewRecorder()
	router.ServeHTTP(w, newRequest(t, http.MethodPut, "/v1/collections", bearer))
	if w.Code != http.StatusMethodNotAllowed || decodeErrorCode(t, w.Body.Bytes()) != "method_not_allowed" {
		t.Fatalf("wrong method: %d %s", w.Code, w.Body)
	}
}

func TestSafePath(t *testing.T) {
	backend, seen := newRecordingStorage(t)
	router := newGatewayRouter(t, backend.URL, gatewayConfig{})

	for _, path := range []string{
		"/v1/../internal/v1/api-keys",
		"/v1/collections/../objects",
		"/v1/collections/./objects",
		"/v1/%2e%2e/internal/v1/api-keys",
		"/v1/collections/%2E%2E/objects",
		"/v1/collections/a%2fb/objects",
		"/v1/collections/a%5cb/objects",
		"/v1/collections//objects",
		"/v1//collections",
		"/v1/collections/a%00b",
	} {
		w := httptest.NewRecorder()
		router.ServeHTTP(w, newRequest(t, http.MethodPost, path, bearer))
		if w.Code != http.StatusBadRequest {
			t.Errorf("%s: expected 400, got %d", path, w.Code)
			continue
		}
		if code := decodeErrorCode(t, w.Body.Bytes()); code != "invalid_path" {
			t.Errorf("%s: expected invalid_path, got %q", path, code)
		}
	}
	if got := len(seen()); got != 0 {
		t.Errorf("unsafe paths reached storage (%d requests)", got)
	}
}

func TestBodyLimit(t *testing.T) {
	backend, seen := newRecordingStorage(t)
	g := newTestGateway(t, backend.URL)
	g.maxBodyBytes = 10
	router := newRouterFor(t, g, backend.URL, gatewayConfig{})
	path := "/v1/collections/jobs/objects"

	t.Run("within limit is forwarded", func(t *testing.T) {
		w := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader("0123456789"))
		bearer(req)
		router.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
		}
	})

	t.Run("declared length over limit is rejected up front", func(t *testing.T) {
		before := len(seen())
		w := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader("0123456789x"))
		bearer(req)
		router.ServeHTTP(w, req)
		if w.Code != http.StatusRequestEntityTooLarge {
			t.Fatalf("expected 413, got %d: %s", w.Code, w.Body.String())
		}
		if code := decodeErrorCode(t, w.Body.Bytes()); code != "payload_too_large" {
			t.Errorf("expected payload_too_large, got %q", code)
		}
		if len(seen()) != before {
			t.Errorf("oversized request reached storage")
		}
	})

	t.Run("chunked body over limit is cut off", func(t *testing.T) {
		front := httptest.NewServer(router)
		defer front.Close()
		req, err := http.NewRequest(http.MethodPost, front.URL+path, io.NopCloser(strings.NewReader(strings.Repeat("x", 1024))))
		if err != nil {
			t.Fatal(err)
		}
		req.ContentLength = -1 // force chunked transfer encoding
		bearer(req)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("do request: %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusRequestEntityTooLarge {
			body, _ := io.ReadAll(resp.Body)
			t.Fatalf("expected 413, got %d: %s", resp.StatusCode, body)
		}
	})
}

// Uploads are streamed: the payload arrives byte for byte, whatever its size and encoding.
func TestUploadPayloadIsRelayedUntouched(t *testing.T) {
	backend, seen := newRecordingStorage(t)
	router := newGatewayRouter(t, backend.URL, gatewayConfig{})
	payload := "{\"not\":\"parsed\"}\x00\xff binary \r\n" + strings.Repeat("z", 100000)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, call{method: "POST", target: "/v1/collections/jobs/objects", body: payload}.request())
	if w.Code != http.StatusOK {
		t.Fatalf("status %d: %s", w.Code, w.Body)
	}
	if got := seen()[0].body; got != payload {
		t.Fatalf("payload changed in transit (%d bytes in, %d out)", len(payload), len(got))
	}
}

// The payload is not relayed: storage's redirect to a signed object-store URL is, with no caching, and
// the gateway does not follow it.
func TestDataIsARedirectToTheObjectStore(t *testing.T) {
	const signed = "https://s3.example.com/tagona/key?X-Amz-Signature=abc&X-Amz-Expires=60"
	var answer func(w http.ResponseWriter)
	backend, seen := newReplyingStorage(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/data") {
			answer(w)
			return
		}
		w.Header().Set("Location", signed) // any other route may not set it
		w.Write([]byte(`{}`))
	})
	router := newGatewayRouter(t, backend.URL, gatewayConfig{})
	get := func() *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		router.ServeHTTP(w, call{method: "GET", target: "/v1/collections/jobs/objects/" + objID + "/data"}.request())
		return w
	}

	answer = func(w http.ResponseWriter) {
		w.Header().Set("Location", signed)
		w.Header().Set("Cache-Control", "private, max-age=600")
		w.Header().Set("Set-Cookie", "internal=1")
		w.WriteHeader(http.StatusTemporaryRedirect)
	}
	w := get()
	if w.Code != http.StatusTemporaryRedirect || w.Header().Get("Location") != signed {
		t.Fatalf("status %d, Location %q", w.Code, w.Header().Get("Location"))
	}
	if w.Header().Get("Cache-Control") != "no-store" || w.Header().Get("Set-Cookie") != "" {
		t.Errorf("headers = %v", w.Header())
	}
	if n := len(seen()); n != 1 {
		t.Errorf("storage was called %d times: the redirect must not be followed", n)
	}

	for _, bad := range []string{"", "/relative", "file:///etc/passwd", "javascript:alert(1)", "http://"} {
		answer = func(w http.ResponseWriter) {
			w.Header().Set("Location", bad)
			w.WriteHeader(http.StatusTemporaryRedirect)
		}
		if w := get(); w.Code != http.StatusBadGateway || w.Header().Get("Location") != "" {
			t.Errorf("Location %q: status %d, Location %q", bad, w.Code, w.Header().Get("Location"))
		}
	}

	answer = func(w http.ResponseWriter) { writeError(w, http.StatusNotFound, "not_found", "object not found") }
	if w := get(); w.Code != http.StatusNotFound || w.Header().Get("Location") != "" {
		t.Errorf("not found: %d", w.Code)
	}
}
