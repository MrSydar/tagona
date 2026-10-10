package main

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"
)

// gateway holds what the route handlers need. Each handler follows the same pattern: check the
// request against what its route accepts (rejecting everything else), then build a new request for
// the internal service from the validated values and relay the answer. See sanitize.go.
type gateway struct {
	storage    *upstream
	keystorage *upstream
	// maxBodyBytes caps object uploads (the only streamed body).
	maxBodyBytes int64
	// onKeyDeleted runs after a key was deleted through the admin routes, to drop cached verdicts.
	onKeyDeleted func()
}

// jsonBody builds the body of a JSON request from v.
func jsonBody(ur *upstreamRequest, v any) {
	payload, err := json.Marshal(v)
	if err != nil { // only validated, plain values reach here
		panic(err)
	}
	ur.body = bytes.NewReader(payload)
	ur.contentLength = int64(len(payload))
	ur.contentType = "application/json"
}

// ---- collections ------------------------------------------------------------------------------

func (g *gateway) listCollections(w http.ResponseWriter, r *http.Request) {
	if e := noBody(r); e != nil {
		reject(w, e)
		return
	}
	params, e := queryParams(r, "limit", "cursor")
	if e != nil {
		reject(w, e)
		return
	}
	// The range of the limit is storage's to enforce; here only the form is checked, as for tags.
	q := url.Values{}
	limit, e := optionalInt(params, "limit", "invalid_limit")
	if e == nil {
		var cursor string
		if cursor, e = optionalCursor(params); e == nil {
			set(q, "limit", limit)
			set(q, "cursor", cursor)
		}
	}
	if e != nil {
		reject(w, e)
		return
	}
	g.forward(w, r, g.storage, upstreamRequest{method: http.MethodGet, path: "/collections", query: q}, nil)
}

// listTaggers lists the tagger versions available for new collections. It takes nothing.
func (g *gateway) listTaggers(w http.ResponseWriter, r *http.Request) {
	if e := checkBare(r); e != nil {
		reject(w, e)
		return
	}
	g.forward(w, r, g.storage, upstreamRequest{method: http.MethodGet, path: "/taggers"}, nil)
}

func (g *gateway) createCollection(w http.ResponseWriter, r *http.Request) {
	if _, e := queryParams(r); e != nil {
		reject(w, e)
		return
	}
	var req struct {
		Name string `json:"name"`
		// TaggerVersion is optional: storage then uses the version of the tagger that runs.
		TaggerVersion *string `json:"tagger_version,omitempty"`
	}
	if e := decodeJSON(w, r, &req, maxJSONBodyBytes); e != nil {
		reject(w, e)
		return
	}
	if !collectionRe.MatchString(req.Name) {
		reject(w, badRequest("invalid_name",
			"collection name must be 1-64 chars, lowercase letters, digits, underscore, hyphen, starting with a letter"))
		return
	}
	if req.TaggerVersion != nil && !validTaggerVersion(*req.TaggerVersion) {
		reject(w, badRequest("invalid_tagger_version", "tagger_version must be 1 to 128 bytes of text without control characters"))
		return
	}
	ur := upstreamRequest{method: http.MethodPost, path: "/collections"}
	jsonBody(&ur, req)
	g.forward(w, r, g.storage, ur, nil)
}

func (g *gateway) deleteCollection(w http.ResponseWriter, r *http.Request) {
	coll, e := collectionParam(r)
	if e == nil {
		e = checkBare(r)
	}
	if e != nil {
		reject(w, e)
		return
	}
	g.forward(w, r, g.storage, upstreamRequest{method: http.MethodDelete, path: "/collections/" + coll}, nil)
}

func (g *gateway) listCollectionTags(w http.ResponseWriter, r *http.Request) {
	coll, e := collectionParam(r)
	if e != nil {
		reject(w, e)
		return
	}
	if e := noBody(r); e != nil {
		reject(w, e)
		return
	}
	params, e := queryParams(r, "prefix", "limit", "cursor")
	if e != nil {
		reject(w, e)
		return
	}
	q := url.Values{}
	if prefix := params["prefix"]; prefix != "" {
		if len(prefix) > maxNameBytes || !utf8.ValidString(prefix) {
			reject(w, badRequest("invalid_prefix", "prefix must be valid UTF-8 of at most 128 bytes"))
			return
		}
		q.Set("prefix", prefix)
	}
	limit, e := optionalInt(params, "limit", "invalid_limit")
	if e == nil {
		var cursor string
		if cursor, e = optionalCursor(params); e == nil {
			set(q, "limit", limit)
			set(q, "cursor", cursor)
		}
	}
	if e != nil {
		reject(w, e)
		return
	}
	g.forward(w, r, g.storage, upstreamRequest{method: http.MethodGet, path: "/collections/" + coll + "/tags", query: q}, nil)
}

