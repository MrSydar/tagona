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

// GetSupportedTypes fetches supported data types from the tagging engine.
func (t *InstrumentedTagger) GetSupportedTypes(ctx context.Context) ([]string, error) {
	start := time.Now()
	result, err := t.inner.GetSupportedTypes(ctx)
	metrics.RecordTaggerLatency("get_supported_types", start)
	return result, err
}

// Tag requests tag evaluation for an object.
func (t *InstrumentedTagger) Tag(ctx context.Context, collection, objectID string, tags []string) (map[string]bool, error) {
	start := time.Now()
	result, err := t.inner.Tag(ctx, collection, objectID, tags)
	metrics.RecordTaggerLatency("tag", start)
	return result, err
}
