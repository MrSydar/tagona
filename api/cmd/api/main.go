package main

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"mrsydar/tagona/api/internal/metrics"
	"mrsydar/tagona/api/internal/storageapi"
	"mrsydar/tagona/api/openapi"
)

func main() {
	programLevel := new(slog.LevelVar)
	programLevel.Set(slog.LevelDebug)
	h := slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: programLevel})
	slog.SetDefault(slog.New(h))

	httpAddr := os.Getenv("API_HTTP_ADDR")
	if httpAddr == "" {
		httpAddr = ":8080"
	}
	storageBaseURL := os.Getenv("API_STORAGE_BASE_URL")
	if storageBaseURL == "" {
		fmt.Fprintf(os.Stderr, "config error: API_STORAGE_BASE_URL is required\n")
		os.Exit(1)
	}
	adminUsername := os.Getenv("API_ADMIN_USERNAME")
	adminPassword := os.Getenv("API_ADMIN_PASSWORD")
	if adminUsername == "" || adminPassword == "" {
		slog.Debug("admin credentials not configured; key management endpoints disabled")
	}

	maxBodyBytes := defaultMaxBodyBytes
	if v := os.Getenv("API_MAX_BODY_BYTES"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n < 0 {
			fmt.Fprintf(os.Stderr, "config error: API_MAX_BODY_BYTES must be a non-negative integer\n")
			os.Exit(1)
		}
		maxBodyBytes = n
	}

	keyCacheTTL := envDuration("API_KEY_CACHE_TTL", 30*time.Second)

	slog.Debug("starting tagona api service")

	// One connection pool shared by the proxy, the key client and readyz.
	transport := newStorageTransport()

	// Storage API key client for key validation and management.
	slog.Debug("initializing storage key client", "url", storageBaseURL)
	keyClient := storageapi.New(storageBaseURL, storageapi.WithTransport(transport))

	// Reverse proxy to the storage service.
	slog.Debug("initializing reverse proxy", "url", storageBaseURL)
	target, err := url.Parse(storageBaseURL)
	if err != nil {
		slog.Error("invalid API_STORAGE_BASE_URL", "error", err)
		os.Exit(1)
	}
	proxy := newProxy(target, transport)

	cfg := gatewayConfig{
		maxBodyBytes: maxBodyBytes,
		validator:    keyClient,
		httpClient:   &http.Client{Transport: transport},
	}
	if keyCacheTTL > 0 {
		cache := newCachedValidator(keyClient, keyCacheTTL)
		cfg.validator = cache
		cfg.onKeyDeleted = cache.Purge
	}

	docs, err := newDocsHandlers(openapi.V1)
	if err != nil {
		slog.Error("invalid embedded openapi document", "error", err)
		os.Exit(1)
	}
	cfg.docs = docs

	r := newRouter(storageBaseURL, proxy, keyClient, adminUsername, adminPassword, cfg)

	// HTTP server.
	slog.Debug("starting HTTP server", "addr", httpAddr)
	httpServer := &http.Server{
		Addr:         httpAddr,
		Handler:      r,
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 30 * time.Second,
	}

	go func() {
		slog.Info("starting tagona server", "addr", httpAddr, "storage_url", storageBaseURL)
		if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			slog.Error("http server error", "error", err)
			os.Exit(1)
		}
	}()

	// Graceful shutdown.
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	<-sig

	slog.Info("shutting down")
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer shutdownCancel()

	httpServer.Shutdown(shutdownCtx)
	slog.Info("shutdown complete")
}

// gatewayConfig holds the tunables of the proxied /v1/* surface.
type gatewayConfig struct {
	maxBodyBytes int64
	// validator checks Bearer keys; nil falls back to the key client.
	validator keyValidator
	// onKeyDeleted runs after a key is deleted through the gateway.
	onKeyDeleted func()
	// httpClient is used for the storage readiness probe; nil uses the default.
	httpClient *http.Client
	// docs serves the OpenAPI document and interactive docs; nil disables them.
	docs *docsHandlers
}

// newRouter builds the chi router. Admin key-management routes are static
// routes; the storage routes forwarded to the proxy are an explicit allowlist
// (see proxiedRoutes), so anything else under /v1/ is a 404.
func newRouter(storageBaseURL string, proxy http.Handler, keyClient *storageapi.Client, adminUsername, adminPassword string, cfg gatewayConfig) http.Handler {
	slog.Debug("creating router")
	validator := cfg.validator
	if validator == nil {
		validator = keyClient
	}
	httpClient := cfg.httpClient
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	r := chi.NewRouter()
	r.Use(requestLogger())
	r.Use(metrics.Middleware)
	r.Use(safePath)

	r.Get("/healthz", healthz)
	r.Get("/readyz", readyz(storageBaseURL, httpClient))
	r.Get("/metrics", promhttp.Handler().ServeHTTP)

	// Public documentation, registered as static routes ahead of the auth-guarded API.
	if cfg.docs != nil {
		cfg.docs.register(r)
	}

	r.Post("/v1/admin/api-keys", adminAuth(adminUsername, adminPassword, createAPIKey(keyClient)).ServeHTTP)
	r.Get("/v1/admin/api-keys", adminAuth(adminUsername, adminPassword, listAPIKeys(keyClient)).ServeHTTP)
	r.Delete("/v1/admin/api-keys/{id}", adminAuth(adminUsername, adminPassword, deleteAPIKey(keyClient, cfg.onKeyDeleted)).ServeHTTP)

	// Allowlisted storage routes require a Bearer API key before proxying.
	registerProxiedRoutes(r, proxy,
		func(next http.Handler) http.Handler { return apiKeyAuth(validator, next) },
		limitBody(cfg.maxBodyBytes),
	)
	r.NotFound(notFound)
	r.MethodNotAllowed(methodNotAllowed)
	return r
}

