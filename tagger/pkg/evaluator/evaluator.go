package evaluator

import "context"

// Evaluator evaluates tags against object content. Objects are bytes: an evaluator decides what it makes
// of them (the text evaluators read them as UTF-8).
type Evaluator interface {
	Evaluate(ctx context.Context, content []byte, tags []string) (map[string]bool, error)
	// Version identifies this evaluator and, when it has one, its model: "<implementation>" or
	// "<implementation>:<model>", for example "grep" or "decisions/openai:gpt-6-luna". Different taggers
	// may tag the same object differently, so a collection records the version it is tagged with.
	Version() string
}
