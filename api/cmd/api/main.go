package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"mrsydar/tagona/api/internal/metrics"
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

	slog.Debug("starting tagona api service")

	// Reverse proxy to the storage service.
	slog.Debug("initializing reverse proxy", "url", storageBaseURL)
	target, err := url.Parse(storageBaseURL)
	if err != nil {
		slog.Error("invalid API_STORAGE_BASE_URL", "error", err)
		os.Exit(1)
	}
	proxy := httputil.NewSingleHostReverseProxy(target)

	slog.Debug("creating router")
	r := chi.NewRouter()
	r.Use(requestLogger())
	r.Use(metrics.Middleware)

	r.Get("/healthz", healthz)
	r.Get("/readyz", readyz(storageBaseURL))
	r.Get("/metrics", promhttp.Handler().ServeHTTP)
	r.Handle("/v1/*", proxy)
	r.NotFound(notFound)

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
