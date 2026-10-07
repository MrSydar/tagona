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
	storageBaseURL := os.Getenv("TAGGER_STORAGE_BASE_URL")
	if storageBaseURL == "" {
		storageBaseURL = "http://localhost:8082"
	}
	evaluatorImpl := os.Getenv("TAGGER_EVALUATOR_IMPL")
	if evaluatorImpl == "" {
		evaluatorImpl = "grep"
	}

	ev, err := evaluator.New(evaluatorImpl, os.LookupEnv)
	if err != nil {
		fmt.Fprintf(os.Stderr, "evaluator configuration error: %v\n", err)
		os.Exit(1)
	}

	storageClient := storageclient.NewInternal(storageBaseURL)
	srv := server.NewServer(storageClient, ev, evaluatorImpl)

	httpServer := &http.Server{
		Addr:         httpAddr,
		Handler:      srv.Router(),
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 30 * time.Second,
	}

	go func() {
		slog.Info("starting tagger server", "addr", httpAddr, "storage_url", storageBaseURL)
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
