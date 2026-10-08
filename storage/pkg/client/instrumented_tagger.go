package client

import (
	"context"
	"time"

	"mrsydar/tagona/storage/internal/metrics"
)

// InstrumentedTagger wraps a Tagger and records Prometheus metrics.
type InstrumentedTagger struct {
	inner Tagger
}

// NewInstrumentedTagger wraps the given Tagger with metrics.
func NewInstrumentedTagger(inner Tagger) Tagger {
	return &InstrumentedTagger{inner: inner}
}

// Versions fetches the versions the tagging engine serves.
func (t *InstrumentedTagger) Versions(ctx context.Context) ([]string, error) {
	start := time.Now()
	result, err := t.inner.Versions(ctx)
	metrics.RecordTaggerLatency("version", start)
	return result, err
}

// Tag requests tag evaluation for an object.
func (t *InstrumentedTagger) Tag(ctx context.Context, collection, objectID, taggerVersion string, tags []string) (map[string]bool, error) {
	start := time.Now()
	result, err := t.inner.Tag(ctx, collection, objectID, taggerVersion, tags)
	metrics.RecordTaggerLatency("tag", start)
	return result, err
}
