package client

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// publicPrefix is the path prefix of the public API served by the api gateway.
const publicPrefix = "/v1"

// Client is an HTTP client for the Tagona API. By default it speaks the public API of the api
// gateway (paths under /v1); NewInternal makes it speak the storage service's own API.
type Client struct {
	baseURL string
	prefix  string
	token   string
	http    *http.Client
}

// New creates a client for the public API (paths under /v1).
func New(baseURL string) *Client {
	slog.Debug("New", "baseURL", baseURL)
	return &Client{
		baseURL: strings.TrimSuffix(baseURL, "/"),
		prefix:  publicPrefix,
		http:    &http.Client{Timeout: 30 * time.Second},
	}
}

// NewInternal creates a client for the storage service's own API, which has no /v1 prefix and
// no authentication. Services on the internal network (the tagger) use it.
func NewInternal(baseURL string) *Client {
	slog.Debug("NewInternal", "baseURL", baseURL)
	return &Client{
		baseURL: strings.TrimSuffix(baseURL, "/"),
		http:    &http.Client{Timeout: 30 * time.Second},
	}
}

// NewWithToken creates a client for the public API that sends the given API key as a
// Bearer token on every request.
func NewWithToken(baseURL, token string) *Client {
	slog.Debug("NewWithToken", "baseURL", baseURL)
	return &Client{
		baseURL: strings.TrimSuffix(baseURL, "/"),
		prefix:  publicPrefix,
		token:   token,
		http:    &http.Client{Timeout: 30 * time.Second},
	}
}

