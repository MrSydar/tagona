package evaluator

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
)

// CompletionsOpenAI is the "completions/openai" evaluator: an LLM classifies the text against the
// requested tags through a chat completions API in the OpenAI dialect (POST {base}{path} with
// model/messages, answer in choices[0].message.content). Any vendor that speaks it works with its
// own base URL; see httpconfig.go for everything that can be configured.
type CompletionsOpenAI struct {
	cfg          HTTPConfig
	systemPrompt string
	client       *http.Client
}

// Defaults of the "completions/openai" evaluator.
var completionsOpenAIDefaults = HTTPDefaults{
	BaseURL: "https://api.openai.com/v1",
	Path:    "/chat/completions",
	Model:   "gpt-4o-mini",
}

// DefaultCompletionsSystemPrompt is the system message unless TAGGER_COMPLETIONS_OPENAI_SYSTEM_PROMPT is set.
const DefaultCompletionsSystemPrompt = "You are a tag evaluation engine that responds only with JSON."

// NewCompletionsOpenAI creates the evaluator from the TAGGER_COMPLETIONS_OPENAI_* settings (see
// httpconfig.go), plus TAGGER_COMPLETIONS_OPENAI_SYSTEM_PROMPT.
func NewCompletionsOpenAI(lookup LookupFunc) (*CompletionsOpenAI, error) {
	const prefix = "TAGGER_COMPLETIONS_OPENAI_"
	cfg, err := LoadHTTPConfig(lookup, prefix, completionsOpenAIDefaults)
	if err != nil {
		return nil, err
	}
	prompt := DefaultCompletionsSystemPrompt
	if v, _ := lookup(prefix + "SYSTEM_PROMPT"); v != "" {
		prompt = v
	}
	return newCompletionsOpenAI(cfg, prompt), nil
}

func newCompletionsOpenAI(cfg HTTPConfig, systemPrompt string) *CompletionsOpenAI {
	slog.Debug("completions/openai evaluator", "base_url", cfg.BaseURL, "path", cfg.Path, "model", cfg.Model, "timeout", cfg.Timeout)
	return &CompletionsOpenAI{cfg: cfg, systemPrompt: systemPrompt, client: &http.Client{Timeout: cfg.Timeout}}
}

// Evaluate evaluates tags for the given content using an LLM, which reads it as text.
func (e *CompletionsOpenAI) Evaluate(ctx context.Context, content []byte, tags []string) (map[string]bool, error) {
	slog.Debug("CompletionsOpenAI.Evaluate", "tags_count", len(tags))
	result := make(map[string]bool, len(tags))
	if len(tags) == 0 {
		slog.Debug("no tags provided, returning empty result")
		return result, nil
	}

	// temperature 0 keeps the answers stable; vendors and models that reject it (reasoning models)
	// remove it with TAGGER_COMPLETIONS_OPENAI_PARAMS='{"temperature":null}'.
	respBody, err := e.cfg.post(ctx, e.client, map[string]any{
		"model": e.cfg.Model,
		"messages": []map[string]string{
			{"role": "system", "content": e.systemPrompt},
			{"role": "user", "content": buildPrompt(string(content), tags)},
		},
		"temperature": 0,
	})
	if err != nil {
		return nil, fmt.Errorf("completions/openai: %w", err)
	}
	return parseTagResponse(respBody, tags)
}

func buildPrompt(text string, tags []string) string {
	slog.Debug("buildPrompt called", "tags_count", len(tags))
	t := make([]byte, 0, len(tags)*16)
	t = append(t, '[')
	for i, tag := range tags {
		if i > 0 {
			t = append(t, ", "...)
		}
		t = append(t, '"')
		t = append(t, tag...)
		t = append(t, '"')
	}
	t = append(t, ']')
	return fmt.Sprintf(
		`Analyze the following text and determine which tags apply. Respond ONLY with a JSON object where each key is a tag and the value is true or false.

Example response format:
{"tag1": true, "tag2": false}

Text:
---
%s
---

Tags: %s`,
		text,
		string(t),
	)
}

func parseTagResponse(respBody []byte, expectedTags []string) (map[string]bool, error) {
	slog.Debug("parseTagResponse called", "expected_tags_count", len(expectedTags))
	result := make(map[string]bool, len(expectedTags))
	for _, tag := range expectedTags {
		result[tag] = false
	}

	var apiResp struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}

	if err := json.Unmarshal(respBody, &apiResp); err != nil {
		return nil, fmt.Errorf("unmarshal chat completion response: %w", err)
	}
	if len(apiResp.Choices) == 0 {
		return nil, fmt.Errorf("no choices in chat completion response")
	}

	content := apiResp.Choices[0].Message.Content
	slog.Debug("parseTagResponse content", "content", content)

	return parseContent(content, expectedTags, result)
}

func parseContent(content string, expectedTags []string, result map[string]bool) (map[string]bool, error) {
	var parsed map[string]any

	// Try to parse the content directly as JSON.
	if err := json.Unmarshal([]byte(content), &parsed); err != nil {
		slog.Debug("failed to parse content as JSON, attempting fallback extraction")

		// If content does not start with '{', trim everything up to </think> and try again.
		if !strings.HasPrefix(content, "{") {
			if idx := strings.Index(content, "</think>"); idx != -1 {
				trimmed := strings.TrimSpace(content[idx+len("</think>"):])
				slog.Debug("trimmed content after </think>", "trimmed", trimmed)
				if err := json.Unmarshal([]byte(trimmed), &parsed); err == nil {
					goto populate
				}
			}
		}

		// Final fallback: extract the first JSON object from the text.
		start := strings.IndexByte(content, '{')
		end := strings.LastIndexByte(content, '}')
		if start == -1 || end == -1 || end <= start {
			return nil, fmt.Errorf("could not extract JSON object from content: %s", content)
		}
		if err := json.Unmarshal([]byte(content[start:end+1]), &parsed); err != nil {
			return nil, fmt.Errorf("unmarshal extracted JSON: %w", err)
		}
	}

populate:
	for _, tag := range expectedTags {
		if v, ok := parsed[tag]; ok {
			switch val := v.(type) {
			case bool:
				result[tag] = val
			case string:
				result[tag] = val == "true"
			case float64:
				result[tag] = val != 0
			}
		}
	}

	return result, nil
}

// Version is "completions/openai:<model>".
func (e *CompletionsOpenAI) Version() string { return versionOf("completions/openai", e.cfg.Model) }
