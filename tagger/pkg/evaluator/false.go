package evaluator

import (
	"context"
	"log/slog"
)

// FalseEvaluator returns false for every tag.
type FalseEvaluator struct{}

// NewFalseEvaluator creates a new FalseEvaluator.
func NewFalseEvaluator() *FalseEvaluator {
	slog.Debug("NewFalseEvaluator called")
	return &FalseEvaluator{}
}

// Evaluate returns false for all tags regardless of the content.
func (e *FalseEvaluator) Evaluate(ctx context.Context, content []byte, tags []string) (map[string]bool, error) {
	slog.Debug("FalseEvaluator.Evaluate", "tags_count", len(tags))
	result := make(map[string]bool, len(tags))
	for _, tag := range tags {
		slog.Debug("evaluating tag", "tag", tag)
		result[tag] = false
	}
	return result, nil
}

// Version is "false".
func (e *FalseEvaluator) Version() string { return "false" }
