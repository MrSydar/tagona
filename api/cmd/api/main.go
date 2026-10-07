package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"mrsydar/tagona/api/internal/keystorageapi"
	"mrsydar/tagona/api/internal/metrics"
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
	keystorageBaseURL := os.Getenv("API_KEYSTORAGE_BASE_URL")
	if keystorageBaseURL == "" {
		fmt.Fprintf(os.Stderr, "config error: API_KEYSTORAGE_BASE_URL is required\n")
		os.Exit(1)
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

	// One connection pool shared by the proxies, the key client and readyz.
	transport := newStorageTransport()

	storage, err := newUpstream("storage", storageBaseURL, transport,
		http.StatusBadGateway, "bad_gateway", "storage service not available")
	if err != nil {
		slog.Error("invalid API_STORAGE_BASE_URL", "error", err)
		os.Exit(1)
	}
	keystorage, err := newUpstream("keystorage", keystorageBaseURL, transport,
		http.StatusServiceUnavailable, "not_ready", "key service not available",
		"WWW-Authenticate") // the admin 401 tells clients to use Basic auth
	if err != nil {
		slog.Error("invalid API_KEYSTORAGE_BASE_URL", "error", err)
		os.Exit(1)
	}

	// Keys are validated by keystorage, which is also where key management is sent.
	slog.Debug("initializing keystorage client", "url", keystorageBaseURL)
	keyClient := keystorageapi.New(keystorageBaseURL, keystorageapi.WithTransport(transport))

	cfg := gatewayConfig{
		validator:  keyClient,
		httpClient: &http.Client{Transport: transport},
	}
	if keyCacheTTL > 0 {
		cache := newCachedValidator(keyClient, keyCacheTTL)
		cfg.validator = cache
		cfg.onKeyDeleted = cache.Purge
	}
	gw := &gateway{storage: storage, keystorage: keystorage, maxBodyBytes: maxBodyBytes, onKeyDeleted: cfg.onKeyDeleted}

	docs, err := newDocsHandlers(openapi.V1)
	if err != nil {
		slog.Error("invalid embedded openapi document", "error", err)
		os.Exit(1)
	}
	cfg.docs = docs

	r := newRouter([]string{storageBaseURL, keystorageBaseURL}, gw, cfg)

	// HTTP server.
	slog.Debug("starting HTTP server", "addr", httpAddr)
	httpServer := &http.Server{
		Addr:         httpAddr,
		Handler:      r,
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 30 * time.Second,
	}

	go func() {
		slog.Info("starting tagona server", "addr", httpAddr, "storage_url", storageBaseURL, "keystorage_url", keystorageBaseURL)
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

// gatewayConfig holds the router's collaborators.
type gatewayConfig struct {
	// validator checks Bearer keys.
	validator keyValidator
	// onKeyDeleted runs after a key is deleted through the admin routes.
	onKeyDeleted func()
	// httpClient is used for the readiness probes; nil uses the default.
	httpClient *http.Client
	// docs serves the OpenAPI document and interactive docs; nil disables them.
	docs *docsHandlers
}

// newRouter builds the chi router. Every route below is a handler that checks the request and
// builds a new one for the internal service (see handlers.go); nothing is forwarded as it came in,
// and anything not listed under /v1/ is a 404. upstreams are the services whose readiness /readyz
// reports.
func newRouter(upstreams []string, g *gateway, cfg gatewayConfig) http.Handler {
	slog.Debug("creating router")
	httpClient := cfg.httpClient
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	r := chi.NewRouter()
	r.Use(requestLogger())
	r.Use(metrics.Middleware)
	r.Use(safePath)

	r.Get("/healthz", healthz)
	r.Get("/readyz", readyz(upstreams, httpClient))
	r.Get("/metrics", promhttp.Handler().ServeHTTP)

	// Public documentation, registered as static routes ahead of the auth-guarded API.
	if cfg.docs != nil {
		cfg.docs.register(r)
	}

	// Key management: keystorage checks the admin credentials, which the handlers pass on.
	r.Group(func(r chi.Router) {
		r.Use(limitBody(adminMaxBodyBytes))
		r.Post("/v1/admin/api-keys", g.createKey)
		r.Get("/v1/admin/api-keys", g.listKeys)
		r.Delete("/v1/admin/api-keys/{id}", g.deleteKey)
	})

	// Everything else requires a Bearer API key.
	r.Group(func(r chi.Router) {
		r.Use(func(next http.Handler) http.Handler { return apiKeyAuth(cfg.validator, next) })

		r.Group(func(r chi.Router) {
			r.Use(limitBody(maxJSONBodyBytes))
			r.Get("/v1/collections", g.listCollections)
			r.Post("/v1/collections", g.createCollection)
			r.Delete("/v1/collections/{collection}", g.deleteCollection)
			r.Get("/v1/collections/{collection}/tags", g.listCollectionTags)
			r.Post("/v1/collections/{collection}/objects/query", g.queryObjects)
			r.Get("/v1/collections/{collection}/objects/{id}", g.getObject)
			r.Get("/v1/collections/{collection}/objects/{id}/data", g.getObjectData)
			r.Get("/v1/collections/{collection}/objects/{id}/tags", g.getObjectTags)
			r.Delete("/v1/collections/{collection}/objects/{id}", g.deleteObject)
		})
		r.With(limitBody(g.maxBodyBytes)).Post("/v1/collections/{collection}/objects", g.putObject)
	})
	r.NotFound(notFound)
	r.MethodNotAllowed(methodNotAllowed)
	return r
}

func healthz(w http.ResponseWriter, r *http.Request) {
	slog.Debug("healthz handler called")
	w.WriteHeader(http.StatusOK)
	w.Write([]byte("ok"))
}

// readyz is ready only when every upstream answers its own /readyz with 200.
func readyz(upstreams []string, client *http.Client) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) {
		slog.Debug("readyz handler called")
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()
		for _, base := range upstreams {
			req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/readyz", nil)
			if err != nil {
				slog.Error("readyz build request failed", "upstream", base, "error", err)
				writeError(w, http.StatusServiceUnavailable, "not_ready", "a backing service is not available")
				return
			}
			resp, err := client.Do(req)
			if err != nil {
				slog.Error("readyz upstream check failed", "upstream", base, "error", err)
				writeError(w, http.StatusServiceUnavailable, "not_ready", "a backing service is not available")
				return
			}
			resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				slog.Error("readyz upstream not ready", "upstream", base, "status", resp.StatusCode)
				writeError(w, http.StatusServiceUnavailable, "not_ready", "a backing service is not available")
				return
			}
		}
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("ok"))
	}
}

func notFound(w http.ResponseWriter, r *http.Request) {
	slog.Debug("notFound handler called")
	writeError(w, http.StatusNotFound, "not_found", "not found")
}

// apiKeyAuth enforces `Authorization: Bearer <api key>` before proxying to the
// storage service. The key is validated against the keystorage service.
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
		res, err := keyClient.ValidateKey(r.Context(), key)
		if err != nil {
			slog.Error("api key validation failed", "error", err)
			writeError(w, http.StatusServiceUnavailable, "not_ready", "key service not available")
			return
		}
		switch res.Verdict {
		case keystorageapi.Valid:
		case keystorageapi.Expired:
			// Its own code, so a client that holds an expired key knows to get a new one.
			slog.Debug("apiKeyAuth rejected: expired api key")
			writeError(w, http.StatusUnauthorized, "expired_api_key", "api key has expired")
			return
		default:
			slog.Debug("apiKeyAuth rejected: invalid or unknown api key")
			writeError(w, http.StatusUnauthorized, "invalid_api_key", "invalid or unknown api key")
			return
		}
		next.ServeHTTP(w, r)
	})
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
