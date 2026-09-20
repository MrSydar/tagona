package client

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"mrsydar/tagona/storage/pkg/client"
)

// Client is an HTTP client for the tagging engine that satisfies storage/client.Tagger.
type Client struct {
	baseURL string
	http    *http.Client
}

var _ client.Tagger = (*Client)(nil)

// New creates a new tag engine client.
// If timeout is zero or negative, it defaults to 30 seconds.
func New(baseURL string, timeout time.Duration) *Client {
	slog.Debug("tagger client New", "base_url", baseURL, "timeout", timeout)
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	return &Client{
		baseURL: baseURL,
		http:    &http.Client{Timeout: timeout},
	}
}

// supportedTypesResponse matches the /v1/supported-types response shape.
type supportedTypesResponse struct {
	Types []string `json:"types"`
}

// tagRequest is the request body for /v1/tag.
type tagRequest struct {
	Collection string   `json:"collection"`
	ObjectID   string   `json:"object_id"`
	Tags       []string `json:"tags"`
}

// tagResponse is the response body for /v1/tag.
type tagResponse struct {
	Tags map[string]bool `json:"tags"`
}

// GetSupportedTypes fetches supported data types from the tagging engine.
func (c *Client) GetSupportedTypes(ctx context.Context) ([]string, error) {
	slog.Debug("tagger client GetSupportedTypes")
	req, err := http.NewRequestWithContext(ctx, "GET", c.baseURL+"/v1/supported-types", nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch supported types: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("fetch supported types: status %d", resp.StatusCode)
	}
	var result supportedTypesResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("decode supported types: %w", err)
	}
	return result.Types, nil
}

// Tag requests tag evaluation for an object with retries and exponential backoff.
func (c *Client) Tag(ctx context.Context, collection, objectID string, tags []string) (map[string]bool, error) {
	slog.Debug("tagger client Tag", "collection", collection, "object_id", objectID, "tags_count", len(tags))
	payload := tagRequest{
		Collection: collection,
		ObjectID:   objectID,
		Tags:       tags,
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}

	var lastErr error
	for attempt := 0; attempt < 2; attempt++ {
		slog.Debug("tagger client Tag attempt", "attempt", attempt)
		if attempt > 0 {
			delay := time.Duration(200*(1<<attempt)) * time.Millisecond
			if delay > 800*time.Millisecond {
				delay = 800 * time.Millisecond
			}
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(delay):
			}
		}

		req, err := http.NewRequestWithContext(ctx, "POST", c.baseURL+"/v1/tag", bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Content-Type", "application/json")

		resp, err := c.http.Do(req)
		if err != nil {
			lastErr = fmt.Errorf("tag request failed: %w", err)
			continue
		}

		if resp.StatusCode == http.StatusGatewayTimeout {
			resp.Body.Close()
			lastErr = fmt.Errorf("tag request timed out")
			continue
		}
		if resp.StatusCode >= 500 {
			resp.Body.Close()
			lastErr = fmt.Errorf("tag request server error: %d", resp.StatusCode)
			continue
		}
		if resp.StatusCode != http.StatusOK {
			resp.Body.Close()
			return nil, fmt.Errorf("tag request unexpected status: %d", resp.StatusCode)
		}

		var result tagResponse
		if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
			resp.Body.Close()
			lastErr = fmt.Errorf("decode tag response: %w", err)
			continue
		}
		resp.Body.Close()
		return result.Tags, nil
	}

	return nil, fmt.Errorf("tag engine failure after retries: %w", lastErr)
}