// ---- objects ----------------------------------------------------------------------------------

func (g *gateway) putObject(w http.ResponseWriter, r *http.Request) {
	coll, e := collectionParam(r)
	if e != nil {
		reject(w, e)
		return
	}
	params, e := queryParams(r, "date", "ttl_seconds", "metadata", "tags")
	if e != nil {
		reject(w, e)
		return
	}
	q := url.Values{}
	date, e := optionalDate(params, "date", "invalid_date")
	if e == nil {
		var ttl, metadata, tags string
		if ttl, e = optionalInt(params, "ttl_seconds", "invalid_ttl"); e == nil {
			if metadata, e = optionalMetadata(params); e == nil {
				if tags, e = optionalTags(params); e == nil {
					set(q, "date", date)
					set(q, "ttl_seconds", ttl)
					set(q, "metadata", metadata)
					set(q, "tags", tags)
				}
			}
		}
	}
	if e != nil {
		reject(w, e)
		return
	}
	// The payload is streamed as is, under the gateway's size cap; whatever Content-Type the
	// client declared, storage is always told it is opaque bytes.
	ur := upstreamRequest{
		method: http.MethodPost, path: "/collections/" + coll + "/objects", query: q,
		body: r.Body, contentLength: r.ContentLength, contentType: "application/octet-stream",
	}
	g.forward(w, r, g.storage, ur, nil)
}

func (g *gateway) objectRequest(w http.ResponseWriter, r *http.Request, method, suffix string, onResponse ...func(*http.Response)) {
	coll, e := collectionParam(r)
	var id string
	if e == nil {
		id, e = uuidParam(r, "id", "object")
	}
	if e == nil {
		e = checkBare(r)
	}
	if e != nil {
		reject(w, e)
		return
	}
	var on func(*http.Response)
	if len(onResponse) > 0 {
		on = onResponse[0]
	}
	g.forward(w, r, g.storage, upstreamRequest{method: method, path: "/collections/" + coll + "/objects/" + id + suffix}, on)
}

func (g *gateway) getObject(w http.ResponseWriter, r *http.Request) {
	g.objectRequest(w, r, http.MethodGet, "")
}

// getObjectData answers with a redirect to a short-lived URL of the object store, which storage signs: the
// payload does not pass through the services. The redirect is the only case where storage's Location header
// reaches a client, and only if it is an http(s) URL.
func (g *gateway) getObjectData(w http.ResponseWriter, r *http.Request) {
	g.objectRequest(w, r, http.MethodGet, "/data", func(resp *http.Response) {
		if resp.StatusCode != http.StatusTemporaryRedirect {
			return
		}
		loc, err := url.Parse(resp.Header.Get("Location"))
		if err != nil || (loc.Scheme != "http" && loc.Scheme != "https") || loc.Host == "" {
			slog.Error("storage sent an unusable download URL", "error", err)
			resp.StatusCode, resp.Body = http.StatusBadGateway, io.NopCloser(strings.NewReader(
				`{"error":{"code":"bad_gateway","message":"storage service not available"}}`))
			resp.ContentLength = -1
			resp.Header = http.Header{"Content-Type": {"application/json"}}
			return
		}
		w.Header().Set("Location", loc.String())
		w.Header().Set("Cache-Control", "no-store") // the URL is a credential until it expires
	})
}
func (g *gateway) deleteObject(w http.ResponseWriter, r *http.Request) {
	g.objectRequest(w, r, http.MethodDelete, "")
}

