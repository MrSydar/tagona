// Package server is keystorage's HTTP API.
//
// Two surfaces share one listener, which is reachable only on the internal compose network:
//
//   - POST /api-keys/validate checks a raw key. It needs no credentials: its callers
//     (the api gateway, and other services such as the Plus translator) hold the key they are
//     checking, not a service credential.
//   - /api-keys (create, list) and /api-keys/{id} (delete) manage keys and authenticate the admin credentials itself (HTTP Basic),
//     so reaching the network is not enough to mint, list or delete a key. The public api exposes
//     only this surface, as a strict allowlisting proxy; the validate route is never forwarded.
package server

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/go-chi/chi/v5"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"mrsydar/tagona/keystorage/internal/db"
	"mrsydar/tagona/keystorage/internal/keys"
	"mrsydar/tagona/keystorage/internal/metrics"
)

const (
	maxBodyBytes = 4 << 10
	maxNameBytes = 128
	// hardMaxTTLSeconds bounds ttl_seconds whatever the configuration says (100 years), so that an
	// absurd value is a 400 and not an out-of-range timestamp in the database.
	hardMaxTTLSeconds = 100 * 365 * 24 * 3600
)

var uuidRe = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// Store is the persistence the server needs; *db.DB implements it.
type Store interface {
	Ping(ctx context.Context) error
	Create(ctx context.Context, keyHash, keyPrefix, name string, ttlSeconds int64) (*db.APIKey, error)
	List(ctx context.Context) ([]db.APIKey, error)
	// GetByHash returns db.ErrNotFound for an unknown key, and the key with db.ErrExpired for one
	// whose expiry has passed.
	GetByHash(ctx context.Context, keyHash string) (*db.APIKey, error)
	Delete(ctx context.Context, id string) (bool, error)
}

// Server holds the dependencies of the HTTP handlers.
type Server struct {
	store         Store
	adminUsername string
	adminPassword string
	defaultTTL    time.Duration
	maxTTL        time.Duration
}

// Option customizes a Server.
type Option func(*Server)

// WithKeyTTL sets the lifetime of a key created without ttl_seconds (0: it never expires) and the
// largest ttl_seconds that may be asked for (0: no cap). With a cap but no default, a key created
// without ttl_seconds gets the cap, so that no key outlives it.
func WithKeyTTL(defaultTTL, maxTTL time.Duration) Option {
	return func(s *Server) { s.defaultTTL, s.maxTTL = defaultTTL, maxTTL }
}

// New creates a server. Empty admin credentials disable key management.
func New(store Store, adminUsername, adminPassword string, opts ...Option) *Server {
	s := &Server{store: store, adminUsername: adminUsername, adminPassword: adminPassword}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

// ttlSeconds is the lifetime to give a new key: the requested one, or the default, or 0 for none.
func (s *Server) ttlSeconds(requested *int64) (int64, *requestProblem) {
	if requested == nil {
		switch {
		case s.defaultTTL > 0:
			return int64(s.defaultTTL.Seconds()), nil
		case s.maxTTL > 0:
			return int64(s.maxTTL.Seconds()), nil
		}
		return 0, nil
	}
	if *requested < 1 {
		return 0, &requestProblem{"invalid_ttl", "ttl_seconds must be at least 1"}
	}
	if *requested > hardMaxTTLSeconds {
		return 0, &requestProblem{"invalid_ttl", fmt.Sprintf("ttl_seconds must be at most %d", hardMaxTTLSeconds)}
	}
	if s.maxTTL > 0 && time.Duration(*requested)*time.Second > s.maxTTL {
		return 0, &requestProblem{"invalid_ttl", fmt.Sprintf("ttl_seconds must be at most %d", int64(s.maxTTL.Seconds()))}
	}
	return *requested, nil
}

type requestProblem struct{ code, message string }

// Router builds the routes.
func (s *Server) Router() http.Handler {
	r := chi.NewRouter()
	r.Use(requestLogger)
	r.Use(metrics.Middleware)

	r.Get("/healthz", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok")) })
	r.Get("/readyz", s.readyz)
	r.Get("/metrics", promhttp.Handler().ServeHTTP)

	r.Post("/api-keys/validate", s.validate)

	r.Group(func(r chi.Router) {
		r.Use(s.adminAuth)
		r.Post("/api-keys", s.create)
		r.Get("/api-keys", s.list)
		r.Delete("/api-keys/{id}", s.delete)
	})

	r.NotFound(func(w http.ResponseWriter, _ *http.Request) {
		writeError(w, http.StatusNotFound, "not_found", "not found")
	})
	r.MethodNotAllowed(func(w http.ResponseWriter, _ *http.Request) {
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed")
	})
	return r
}

func (s *Server) readyz(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	if err := s.store.Ping(ctx); err != nil {
		slog.Error("readyz database ping failed", "error", err)
		writeError(w, http.StatusServiceUnavailable, "not_ready", "database not available")
		return
	}
	_, _ = w.Write([]byte("ok"))
}

// adminAuth guards key management with the admin credentials. Bearer API keys are rejected even
// when valid, and without configured credentials the endpoints are disabled entirely.
func (s *Server) adminAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.adminUsername == "" || s.adminPassword == "" {
			writeError(w, http.StatusForbidden, "admin_disabled", "admin credentials are not configured")
			return
		}
		if strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") {
			writeError(w, http.StatusForbidden, "forbidden", "api keys cannot be used for key management")
			return
		}
		user, pass, ok := r.BasicAuth()
		// Both comparisons always run, so the response time does not reveal which one failed.
		userOK := subtle.ConstantTimeCompare([]byte(user), []byte(s.adminUsername)) == 1
		passOK := subtle.ConstantTimeCompare([]byte(pass), []byte(s.adminPassword)) == 1
		if !ok || !userOK || !passOK {
			w.Header().Set("WWW-Authenticate", `Basic realm="tagona-admin"`)
			writeError(w, http.StatusUnauthorized, "invalid_admin_credentials", "admin authentication required")
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) create(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name       string `json:"name"`
		TTLSeconds *int64 `json:"ttl_seconds"`
	}
	if !decode(w, r, &req) {
		return
	}
	if req.Name == "" || !utf8.ValidString(req.Name) || len(req.Name) > maxNameBytes {
		writeError(w, http.StatusBadRequest, "invalid_name", "api key name must be 1-128 bytes of valid UTF-8")
		return
	}
	ttl, problem := s.ttlSeconds(req.TTLSeconds)
	if problem != nil {
		writeError(w, http.StatusBadRequest, problem.code, problem.message)
		return
	}
	raw, hash, prefix, err := keys.Generate()
	if err != nil {
		slog.Error("generate api key failed", "error", err)
		writeError(w, http.StatusInternalServerError, "internal_error", "failed to generate api key")
		return
	}
	key, err := s.store.Create(r.Context(), hash, prefix, req.Name, ttl)
	if err != nil {
		slog.Error("create api key failed", "error", err)
		writeError(w, http.StatusInternalServerError, "internal_error", "failed to create api key")
		return
	}
	// The raw key appears only in this response; only its hash is stored.
	writeJSON(w, http.StatusCreated, map[string]any{
		"id": key.ID, "name": key.Name, "key": raw, "created_at": key.CreatedAt, "expires_at": key.ExpiresAt,
	})
}

