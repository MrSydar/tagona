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
		{call{name: "create collection", method: "POST", target: "/v1/collections", body: `{"name":"jobs","data_type":"txt"}`},
			"POST", "/collections", `{"name":"jobs","data_type":"txt"}`},
		{call{name: "delete collection", method: "DELETE", target: "/v1/collections/jobs"}, "DELETE", "/collections/jobs", ""},
		{call{name: "collection tags", method: "GET", target: "/v1/collections/jobs/tags?limit=007&prefix=go&cursor=abc_-"},
			"GET", "/collections/jobs/tags?cursor=abc_-&limit=7&prefix=go", ""},
		{call{name: "collection tags, empty params dropped", method: "GET", target: "/v1/collections/jobs/tags?limit=&prefix="},
			"GET", "/collections/jobs/tags", ""},
		{call{name: "upload", method: "POST", target: "/v1/collections/jobs/objects?data_type=txt&ttl_seconds=0060&date=2026-01-02T03:04:05%2B02:00", body: "hello"},
			"POST", "/collections/jobs/objects?data_type=txt&date=2026-01-02T01%3A04%3A05Z&ttl_seconds=60", "hello"},
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
		{call{name: "parameter on delete", method: "DELETE", target: "/v1/collections/jobs?force=1"}, 400, "invalid_parameter"},
		{call{name: "unknown parameter on tags", method: "GET", target: "/v1/collections/jobs/tags?limit=1&sort=desc"}, 400, "invalid_parameter"},
		{call{name: "repeated parameter", method: "GET", target: "/v1/collections/jobs/tags?limit=1&limit=2"}, 400, "invalid_parameter"},
		{call{name: "parameter on object", method: "GET", target: "/v1/collections/jobs/objects/" + objID + "?x=1"}, 400, "invalid_parameter"},
		{call{name: "parameter on query", method: "POST", target: "/v1/collections/jobs/objects/query?limit=1", body: `{}`}, 400, "invalid_parameter"},
		{call{name: "parameter on create", method: "POST", target: "/v1/collections?x=1", body: `{"name":"a","data_type":"txt"}`}, 400, "invalid_parameter"},
		{call{name: "unknown parameter on upload", method: "POST", target: "/v1/collections/jobs/objects?data_type=txt&owner=x", body: "x"}, 400, "invalid_parameter"},
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
		{call{name: "bad ttl", method: "POST", target: "/v1/collections/jobs/objects?data_type=txt&ttl_seconds=-1", body: "x"}, 400, "invalid_ttl"},
		{call{name: "bad date", method: "POST", target: "/v1/collections/jobs/objects?data_type=txt&date=yesterday", body: "x"}, 400, "invalid_date"},
		{call{name: "bad data type", method: "POST", target: "/v1/collections/jobs/objects?data_type=%3Cscript%3E", body: "x"}, 400, "invalid_data_type"},
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
		{call{name: "unknown field in create", method: "POST", target: "/v1/collections", body: `{"name":"a","data_type":"txt","owner":"x"}`}, 400, "invalid_json"},
		{call{name: "bad collection name in body", method: "POST", target: "/v1/collections", body: `{"name":"A b","data_type":"txt"}`}, 400, "invalid_name"},
		{call{name: "bad data type in body", method: "POST", target: "/v1/collections", body: `{"name":"a","data_type":"t x t"}`}, 400, "invalid_data_type"},
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
		{call{name: "json", method: "POST", target: "/v1/collections", body: `{"name":"a","data_type":"txt"}`, headers: hostile},
			[]string{"Content-Length", "Content-Type", "User-Agent"}},
		{call{name: "upload", method: "POST", target: "/v1/collections/jobs/objects?data_type=txt", body: "payload", headers: hostile},
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
		router.ServeHTTP(httptest.NewRecorder(), call{method: "POST", target: "/v1/collections/jobs/objects?data_type=txt", body: "x",
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
	router.ServeHTTP(w, call{method: "POST", target: "/v1/collections", body: `{"name":"jobs","data_type":"txt"}`}.request())

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
	path := "/v1/collections/jobs/objects?data_type=txt"

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
	router.ServeHTTP(w, call{method: "POST", target: "/v1/collections/jobs/objects?data_type=txt", body: payload}.request())
	if w.Code != http.StatusOK {
		t.Fatalf("status %d: %s", w.Code, w.Body)
	}
	if got := seen()[0].body; got != payload {
		t.Fatalf("payload changed in transit (%d bytes in, %d out)", len(payload), len(got))
	}
}