// setAuth sets the Authorization header on the request when a token is configured.
func (c *Client) setAuth(req *http.Request) {
	if c.token == "" {
		return
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
}

// Collection represents a collection from the storage service.
type Collection struct {
	Name     string `json:"name"`
	DataType string `json:"data_type"`
}

// Object represents object metadata from the storage service.
type Object struct {
	ID          string          `json:"id"`
	Collection  string          `json:"collection"`
	DataType    string          `json:"data_type"`
	Date        time.Time       `json:"date"`
	SizeBytes   int64           `json:"size_bytes"`
	ContentHash string          `json:"content_hash"`
	CreatedAt   time.Time       `json:"created_at"`
	ExpiresAt   *time.Time      `json:"expires_at,omitempty"`
	PayloadKey  string          `json:"payload_key,omitempty"`
	Tags        map[string]bool `json:"tags,omitempty"`
}

// ObjectUploadResponse is returned after uploading an object.
type ObjectUploadResponse struct {
	ID          string    `json:"id"`
	Collection  string    `json:"collection"`
	DataType    string    `json:"data_type"`
	Date        time.Time `json:"date"`
	SizeBytes   int64     `json:"size_bytes"`
	ContentHash string    `json:"content_hash"`
}

// TagsQueryRequest is the request for querying objects by tags.
type TagsQueryRequest struct {
	Tags       map[string]bool `json:"tags,omitempty"`
	Date       *DateFilter     `json:"date,omitempty"`
	Limit      int             `json:"limit"`
	Cursor     string          `json:"cursor,omitempty"`
	TimeoutMs  int             `json:"timeout_ms,omitempty"`
	BestEffort bool            `json:"best_effort,omitempty"`
	// Evaluate controls whether tags that are not yet known are evaluated by the
	// tagging engine. Nil (the default) and true evaluate them; false answers from
	// already-known tags only, so objects with an unevaluated requested tag are
	// not returned.
	Evaluate *bool `json:"evaluate,omitempty"`
}

// DateFilter defines date constraints.
type DateFilter struct {
	GT  *time.Time `json:"gt,omitempty"`
	GTE *time.Time `json:"gte,omitempty"`
	LT  *time.Time `json:"lt,omitempty"`
	LTE *time.Time `json:"lte,omitempty"`
	EQ  *time.Time `json:"eq,omitempty"`
}

// TagsQueryResponse is the response for tag queries.
type TagsQueryResponse struct {
	Objects []Object `json:"objects"`
	Next    string   `json:"next,omitempty"`
}

// TagResponse is returned when getting tags for an object.
type TagResponse struct {
	ID   string          `json:"id"`
	Tags map[string]bool `json:"tags"`
}

// TagStat describes one tag registered in a collection. Tags are sparse: an
// object has a value for a tag only once the tag was evaluated for it.
type TagStat struct {
	Tag string `json:"tag"`
	// TrueCount and FalseCount are the objects the tag is known true/false for.
	TrueCount   int64     `json:"true_count"`
	FalseCount  int64     `json:"false_count"`
	FirstSeenAt time.Time `json:"first_seen_at"`
}

// CollectionTags is the response for listing a collection's tags.
type CollectionTags struct {
	Collection   string    `json:"collection"`
	TotalObjects int64     `json:"total_objects"`
	Tags         []TagStat `json:"tags"`
	Next         string    `json:"next,omitempty"`
}

// CollectionTagsOptions narrows and pages ListCollectionTags. Zero values use
// the server defaults.
type CollectionTagsOptions struct {
	Prefix string
	Limit  int
	Cursor string
}

// ObjectTags is returned by GetObjectTagsWithOptions. A nil value means the tag
// was requested but has not been evaluated for the object (only possible with
// Evaluate set to false).
type ObjectTags struct {
	ID   string           `json:"id"`
	Tags map[string]*bool `json:"tags"`
}

// GetObjectTagsOptions are the parameters of GetObjectTagsWithOptions.
type GetObjectTagsOptions struct {
	// Tags are the tag names to return; empty returns every known tag.
	Tags []string
	// Evaluate controls whether requested tags that are not yet known are
	// evaluated by the tagging engine. Nil (the default) and true evaluate them;
	// false returns known values and nil for the rest.
	Evaluate *bool
}

// ErrorResponse is the standard error response format.
type ErrorResponse struct {
	Error struct {
		Code    string         `json:"code"`
		Message string         `json:"message"`
		Details map[string]any `json:"details,omitempty"`
	} `json:"error"`
}

// httpError reads the response body and returns an error containing both status code and body.
func httpError(prefix string, resp *http.Response) error {
	body, _ := io.ReadAll(resp.Body)
	return fmt.Errorf("%s: status %d: %s", prefix, resp.StatusCode, string(body))
}

// ListCollections returns all collections.
func (c *Client) ListCollections(ctx context.Context) ([]Collection, error) {
	slog.Debug("ListCollections: called")
	url := c.baseURL + c.prefix + "/collections"
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, err
	}
	c.setAuth(req)
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("list collections: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, httpError("list collections", resp)
	}
	var result struct {
		Collections []Collection `json:"collections"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("decode collections: %w", err)
	}
	return result.Collections, nil
}

// ListCollectionTags returns the total number of objects in a collection and the
// tags registered in it, with per-tag object counts, ordered by tag name. When
// the result has a Next cursor, pass it in opts.Cursor to fetch the next page.
func (c *Client) ListCollectionTags(ctx context.Context, collection string, opts CollectionTagsOptions) (*CollectionTags, error) {
	slog.Debug("ListCollectionTags", "collection", collection, "prefix", opts.Prefix, "limit", opts.Limit)
	params := url.Values{}
	if opts.Prefix != "" {
		params.Set("prefix", opts.Prefix)
	}
	if opts.Limit > 0 {
		params.Set("limit", strconv.Itoa(opts.Limit))
	}
	if opts.Cursor != "" {
		params.Set("cursor", opts.Cursor)
	}
	target := fmt.Sprintf("%s%s/collections/%s/tags", c.baseURL, c.prefix, collection)
	if len(params) > 0 {
		target += "?" + params.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, "GET", target, nil)
	if err != nil {
		return nil, err
	}
	c.setAuth(req)
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("list collection tags: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, httpError("list collection tags", resp)
	}
	var result CollectionTags
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("decode collection tags: %w", err)
	}
	return &result, nil
}

// CreateCollection creates a new collection.
func (c *Client) CreateCollection(ctx context.Context, name, dataType string) (*Collection, error) {
	slog.Debug("CreateCollection", "name", name, "dataType", dataType)
	reqBody, err := json.Marshal(map[string]string{"name": name, "data_type": dataType})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, "POST", c.baseURL+c.prefix+"/collections", bytes.NewReader(reqBody))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	c.setAuth(req)
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("create collection: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusCreated {
		return nil, httpError("create collection", resp)
	}
	var coll Collection
	if err := json.NewDecoder(resp.Body).Decode(&coll); err != nil {
		return nil, fmt.Errorf("decode collection: %w", err)
	}
	return &coll, nil
}

// GetObjectMetadata fetches object metadata by collection and ID.
func (c *Client) GetObjectMetadata(ctx context.Context, collection, id string) (*Object, error) {
	slog.Debug("GetObjectMetadata", "collection", collection, "id", id)
	url := fmt.Sprintf("%s%s/collections/%s/objects/%s", c.baseURL, c.prefix, collection, id)
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, err
	}
	c.setAuth(req)
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch metadata: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, httpError("fetch metadata", resp)
	}
	var obj Object
	if err := json.NewDecoder(resp.Body).Decode(&obj); err != nil {
		return nil, fmt.Errorf("decode metadata: %w", err)
	}
	return &obj, nil
}

// GetObjectData downloads object payload by collection and ID.
func (c *Client) GetObjectData(ctx context.Context, collection, id string) ([]byte, error) {
	slog.Debug("GetObjectData", "collection", collection, "id", id)
	url := fmt.Sprintf("%s%s/collections/%s/objects/%s/data", c.baseURL, c.prefix, collection, id)
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, err
	}
	c.setAuth(req)
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch data: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, httpError("fetch data", resp)
	}
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read data: %w", err)
	}
	return data, nil
}

// GetObjectTags fetches tags for an object.
func (c *Client) GetObjectTags(ctx context.Context, collection, id string, tags []string) (*TagResponse, error) {
	var result TagResponse
	if err := c.fetchObjectTags(ctx, collection, id, tags, nil, &result); err != nil {
		return nil, err
	}
	return &result, nil
}

// GetObjectTagsWithOptions is like GetObjectTags but can skip evaluation: with
// opts.Evaluate set to false the tagging engine is not called, and requested
// tags that are not yet known come back as nil.
func (c *Client) GetObjectTagsWithOptions(ctx context.Context, collection, id string, opts GetObjectTagsOptions) (*ObjectTags, error) {
	var result ObjectTags
	if err := c.fetchObjectTags(ctx, collection, id, opts.Tags, opts.Evaluate, &result); err != nil {
		return nil, err
	}
	return &result, nil
}

func (c *Client) fetchObjectTags(ctx context.Context, collection, id string, tags []string, evaluate *bool, out any) error {
	slog.Debug("GetObjectTags", "collection", collection, "id", id, "tags", tags)
	tagsParam := strings.Join(tags, ",")
	reqURL := fmt.Sprintf("%s%s/collections/%s/objects/%s/tags", c.baseURL, c.prefix, collection, id)
	u, parseErr := url.Parse(reqURL)
	if parseErr != nil {
		return parseErr
	}
	q := u.Query()
	q.Set("tags", tagsParam)
	if evaluate != nil {
		q.Set("evaluate", strconv.FormatBool(*evaluate))
	}
	u.RawQuery = q.Encode()
	req, err := http.NewRequestWithContext(ctx, "GET", u.String(), nil)
	if err != nil {
		return err
	}
	c.setAuth(req)
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("fetch tags: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return httpError("fetch tags", resp)
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("decode tags: %w", err)
	}
	return nil
}

// QueryObjects queries objects by tags.
func (c *Client) QueryObjects(ctx context.Context, collection string, req TagsQueryRequest) (*TagsQueryResponse, error) {
	slog.Debug("QueryObjects", "collection", collection)
	reqBody, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	url := fmt.Sprintf("%s%s/collections/%s/objects/query", c.baseURL, c.prefix, collection)
	hreq, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(reqBody))
	if err != nil {
		return nil, err
	}
	hreq.Header.Set("Content-Type", "application/json")
	c.setAuth(hreq)
	resp, err := c.http.Do(hreq)
	if err != nil {
		return nil, fmt.Errorf("query objects: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, httpError("query objects", resp)
	}
	var result TagsQueryResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("decode query response: %w", err)
	}
	return &result, nil
}

// UploadObject uploads an object body to the storage service.
func (c *Client) UploadObject(ctx context.Context, collection, dataType string, data []byte, date time.Time, ttlSeconds int) (*ObjectUploadResponse, error) {
	slog.Debug("UploadObject", "collection", collection, "dataType", dataType, "dataLen", len(data), "ttl", ttlSeconds)
	q := fmt.Sprintf("%s%s/collections/%s/objects?data_type=%s", c.baseURL, c.prefix, collection, dataType)
	if !date.IsZero() {
		q += fmt.Sprintf("&date=%s", date.Format(time.RFC3339))
	}
	if ttlSeconds > 0 {
		q += fmt.Sprintf("&ttl_seconds=%d", ttlSeconds)
	}
	req, err := http.NewRequestWithContext(ctx, "POST", q, bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/octet-stream")
	c.setAuth(req)
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("upload object: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
		return nil, httpError("upload object", resp)
	}
	var result ObjectUploadResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("decode upload response: %w", err)
	}
	return &result, nil
}

// DeleteCollection deletes a collection by name.
func (c *Client) DeleteCollection(ctx context.Context, collection string) error {
	slog.Debug("DeleteCollection", "collection", collection)
	url := fmt.Sprintf("%s%s/collections/%s", c.baseURL, c.prefix, collection)
	req, err := http.NewRequestWithContext(ctx, "DELETE", url, nil)
	if err != nil {
		return err
	}
	c.setAuth(req)
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("delete collection: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		return httpError("delete collection", resp)
	}
	return nil
}

// DeleteObject deletes an object by collection and ID.
func (c *Client) DeleteObject(ctx context.Context, collection, id string) error {
	slog.Debug("DeleteObject", "collection", collection, "id", id)
	url := fmt.Sprintf("%s%s/collections/%s/objects/%s", c.baseURL, c.prefix, collection, id)
	req, err := http.NewRequestWithContext(ctx, "DELETE", url, nil)
	if err != nil {
		return err
	}
	c.setAuth(req)
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("delete object: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		return httpError("delete object", resp)
	}
	return nil
}
