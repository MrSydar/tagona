package evaluator

import (
	"context"
	"fmt"
	"log/slog"
)

// SystemoneEvaluator is a pluggable-backend evaluator. It delegates evaluation
// to a concrete backend selected via TAGGER_SYSTEMONE_BACKEND.
type SystemoneEvaluator struct {
	backend string
	inner   Evaluator
}

// NewSystemoneEvaluator creates a SystemoneEvaluator backed by the given
// backend.
func NewSystemoneEvaluator(backend string) (*SystemoneEvaluator, error) {
	slog.Debug("NewSystemoneEvaluator called", "backend", backend)
	switch backend {
	case "vercel":
		return &SystemoneEvaluator{backend: backend, inner: NewVercelEvaluatorFromEnv()}, nil
	default:
		return nil, fmt.Errorf("unknown systemone backend: %s", backend)
	}
}

// Evaluate evaluates tags for the given content using the configured backend.
func (e *SystemoneEvaluator) Evaluate(ctx context.Context, dataType DataType, content []byte, tags []string) (map[string]bool, error) {
	slog.Debug("SystemoneEvaluator.Evaluate", "backend", e.backend, "data_type", dataType, "tags_count", len(tags))
	return e.inner.Evaluate(ctx, dataType, content, tags)
}

// GetSupportedDataTypes returns the data types supported by the configured backend.
func (e *SystemoneEvaluator) GetSupportedDataTypes() []string {
	slog.Debug("SystemoneEvaluator.GetSupportedDataTypes called", "backend", e.backend)
	return e.inner.GetSupportedDataTypes()
}
