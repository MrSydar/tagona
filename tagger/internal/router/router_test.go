package router

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// stubTagger plays a tagger service: it serves versions and answers /tag with its name.
type stubTagger struct {
	*httptest.Server
	versionCalls atomic.Int32
	tagBodies    chan string
	tagStatus    int
}

func newStub(t *testing.T, name string, versions ...string) *stubTagger {
	t.Helper()
	s := &stubTagger{tagBodies: make(chan string, 10), tagStatus: http.StatusOK}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/version":
			s.versionCalls.Add(1)
			json.NewEncoder(w).Encode(map[string]any{"version": versions})
		case "/tag":
			b, _ := io.ReadAll(r.Body)
			s.tagBodies <- string(b)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(s.tagStatus)
			json.NewEncoder(w).Encode(map[string]any{"tags": map[string]bool{name: true}})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(s.Close)
	return s
}

func do(h http.Handler, method, path, body string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(method, path, strings.NewReader(body)))
	return rec
}

func versions(t *testing.T, h http.Handler) []string {
	t.Helper()
	rec := do(h, http.MethodGet, "/version", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /version: %d %s", rec.Code, rec.Body)
	}
	var got struct{ Version []string }
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	return got.Version
}

func tagBody(version string) string {
	return `{"collection":"jobs","object_id":"1","tagger_version":"` + version + `","tags":["a"]}`
}

func TestVersionIsTheUnionInConfiguredOrder(t *testing.T) {
	a := newStub(t, "a", "grep", "shared")
	b := newStub(t, "b", "shared", "decisions/openai:m")
	h := New([]string{a.URL, b.URL}, 0).Handler()
	got := versions(t, h)
	want := []string{"grep", "shared", "decisions/openai:m"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("versions = %v, want %v", got, want)
	}
}

func TestTagGoesToTheTaggerOfTheVersionAndTheAnswerIsRelayed(t *testing.T) {
	grep := newStub(t, "grep-answer", "grep")
	llm := newStub(t, "llm-answer", "decisions/openai:m")
	h := New([]string{grep.URL, llm.URL}, 0).Handler()

	for _, tt := range []struct {
		version string
		from    *stubTagger
		answer  string
	}{{"decisions/openai:m", llm, "llm-answer"}, {"grep", grep, "grep-answer"}} {
		rec := do(h, http.MethodPost, "/tag", tagBody(tt.version))
		if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), tt.answer) || rec.Header().Get("Content-Type") != "application/json" {
			t.Fatalf("%s: %d %s", tt.version, rec.Code, rec.Body)
		}
		if got := <-tt.from.tagBodies; got != tagBody(tt.version) {
			t.Fatalf("the tagger received %q: the body must go on unchanged", got)
		}
	}
	if len(grep.tagBodies)+len(llm.tagBodies) != 0 {
		t.Fatal("a request reached a tagger of another version")
	}

	// whatever the tagger answers is relayed, an error included
	llm.tagStatus = http.StatusConflict
	rec := do(h, http.MethodPost, "/tag", tagBody("decisions/openai:m"))
	if rec.Code != http.StatusConflict {
		t.Fatalf("the status of the tagger was not relayed: %d", rec.Code)
	}
}

func TestTagOfAVersionNobodyServes(t *testing.T) {
	h := New([]string{newStub(t, "a", "grep").URL}, 0).Handler()
	rec := do(h, http.MethodPost, "/tag", tagBody("nope"))
	var e struct {
		Error struct {
			Code    string
			Details struct {
				Expected string
				Running  []string
			}
		}
	}
	if rec.Code != http.StatusConflict || json.Unmarshal(rec.Body.Bytes(), &e) != nil ||
		e.Error.Code != "tagger_version_mismatch" || e.Error.Details.Expected != "nope" || len(e.Error.Details.Running) != 1 || e.Error.Details.Running[0] != "grep" {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	for _, body := range []string{``, `nope`, `{"collection":"jobs"}`, `{"tagger_version":""}`} {
		if rec := do(h, http.MethodPost, "/tag", body); rec.Code != http.StatusBadRequest {
			t.Errorf("%q: %d", body, rec.Code)
		}
	}
}

func TestATaggerThatIsDownIsSkipped(t *testing.T) {
	dead := httptest.NewServer(http.NotFoundHandler())
	deadURL := dead.URL
	dead.Close()
	live := newStub(t, "live", "grep")
	h := New([]string{deadURL, live.URL}, 0).Handler()

	if got := versions(t, h); len(got) != 1 || got[0] != "grep" {
		t.Fatalf("versions = %v", got)
	}
	if rec := do(h, http.MethodGet, "/readyz", ""); rec.Code != http.StatusOK {
		t.Fatalf("readyz with one tagger up: %d", rec.Code)
	}
	if rec := do(h, http.MethodPost, "/tag", tagBody("grep")); rec.Code != http.StatusOK {
		t.Fatalf("tag: %d %s", rec.Code, rec.Body)
	}

	// nobody answers
	none := New([]string{deadURL}, 0).Handler()
	if rec := do(none, http.MethodGet, "/version", ""); rec.Code != http.StatusBadGateway {
		t.Fatalf("version with no tagger: %d", rec.Code)
	}
	if rec := do(none, http.MethodGet, "/readyz", ""); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("readyz with no tagger: %d", rec.Code)
	}
	if rec := do(none, http.MethodGet, "/healthz", ""); rec.Code != http.StatusOK {
		t.Fatalf("healthz: %d", rec.Code)
	}
}

