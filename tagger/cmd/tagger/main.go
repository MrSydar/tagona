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

	var ev evaluator.Evaluator
	switch evaluatorImpl {
	case "grep":
		ev = evaluator.NewGrepEvaluator()
	case "false":
		ev = evaluator.NewFalseEvaluator()
	case "openai":
		openaiAPIKey := os.Getenv("TAGGER_OPENAI_API_KEY")
		openaiBaseURL := os.Getenv("TAGGER_OPENAI_BASE_URL")
		if openaiBaseURL == "" {
			openaiBaseURL = "https://api.openai.com/v1"
		}
		openaiModel := os.Getenv("TAGGER_OPENAI_MODEL")
		if openaiModel == "" {
			openaiModel = "gpt-4o-mini"
		}
		openaiTimeoutStr := os.Getenv("TAGGER_OPENAI_TIMEOUT")
		openaiTimeout := 60 * time.Second
		if openaiTimeoutStr != "" {
			if d, err := time.ParseDuration(openaiTimeoutStr); err == nil {
				openaiTimeout = d
			} else {
				slog.Warn("invalid TAGGER_OPENAI_TIMEOUT, using default", "default", openaiTimeout, "error", err)
			}
		}
		ev = evaluator.NewOpenAIEvaluator(openaiAPIKey, openaiBaseURL, openaiModel, openaiTimeout)
	case "systemone":
		backend := os.Getenv("TAGGER_SYSTEMONE_BACKEND")
		if backend == "" {
			backend = "vercel"
		}
		sone, err := evaluator.NewSystemoneEvaluator(backend)
		if err != nil {
			fmt.Fprintf(os.Stderr, "failed to create systemone evaluator: %v\n", err)
			os.Exit(1)
		}
		ev = sone
	default:
		fmt.Fprintf(os.Stderr, "unknown evaluator implementation: %s\n", evaluatorImpl)
		os.Exit(1)
	}

	storageClient := storageclient.New(storageBaseURL)
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
