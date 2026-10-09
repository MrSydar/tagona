package client

import (
	"context"

	"mrsydar/tagona/storage/internal/metrics"
)

// LimitedTagger wraps a Tagger and lets at most a fixed number of Tag calls run at once, in the whole service:
// the others wait for their turn (or for their context to end). Queries evaluate several objects at once, so
// this is what keeps many queries together from overwhelming the tagger, or the vendor behind it.
type LimitedTagger struct {
	inner Tagger
	slots chan struct{}
}

// NewLimitedTagger wraps inner so that at most n Tag calls are in flight (at least 1).
func NewLimitedTagger(inner Tagger, n int) *LimitedTagger {
	return &LimitedTagger{inner: inner, slots: make(chan struct{}, max(1, n))}
}

// Versions fetches the versions the tagging engine serves; it is not limited.
func (t *LimitedTagger) Versions(ctx context.Context) ([]string, error) {
	return t.inner.Versions(ctx)
}

// Tag requests tag evaluation for an object once a slot is free.
func (t *LimitedTagger) Tag(ctx context.Context, collection, objectID, taggerVersion string, tags []string) (map[string]bool, error) {
	select {
	case t.slots <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	metrics.TaggerInFlight.Inc()
	defer func() {
		metrics.TaggerInFlight.Dec()
		<-t.slots
	}()
	return t.inner.Tag(ctx, collection, objectID, taggerVersion, tags)
}
