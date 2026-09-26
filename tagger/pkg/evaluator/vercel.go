package evaluator

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"time"
)

// VercelEvaluator is the systemone "vercel" backend. It evaluates tags using
// the Vercel AI Gateway evaluate endpoint. Each tag becomes a boolean question
// and the tag is true if the answer's probability meets the classification
// threshold.
type VercelEvaluator struct {
	apiKey     string
	baseURL    string
	model      string
	threshold  float64
	httpClient *http.Client
}

// NewVercelEvaluator creates an evaluator backed by the Vercel AI Gateway
// evaluate endpoint. A tag evaluates to true when the gateway reports a
// probability greater than or equal to threshold.
func NewVercelEvaluator(apiKey, baseURL, model string, threshold float64, timeout time.Duration) *VercelEvaluator {
	slog.Debug("NewVercelEvaluator called", "base_url", baseURL, "model", model, "threshold", threshold, "timeout", timeout)
	return &VercelEvaluator{
		apiKey:    apiKey,
		baseURL:   baseURL,
		model:     model,
		threshold: threshold,
		httpClient: &http.Client{
			Timeout: timeout,
		},
	}
}

// NewVercelEvaluatorFromEnv creates a VercelEvaluator configured from the
// TAGGER_VERCEL_* environment variables.
func NewVercelEvaluatorFromEnv() *VercelEvaluator {
	slog.Debug("NewVercelEvaluatorFromEnv called")
	apiKey := os.Getenv("TAGGER_VERCEL_API_KEY")
	baseURL := os.Getenv("TAGGER_VERCEL_BASE_URL")
	if baseURL == "" {
		baseURL = "https://ai-gateway.vercel.sh/v1"
	}
	model := os.Getenv("TAGGER_VERCEL_MODEL")
	if model == "" {
		model = "typesafe-ai/jev"
	}
	thresholdStr := os.Getenv("TAGGER_VERCEL_THRESHOLD")
	threshold := 0.5
	if thresholdStr != "" {
		if t, err := strconv.ParseFloat(thresholdStr, 64); err == nil {
			threshold = t
		} else {
			slog.Warn("invalid TAGGER_VERCEL_THRESHOLD, using default", "default", threshold, "error", err)
		}
	}
	timeoutStr := os.Getenv("TAGGER_VERCEL_TIMEOUT")
	timeout := 60 * time.Second
	if timeoutStr != "" {
		if d, err := time.ParseDuration(timeoutStr); err == nil {
			timeout = d
		} else {
			slog.Warn("invalid TAGGER_VERCEL_TIMEOUT, using default", "default", timeout, "error", err)
		}
	}
	return NewVercelEvaluator(apiKey, baseURL, model, threshold, timeout)
}

// Evaluate evaluates tags for the given content by sending one boolean
// question per tag to the Vercel AI Gateway. Answers missing from the
// response default to false. For now, only txt data type is supported.
func (e *VercelEvaluator) Evaluate(ctx context.Context, dataType DataType, content []byte, tags []string) (map[string]bool, error) {
	slog.Debug("VercelEvaluator.Evaluate", "data_type", dataType, "tags_count", len(tags))
	result := make(map[string]bool, len(tags))
	if dataType != DataTypeTxt {
		slog.Debug("non-txt data type, returning false for all tags")
		for _, tag := range tags {
			result[tag] = false
		}
		return result, nil
	}

	if len(tags) == 0 {
		slog.Debug("no tags provided, returning empty result")
		return result, nil
	}

	respBody, err := e.callEvaluate(ctx, string(content), tags)
	if err != nil {
		return nil, err
	}

	return parseEvaluateResponse(respBody, tags, e.threshold)
}

type evaluateQuestion struct {
	Type         string `json:"type"`
	Instructions string `json:"instructions"`
}

type evaluateRequest struct {
	Model     string                      `json:"model"`
	State     string                      `json:"state"`
	Questions map[string]evaluateQuestion `json:"questions"`
}

type evaluateAnswer struct {
	Probability float64 `json:"probability"`
}

type evaluateResponse struct {
	Answers map[string]evaluateAnswer `json:"answers"`
}

func buildInstructions(tag string) string {
	slog.Debug("buildInstructions called", "tag", tag)
	return fmt.Sprintf(
		`Analyze the text and determine whether the tag "%s" applies. Answer true if the text matches the tag and false otherwise.`,
		tag)
}

func (e *VercelEvaluator) callEvaluate(ctx context.Context, state string, tags []string) ([]byte, error) {
	slog.Debug("callEvaluate called", "tags_count", len(tags))
	questions := make(map[string]evaluateQuestion, len(tags))
	for _, tag := range tags {
		questions[tag] = evaluateQuestion{
			Type:         "boolean",
			Instructions: buildInstructions(tag),
		}
	}
	body, err := json.Marshal(evaluateRequest{
		Model:     e.model,
		State:     state,
		Questions: questions,
	})
	if err != nil {
		return nil, fmt.Errorf("marshal evaluate request: %w", err)
	}

	url := e.baseURL + "/evaluate"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("create evaluate request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if e.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+e.apiKey)
	}

	resp, err := e.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("evaluate request failed: %w", err)
	}
	defer resp.Body.Close()

	respBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read evaluate response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("evaluate returned status %d: %s", resp.StatusCode, string(respBytes))
	}

	return respBytes, nil
}

func parseEvaluateResponse(respBody []byte, expectedTags []string, threshold float64) (map[string]bool, error) {
	slog.Debug("parseEvaluateResponse called", "expected_tags_count", len(expectedTags), "threshold", threshold)

	var apiResp evaluateResponse
	if err := json.Unmarshal(respBody, &apiResp); err != nil {
		return nil, fmt.Errorf("unmarshal evaluate response: %w", err)
	}

	result := make(map[string]bool, len(expectedTags))
	for _, tag := range expectedTags {
		answer, ok := apiResp.Answers[tag]
		if !ok {
			slog.Debug("no answer for tag, defaulting to false", "tag", tag)
			result[tag] = false
			continue
		}
		result[tag] = answer.Probability >= threshold
		slog.Debug("evaluating tag", "tag", tag, "probability", answer.Probability, "result", result[tag])
	}

	return result, nil
}

// GetSupportedDataTypes returns the data types supported by this evaluator.
func (e *VercelEvaluator) GetSupportedDataTypes() []string {
	slog.Debug("VercelEvaluator.GetSupportedDataTypes called")
	return []string{string(DataTypeTxt)}
}
