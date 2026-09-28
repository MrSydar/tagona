package main

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"mrsydar/tagona/api/internal/metrics"
	"mrsydar/tagona/api/internal/storageapi"
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

	slog.Debug("starting tagona api service")

	// Storage API key client for key validation and management.
	slog.Debug("initializing storage key client", "url", storageBaseURL)
	keyClient := storageapi.New(storageBaseURL)

	// Reverse proxy to the storage service.
	slog.Debug("initializing reverse proxy", "url", storageBaseURL)
	target, err := url.Parse(storageBaseURL)
	if err != nil {
		slog.Error("invalid API_STORAGE_BASE_URL", "error", err)
		os.Exit(1)
	}
	proxy := httputil.NewSingleHostReverseProxy(target)

	r := newRouter(storageBaseURL, proxy, keyClient, adminUsername, adminPassword)

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

// newRouter builds the chi router. Admin key-management routes are registered
// as static routes so chi resolves them ahead of the /v1/* proxy catch-all.
func newRouter(storageBaseURL string, proxy http.Handler, keyClient *storageapi.Client, adminUsername, adminPassword string) http.Handler {
	slog.Debug("creating router")
	r := chi.NewRouter()
	r.Use(requestLogger())
	r.Use(metrics.Middleware)

	r.Get("/healthz", healthz)
	r.Get("/readyz", readyz(storageBaseURL))
	r.Get("/metrics", promhttp.Handler().ServeHTTP)

	r.Post("/v1/admin/api-keys", adminAuth(adminUsername, adminPassword, createAPIKey(keyClient)).ServeHTTP)
	r.Get("/v1/admin/api-keys", adminAuth(adminUsername, adminPassword, listAPIKeys(keyClient)).ServeHTTP)
	r.Delete("/v1/admin/api-keys/{id}", adminAuth(adminUsername, adminPassword, deleteAPIKey(keyClient)).ServeHTTP)

	// All other /v1/* traffic requires a Bearer API key before proxying.
	r.Handle("/v1/*", apiKeyAuth(keyClient, proxy))
	r.NotFound(notFound)
	return r
}

func healthz(w http.ResponseWriter, r *http.Request) {
	slog.Debug("healthz handler called")
	w.WriteHeader(http.StatusOK)
	w.Write([]byte("ok"))
}

func readyz(storageBaseURL string) func(http.ResponseWriter, *http.Request) {
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
		resp, err := http.DefaultClient.Do(req)
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
func apiKeyAuth(keyClient *storageapi.Client, next http.Handler) http.Handler {
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

func deleteAPIKey(keyClient *storageapi.Client) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		slog.Debug("deleteAPIKey handler called")
		id := chi.URLParam(r, "id")
		slog.Debug("deleting api key", "id", id)
		if err := keyClient.DeleteKey(r.Context(), id); err != nil {
			writeStorageError(w, err)
			return
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
