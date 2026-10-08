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

// Evaluate returns true for a tag if the content contains the tag as a substring.
func (e *GrepEvaluator) Evaluate(ctx context.Context, content []byte, tags []string) (map[string]bool, error) {
	slog.Debug("GrepEvaluator.Evaluate", "tags_count", len(tags))
	result := make(map[string]bool, len(tags))
	contentStr := string(content)
	for _, tag := range tags {
		result[tag] = strings.Contains(contentStr, tag)
		slog.Debug("evaluating tag", "tag", tag, "result", result[tag])
	}
	return result, nil
}

// Version is "grep".
func (e *GrepEvaluator) Version() string { return "grep" }
