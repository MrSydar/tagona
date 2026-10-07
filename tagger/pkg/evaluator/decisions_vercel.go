package evaluator

import (
	"encoding/json"
	"fmt"
)

// NewDecisionsVercel creates the "decisions/vercel" evaluator, which speaks the Vercel AI Gateway's
// evaluate endpoint (POST {base}/evaluate) with one boolean question per tag. Any vendor with the same
// API works with its own base URL. It reads the TAGGER_DECISIONS_VERCEL_* settings; see httpconfig.go
// and decisions.go.
func NewDecisionsVercel(lookup LookupFunc) (*Decisions, error) {
	return newDecisions("decisions/vercel", "TAGGER_DECISIONS_VERCEL_", HTTPDefaults{
		BaseURL: "https://ai-gateway.vercel.sh/v1",
		Path:    "/evaluate",
		Model:   "typesafe-ai/jev",
	}, vercelDecisionsDialect{}, lookup)
}

type vercelDecisionsDialect struct{}

type vercelQuestion struct {
	Type         string `json:"type"`
	Instructions string `json:"instructions"`
}

// The Vercel endpoint keys questions and answers by name, so the tag is the name.
func (vercelDecisionsDialect) questionName(_ int, tag string) string { return tag }

func (vercelDecisionsDialect) request(model, input string, qs []question) map[string]any {
	questions := make(map[string]vercelQuestion, len(qs))
	for _, q := range qs {
		questions[q.name] = vercelQuestion{Type: "boolean", Instructions: q.instructions}
	}
	return map[string]any{"model": model, "state": input, "questions": questions}
}

func (vercelDecisionsDialect) probabilities(body []byte) (map[string]float64, error) {
	var resp struct {
		Answers map[string]struct {
			Probability *float64 `json:"probability"`
		} `json:"answers"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("unmarshal evaluate response: %w", err)
	}
	out := make(map[string]float64, len(resp.Answers))
	for name, a := range resp.Answers {
		if a.Probability != nil {
			out[name] = *a.Probability
		}
	}
	return out, nil
}