func healthz(w http.ResponseWriter, r *http.Request) {
	slog.Debug("healthz handler called")
	w.WriteHeader(http.StatusOK)
	w.Write([]byte("ok"))
}

func readyz(storageBaseURL string, client *http.Client) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) {
		slog.Debug("readyz handler called")
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, storageBaseURL+"/readyz", nil)
		if err != nil {
			slog.Error("readyz build request failed", "error", err)
			writeError(w, http.StatusServiceUnavailable, "not_ready", "storage service not available")
			return
		}
		resp, err := client.Do(req)
		if err != nil {
			slog.Error("readyz storage check failed", "error", err)
			writeError(w, http.StatusServiceUnavailable, "not_ready", "storage service not available")
			return
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			slog.Error("readyz storage not ready", "status", resp.StatusCode)
			writeError(w, http.StatusServiceUnavailable, "not_ready", "storage service not available")
			return
		}
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("ok"))
	}
}

func notFound(w http.ResponseWriter, r *http.Request) {
	slog.Debug("notFound handler called")
	writeError(w, http.StatusNotFound, "not_found", "not found")
}

// adminAuth guards the key-management endpoints with admin HTTP Basic auth.
// Bearer API keys are rejected there even when valid. When admin credentials
// are not configured, the endpoints are disabled entirely.
func adminAuth(username, password string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		slog.Debug("adminAuth middleware called")
		if username == "" || password == "" {
			slog.Debug("adminAuth rejected: credentials not configured")
			writeError(w, http.StatusForbidden, "admin_disabled", "admin credentials are not configured")
			return
		}
		if strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") {
			slog.Debug("adminAuth rejected: api keys cannot be used for key management")
			writeError(w, http.StatusForbidden, "forbidden", "api keys cannot be used for key management")
			return
		}
		user, pass, ok := r.BasicAuth()
		if !ok ||
			subtle.ConstantTimeCompare([]byte(user), []byte(username)) != 1 ||
			subtle.ConstantTimeCompare([]byte(pass), []byte(password)) != 1 {
			slog.Debug("adminAuth rejected: invalid admin credentials")
			w.Header().Set("WWW-Authenticate", `Basic realm="tagona-admin"`)
			writeError(w, http.StatusUnauthorized, "invalid_admin_credentials", "admin authentication required")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// apiKeyAuth enforces `Authorization: Bearer <api key>` before proxying to the
// storage service. The key is validated against the storage service.
func apiKeyAuth(keyClient keyValidator, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		slog.Debug("apiKeyAuth middleware called")
		auth := r.Header.Get("Authorization")
		key := strings.TrimPrefix(auth, "Bearer ")
		if !strings.HasPrefix(auth, "Bearer ") || key == "" {
			slog.Debug("apiKeyAuth rejected: missing bearer api key")
			writeError(w, http.StatusUnauthorized, "missing_api_key", "authorization header with bearer api key is required")
			return
		}
		valid, err := keyClient.ValidateKey(r.Context(), key)
		if err != nil {
			slog.Error("api key validation failed", "error", err)
			writeError(w, http.StatusServiceUnavailable, "not_ready", "storage service not available")
			return
		}
		if !valid {
			slog.Debug("apiKeyAuth rejected: invalid or unknown api key")
			writeError(w, http.StatusUnauthorized, "invalid_api_key", "invalid or unknown api key")
			return
		}
		next.ServeHTTP(w, r)
	})
}

func createAPIKey(keyClient *storageapi.Client) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		slog.Debug("createAPIKey handler called")
		var req struct {
			Name string `json:"name"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_json", "invalid request body")
			return
		}
		slog.Debug("creating api key", "name", req.Name)
		key, err := keyClient.CreateKey(r.Context(), req.Name)
		if err != nil {
			writeStorageError(w, err)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(key)
	}
}

func listAPIKeys(keyClient *storageapi.Client) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		slog.Debug("listAPIKeys handler called")
		keys, err := keyClient.ListKeys(r.Context())
		if err != nil {
			writeStorageError(w, err)
			return
		}
		if keys == nil {
			keys = []storageapi.APIKeyInfo{}
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"keys": keys})
	}
}

func deleteAPIKey(keyClient *storageapi.Client, onDeleted func()) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		slog.Debug("deleteAPIKey handler called")
		id := chi.URLParam(r, "id")
		slog.Debug("deleting api key", "id", id)
		if err := keyClient.DeleteKey(r.Context(), id); err != nil {
			writeStorageError(w, err)
			return
		}
		if onDeleted != nil {
			onDeleted()
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

// writeStorageError forwards the storage service error status and shape when
// the error is an HTTPError; transport failures map to 503 not_ready.
func writeStorageError(w http.ResponseWriter, err error) {
	var httpErr *storageapi.HTTPError
	if errors.As(err, &httpErr) {
		slog.Debug("storage returned error", "status", httpErr.Status, "code", httpErr.Code)
		writeError(w, httpErr.Status, httpErr.Code, httpErr.Message)
		return
	}
	slog.Error("storage request failed", "error", err)
	writeError(w, http.StatusServiceUnavailable, "not_ready", "storage service not available")
}

func writeError(w http.ResponseWriter, status int, code, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]string{
			"code":    code,
			"message": message,
		},
	})
}

func requestLogger() func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			next.ServeHTTP(w, r)
			slog.Info("http request",
				"method", r.Method,
				"path", r.URL.Path,
				"duration", time.Since(start),
			)
		})
	}
}
