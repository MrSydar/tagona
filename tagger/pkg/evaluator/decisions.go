package evaluator

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
)

// The "decisions" evaluators ask a vendor one yes/no question per tag about the text and report the
// vendor's probability for "yes" as the tag's confidence. Vendors differ in the wire format
// ("dialect"): decisions/openai speaks OpenAI's Decisions API (POST /decisions), decisions/vercel the
// Vercel AI Gateway's evaluate endpoint. Everything else (batching, instructions, the HTTP
// settings of httpconfig.go) is shared.

// question is one tag put to the vendor.
type question struct {
	name         string // how the vendor refers to it in the answer
	tag          string
	instructions string
}

// imageDialect is implemented by the dialects whose vendor can look at an image. The image goes in as it is: its
// base64 is built from the stored bytes, with the stored media type, and nothing else is done to it.
type imageDialect interface {
	// imageRequest builds the body of one request about the image.
	imageRequest(model, mediaType, imageBase64 string, qs []question) map[string]any
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

// DefaultDecisionsImageInstructions is the question put for each tag about an image, unless the evaluator's
// IMAGE_INSTRUCTIONS setting replaces it; {tag} stands for the tag.
const DefaultDecisionsImageInstructions = `Analyze the image and determine whether the tag "{tag}" applies. Answer true if the image matches the tag and false otherwise.`

// Decisions is a "decisions/..." evaluator.
type Decisions struct {
	label        string
	cfg          HTTPConfig
	dialect      decisionsDialect
	batchSize    int
	instructions string
	// imageInstructions is the question for an image.
	imageInstructions string
	client            *http.Client
}

// newDecisions reads the settings of a decisions evaluator from prefix: the HTTP settings of
// httpconfig.go plus
//
//	<P>BATCH_SIZE    questions per request (default 50); more tags are sent in several requests.
//	                 0 sends them all in one request
//	<P>INSTRUCTIONS  the question for each tag; must contain {tag}
//	<P>IMAGE_INSTRUCTIONS  the question for each tag when the object is an image; must contain {tag}
func newDecisions(label, prefix string, defaults HTTPDefaults, dialect decisionsDialect, lookup LookupFunc) (*Decisions, error) {
	cfg, err := LoadHTTPConfig(lookup, prefix, defaults)
	var errs []error
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
	imageInstructions := DefaultDecisionsImageInstructions
	if v, _ := lookup(prefix + "IMAGE_INSTRUCTIONS"); v != "" {
		imageInstructions = v
	}
	if !strings.Contains(imageInstructions, "{tag}") {
		errs = append(errs, fmt.Errorf("%sIMAGE_INSTRUCTIONS must contain {tag}", prefix))
	}
	if err := errors.Join(errs...); err != nil {
		return nil, err
	}
	slog.Debug("decisions evaluator", "label", label, "base_url", cfg.BaseURL, "path", cfg.Path, "model", cfg.Model,
		"batch_size", batch, "timeout", cfg.Timeout)
	return &Decisions{
		label: label, cfg: cfg, dialect: dialect, batchSize: batch,
		instructions: instructions, imageInstructions: imageInstructions, client: &http.Client{Timeout: cfg.Timeout},
	}, nil
}

// Evaluate asks one question per tag about the content: an image (by its stored media type) goes to the vendor as
// an image when the vendor's dialect supports that, anything else is read as text. The confidence of a tag is the
// vendor's probability for "yes"; a tag the vendor does not answer, or declines to, is 0.
func (e *Decisions) Evaluate(ctx context.Context, content Content, tags []string) (map[string]float64, error) {
	slog.Debug("Decisions.Evaluate", "label", e.label, "tags_count", len(tags))
	result := make(map[string]float64, len(tags))
	for _, tag := range tags {
		result[tag] = 0
	}
	if len(tags) == 0 {
		return result, nil
	}

	// An image is sent as a base64 data URL, as it is stored; text is the bytes read as UTF-8.
	instructions, build := e.instructions, func(qs []question) map[string]any {
		return e.dialect.request(e.cfg.Model, string(content.Data), qs)
	}
	if img, ok := e.dialect.(imageDialect); ok && content.IsImage() {
		encoded := base64.StdEncoding.EncodeToString(content.Data)
		instructions = e.imageInstructions
		build = func(qs []question) map[string]any {
			return img.imageRequest(e.cfg.Model, content.MediaType(), encoded, qs)
		}
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
				instructions: strings.ReplaceAll(instructions, "{tag}", tag),
			}
		}
		body, err := e.cfg.post(ctx, e.client, build(qs))
		if err != nil {
			return nil, fmt.Errorf("%s: %w", e.label, err)
		}
		probs, err := e.dialect.probabilities(body)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", e.label, err)
		}
		for _, q := range qs {
			p, answered := probs[q.name]
			if answered {
				result[q.tag] = min(max(p, 0), 1)
			}
			slog.Debug("evaluating tag", "tag", q.tag, "answered", answered, "probability", p, "result", result[q.tag])
		}
	}
	return result, nil
}

// Version is "<label>:<model>", e.g. "decisions/openai:gpt-6-luna".
func (e *Decisions) Version() string { return versionOf(e.label, e.cfg.Model) }