// replaceMetadata (PUT) replaces all metadata of an object; mergeMetadata (PATCH) changes some of it.
func (g *gateway) replaceMetadata(w http.ResponseWriter, r *http.Request) {
	// Decoded with pointer values, because a JSON null would become an empty string in a plain string.
	var body map[string]*string
	g.metadataRequest(w, r, http.MethodPut, "metadata", &body, func() (any, *requestError) {
		if body == nil {
			return nil, badRequest("invalid_json", "the request body must be a JSON object")
		}
		out := make(map[string]string, len(body))
		for k, v := range body {
			if v == nil {
				return nil, badRequest("invalid_json", "a value must be a string: use PATCH to remove a key")
			}
			out[k] = *v
		}
		return out, nil
	})
}

func (g *gateway) mergeMetadata(w http.ResponseWriter, r *http.Request) {
	var body map[string]*string
	g.metadataRequest(w, r, http.MethodPatch, "metadata", &body, func() (any, *requestError) {
		if body == nil {
			return nil, badRequest("invalid_json", "the request body must be a JSON object")
		}
		return body, nil
	})
}

// changeObjectTags (PATCH) forces tags of an object: true or false sets one, null deletes it.
func (g *gateway) changeObjectTags(w http.ResponseWriter, r *http.Request) {
	var body map[string]*bool
	g.metadataRequest(w, r, http.MethodPatch, "tags", &body, func() (any, *requestError) {
		if body == nil {
			return nil, badRequest("invalid_json", "the request body must be a JSON object")
		}
		return body, nil
	})
}

// metadataRequest validates the path and the JSON object body of a metadata or tags request (what is changed is
// the object's `what`) and makes a clean one to storage from what value builds out of the decoded body.
func (g *gateway) metadataRequest(w http.ResponseWriter, r *http.Request, method, what string, dst any, value func() (any, *requestError)) {
	coll, e := collectionParam(r)
	var id string
	if e == nil {
		id, e = uuidParam(r, "id", "object")
	}
	if e == nil {
		_, e = queryParams(r)
	}
	if e == nil {
		e = decodeJSON(w, r, dst, maxMetadataBytes)
	}
	var out any
	if e == nil {
		out, e = value()
	}
	if e != nil {
		reject(w, e)
		return
	}
	ur := upstreamRequest{method: method, path: "/collections/" + coll + "/objects/" + id + "/" + what}
	jsonBody(&ur, out)
	g.forward(w, r, g.storage, ur, nil)
}

func (g *gateway) getObjectTags(w http.ResponseWriter, r *http.Request) {
	coll, e := collectionParam(r)
	var id string
	if e == nil {
		id, e = uuidParam(r, "id", "object")
	}
	if e == nil {
		e = noBody(r)
	}
	var params map[string]string
	if e == nil {
		params, e = queryParams(r, "tags", "evaluate")
	}
	q := url.Values{}
	if e == nil {
		if tags := params["tags"]; tags != "" {
			if len(tags) > maxTagsParamBytes || !cleanText(tags) {
				e = badRequest("invalid_tags", "tags must be a comma-separated list of valid text")
			} else {
				q.Set("tags", tags)
			}
		}
	}
	if e == nil {
		var evaluate string
		if evaluate, e = optionalBool(params, "evaluate", "invalid_evaluate"); e == nil {
			set(q, "evaluate", evaluate)
		}
	}
	if e != nil {
		reject(w, e)
		return
	}
	g.forward(w, r, g.storage, upstreamRequest{method: http.MethodGet, path: "/collections/" + coll + "/objects/" + id + "/tags", query: q}, nil)
}

// dateFilter and queryBody are everything a tag query may contain; the decoder refuses any other field.
type dateFilter struct {
	GT  *time.Time `json:"gt,omitempty"`
	GTE *time.Time `json:"gte,omitempty"`
	LT  *time.Time `json:"lt,omitempty"`
	LTE *time.Time `json:"lte,omitempty"`
	EQ  *time.Time `json:"eq,omitempty"`
}

// toUTC normalizes the timestamps, as is done for those in the query string.
func (d *dateFilter) toUTC() {
	if d == nil {
		return
	}
	for _, t := range []**time.Time{&d.GT, &d.GTE, &d.LT, &d.LTE, &d.EQ} {
		if *t != nil {
			u := (*t).UTC()
			*t = &u
		}
	}
}

