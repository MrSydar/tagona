package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	storageclient "mrsydar/tagona/storage/pkg/client"
	"mrsydar/tagona/tagger/internal/router"
	"mrsydar/tagona/tagger/internal/server"
	"mrsydar/tagona/tagger/pkg/evaluator"
)

func main() {
	programLevel := new(slog.LevelVar)
	programLevel.Set(slog.LevelDebug)
	h := slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: programLevel})
	slog.SetDefault(slog.New(h))

	httpAddr := os.Getenv("TAGGER_HTTP_ADDR")
	if httpAddr == "" {
		httpAddr = ":8081"
	}
	evaluatorImpl := os.Getenv("TAGGER_EVALUATOR_IMPL")
	if evaluatorImpl == "" {
		evaluatorImpl = "grep"
	}
	if evaluatorImpl == "router" {
		runRouter(httpAddr)
		return
	}

	storageBaseURL := os.Getenv("TAGGER_STORAGE_BASE_URL")
	if storageBaseURL == "" {
		storageBaseURL = "http://localhost:8082"
	}
	ev, err := evaluator.New(evaluatorImpl, os.LookupEnv)
	if err != nil {
		fmt.Fprintf(os.Stderr, "evaluator configuration error: %v\n", err)
		os.Exit(1)
	}

	// The version names what tagged the objects: "<implementation>[:<model>]", which the evaluator knows.
	// TAGGER_VERSION replaces it, to keep a collection's version through a change of implementation or
	// model that is known to tag alike (or to name one that is not what it seems).
	version := ev.Version()
	if v := os.Getenv("TAGGER_VERSION"); v != "" {
		if err := evaluator.ValidateVersion(v); err != nil {
			fmt.Fprintf(os.Stderr, "TAGGER_VERSION: %v\n", err)
			os.Exit(1)
		}
		version = v
	}

	srv := server.NewServer(storageclient.NewInternal(storageBaseURL), ev, evaluatorImpl, version)
	serve(httpAddr, srv.Router(), 30*time.Second, "storage_url", storageBaseURL, "version", version)
}

// runRouter runs the tagger implementation "router": it evaluates nothing and routes to other tagger
// services (TAGGER_ROUTER_URLS), each serving its own versions.
func runRouter(httpAddr string) {
	if os.Getenv("TAGGER_VERSION") != "" {
		fmt.Fprintln(os.Stderr, "TAGGER_VERSION cannot be used with the router: it serves the versions of its taggers")
		os.Exit(1)
	}
	rt, err := router.FromEnv(os.LookupEnv)
	if err != nil {
		fmt.Fprintf(os.Stderr, "router configuration error: %v\n", err)
		os.Exit(1)
	}
	// longer than a tagger's own limit, so that a slow answer of one of them is not cut off here
	serve(httpAddr, rt.Handler(), 60*time.Second, "router", true)
}

// serve runs the HTTP server until the process is told to stop.
func serve(addr string, handler http.Handler, writeTimeout time.Duration, logFields ...any) {
	httpServer := &http.Server{
		Addr:         addr,
		Handler:      handler,
		ReadTimeout:  30 * time.Second,
		WriteTimeout: writeTimeout,
	}

	go func() {
		slog.Info("starting tagger server", append([]any{"addr", addr}, logFields...)...)
		if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			slog.Error("http server error", "error", err)
			os.Exit(1)
		}
	}()

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	<-sig

	slog.Info("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	httpServer.Shutdown(shutdownCtx)
	slog.Info("shutdown complete")
}
