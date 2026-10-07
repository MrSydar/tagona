package evaluator

import (
	"fmt"
	"strings"
)

// Names are the evaluators TAGGER_EVALUATOR_IMPL can select. A name is "<kind>/<dialect>" for the
// LLM-backed ones: the dialect is the wire format, and a vendor that speaks it is used with its own
// base URL.
var Names = []string{"grep", "false", "completions/openai", "decisions/openai", "decisions/vercel"}

// legacyNames maps names that were renamed to their replacements, for a helpful error.
var legacyNames = map[string]string{
	"openai":    "completions/openai (settings TAGGER_OPENAI_* are now TAGGER_COMPLETIONS_OPENAI_*)",
	"systemone": "decisions/vercel (TAGGER_SYSTEMONE_BACKEND is gone; settings TAGGER_VERCEL_* are now TAGGER_DECISIONS_VERCEL_*)",
}

// New creates the evaluator selected by impl, reading its settings through lookup (os.LookupEnv in
// production).
func New(impl string, lookup LookupFunc) (Evaluator, error) {
	switch impl {
	case "grep":
		return NewGrepEvaluator(), nil
	case "false":
		return NewFalseEvaluator(), nil
	case "completions/openai":
		ev, err := NewCompletionsOpenAI(lookup)
		if err != nil {
			return nil, err
		}
		return ev, nil
	case "decisions/openai":
		ev, err := NewDecisionsOpenAI(lookup)
		if err != nil {
			return nil, err
		}
		return ev, nil
	case "decisions/vercel":
		ev, err := NewDecisionsVercel(lookup)
		if err != nil {
			return nil, err
		}
		return ev, nil
	}
	if replacement, ok := legacyNames[impl]; ok {
		return nil, fmt.Errorf("evaluator %q was renamed: use %s", impl, replacement)
	}
	return nil, fmt.Errorf("unknown evaluator %q (available: %s)", impl, strings.Join(Names, ", "))
}
