package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
)

// upstream is an internal service the gateway calls. The gateway never forwards an inbound
// request: every route handler validates what the client sent and builds a new request here from
// the validated values, so nothing the client controls reaches an internal service unless a
// handler put it there on purpose (method, path, query parameters, headers and body are all
// chosen by the gateway).
type upstream struct {
	name   string
	base   *url.URL
	client *http.Client

	// passHeaders are response headers, besides Content-Type and Content-Length, that this service
	// is allowed to set on the client's answer.
	passHeaders []string

	// unavailable describes the answer to the client when this service cannot be reached.
	unavailableStatus  int
	unavailableCode    string
	unavailableMessage string
}

func newUpstream(name, baseURL string, rt http.RoundTripper, status int, code, message string, passHeaders ...string) (*upstream, error) {
	base, err := url.Parse(baseURL)
	if err != nil || base.Scheme == "" || base.Host == "" {
		return nil, fmt.Errorf("invalid %s base URL %q", name, baseURL)
	}
	// No client-side timeout: uploads and long queries are bounded by the inbound request's
	// context and the server's limits, not cut off here.
	return &upstream{
		name: name,
		base: base,
		// A redirect from an internal service is the answer, not something to follow: storage sends a
		// client to a download URL, and the gateway hands that on.
		client: &http.Client{Transport: rt, CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		}},
		passHeaders:        passHeaders,
		unavailableStatus:  status,
		unavailableCode:    code,
		unavailableMessage: message,
	}, nil
}

// upstreamRequest is a request the gateway has built itself.
type upstreamRequest struct {
	method string
	// path is built from validated segments only (see the validators in sanitize.go).
	path  string
	query url.Values
	// body, contentLength and contentType describe the payload; body is nil for none.
	body          io.Reader
	contentLength int64 // -1 when unknown
	contentType   string
	// authorization is the one inbound header that is passed on, and only to keystorage, which
	// authenticates admin credentials itself.
	authorization string
}

func (u *upstream) do(ctx context.Context, ur upstreamRequest) (*http.Response, error) {
	target := *u.base
	target.Path = u.base.Path + ur.path
	target.RawPath = ""
	target.RawQuery = ur.query.Encode()
	target.Fragment = ""

	body := ur.body
	if body == nil {
		body = http.NoBody
	}
	req, err := http.NewRequestWithContext(ctx, ur.method, target.String(), body)
	if err != nil {
		return nil, err
	}
	if ur.body != nil {
		req.ContentLength = ur.contentLength // -1 sends a chunked body
		req.Header.Set("Content-Type", ur.contentType)
	}
	if ur.authorization != "" {
		req.Header.Set("Authorization", ur.authorization)
	}
	req.Header.Set("User-Agent", "tagona-api")
	return u.client.Do(req)
}

// relay writes an upstream response to the client. Only the status, the body, the content headers
// and the few headers the service is explicitly allowed to set (passHeaders) are passed on:
// everything else an internal service sets stays inside.
func relay(w http.ResponseWriter, resp *http.Response, passHeaders []string) {
	defer resp.Body.Close()
	h := w.Header()
	if v := resp.Header.Get("Content-Type"); v != "" {
		h.Set("Content-Type", v)
	}
	if resp.ContentLength >= 0 {
		h.Set("Content-Length", strconv.FormatInt(resp.ContentLength, 10))
	}
	for _, name := range passHeaders {
		if v := resp.Header.Get(name); v != "" {
			h.Set(name, v)
		}
	}
	h.Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(resp.StatusCode)
	if _, err := io.Copy(w, resp.Body); err != nil {
		slog.Warn("relaying upstream response failed", "error", err)
	}
}

// forward sends ur to u and relays the answer. onResponse, when set, runs after the answer has
// arrived and before it is written to the client.
func (g *gateway) forward(w http.ResponseWriter, r *http.Request, u *upstream, ur upstreamRequest, onResponse func(*http.Response)) {
	resp, err := u.do(r.Context(), ur)
	if err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			writeError(w, http.StatusRequestEntityTooLarge, "payload_too_large", "request body too large")
			return
		}
		slog.Error("upstream request failed", "service", u.name, "method", ur.method, "path", ur.path, "error", err)
		writeError(w, u.unavailableStatus, u.unavailableCode, u.unavailableMessage)
		return
	}
	if onResponse != nil {
		onResponse(resp)
	}
	relay(w, resp, u.passHeaders)
}
