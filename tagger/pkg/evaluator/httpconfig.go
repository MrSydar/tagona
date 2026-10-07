package evaluator

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// The LLM-backed evaluators talk to vendors that mostly follow one of a few wire formats ("dialects")
// but differ in the base URL, sometimes in the endpoint path, in how the key is sent, and in the extra
// parameters they want or reject. Every such evaluator therefore reads the same set of settings, named
// after its label: the evaluator "decisions/openai" reads TAGGER_DECISIONS_OPENAI_*. The dialect names
// the wire format, not the vendor: any vendor that speaks it is configured with its own base URL.
//
//	<P>API_KEY      key sent to the vendor; empty sends no credentials (local servers)
//	<P>BASE_URL     scheme, host and any path prefix, e.g. https://api.openai.com/v1
//	<P>PATH         the endpoint below the base URL, e.g. /chat/completions
//	<P>MODEL        model name
//	<P>TIMEOUT      per-request timeout (Go duration)
//	<P>AUTH_HEADER  header carrying the key (default Authorization)
//	<P>AUTH_SCHEME  prefix of the header value (default "Bearer"); set it empty to send the bare key,
//	                as vendors that use e.g. an "api-key" header expect
//	<P>HEADERS      extra request headers, as a JSON object: {"OpenAI-Organization":"org-1"}
//	<P>QUERY        extra query parameters, as a query string: api-version=2024-10-21
//	<P>PARAMS       extra top-level fields of the request body, as a JSON object. They override the
//	                evaluator's own fields of the same name, and a null value removes one:
//	                {"max_completion_tokens":50,"temperature":null}

// HTTPConfig is the connection and request customization shared by the LLM-backed evaluators.
type HTTPConfig struct {
	APIKey     string
	BaseURL    string
	Path       string
	Model      string
	Timeout    time.Duration
	AuthHeader string
	AuthScheme string
	Headers    map[string]string
	Query      url.Values
	Params     map[string]any
}

// HTTPDefaults are the values a dialect uses for settings that are not given.
type HTTPDefaults struct {
	BaseURL string
	Path    string
	Model   string
}

var headerNameRe = regexp.MustCompile(`^[A-Za-z0-9-]{1,64}$`)

// forbiddenHeaders cannot be set through <P>HEADERS: the client sets them itself.
var forbiddenHeaders = map[string]bool{"host": true, "content-length": true, "content-type": true, "transfer-encoding": true}

// LookupFunc reads a setting and reports whether it is set, like os.LookupEnv.
type LookupFunc = func(key string) (string, bool)

// LoadHTTPConfig reads the settings for the evaluator whose environment prefix is prefix (for
// example "TAGGER_DECISIONS_OPENAI_"). Every problem is reported, none is papered over with a default.
func LoadHTTPConfig(lookup LookupFunc, prefix string, d HTTPDefaults) (HTTPConfig, error) {
	var errs []error
	value := func(name string) string {
		v, _ := lookup(prefix + name)
		return v
	}
	get := func(name, def string) string {
		if v := value(name); v != "" {
			return v
		}
		return def
	}
	bad := func(name string, err error) { errs = append(errs, fmt.Errorf("%s%s: %w", prefix, name, err)) }

	cfg := HTTPConfig{
		APIKey:     value("API_KEY"),
		BaseURL:    strings.TrimRight(get("BASE_URL", d.BaseURL), "/"),
		Path:       get("PATH", d.Path),
		Model:      get("MODEL", d.Model),
		AuthHeader: get("AUTH_HEADER", "Authorization"),
		AuthScheme: "Bearer",
	}
	// An empty AUTH_SCHEME is meaningful (bare key), so look at whether it is set at all.
	if v, ok := lookup(prefix + "AUTH_SCHEME"); ok {
		cfg.AuthScheme = v
	}

	if u, err := url.Parse(cfg.BaseURL); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		bad("BASE_URL", fmt.Errorf("%q is not an http(s) URL", cfg.BaseURL))
	}
	if !strings.HasPrefix(cfg.Path, "/") {
		bad("PATH", fmt.Errorf("%q must start with /", cfg.Path))
	}
	if cfg.Model == "" {
		bad("MODEL", errors.New("is required"))
	}
	if !headerNameRe.MatchString(cfg.AuthHeader) || forbiddenHeaders[strings.ToLower(cfg.AuthHeader)] {
		bad("AUTH_HEADER", fmt.Errorf("%q is not a usable header name", cfg.AuthHeader))
	}

	cfg.Timeout = 60 * time.Second
	if v := value("TIMEOUT"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil || d <= 0 {
			bad("TIMEOUT", fmt.Errorf("%q is not a positive duration", v))
		} else {
			cfg.Timeout = d
		}
	}

	if v := value("HEADERS"); v != "" {
		if err := json.Unmarshal([]byte(v), &cfg.Headers); err != nil {
			bad("HEADERS", fmt.Errorf("must be a JSON object of strings: %w", err))
		}
		for name := range cfg.Headers {
			if !headerNameRe.MatchString(name) || forbiddenHeaders[strings.ToLower(name)] {
				bad("HEADERS", fmt.Errorf("%q is not a usable header name", name))
			}
		}
	}
	if v := value("QUERY"); v != "" {
		q, err := url.ParseQuery(strings.TrimPrefix(v, "?"))
		if err != nil {
			bad("QUERY", fmt.Errorf("must be a query string such as api-version=1: %w", err))
		}
		cfg.Query = q
	}
	if v := value("PARAMS"); v != "" {
		if err := json.Unmarshal([]byte(v), &cfg.Params); err != nil {
			bad("PARAMS", fmt.Errorf("must be a JSON object: %w", err))
		}
	}
	return cfg, errors.Join(errs...)
}

// endpoint returns the endpoint URL with the configured extra query parameters.
func (c HTTPConfig) endpoint() string {
	u := c.BaseURL + c.Path
	if len(c.Query) > 0 {
		sep := "?"
		if strings.Contains(u, "?") {
			sep = "&"
		}
		u += sep + c.Query.Encode()
	}
	return u
}

// mergeParams applies PARAMS to the evaluator's own request fields.
func (c HTTPConfig) mergeParams(body map[string]any) map[string]any {
	for k, v := range c.Params {
		if v == nil {
			delete(body, k)
			continue
		}
		body[k] = v
	}
	return body
}

// post sends body (after applying PARAMS) as JSON and returns the response body of a 200 answer.
func (c HTTPConfig) post(ctx context.Context, client *http.Client, body map[string]any) ([]byte, error) {
	payload, err := json.Marshal(c.mergeParams(body))
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint(), bytes.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range c.Headers {
		req.Header.Set(k, v)
	}
	if c.APIKey != "" {
		value := c.APIKey
		if c.AuthScheme != "" {
			value = c.AuthScheme + " " + c.APIKey
		}
		req.Header.Set(c.AuthHeader, value)
	}

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s returned status %d: %s", c.Path, resp.StatusCode, truncate(string(respBody), 1024))
	}
	return respBody, nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

func parseFloatEnv(lookup LookupFunc, name string, def, min, max float64) (float64, error) {
	v, _ := lookup(name)
	if v == "" {
		return def, nil
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil || f < min || f > max {
		return 0, fmt.Errorf("%s: %q is not a number between %g and %g", name, v, min, max)
	}
	return f, nil
}

func parseIntEnv(lookup LookupFunc, name string, def, min int) (int, error) {
	v, _ := lookup(name)
	if v == "" {
		return def, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < min {
		return 0, fmt.Errorf("%s: %q is not an integer >= %d", name, v, min)
	}
	return n, nil
}
