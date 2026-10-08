package evaluator

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
)

// The "decisions" evaluators ask a vendor one yes/no question per tag about the text and call the tag
// true when the vendor's probability for "yes" reaches a threshold. Vendors differ in the wire format
// ("dialect"): decisions/openai speaks OpenAI's Decisions API (POST /decisions), decisions/vercel the
// Vercel AI Gateway's evaluate endpoint. Everything else (batching, threshold, instructions, the HTTP
// settings of httpconfig.go) is shared.

// question is one tag put to the vendor.
type question struct {
	name         string // how the vendor refers to it in the answer
	tag          string
	instructions string
}

// decisionsDialect is a vendor wire format.
type decisionsDialect interface {
	// questionName returns the name for the i-th question of a request, given its tag.
	questionName(i int, tag string) string
	// request builds the body of one request.
	request(model, input string, qs []question) map[string]any
	// probabilities extracts the probability of "yes" per question name. A question the vendor did
	// not answer, or declined to, is simply absent.
	probabilities(body []byte) (map[string]float64, error)
}

// DefaultDecisionsInstructions is the question put for each tag unless the evaluator's
// INSTRUCTIONS setting replaces it; {tag} stands for the tag.
const DefaultDecisionsInstructions = `Analyze the text and determine whether the tag "{tag}" applies. Answer true if the text matches the tag and false otherwise.`

// Decisions is a "decisions/..." evaluator.
type Decisions struct {
	label        string
	cfg          HTTPConfig
	dialect      decisionsDialect
	threshold    float64
	batchSize    int
	instructions string
	client       *http.Client
}

// newDecisions reads the settings of a decisions evaluator from prefix: the HTTP settings of
// httpconfig.go plus
//
//	<P>THRESHOLD     probability at or above which a tag is true (default 0.5, between 0 and 1)
//	<P>BATCH_SIZE    questions per request (default 50); more tags are sent in several requests.
//	                 0 sends them all in one request
//	<P>INSTRUCTIONS  the question for each tag; must contain {tag}
func newDecisions(label, prefix string, defaults HTTPDefaults, dialect decisionsDialect, lookup LookupFunc) (*Decisions, error) {
	cfg, err := LoadHTTPConfig(lookup, prefix, defaults)
	var errs []error
	if err != nil {
		errs = append(errs, err)
	}
	threshold, err := parseFloatEnv(lookup, prefix+"THRESHOLD", 0.5, 0, 1)
	if err != nil {
		errs = append(errs, err)
	}
	batch, err := parseIntEnv(lookup, prefix+"BATCH_SIZE", 50, 0)
	if err != nil {
		errs = append(errs, err)
	}
	instructions := DefaultDecisionsInstructions
	if v, _ := lookup(prefix + "INSTRUCTIONS"); v != "" {
		instructions = v
	}
	if !strings.Contains(instructions, "{tag}") {
		errs = append(errs, fmt.Errorf("%sINSTRUCTIONS must contain {tag}", prefix))
	}
	if err := errors.Join(errs...); err != nil {
		return nil, err
	}
	slog.Debug("decisions evaluator", "label", label, "base_url", cfg.BaseURL, "path", cfg.Path, "model", cfg.Model,
		"threshold", threshold, "batch_size", batch, "timeout", cfg.Timeout)
	return &Decisions{
		label: label, cfg: cfg, dialect: dialect, threshold: threshold, batchSize: batch,
		instructions: instructions, client: &http.Client{Timeout: cfg.Timeout},
	}, nil
}

// Evaluate asks one question per tag about the content, read as text. A tag the vendor does not answer is
// false.
func (e *Decisions) Evaluate(ctx context.Context, content []byte, tags []string) (map[string]bool, error) {
	slog.Debug("Decisions.Evaluate", "label", e.label, "tags_count", len(tags))
	result := make(map[string]bool, len(tags))
	for _, tag := range tags {
		result[tag] = false
	}
	if len(tags) == 0 {
		return result, nil
	}

	size := e.batchSize
	if size <= 0 {
		size = len(tags)
	}
	for start := 0; start < len(tags); start += size {
		batch := tags[start:min(start+size, len(tags))]
		qs := make([]question, len(batch))
		for i, tag := range batch {
			qs[i] = question{
				name:         e.dialect.questionName(i, tag),
				tag:          tag,
				instructions: strings.ReplaceAll(e.instructions, "{tag}", tag),
			}
		}
		body, err := e.cfg.post(ctx, e.client, e.dialect.request(e.cfg.Model, string(content), qs))
		if err != nil {
			return nil, fmt.Errorf("%s: %w", e.label, err)
		}
		probs, err := e.dialect.probabilities(body)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", e.label, err)
		}
		for _, q := range qs {
			p, answered := probs[q.name]
			result[q.tag] = answered && p >= e.threshold
			slog.Debug("evaluating tag", "tag", q.tag, "answered", answered, "probability", p, "result", result[q.tag])
		}
	}
	return result, nil
}

// Version is "<label>:<model>", e.g. "decisions/openai:gpt-6-luna".
func (e *Decisions) Version() string { return versionOf(e.label, e.cfg.Model) }
