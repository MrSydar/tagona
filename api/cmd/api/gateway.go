package main

import (
	"errors"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
)

// defaultMaxBodyBytes is the default request body cap enforced by the gateway.
// It is a backstop above storage's own per-object limit, which produces the
// friendlier payload_too_large error for normal uploads.
const defaultMaxBodyBytes int64 = 32 << 20

// proxiedRoutes is the allowlist of storage routes the gateway forwards. A
// route added to storage stays private until it is listed here.
var proxiedRoutes = []struct{ method, pattern string }{
	{http.MethodGet, "/v1/collections"},
	{http.MethodPost, "/v1/collections"},
	{http.MethodDelete, "/v1/collections/{collection}"},
	{http.MethodGet, "/v1/collections/{collection}/tags"},
	{http.MethodPost, "/v1/collections/{collection}/objects"},
	{http.MethodPost, "/v1/collections/{collection}/objects/query"},
	{http.MethodGet, "/v1/collections/{collection}/objects/{id}"},
	{http.MethodGet, "/v1/collections/{collection}/objects/{id}/data"},
	{http.MethodGet, "/v1/collections/{collection}/objects/{id}/tags"},
	{http.MethodDelete, "/v1/collections/{collection}/objects/{id}"},
}

// newProxy builds the reverse proxy to the storage service. The Authorization
// header is stripped (storage is auth-free and must never see API keys), and
// ReverseProxy drops inbound Forwarded/X-Forwarded-* headers when Rewrite is
// used, so clients cannot spoof them towards storage.
func newProxy(target *url.URL, rt http.RoundTripper) *httputil.ReverseProxy {
	return &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(target)
			pr.Out.Header.Del("Authorization")
		},
		Transport:    rt,
		ErrorHandler: proxyError,
	}
}

func proxyError(w http.ResponseWriter, r *http.Request, err error) {
	var maxErr *http.MaxBytesError
	if errors.As(err, &maxErr) {
		writeError(w, http.StatusRequestEntityTooLarge, "payload_too_large", "request body too large")
		return
	}
	slog.Error("proxy request failed", "path", r.URL.Path, "error", err)
	writeError(w, http.StatusBadGateway, "bad_gateway", "storage service not available")
}

// safePath rejects paths that could be used to escape the allowlisted route
// space once forwarded: dot segments, empty segments, backslashes, NUL bytes
// and encoded slashes.
func safePath(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !isSafePath(r.URL.Path, r.URL.EscapedPath()) {
			slog.Debug("safePath rejected request", "path", r.URL.Path)
			writeError(w, http.StatusBadRequest, "invalid_path", "invalid request path")
			return
		}
		next.ServeHTTP(w, r)
	})
}

func isSafePath(decoded, escaped string) bool {
	if strings.ContainsAny(decoded, "\\\x00") {
		return false
	}
	lower := strings.ToLower(escaped)
	if strings.Contains(lower, "%2f") || strings.Contains(lower, "%5c") {
		return false
	}
	if strings.Contains(decoded, "//") {
		return false
	}
	for _, seg := range strings.Split(decoded, "/") {
		if seg == "." || seg == ".." {
			return false
		}
	}
	return true
}

// limitBody caps request bodies. A declared Content-Length over the limit is
// rejected up front; chunked or understated bodies are cut off by
// MaxBytesReader while streaming. A max <= 0 disables the limit.
func limitBody(max int64) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		if max <= 0 {
			return next
		}
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.ContentLength > max {
				writeError(w, http.StatusRequestEntityTooLarge, "payload_too_large", "request body too large")
				return
			}
			r.Body = http.MaxBytesReader(w, r.Body, max)
			next.ServeHTTP(w, r)
		})
	}
}

func methodNotAllowed(w http.ResponseWriter, r *http.Request) {
	writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed")
}

// registerProxiedRoutes mounts the allowlist behind the given middlewares.
func registerProxiedRoutes(r chi.Router, proxy http.Handler, mw ...func(http.Handler) http.Handler) {
	r.Group(func(r chi.Router) {
		r.Use(mw...)
		for _, route := range proxiedRoutes {
			r.Method(route.method, route.pattern, proxy)
		}
	})
}

// newStorageTransport returns the transport shared by every storage-bound
// caller, with a pool sized for a gateway that talks to a single host.
func newStorageTransport() *http.Transport {
	return &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: (&net.Dialer{
			Timeout:   5 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		MaxIdleConns:        128,
		MaxIdleConnsPerHost: 64,
		IdleConnTimeout:     90 * time.Second,
	}
}

func envDuration(name string, def time.Duration) time.Duration {
	v := os.Getenv(name)
	if v == "" {
		return def
	}
	d, err := time.ParseDuration(v)
	if err != nil || d < 0 {
		slog.Error("invalid duration env var, using default", "name", name, "value", v, "default", def)
		return def
	}
	return d
}

func envFloat(name string, def float64) float64 {
	v := os.Getenv(name)
	if v == "" {
		return def
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil || f < 0 {
		slog.Error("invalid number env var, using default", "name", name, "value", v, "default", def)
		return def
	}
	return f
}
