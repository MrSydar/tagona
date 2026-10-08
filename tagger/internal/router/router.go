// Package router is the tagger implementation "router": a tagger that evaluates nothing itself and
// routes to other tagger services. Each of them serves its own version(s); the router serves their union
// and hands a tag request to the tagger that serves the version it asks for, which fetches the object,
// evaluates the tags and answers. This is how one deployment supports several taggers at once.
package router

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"mrsydar/tagona/tagger/internal/metrics"
	taggerclient "mrsydar/tagona/tagger/pkg/client"
)

const (
	versionTimeout = 5 * time.Second // for one target's /version
	maxTagBody     = 1 << 20
)

// target is one tagger service and the versions it served when it was last asked.
type target struct {
	url      string
	versions []string
	err      error
}

// Router routes to the tagger services at urls.
type Router struct {
	urls []string
	ttl  time.Duration
	http *http.Client

	mu       sync.Mutex
	targets  []target
	fetched  time.Time
	warnedOn map[string]bool // versions served twice that were already reported
}

// New creates a router over the given tagger base URLs. The versions the taggers serve are asked for again
// when what is known is older than ttl (0: for every request).
func New(urls []string, ttl time.Duration) *Router {
	return &Router{urls: urls, ttl: ttl, http: &http.Client{}, warnedOn: map[string]bool{}}
}

// FromEnv creates the router TAGGER_ROUTER_URLS describes: the base URLs of the tagger services, comma
// separated. TAGGER_ROUTER_CACHE_TTL (default 5s, 0 for none) says how long what they serve is remembered.
func FromEnv(lookup func(string) (string, bool)) (*Router, error) {
	list, _ := lookup("TAGGER_ROUTER_URLS")
	var urls []string
	seen := map[string]bool{}
	for _, raw := range strings.Split(list, ",") {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			continue
		}
		u, err := url.Parse(raw)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return nil, fmt.Errorf("TAGGER_ROUTER_URLS: %q is not an http(s) URL", raw)
		}
		raw = strings.TrimRight(raw, "/")
		if seen[raw] {
			return nil, fmt.Errorf("TAGGER_ROUTER_URLS: %q is listed twice", raw)
		}
		seen[raw] = true
		urls = append(urls, raw)
	}
	if len(urls) == 0 {
		return nil, errors.New("TAGGER_ROUTER_URLS must list the taggers to route to, for example http://tagger-grep:8081,http://tagger-llm:8081")
	}
	ttl := 5 * time.Second
	if v, ok := lookup("TAGGER_ROUTER_CACHE_TTL"); ok && v != "" {
		d, err := time.ParseDuration(v)
		if err != nil || d < 0 {
			return nil, fmt.Errorf("TAGGER_ROUTER_CACHE_TTL: %q is not a duration of 0 or more", v)
		}
		ttl = d
	}
	return New(urls, ttl), nil
}

// snapshot returns the targets and what they serve, asking them again when it is time to.
func (r *Router) snapshot(ctx context.Context) []target {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.targets != nil && r.ttl > 0 && time.Since(r.fetched) < r.ttl {
		return r.targets
	}
	targets := make([]target, len(r.urls))
	var wg sync.WaitGroup
	for i, url := range r.urls {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// the answer is kept for later requests, so it must not die with the request that asked for it
			ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), versionTimeout)
			defer cancel()
			versions, err := taggerclient.New(url, versionTimeout).Versions(ctx)
			if err != nil {
				slog.Warn("router: a tagger did not report its versions", "url", url, "error", err)
			}
			targets[i] = target{url: url, versions: versions, err: err}
		}()
	}
	wg.Wait()
	r.targets, r.fetched = targets, time.Now()
	return targets
}

// served lists the versions the taggers serve in the order of the configuration, each once, and the tagger
// that serves each: the first one in the configuration when two serve the same version.
func (r *Router) served(ctx context.Context) (versions []string, owner map[string]string) {
	targets := r.snapshot(ctx)
	r.mu.Lock()
	defer r.mu.Unlock()
	owner = map[string]string{}
	for _, t := range targets {
		for _, v := range t.versions {
			if first, taken := owner[v]; taken {
				if first != t.url && !r.warnedOn[v] {
					r.warnedOn[v] = true
					slog.Warn("router: a version is served by two taggers; the first one in the configuration is used", "version", v, "used", first, "ignored", t.url)
				}
				continue
			}
			owner[v] = t.url
			versions = append(versions, v)
		}
	}
	return versions, owner
}

// Handler is the router's HTTP API: the API of a tagger.
func (r *Router) Handler() http.Handler {
	mux := chi.NewRouter()
	mux.Use(metrics.Middleware)
	mux.Get("/healthz", func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("ok")) })
	mux.Get("/readyz", r.readyz)
	mux.Get("/version", r.version)
	mux.Post("/tag", r.tag)
	mux.Get("/metrics", promhttp.Handler().ServeHTTP)
	return mux
}

// readyz is ready when at least one tagger answers.
func (r *Router) readyz(w http.ResponseWriter, req *http.Request) {
	if versions, _ := r.served(req.Context()); len(versions) == 0 {
		writeError(w, http.StatusServiceUnavailable, "not_ready", "no tagger answers")
		return
	}
	w.Write([]byte("ok"))
}

func (r *Router) version(w http.ResponseWriter, req *http.Request) {
	versions, _ := r.served(req.Context())
	if len(versions) == 0 {
		writeError(w, http.StatusBadGateway, "no_tagger", "no tagger answers")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string][]string{"version": versions})
}

// tag hands the request to the tagger that serves its tagger_version and relays the answer as it is.
func (r *Router) tag(w http.ResponseWriter, req *http.Request) {
	body, err := io.ReadAll(io.LimitReader(req.Body, maxTagBody+1))
	var ask struct {
		TaggerVersion string `json:"tagger_version"`
	}
	if err != nil || len(body) > maxTagBody || json.Unmarshal(body, &ask) != nil || ask.TaggerVersion == "" {
		writeError(w, http.StatusBadRequest, "invalid_request", "a JSON body with tagger_version is required")
		return
	}

	versions, owner := r.served(req.Context())
	url, ok := owner[ask.TaggerVersion]
	if !ok {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{
			"code":    "tagger_version_mismatch",
			"message": fmt.Sprintf("no tagger serves %q; the router serves %s", ask.TaggerVersion, listOf(versions)),
			"details": map[string]any{"expected": ask.TaggerVersion, "running": versions},
		}})
		return
	}

	out, err := http.NewRequestWithContext(req.Context(), http.MethodPost, url+"/tag", bytes.NewReader(body))
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", "could not build the request")
		return
	}
	out.Header.Set("Content-Type", "application/json")
	resp, err := r.http.Do(out)
	if err != nil {
		slog.Error("router: the tagger did not answer", "url", url, "version", ask.TaggerVersion, "error", err)
		writeError(w, http.StatusBadGateway, "tagger_unreachable", "the tagger of this version did not answer")
		return
	}
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); ct != "" {
		w.Header().Set("Content-Type", ct)
	}
	w.WriteHeader(resp.StatusCode)
	io.Copy(w, resp.Body)
}

// listOf writes a list of versions for a message: ["grep","false"].
func listOf(items []string) string {
	b, _ := json.Marshal(items)
	return string(b)
}

func writeError(w http.ResponseWriter, status int, code, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{"code": code, "message": message}})
}
