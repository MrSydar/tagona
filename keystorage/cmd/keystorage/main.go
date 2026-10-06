// Command keystorage stores and validates API keys. It is the only service with access to the
// keys schema of the database, under its own role.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"mrsydar/tagona/keystorage/internal/config"
	"mrsydar/tagona/keystorage/internal/db"
	"mrsydar/tagona/keystorage/internal/server"
	"mrsydar/tagona/keystorage/migrations"
)

func main() {
	level := new(slog.LevelVar)
	level.Set(slog.LevelDebug)
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: level})))

	cfg, err := config.Load("KEYSTORAGE_")
	if err != nil {
		fmt.Fprintf(os.Stderr, "config error: %v\n", err)
		os.Exit(1)
	}
	if cfg.AdminUsername == "" || cfg.AdminPassword == "" {
		slog.Warn("admin credentials not configured; key management endpoints are disabled")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	pool, err := db.Connect(ctx, cfg.PGDSN, cfg.PGSchema)
	if err != nil {
		slog.Error("database connection failed", "error", err) // the DSN carries a password: never log it
		os.Exit(1)
	}
	defer pool.Close()
	if err := db.Migrate(ctx, pool, migrations.FS); err != nil {
		slog.Error("migrations failed", "error", err)
		os.Exit(1)
	}
	cancel()

	httpServer := &http.Server{
		Addr:         cfg.HTTPAddr,
		Handler:      server.New(db.New(pool), cfg.AdminUsername, cfg.AdminPassword).Router(),
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 30 * time.Second,
	}
	go func() {
		slog.Info("starting keystorage", "addr", cfg.HTTPAddr, "schema", cfg.PGSchema)
		if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Error("http server error", "error", err)
			os.Exit(1)
		}
	}()

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	<-sig
	slog.Info("shutting down")
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer shutdownCancel()
	_ = httpServer.Shutdown(shutdownCtx)
}
