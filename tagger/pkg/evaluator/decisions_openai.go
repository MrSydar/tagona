package evaluator

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"strconv"
)

// NewDecisionsOpenAI creates the "decisions/openai" evaluator, which speaks OpenAI's Decisions API
// (POST {base}/decisions) with one predicate question per tag. Any vendor with the same API works
// with its own base URL. It reads the TAGGER_DECISIONS_OPENAI_* settings; see httpconfig.go and
// decisions.go.
func NewDecisionsOpenAI(lookup LookupFunc) (*Decisions, error) {
	return newDecisions("decisions/openai", "TAGGER_DECISIONS_OPENAI_", HTTPDefaults{
		BaseURL: "https://api.openai.com/v1",
		Path:    "/decisions",
		Model:   "gpt-6-luna", // the only model the Decisions API accepts at the time of writing
	}, openAIDecisionsDialect{}, lookup)
}

type openAIDecisionsDialect struct{}

type openAIPredicate struct {
	Type         string `json:"type"`
	Name         string `json:"name"`
	Instructions string `json:"instructions"`
}

// Question names are positional: they are what the answers refer to, they cannot collide, and they
// are safe whatever characters a tag contains. The tag itself is in the instructions.
func (openAIDecisionsDialect) questionName(i int, _ string) string { return "q" + strconv.Itoa(i) }

func (openAIDecisionsDialect) request(model, input string, qs []question) map[string]any {
	questions := make([]openAIPredicate, len(qs))
	for i, q := range qs {
		questions[i] = openAIPredicate{Type: "predicate", Name: q.name, Instructions: q.instructions}
	}
	return map[string]any{"model": model, "input": input, "questions": questions}
}

func (openAIDecisionsDialect) probabilities(body []byte) (map[string]float64, error) {
	var resp struct {
		Answers []struct {
			Type        string   `json:"type"`
			Name        string   `json:"name"`
			Probability *float64 `json:"probability"`
		} `json:"answers"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("unmarshal decisions response: %w", err)
	}
	out := make(map[string]float64, len(resp.Answers))
	for _, a := range resp.Answers {
		switch {
		case a.Type == "refusal":
			// The model declined to answer: no answer, so the tag is false.
			slog.Warn("decision refused", "question", a.Name)
		case a.Type == "predicate" && a.Probability != nil:
			out[a.Name] = *a.Probability
		}
	}
	return out, nil
}
