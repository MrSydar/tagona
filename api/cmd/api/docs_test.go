package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"go.yaml.in/yaml/v3"

	"mrsydar/tagona/api/openapi"
)

type specDoc struct {
	OpenAPI string `yaml:"openapi"`
	Info    struct {
		Title   string `yaml:"title"`
		Version string `yaml:"version"`
	} `yaml:"info"`
	Paths map[string]map[string]yaml.Node `yaml:"paths"`
}

func loadSpec(t *testing.T) specDoc {
	t.Helper()
	var doc specDoc
	if err := yaml.Unmarshal(openapi.V1, &doc); err != nil {
		t.Fatalf("parse embedded openapi: %v", err)
	}
	return doc
}

func newDocsRouter(t *testing.T) http.Handler {
	t.Helper()
	backend, _ := newRecordingStorage(t)
	docs, err := newDocsHandlers(openapi.V1)
	if err != nil {
		t.Fatalf("newDocsHandlers: %v", err)
	}
	return newGatewayRouter(t, backend.URL, gatewayConfig{docs: docs})
}

// The spec is a hand-written contract; this fails when a route is added to or
// removed from the gateway without updating api/openapi/v1.yaml (or vice versa).
func TestOpenAPIMatchesRoutes(t *testing.T) {
	router := newDocsRouter(t).(chi.Routes)

	inRouter := map[string]bool{}
	err := chi.Walk(router, func(method, route string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		route = strings.TrimSuffix(route, "/")
		if strings.HasPrefix(route, "/v1/collections") || strings.HasPrefix(route, "/v1/admin") || strings.HasPrefix(route, "/v1/taggers") {
			inRouter[method+" "+route] = true
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk routes: %v", err)
	}

	inSpec := map[string]bool{}
	for path, item := range loadSpec(t).Paths {
		for method := range item {
			switch strings.ToUpper(method) {
			case "GET", "POST", "PUT", "PATCH", "DELETE":
				inSpec[strings.ToUpper(method)+" "+path] = true
			}
		}
	}

	var missing, extra []string
	for r := range inRouter {
		if !inSpec[r] {
			missing = append(missing, r)
		}
	}
	for s := range inSpec {
		if !inRouter[s] {
			extra = append(extra, s)
		}
	}
	sort.Strings(missing)
	sort.Strings(extra)
	if len(missing) > 0 {
		t.Errorf("routes served but not documented in api/openapi/v1.yaml:\n  %s", strings.Join(missing, "\n  "))
	}
	if len(extra) > 0 {
		t.Errorf("operations documented in api/openapi/v1.yaml but not served:\n  %s", strings.Join(extra, "\n  "))
	}
	if len(inRouter) == 0 {
		t.Fatal("found no routes; the walk is broken")
	}
}

func TestOpenAPIReferencesResolveAndOperationIDsAreUnique(t *testing.T) {
	var root any
	if err := yaml.Unmarshal(openapi.V1, &root); err != nil {
		t.Fatal(err)
	}

	resolve := func(ref string) bool {
		if !strings.HasPrefix(ref, "#/") {
			return false
		}
		cur := root
		for _, part := range strings.Split(strings.TrimPrefix(ref, "#/"), "/") {
			m, ok := cur.(map[string]any)
			if !ok {
				return false
			}
			if cur, ok = m[part]; !ok {
				return false
			}
		}
		return true
	}

	seenIDs := map[string]bool{}
	var walk func(v any, path string)
	walk = func(v any, path string) {
		switch n := v.(type) {
		case map[string]any:
			for k, child := range n {
				if k == "$ref" {
					if ref, ok := child.(string); !ok || !resolve(ref) {
						t.Errorf("%s: unresolved $ref %v", path, child)
					}
					continue
				}
				if k == "operationId" {
					id, _ := child.(string)
					if id == "" || seenIDs[id] {
						t.Errorf("%s: operationId %q is empty or duplicated", path, id)
					}
					seenIDs[id] = true
				}
				walk(child, path+"/"+k)
			}
		case []any:
			for i, child := range n {
				walk(child, path)
				_ = i
			}
		}
	}
	walk(root, "")
	if len(seenIDs) == 0 {
		t.Error("no operations found")
	}
}

func TestDocsEndpointsArePublic(t *testing.T) {
	router := newDocsRouter(t)
	get := func(path string, hdr map[string]string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, path, nil) // no Authorization header
		for k, v := range hdr {
			req.Header.Set(k, v)
		}
		router.ServeHTTP(w, req)
		return w
	}

	t.Run("json spec", func(t *testing.T) {
		w := get("/v1/openapi.json", nil)
		if w.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d", w.Code)
		}
		if ct := w.Header().Get("Content-Type"); ct != "application/json" {
			t.Errorf("Content-Type = %q", ct)
		}
		var doc map[string]any
		if err := json.Unmarshal(w.Body.Bytes(), &doc); err != nil {
			t.Fatalf("response is not valid JSON: %v", err)
		}
		if v, _ := doc["openapi"].(string); !strings.HasPrefix(v, "3.") {
			t.Errorf("openapi = %v", doc["openapi"])
		}
		paths, _ := doc["paths"].(map[string]any)
		if len(paths) != len(loadSpec(t).Paths) {
			t.Errorf("JSON has %d paths, YAML has %d", len(paths), len(loadSpec(t).Paths))
		}
	})

	t.Run("yaml spec", func(t *testing.T) {
		w := get("/v1/openapi.yaml", nil)
		if w.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d", w.Code)
		}
		if ct := w.Header().Get("Content-Type"); ct != "application/yaml" {
			t.Errorf("Content-Type = %q", ct)
		}
		if w.Body.String() != string(openapi.V1) {
			t.Error("served YAML differs from the embedded document")
		}
	})

	t.Run("interactive docs", func(t *testing.T) {
		w := get("/v1/docs", nil)
		if w.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d", w.Code)
		}
		if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
			t.Errorf("Content-Type = %q", ct)
		}
		body := w.Body.String()
		for _, want := range []string{"swagger-ui-bundle.js", `url: "openapi.json"`, "swagger-ui-dist@" + swaggerUIVersion} {
			if !strings.Contains(body, want) {
				t.Errorf("docs page is missing %q", want)
			}
		}
		// Every external asset must be integrity-pinned.
		if strings.Count(body, "https://cdn.jsdelivr.net") != strings.Count(body, "integrity=") {
			t.Error("a CDN asset is loaded without an integrity attribute")
		}
	})

	t.Run("conditional requests", func(t *testing.T) {
		first := get("/v1/openapi.json", nil)
		tag := first.Header().Get("ETag")
		if tag == "" {
			t.Fatal("missing ETag")
		}
		if w := get("/v1/openapi.json", map[string]string{"If-None-Match": tag}); w.Code != http.StatusNotModified {
			t.Errorf("expected 304 for matching ETag, got %d", w.Code)
		}
		if w := get("/v1/openapi.json", map[string]string{"If-None-Match": `"other"`}); w.Code != http.StatusOK {
			t.Errorf("expected 200 for stale ETag, got %d", w.Code)
		}
	})

	t.Run("head", func(t *testing.T) {
		for _, path := range []string{"/v1/openapi.json", "/v1/openapi.yaml", "/v1/docs"} {
			w := httptest.NewRecorder()
			router.ServeHTTP(w, httptest.NewRequest(http.MethodHead, path, nil))
			if w.Code != http.StatusOK || w.Header().Get("ETag") == "" {
				t.Errorf("HEAD %s: status %d, ETag %q", path, w.Code, w.Header().Get("ETag"))
			}
		}
	})

	t.Run("read only", func(t *testing.T) {
		w := httptest.NewRecorder()
		router.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/v1/openapi.json", nil))
		if w.Code != http.StatusMethodNotAllowed {
			t.Errorf("POST: expected 405, got %d", w.Code)
		}
	})

	t.Run("api stays protected", func(t *testing.T) {
		if w := get("/v1/collections", nil); w.Code != http.StatusUnauthorized {
			t.Errorf("expected 401 on /v1/collections without a key, got %d", w.Code)
		}
	})
}

