package evaluator

import (
	"context"
	"log/slog"
)

// FalseEvaluator returns 0 (false) for every tag.
type FalseEvaluator struct{}

// NewFalseEvaluator creates a new FalseEvaluator.
func NewFalseEvaluator() *FalseEvaluator {
	slog.Debug("NewFalseEvaluator called")
	return &FalseEvaluator{}
}

// Evaluate returns 0 for all tags regardless of the content.
func (e *FalseEvaluator) Evaluate(ctx context.Context, content Content, tags []string) (map[string]float64, error) {
	slog.Debug("FalseEvaluator.Evaluate", "tags_count", len(tags))
	result := make(map[string]float64, len(tags))
	for _, tag := range tags {
		slog.Debug("evaluating tag", "tag", tag)
		result[tag] = 0
	}
	return result, nil
}

// Version is "false".
func (e *FalseEvaluator) Version() string { return "false" }
