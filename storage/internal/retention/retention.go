package retention

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"

	"mrsydar/tagona/storage/internal/db"
)

// objectStore is the part of the database the sweeper uses. *db.DB implements it.
type objectStore interface {
	ListExpiredObjects(ctx context.Context, after *db.ExpiredObject, limit int) ([]db.ExpiredObject, error)
	DeleteObject(ctx context.Context, id string) (string, error)
}

// payloadStore deletes object payloads. *storage.S3Store implements it.
type payloadStore interface {
	Delete(ctx context.Context, key string) error
}

// Sweeper periodically removes expired objects.
//
// Expired objects are already invisible to readers (every read filters on
// expires_at); the sweeper reclaims their space. Each run drains the whole
// backlog in batches of batchSize, so removal keeps up with expiry no matter
// how many objects expire at once.
type Sweeper struct {
	db        objectStore
	store     payloadStore
	interval  time.Duration
	batchSize int
}

// NewSweeper creates a retention sweeper that runs every interval and lists
// expired objects batchSize at a time. A batchSize below 1 is treated as 1.
func NewSweeper(database objectStore, store payloadStore, interval time.Duration, batchSize int) *Sweeper {
	slog.Debug("NewSweeper: created", "interval", interval, "batchSize", batchSize)
	if batchSize < 1 {
		batchSize = 1
	}
	return &Sweeper{db: database, store: store, interval: interval, batchSize: batchSize}
}

// Start begins the background sweep loop and returns when ctx is cancelled.
func (s *Sweeper) Start(ctx context.Context) {
	slog.Debug("retention sweeper started", "interval", s.interval, "batchSize", s.batchSize)
	ticker := time.NewTicker(s.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			slog.Debug("retention sweeper shutting down")
			return
		case <-ticker.C:
			start := time.Now()
			res, err := s.sweep(ctx)
			if res.deleted > 0 || res.failed > 0 {
				slog.Info("retention sweep finished", "deleted", res.deleted, "failed", res.failed, "duration", time.Since(start))
			}
			if err != nil && !errors.Is(err, context.Canceled) {
				slog.Error("retention sweep failed", "error", err)
			}
		}
	}
}

// sweepResult counts what one run did.
type sweepResult struct {
	deleted int // objects fully removed (payload and row)
	failed  int // objects left in place; they are retried on the next run
}

// sweep removes every object that is expired when it asks, one batch at a time.
//
// It walks the backlog with a keyset cursor over (expires_at, id) instead of
// re-querying from the start, so an object that cannot be removed does not
// block the ones behind it and the walk always terminates. A failed object
// stays in the database and is picked up again by the next run.
func (s *Sweeper) sweep(ctx context.Context) (sweepResult, error) {
	var res sweepResult
	var cursor *db.ExpiredObject
	for {
		if err := ctx.Err(); err != nil {
			return res, err
		}
		batch, err := s.db.ListExpiredObjects(ctx, cursor, s.batchSize)
		if err != nil {
			return res, fmt.Errorf("list expired objects: %w", err)
		}
		for _, obj := range batch {
			if s.remove(ctx, obj) {
				res.deleted++
			} else {
				res.failed++
			}
		}
		if len(batch) < s.batchSize {
			return res, nil // reached the end of the backlog
		}
		last := batch[len(batch)-1]
		cursor = &last
	}
}

// remove deletes one expired object and reports whether it is gone.
//
// The payload goes first. If that fails the row is kept, so the object is
// retried next run; deleting the row first would leave an orphaned payload
// that nothing refers to any more. Deleting a payload that is already gone is
// a no-op in S3, which makes the retry safe.
func (s *Sweeper) remove(ctx context.Context, obj db.ExpiredObject) bool {
	if err := s.store.Delete(ctx, obj.PayloadKey); err != nil {
		slog.Warn("failed to delete expired object payload; will retry on the next sweep", "id", obj.ID, "key", obj.PayloadKey, "error", err)
		return false
	}
	if _, err := s.db.DeleteObject(ctx, obj.ID); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return true // already removed, e.g. by an explicit delete or another sweeper
		}
		slog.Warn("failed to delete expired object row; will retry on the next sweep", "id", obj.ID, "error", err)
		return false
	}
	slog.Debug("deleted expired object", "id", obj.ID)
	return true
}
