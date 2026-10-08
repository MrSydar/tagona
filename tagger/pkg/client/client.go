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

// versionResponse matches the /version response shape.
type versionResponse struct {
	Version []string `json:"version"`
}

// tagRequest is the request body for /tag.
type tagRequest struct {
	Collection    string   `json:"collection"`
	ObjectID      string   `json:"object_id"`
	TaggerVersion string   `json:"tagger_version"`
	Tags          []string `json:"tags"`
}

// errorResponse is the shape of the engine's errors.
type errorResponse struct {
	Error struct {
		Code    string `json:"code"`
		Details struct {
			Expected string   `json:"expected"`
			Running  []string `json:"running"`
		} `json:"details"`
	} `json:"error"`
}

// tagResponse is the response body for /tag.
type tagResponse struct {
	Tags map[string]bool `json:"tags"`
}

// Versions fetches the versions the tagging engine serves.
func (c *Client) Versions(ctx context.Context) ([]string, error) {
	slog.Debug("tagger client Versions")
	req, err := http.NewRequestWithContext(ctx, "GET", c.baseURL+"/version", nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch versions: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("fetch versions: status %d", resp.StatusCode)
	}
	var result versionResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("decode versions: %w", err)
	}
	if len(result.Version) == 0 {
		return nil, fmt.Errorf("the tagging engine reported no version")
	}
	return result.Version, nil
}

// Tag requests tag evaluation for an object with retries and exponential backoff.
func (c *Client) Tag(ctx context.Context, collection, objectID, taggerVersion string, tags []string) (map[string]bool, error) {
	slog.Debug("tagger client Tag", "collection", collection, "object_id", objectID, "tagger_version", taggerVersion, "tags_count", len(tags))
	payload := tagRequest{
		Collection:    collection,
		ObjectID:      objectID,
		TaggerVersion: taggerVersion,
		Tags:          tags,
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

		req, err := http.NewRequestWithContext(ctx, "POST", c.baseURL+"/tag", bytes.NewReader(body))
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
		if resp.StatusCode == http.StatusConflict {
			var e errorResponse
			_ = json.NewDecoder(resp.Body).Decode(&e)
			resp.Body.Close()
			if e.Error.Code == "tagger_version_mismatch" {
				return nil, &client.VersionMismatchError{Expected: e.Error.Details.Expected, Running: e.Error.Details.Running}
			}
			return nil, fmt.Errorf("tag request unexpected status: %d", resp.StatusCode)
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
