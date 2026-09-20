package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	storageclient "mrsydar/tagbase/storage/pkg/client"
	"mrsydar/tagbase/tagger/internal/server"
	"mrsydar/tagbase/tagger/pkg/evaluator"
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
		storageBaseURL = "http://localhost:8080"
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
	case "vercel":
		vercelAPIKey := os.Getenv("TAGGER_VERCEL_API_KEY")
		vercelBaseURL := os.Getenv("TAGGER_VERCEL_BASE_URL")
		if vercelBaseURL == "" {
			vercelBaseURL = "https://ai-gateway.vercel.sh/v1"
		}
		vercelModel := os.Getenv("TAGGER_VERCEL_MODEL")
		if vercelModel == "" {
			vercelModel = "typesafe-ai/jev"
		}
		vercelThresholdStr := os.Getenv("TAGGER_VERCEL_THRESHOLD")
		vercelThreshold := 0.5
		if vercelThresholdStr != "" {
			if t, err := strconv.ParseFloat(vercelThresholdStr, 64); err == nil {
				vercelThreshold = t
			} else {
				slog.Warn("invalid TAGGER_VERCEL_THRESHOLD, using default", "default", vercelThreshold, "error", err)
			}
		}
		vercelTimeoutStr := os.Getenv("TAGGER_VERCEL_TIMEOUT")
		vercelTimeout := 60 * time.Second
		if vercelTimeoutStr != "" {
			if d, err := time.ParseDuration(vercelTimeoutStr); err == nil {
				vercelTimeout = d
			} else {
				slog.Warn("invalid TAGGER_VERCEL_TIMEOUT, using default", "default", vercelTimeout, "error", err)
			}
		}
		ev = evaluator.NewVercelEvaluator(vercelAPIKey, vercelBaseURL, vercelModel, vercelThreshold, vercelTimeout)
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
