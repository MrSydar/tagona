package evaluator

import (
	"context"
	"mime"
	"strings"
)

// Content is an object's payload and the media type its uploader stored for it (the `content_type` metadata), which
// is empty when there is none. The payload is never touched: an evaluator decides what it makes of the bytes.
type Content struct {
	Data []byte
	Type string
}

// Text is text content with no media type.
func Text(s string) Content { return Content{Data: []byte(s)} }

// MediaType is the media type of the content, lower case and without parameters; it is empty when there is none or
// it does not parse.
func (c Content) MediaType() string {
	mt, _, err := mime.ParseMediaType(c.Type)
	if err != nil {
		return ""
	}
	return mt
}

// IsImage is whether the content is declared to be an image (`image/...`). The format is not checked: which
// formats the vendor accepts is the uploader's concern.
func (c Content) IsImage() bool { return strings.HasPrefix(c.MediaType(), "image/") }

// Evaluator evaluates tags against object content. Objects are bytes: an evaluator decides what it makes
// of them (the text evaluators read them as UTF-8; the vision-capable ones look at images).
type Evaluator interface {
	// Evaluate returns, for each tag, the evaluator's confidence that the tag applies, from 0 (certainly not)
	// to 1 (certainly). Evaluators that only know yes or no answer 1 or 0.
	Evaluate(ctx context.Context, content Content, tags []string) (map[string]float64, error)
	// Version identifies this evaluator and, when it has one, its model: "<implementation>" or
	// "<implementation>:<model>", for example "grep" or "decisions/openai:gpt-6-luna". Different taggers
	// may tag the same object differently, so a collection records the version it is tagged with.
	Version() string
}