type queryBody struct {
	Tags       map[string]bool `json:"tags,omitempty"`
	Date       *dateFilter     `json:"date,omitempty"`
	Limit      *int            `json:"limit,omitempty"`
	Cursor     string          `json:"cursor,omitempty"`
	TimeoutMs  *int            `json:"timeout_ms,omitempty"`
	BestEffort *bool           `json:"best_effort,omitempty"`
	Evaluate   *bool           `json:"evaluate,omitempty"`
}

func (g *gateway) queryObjects(w http.ResponseWriter, r *http.Request) {
	coll, e := collectionParam(r)
	if e == nil {
		_, e = queryParams(r)
	}
	var req queryBody
	if e == nil {
		e = decodeJSON(w, r, &req, maxJSONBodyBytes)
	}
	if e == nil && req.Cursor != "" && !cursorRe.MatchString(req.Cursor) {
		e = badRequest("invalid_cursor", "cursor is not valid")
	}
	if e != nil {
		reject(w, e)
		return
	}
	req.Date.toUTC()
	// The request storage receives is this struct re-encoded, not the client's bytes.
	ur := upstreamRequest{method: http.MethodPost, path: "/collections/" + coll + "/objects/query"}
	jsonBody(&ur, req)
	g.forward(w, r, g.storage, ur, nil)
}

// ---- API key management (forwarded to keystorage, which checks the admin credentials) --------

// adminAuthorization is the credential header to pass on, or "" when there is none worth passing.
func adminAuthorization(r *http.Request) string {
	v := r.Header.Get("Authorization")
	if len(v) > maxAuthHeaderBytes {
		return ""
	}
	return v
}

func (g *gateway) createKey(w http.ResponseWriter, r *http.Request) {
	if _, e := queryParams(r); e != nil {
		reject(w, e)
		return
	}
	// ttl_seconds is forwarded as is: keystorage owns the limits (at least 1, at most the configured
	// maximum, and the default when it is left out).
	var req struct {
		Name       string `json:"name"`
		TTLSeconds *int64 `json:"ttl_seconds,omitempty"`
	}
	if e := decodeJSON(w, r, &req, adminMaxBodyBytes); e != nil {
		reject(w, e)
		return
	}
	if len(req.Name) > maxNameBytes {
		reject(w, badRequest("invalid_name", "api key name must be at most 128 bytes"))
		return
	}
	ur := upstreamRequest{method: http.MethodPost, path: "/api-keys", authorization: adminAuthorization(r)}
	jsonBody(&ur, req)
	g.forward(w, r, g.keystorage, ur, nil)
}

func (g *gateway) listKeys(w http.ResponseWriter, r *http.Request) {
	if e := noBody(r); e != nil {
		reject(w, e)
		return
	}
	params, e := queryParams(r, "limit", "cursor")
	if e != nil {
		reject(w, e)
		return
	}
	// The ranges are keystorage's to enforce; here only the form is checked, as for tags.
	q := url.Values{}
	limit, e := optionalInt(params, "limit", "invalid_limit")
	if e == nil {
		var cursor string
		if cursor, e = optionalCursor(params); e == nil {
			set(q, "limit", limit)
			set(q, "cursor", cursor)
		}
	}
	if e != nil {
		reject(w, e)
		return
	}
	g.forward(w, r, g.keystorage, upstreamRequest{method: http.MethodGet, path: "/api-keys", query: q, authorization: adminAuthorization(r)}, nil)
}

func (g *gateway) deleteKey(w http.ResponseWriter, r *http.Request) {
	id, e := uuidParam(r, "id", "api key")
	if e == nil {
		e = checkBare(r)
	}
	if e != nil {
		reject(w, e)
		return
	}
	ur := upstreamRequest{method: http.MethodDelete, path: "/api-keys/" + id, authorization: adminAuthorization(r)}
	g.forward(w, r, g.keystorage, ur, func(resp *http.Response) {
		// A key that was really deleted must stop working at once, not when a cached verdict expires.
		if resp.StatusCode == http.StatusNoContent && g.onKeyDeleted != nil {
			g.onKeyDeleted()
		}
	})
}

// checkBare rejects a request that has a body or any query parameter, for routes that take neither.
func checkBare(r *http.Request) *requestError {
	if e := noBody(r); e != nil {
		return e
	}
	_, e := queryParams(r)
	return e
}
