package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"mrsydar/tagona/storage/internal/config"
	"mrsydar/tagona/storage/internal/db"
	"mrsydar/tagona/storage/internal/retention"
	"mrsydar/tagona/storage/internal/server"
	"mrsydar/tagona/storage/internal/storage"
	"mrsydar/tagona/storage/pkg/client"
	taggerclient "mrsydar/tagona/tagger/pkg/client"
)

func main() {
	programLevel := new(slog.LevelVar)
	programLevel.Set(slog.LevelDebug)
	h := slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: programLevel})
	slog.SetDefault(slog.New(h))

	cfg, err := config.Load("TAGONA_")
	if err != nil {
		fmt.Fprintf(os.Stderr, "config error: %v\n", err)
		os.Exit(1)
	}

	slog.Debug("starting tagona storage service")

	// Postgres.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	pool, err := pgxpool.New(ctx, cfg.PGDSN)
	cancel()
	if err != nil {
		slog.Error("pgx pool error", "error", err)
		os.Exit(1)
	}
	defer pool.Close()

	// Run migrations.
	slog.Debug("running migrations")
	if err := runMigrations(pool); err != nil {
		slog.Error("migrations failed", "error", err)
		os.Exit(1)
	}

	// S3.
	slog.Debug("initializing S3 store", "bucket", cfg.S3Bucket, "endpoint", cfg.S3Endpoint)
	store, err := storage.NewS3Store(cfg.S3Endpoint, cfg.S3Region, cfg.S3Bucket, cfg.S3AccessKey, cfg.S3SecretKey, cfg.S3ForcePathStyle)
	if err != nil {
		slog.Error("s3 store error", "error", err)
		os.Exit(1)
	}
	for i := 0; i < 10; i++ {
		slog.Debug("ensuring S3 bucket exists", "attempt", i+1)
		if err := store.EnsureBucket(context.Background()); err != nil {
			if i == 9 {
				slog.Error("ensure bucket failed after retries", "error", err)
				os.Exit(1)
			}
			slog.Warn("ensure bucket failed, retrying", "error", err, "attempt", i+1)
			time.Sleep(2 * time.Second)
			continue
		}
		break
	}

	// Tag engine client.
	slog.Debug("initializing tag engine client", "url", cfg.TagEngineURL)
	if cfg.TagEngineURL == "" {
		slog.Error("TAGONA_TAG_ENGINE_URL is required")
		os.Exit(1)
	}
	tagClient := taggerclient.New(cfg.TagEngineURL, cfg.TagEngineTimeout)

	slog.Debug("creating server")
	database := db.New(pool)
	limitedTagClient := client.NewLimitedTagger(client.NewInstrumentedTagger(tagClient), cfg.TagEngineMaxConcurrency)
	srv := server.NewServer(cfg, database, store, limitedTagClient)

	// Retention sweeper.
	slog.Debug("starting retention sweeper")
	sweeper := retention.NewSweeper(database, store, cfg.RetentionSweepInterval, cfg.RetentionBatchSize)
	sweeperCtx, sweeperCancel := context.WithCancel(context.Background())
	go sweeper.Start(sweeperCtx)

	// HTTP server.
	httpAddr := cfg.HTTPAddr
	slog.Debug("starting HTTP server", "addr", httpAddr)
	httpServer := &http.Server{
		Addr:         httpAddr,
		Handler:      srv.Router(),
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 30 * time.Second,
	}

	go func() {
		slog.Info("starting tagona server", "addr", httpAddr)
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

	sweeperCancel()
	httpServer.Shutdown(shutdownCtx)
	slog.Info("shutdown complete")
}

func runMigrations(pool *pgxpool.Pool) error {
	slog.Debug("executing migrations")
	// Simple migration runner: execute all .up.sql files in migrations/.
	// In production, use golang-migrate. For MVP, we assume schema is idempotent.
	ctx := context.Background()
	files, err := os.ReadDir("migrations")
	if err != nil {
		return fmt.Errorf("read migrations dir: %w", err)
	}
	for _, f := range files {
		if f.IsDir() || !strings.HasSuffix(f.Name(), ".up.sql") {
			continue
		}
		data, err := os.ReadFile(filepath.Join("migrations", f.Name()))
		if err != nil {
			return fmt.Errorf("read migration %s: %w", f.Name(), err)
		}
		if _, err := pool.Exec(ctx, string(data)); err != nil {
			return fmt.Errorf("exec migration %s: %w", f.Name(), err)
		}
	}
	return nil
}
