package evaluator

import (
	"context"
	"log/slog"
	"strings"
)

// GrepEvaluator matches tags against text content using substring search.
type GrepEvaluator struct{}

// NewGrepEvaluator creates a new GrepEvaluator.
func NewGrepEvaluator() *GrepEvaluator {
	slog.Debug("NewGrepEvaluator called")
	return &GrepEvaluator{}
}

// Evaluate returns 1 for a tag if the content contains the tag as a substring and 0 otherwise.
func (e *GrepEvaluator) Evaluate(ctx context.Context, content Content, tags []string) (map[string]float64, error) {
	slog.Debug("GrepEvaluator.Evaluate", "tags_count", len(tags))
	result := make(map[string]float64, len(tags))
	contentStr := string(content.Data)
	for _, tag := range tags {
		if strings.Contains(contentStr, tag) {
			result[tag] = 1
		}
		slog.Debug("evaluating tag", "tag", tag, "result", result[tag])
	}
	return result, nil
}

// Version is "grep".
func (e *GrepEvaluator) Version() string { return "grep" }