func (s *Server) list(w http.ResponseWriter, r *http.Request) {
	list, err := s.store.List(r.Context())
	if err != nil {
		slog.Error("list api keys failed", "error", err)
		writeError(w, http.StatusInternalServerError, "internal_error", "failed to list api keys")
		return
	}
	if list == nil {
		list = []db.APIKey{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"keys": list})
}

func (s *Server) delete(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if !uuidRe.MatchString(id) { // not a UUID, so no such key (and Postgres would reject the cast)
		writeError(w, http.StatusNotFound, "not_found", "api key not found")
		return
	}
	deleted, err := s.store.Delete(r.Context(), id)
	if err != nil {
		slog.Error("delete api key failed", "error", err)
		writeError(w, http.StatusInternalServerError, "internal_error", "failed to delete api key")
		return
	}
	if !deleted {
		writeError(w, http.StatusNotFound, "not_found", "api key not found")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// validate answers 200 with the key's id, name and expiry when the raw key is valid. Otherwise it
// answers 401: invalid_api_key for a key it does not know, expired_api_key for one that has expired,
// so that a client holding an expired key knows to get a new one.
func (s *Server) validate(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Key string `json:"key"`
	}
	if !decode(w, r, &req) {
		return
	}
	if req.Key == "" {
		writeError(w, http.StatusBadRequest, "missing_key", "key is required")
		return
	}
	key, err := s.store.GetByHash(r.Context(), keys.Hash(req.Key))
	if errors.Is(err, db.ErrNotFound) {
		writeError(w, http.StatusUnauthorized, "invalid_api_key", "invalid or unknown api key")
		return
	}
	if errors.Is(err, db.ErrExpired) {
		writeError(w, http.StatusUnauthorized, "expired_api_key", "api key has expired")
		return
	}
	if err != nil {
		slog.Error("validate api key failed", "error", err)
		writeError(w, http.StatusInternalServerError, "internal_error", "failed to validate api key")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"id": key.ID, "name": key.Name, "expires_at": key.ExpiresAt})
}

func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			writeError(w, http.StatusRequestEntityTooLarge, "payload_too_large", "request body too large")
			return false
		}
		writeError(w, http.StatusBadRequest, "invalid_json", "invalid request body")
		return false
	}
	return true
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, map[string]any{"error": map[string]string{"code": code, "message": message}})
}

// requestLogger logs method, path and duration. It never logs headers or bodies, which carry
// credentials and raw API keys.
func requestLogger(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		next.ServeHTTP(w, r)
		slog.Info("http request", "method", r.Method, "path", r.URL.Path, "duration", time.Since(start))
	})
}
