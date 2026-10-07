// Package keystorageapi is the api gateway's client for the keystorage service. It can only
// validate keys: key management is reached through the gateway's admin proxy, never through
// this client.
package keystorageapi

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Client validates raw API keys against keystorage.
type Client struct {
	baseURL string
	http    *http.Client
}

// Option customizes a Client.
type Option func(*Client)

// WithTransport makes the client use rt, so connection pools can be shared with other
// keystorage-bound callers.
func WithTransport(rt http.RoundTripper) Option {
	return func(c *Client) { c.http.Transport = rt }
}

// New creates a client for the keystorage service at baseURL.
func New(baseURL string, opts ...Option) *Client {
	c := &Client{
		baseURL: strings.TrimSuffix(baseURL, "/"),
		http:    &http.Client{Timeout: 10 * time.Second},
	}
	for _, opt := range opts {
		opt(c)
	}
	return c
}

// Verdict is what keystorage says about a raw key.
type Verdict int

const (
	// Unknown means keystorage has no such key.
	Unknown Verdict = iota
	// Valid means the key exists and has not expired.
	Valid
	// Expired means the key exists but its expiry has passed.
	Expired
)

// Result is the answer to a validation.
type Result struct {
	Verdict Verdict
	// ExpiresAt is when a valid key stops working; nil when it never expires.
	ExpiresAt *time.Time
}

// ValidateKey asks keystorage about rawKey. An error means no answer could be obtained, which is
// never to be read as a verdict.
func (c *Client) ValidateKey(ctx context.Context, rawKey string) (Result, error) {
	body, err := json.Marshal(map[string]string{"key": rawKey})
	if err != nil {
		return Result{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/api-keys/validate", bytes.NewReader(body))
	if err != nil {
		return Result{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return Result{}, fmt.Errorf("validate api key: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))

	switch resp.StatusCode {
	case http.StatusOK:
		var ok struct {
			ExpiresAt *time.Time `json:"expires_at"`
		}
		if err := json.Unmarshal(raw, &ok); err != nil {
			return Result{}, fmt.Errorf("validate api key: unreadable answer: %w", err)
		}
		return Result{Verdict: Valid, ExpiresAt: ok.ExpiresAt}, nil
	case http.StatusUnauthorized:
		var e struct {
			Error struct {
				Code string `json:"code"`
			} `json:"error"`
		}
		_ = json.Unmarshal(raw, &e)
		if e.Error.Code == "expired_api_key" {
			return Result{Verdict: Expired}, nil
		}
		return Result{Verdict: Unknown}, nil
	}
	return Result{}, fmt.Errorf("validate api key: status %d: %s", resp.StatusCode, bytes.TrimSpace(raw))
}