// The document lists every query parameter an operation accepts and the gateway accepts no others: each
// documented parameter must get past the gateway's parameter check, and a parameter that is not
// documented must be refused.
func TestOpenAPIDocumentsEveryQueryParameter(t *testing.T) {
	var root map[string]any
	if err := yaml.Unmarshal(openapi.V1, &root); err != nil {
		t.Fatal(err)
	}
	resolve := func(p any) map[string]any {
		m, _ := p.(map[string]any)
		if ref, ok := m["$ref"].(string); ok {
			cur := any(root)
			for _, part := range strings.Split(strings.TrimPrefix(ref, "#/"), "/") {
				cur = cur.(map[string]any)[part]
			}
			m, _ = cur.(map[string]any)
		}
		return m
	}
	queryParams := func(lists ...any) []string {
		var names []string
		for _, l := range lists {
			items, _ := l.([]any)
			for _, p := range items {
				if m := resolve(p); m["in"] == "query" {
					names = append(names, m["name"].(string))
				}
			}
		}
		sort.Strings(names)
		return names
	}
	samples := map[string]string{
		"limit": "1", "cursor": "abc", "prefix": "a", "tags": "a", "evaluate": "true",
		"date": "2026-01-01T00:00:00Z", "ttl_seconds": "60", "metadata": `{"a":"b"}`,
	}
	bodies := map[string]string{
		"POST /v1/collections":                                     `{"name":"jobs"}`,
		"POST /v1/collections/{collection}/objects":                "payload",
		"POST /v1/collections/{collection}/objects/query":          `{}`,
		"PUT /v1/collections/{collection}/objects/{id}/metadata":   `{}`,
		"PATCH /v1/collections/{collection}/objects/{id}/metadata": `{}`,
		"POST /v1/admin/api-keys":                                  `{"name":"k"}`,
	}

	paths := root["paths"].(map[string]any)
	checked := 0
	for path, item := range paths {
		pathItem := item.(map[string]any)
		for method, op := range pathItem {
			method = strings.ToUpper(method)
			if method != "GET" && method != "POST" && method != "PUT" && method != "PATCH" && method != "DELETE" {
				continue
			}
			operation := op.(map[string]any)
			documented := queryParams(pathItem["parameters"], operation["parameters"])
			target := strings.NewReplacer("{collection}", "jobs", "{id}", objID).Replace(path)
			body := bodies[method+" "+path]

			send := func(query string) (int, string) {
				backend, _ := newRecordingStorage(t)
				router := newGatewayRouter(t, backend.URL, gatewayConfig{})
				w := httptest.NewRecorder()
				if strings.HasPrefix(path, "/v1/admin/") {
					router.ServeHTTP(w, adminRequest(method, target+query, body))
				} else {
					router.ServeHTTP(w, call{method: method, target: target + query, body: body}.request())
				}
				code := ""
				if w.Code == http.StatusBadRequest {
					code = decodeErrorCode(t, w.Body.Bytes())
				}
				return w.Code, code
			}

			for _, name := range documented {
				sample, ok := samples[name]
				if !ok {
					t.Fatalf("%s %s documents the parameter %q: add a sample value for it to this test", method, path, name)
				}
				if status, code := send("?" + name + "=" + url.QueryEscape(sample)); code == "invalid_parameter" {
					t.Errorf("%s %s documents the parameter %q but the gateway refuses it (%d)", method, path, name, status)
				}
			}
			if _, code := send("?undocumented_parameter=1"); code != "invalid_parameter" {
				t.Errorf("%s %s: a parameter that is not documented was not refused (%q)", method, path, code)
			}
			checked++
		}
	}
	if checked < 15 {
		t.Fatalf("checked only %d operations; the walk is broken", checked)
	}
}

// The fields of the query body are the properties of TagsQueryRequest in the document, no more and no fewer.
func TestOpenAPIQueryBodyMatchesTheGateway(t *testing.T) {
	var doc struct {
		Components struct {
			Schemas map[string]struct {
				Properties map[string]any `yaml:"properties"`
			} `yaml:"schemas"`
		} `yaml:"components"`
	}
	if err := yaml.Unmarshal(openapi.V1, &doc); err != nil {
		t.Fatal(err)
	}
	documented := map[string]bool{}
	for name := range doc.Components.Schemas["TagsQueryRequest"].Properties {
		documented[name] = true
	}
	typ := reflect.TypeOf(queryBody{})
	accepted := map[string]bool{}
	for i := 0; i < typ.NumField(); i++ {
		accepted[strings.Split(typ.Field(i).Tag.Get("json"), ",")[0]] = true
	}
	if !reflect.DeepEqual(documented, accepted) {
		t.Errorf("documented fields %v, accepted fields %v", documented, accepted)
	}
}
