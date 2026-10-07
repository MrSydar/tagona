package main

import (
	"log/slog"
	"net"
	"net/http"
	"os"
	"strings"
	"time"
)

// defaultMaxBodyBytes is the default request body cap enforced by the gateway.
// It is a backstop above storage's own per-object limit, which produces the
// friendlier payload_too_large error for normal uploads.
const defaultMaxBodyBytes int64 = 32 << 20

// adminMaxBodyBytes caps key-management request bodies, which are tiny.
const adminMaxBodyBytes int64 = 4 << 10

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

// newStorageTransport returns the transport shared by every internal-service caller, with a pool
// sized for a gateway that talks to a few hosts.
func newStorageTransport() *http.Transport {
	return &http.Transport{
		Proxy:              http.ProxyFromEnvironment,
		DisableCompression: true, // answers are relayed as they are, and no Accept-Encoding is sent
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