// A tagger that was up when the versions were learned and is down when it is asked: 502, so that the caller
// can retry.
func TestTagToATaggerThatWentAway(t *testing.T) {
	a := newStub(t, "a", "grep")
	h := New([]string{a.URL}, time.Minute).Handler()
	versions(t, h) // learned
	a.Close()
	if rec := do(h, http.MethodPost, "/tag", tagBody("grep")); rec.Code != http.StatusBadGateway {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
}

func TestTheFirstTaggerServingAVersionIsUsed(t *testing.T) {
	first := newStub(t, "first", "grep")
	second := newStub(t, "second", "grep")
	h := New([]string{first.URL, second.URL}, 0).Handler()
	if rec := do(h, http.MethodPost, "/tag", tagBody("grep")); !strings.Contains(rec.Body.String(), "first") {
		t.Fatalf("%s", rec.Body)
	}
}

func TestVersionsAreRememberedForTheTTL(t *testing.T) {
	a := newStub(t, "a", "grep")
	h := New([]string{a.URL}, time.Hour).Handler()
	for i := 0; i < 5; i++ {
		versions(t, h)
		do(h, http.MethodPost, "/tag", tagBody("grep"))
	}
	if n := a.versionCalls.Load(); n != 1 {
		t.Fatalf("the tagger was asked for its versions %d times, want 1", n)
	}
	live := newStub(t, "b", "grep")
	h = New([]string{live.URL}, 0).Handler()
	versions(t, h)
	versions(t, h)
	if n := live.versionCalls.Load(); n != 2 {
		t.Fatalf("with no ttl: asked %d times, want 2", n)
	}
}

func TestFromEnv(t *testing.T) {
	env := func(m map[string]string) func(string) (string, bool) {
		return func(k string) (string, bool) { v, ok := m[k]; return v, ok }
	}
	r, err := FromEnv(env(map[string]string{"TAGGER_ROUTER_URLS": " http://a:8081/ , https://b:8081 ", "TAGGER_ROUTER_CACHE_TTL": "0s"}))
	if err != nil || len(r.urls) != 2 || r.urls[0] != "http://a:8081" || r.urls[1] != "https://b:8081" || r.ttl != 0 {
		t.Fatalf("%+v, %v", r, err)
	}
	if r, _ := FromEnv(env(map[string]string{"TAGGER_ROUTER_URLS": "http://a:8081"})); r.ttl != 5*time.Second {
		t.Fatalf("default ttl = %v", r.ttl)
	}
	for name, m := range map[string]map[string]string{
		"nothing":      {},
		"blank":        {"TAGGER_ROUTER_URLS": " , "},
		"not a url":    {"TAGGER_ROUTER_URLS": "tagger:8081"},
		"wrong scheme": {"TAGGER_ROUTER_URLS": "ftp://a"},
		"twice":        {"TAGGER_ROUTER_URLS": "http://a:1,http://a:1/"},
		"bad ttl":      {"TAGGER_ROUTER_URLS": "http://a:1", "TAGGER_ROUTER_CACHE_TTL": "soon"},
		"negative ttl": {"TAGGER_ROUTER_URLS": "http://a:1", "TAGGER_ROUTER_CACHE_TTL": "-1s"},
	} {
		if _, err := FromEnv(env(m)); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
}
