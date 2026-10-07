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

// ValidateKey reports whether rawKey is a known API key. It returns true for a valid key, false
// for an unknown one, and an error when keystorage could not give an answer.
func (c *Client) ValidateKey(ctx context.Context, rawKey string) (bool, error) {
	body, err := json.Marshal(map[string]string{"key": rawKey})
	if err != nil {
		return false, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/api-keys/validate", bytes.NewReader(body))
	if err != nil {
		return false, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return false, fmt.Errorf("validate api key: %w", err)
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK:
		return true, nil
	case http.StatusUnauthorized:
		return false, nil
	}
	msg, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
	return false, fmt.Errorf("validate api key: status %d: %s", resp.StatusCode, bytes.TrimSpace(msg))
}
