package retention

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"mrsydar/tagona/storage/internal/db"
	"mrsydar/tagona/storage/internal/storage"
)

// Sweeper periodically deletes expired objects.
type Sweeper struct {
	db       *db.DB
	store    *storage.S3Store
	interval time.Duration
}

// NewSweeper creates a new retention sweeper.
func NewSweeper(database *db.DB, store *storage.S3Store, interval time.Duration) *Sweeper {
	slog.Debug("NewSweeper: created")
	return &Sweeper{
		db:       database,
		store:    store,
		interval: interval,
	}
}

// Start begins the background sweep loop.
func (s *Sweeper) Start(ctx context.Context) {
	slog.Debug("retention sweeper started", "interval", s.interval)
	ticker := time.NewTicker(s.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			slog.Debug("retention sweeper shutting down")
			return
		case <-ticker.C:
			slog.Debug("retention sweep tick")
			if err := s.sweep(ctx); err != nil {
				slog.Error("retention sweep failed", "error", err)
			}
		}
	}
}

func (s *Sweeper) sweep(ctx context.Context) error {
	slog.Debug("sweep: fetching expired objects")
	ids, err := s.db.ListExpiredObjects(ctx, 100)
	if err != nil {
		return fmt.Errorf("list expired objects: %w", err)
	}
	if len(ids) == 0 {
		slog.Debug("sweep: no expired objects found")
		return nil
	}
	slog.Debug("sweep: processing expired objects", "count", len(ids))
	for _, id := range ids {
		payloadKey, err := s.db.DeleteObject(ctx, id)
		if err != nil {
			slog.Warn("failed to delete expired object from db", "id", id, "error", err)
			continue
		}
		if err := s.store.Delete(ctx, payloadKey); err != nil {
			slog.Warn("failed to delete expired object from S3", "id", id, "key", payloadKey, "error", err)
			continue
		}
		slog.Info("deleted expired object", "id", id)
	}
	return nil
}
