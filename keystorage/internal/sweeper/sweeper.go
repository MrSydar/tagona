// Package sweeper deletes expired API keys once their retention has passed.
package sweeper

import (
	"context"
	"log/slog"
	"time"
)

// Store is what the sweeper needs; *db.DB implements it.
type Store interface {
	DeleteExpired(ctx context.Context, retention time.Duration) (int64, error)
}

// Run deletes keys that expired more than retention ago, every interval, until ctx is done. It sweeps
// once straight away, so keys that expired while the service was down are gone at start.
func Run(ctx context.Context, store Store, retention, interval time.Duration) {
	sweep := func() {
		n, err := store.DeleteExpired(ctx, retention)
		switch {
		case err != nil && ctx.Err() == nil:
			slog.Error("sweeping expired api keys failed", "error", err)
		case n > 0:
			slog.Info("deleted expired api keys", "count", n)
		}
	}
	sweep()
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			sweep()
		}
	}
}
