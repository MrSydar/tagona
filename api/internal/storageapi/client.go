package storageapi

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"
)

// Client is an HTTP client for the storage service internal API key management API.
type Client struct {
	baseURL string
	http    *http.Client
}

// Option customizes a Client.
type Option func(*Client)

// WithTransport makes the client use rt, so connection pools can be shared
// with other storage-bound callers.
func WithTransport(rt http.RoundTripper) Option {
	return func(c *Client) { c.http.Transport = rt }
}

// New creates a new storageapi client.
func New(baseURL string, opts ...Option) *Client {
	slog.Debug("New", "baseURL", baseURL)
	c := &Client{
		baseURL: strings.TrimSuffix(baseURL, "/"),
		http:    &http.Client{Timeout: 30 * time.Second},
	}
	for _, opt := range opts {
		opt(c)
	}
	return c
}

// APIKeyCreated is the response after creating an API key; it contains the raw
// key, which is never returned again afterwards.
type APIKeyCreated struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Key       string    `json:"key"`
	CreatedAt time.Time `json:"created_at"`
}

// APIKeyInfo describes an API key without the raw key.
type APIKeyInfo struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	KeyPrefix string    `json:"key_prefix"`
	CreatedAt time.Time `json:"created_at"`
}

// ErrorResponse is the standard error response format.
type ErrorResponse struct {
	Error struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

// HTTPError is a non-success response returned by the storage service.
type HTTPError struct {
	Prefix  string
	Status  int
	Code    string
	Message string
	Body    string
}

func (e *HTTPError) Error() string {
	return fmt.Sprintf("%s: status %d: %s", e.Prefix, e.Status, e.Body)
}

// httpError reads the response body and returns an HTTPError containing the
// status code and body, mirroring the storage error shape when parseable.
func httpError(prefix string, resp *http.Response) error {
	body, _ := io.ReadAll(resp.Body)
	e := &HTTPError{Prefix: prefix, Status: resp.StatusCode, Body: string(body)}
	var errResp ErrorResponse
	if json.Unmarshal(body, &errResp) == nil && errResp.Error.Code != "" {
		e.Code = errResp.Error.Code
		e.Message = errResp.Error.Message
	} else {
		e.Code = "internal_error"
		e.Message = strings.TrimSpace(string(body))
	}
	return e
}

// CreateKey creates a new API key via the storage service.
func (c *Client) CreateKey(ctx context.Context, name string) (*APIKeyCreated, error) {
	slog.Debug("CreateKey", "name", name)
	reqBody, err := json.Marshal(map[string]string{"name": name})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/internal/v1/api-keys", bytes.NewReader(reqBody))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("create api key: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		return nil, httpError("create api key", resp)
	}
	var result APIKeyCreated
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("decode api key: %w", err)
	}
	return &result, nil
}

// ListKeys returns all API keys from the storage service.
func (c *Client) ListKeys(ctx context.Context) ([]APIKeyInfo, error) {
	slog.Debug("ListKeys: called")
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/internal/v1/api-keys", nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("list api keys: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, httpError("list api keys", resp)
	}
	var result struct {
		Keys []APIKeyInfo `json:"keys"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("decode api keys: %w", err)
	}
	return result.Keys, nil
}

// DeleteKey deletes an API key by ID via the storage service.
func (c *Client) DeleteKey(ctx context.Context, id string) error {
	slog.Debug("DeleteKey", "id", id)
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, fmt.Sprintf("%s/internal/v1/api-keys/%s", c.baseURL, id), nil)
	if err != nil {
		return err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("delete api key: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		return httpError("delete api key", resp)
	}
	return nil
}

// ValidateKey checks a raw API key against the storage service. It returns
// true when the key is valid, false when it is unknown, and an error otherwise.
func (c *Client) ValidateKey(ctx context.Context, rawKey string) (bool, error) {
	slog.Debug("ValidateKey: called")
	reqBody, err := json.Marshal(map[string]string{"key": rawKey})
	if err != nil {
		return false, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/internal/v1/api-keys/validate", bytes.NewReader(reqBody))
	if err != nil {
		return false, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return false, fmt.Errorf("validate api key: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusOK {
		return true, nil
	}
	if resp.StatusCode == http.StatusUnauthorized {
		return false, nil
	}
	return false, httpError("validate api key", resp)
}
