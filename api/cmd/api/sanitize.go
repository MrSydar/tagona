package main

import (
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
	"unicode/utf8"

	"github.com/go-chi/chi/v5"
)

// The gateway checks the shape of what a client sends (types, characters, lengths, which
// parameters exist at all) and rejects the request when anything is off: an unknown query
// parameter, a repeated one, an unknown JSON field, trailing data after a JSON body, a body on a
// request that takes none. Limits that are policy and configurable in the storage service (maximum
// page size, maximum number of tags, ...) are left to it, so they are defined in one place.

// requestError is a request the gateway refuses to forward.
type requestError struct {
	status  int
	code    string
	message string
}

func (e *requestError) Error() string { return e.code + ": " + e.message }

func badRequest(code, format string, args ...any) *requestError {
	return &requestError{http.StatusBadRequest, code, fmt.Sprintf(format, args...)}
}

func reject(w http.ResponseWriter, e *requestError) {
	writeError(w, e.status, e.code, e.message)
}

var (
	collectionRe = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,63}$`)
	uuidRe       = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)
	cursorRe     = regexp.MustCompile(`^[A-Za-z0-9_-]{1,512}$`) // storage cursors are unpadded base64url
	digitsRe     = regexp.MustCompile(`^[0-9]{1,9}$`)
)

const (
	maxJSONBodyBytes   = 256 << 10 // query and collection requests
	maxNameBytes       = 128       // tag and key names
	maxTagsParamBytes  = 16 << 10
	maxAuthHeaderBytes = 1 << 10
	maxMetadataBytes   = 16 << 10 // the metadata parameter of an upload and the body of a metadata request
	maxVersionBytes    = 128      // a tagger version
)

// collectionParam returns the validated {collection} path segment.
func collectionParam(r *http.Request) (string, *requestError) {
	name := chi.URLParam(r, "collection")
	if !collectionRe.MatchString(name) {
		return "", badRequest("invalid_collection_name",
			"collection name must be 1-64 chars, lowercase letters, digits, underscore, hyphen, starting with a letter")
	}
	return name, nil
}

// uuidParam returns a path segment that must be a UUID. A malformed one names nothing that
// exists, so it is a 404 and never reaches an internal service.
func uuidParam(r *http.Request, param, what string) (string, *requestError) {
	id := chi.URLParam(r, param)
	if !uuidRe.MatchString(id) {
		return "", &requestError{http.StatusNotFound, "not_found", what + " not found"}
	}
	return id, nil
}

// noBody rejects a request that carries a body although the route takes none.
func noBody(r *http.Request) *requestError {
	if r.ContentLength != 0 {
		return badRequest("unexpected_body", "this request must not have a body")
	}
	return nil
}

// queryParams returns the query parameters of r. Only the names in allowed may appear, each at
// most once; anything else is rejected. Empty values are dropped, as they mean "not set".
func queryParams(r *http.Request, allowed ...string) (map[string]string, *requestError) {
	values, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		return nil, badRequest("invalid_parameter", "the query string is malformed")
	}
	out := make(map[string]string, len(values))
	for name, vs := range values {
		if !contains(allowed, name) {
			return nil, badRequest("invalid_parameter", "unknown query parameter %q", truncate(name, 40))
		}
		if len(vs) > 1 {
			return nil, badRequest("invalid_parameter", "query parameter %q given more than once", name)
		}
		if vs[0] != "" {
			out[name] = vs[0]
		}
	}
	return out, nil
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return strings.ToValidUTF8(s[:n], "") + "..."
}

// decodeJSON reads a JSON request body into dst, which must have a field for everything that is
// allowed: unknown fields, a wrong type, or anything after the object are errors.
func decodeJSON(w http.ResponseWriter, r *http.Request, dst any, limit int64) *requestError {
	r.Body = http.MaxBytesReader(w, r.Body, limit)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			return &requestError{http.StatusRequestEntityTooLarge, "payload_too_large", "request body too large"}
		}
		return badRequest("invalid_json", "the request body is not a valid JSON object for this endpoint")
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return badRequest("invalid_json", "the request body has data after the JSON object")
	}
	return nil
}

// optionalInt validates a decimal integer parameter and returns it normalized.
func optionalInt(params map[string]string, name, code string) (string, *requestError) {
	v, ok := params[name]
	if !ok {
		return "", nil
	}
	if !digitsRe.MatchString(v) {
		return "", badRequest(code, "%s must be a non-negative integer", name)
	}
	n, _ := strconv.Atoi(v)
	return strconv.Itoa(n), nil
}

// optionalBool validates a boolean parameter and returns it normalized.
func optionalBool(params map[string]string, name, code string) (string, *requestError) {
	v, ok := params[name]
	if !ok {
		return "", nil
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return "", badRequest(code, "%s must be true or false", name)
	}
	return strconv.FormatBool(b), nil
}

// optionalDate validates an RFC 3339 timestamp and returns it normalized.
func optionalDate(params map[string]string, name, code string) (string, *requestError) {
	v, ok := params[name]
	if !ok {
		return "", nil
	}
	t, err := time.Parse(time.RFC3339, v)
	if err != nil {
		return "", badRequest(code, "%s must be an RFC 3339 timestamp", name)
	}
	return t.UTC().Format(time.RFC3339Nano), nil
}

// optionalCursor validates an opaque pagination cursor.
func optionalCursor(params map[string]string) (string, *requestError) {
	v, ok := params["cursor"]
	if !ok {
		return "", nil
	}
	if !cursorRe.MatchString(v) {
		return "", badRequest("invalid_cursor", "cursor is not valid")
	}
	return v, nil
}

// cleanText reports whether s is valid UTF-8 without control characters.
func cleanText(s string) bool {
	if !utf8.ValidString(s) {
		return false
	}
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			return false
		}
	}
	return true
}

// set adds name=value to q unless value is empty.
func set(q url.Values, name, value string) {
	if value != "" {
		q.Set(name, value)
	}
}

// validTaggerVersion reports whether v can be a tagger version: 1 to 128 bytes of valid UTF-8 without control
// characters. What a version means is the tagger's business, so nothing more is required.
func validTaggerVersion(v string) bool {
	return v != "" && len(v) <= maxVersionBytes && cleanText(v)
}

// optionalMetadata validates the metadata parameter of an upload, a JSON object whose values are strings,
// and returns it re-encoded, so that what storage receives is built here and not the client's text. The
// limits on its size and content are storage's.
func optionalMetadata(params map[string]string) (string, *requestError) {
	raw, ok := params["metadata"]
	if !ok || raw == "" {
		return "", nil
	}
	if len(raw) > maxMetadataBytes {
		return "", badRequest("invalid_metadata", "metadata is too large")
	}
	var m map[string]string
	dec := json.NewDecoder(strings.NewReader(raw))
	if err := dec.Decode(&m); err != nil || m == nil {
		return "", badRequest("invalid_metadata", "metadata must be a JSON object whose values are strings")
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return "", badRequest("invalid_metadata", "metadata has data after the JSON object")
	}
	out, _ := json.Marshal(m)
	return string(out), nil
}

// optionalTags validates the tags parameter of an upload, a JSON object whose values are true or false, and
// returns it re-encoded, like optionalMetadata. The limits on the tags are storage's.
func optionalTags(params map[string]string) (string, *requestError) {
	raw, ok := params["tags"]
	if !ok || raw == "" {
		return "", nil
	}
	if len(raw) > maxMetadataBytes {
		return "", badRequest("invalid_tags", "tags is too large")
	}
	var in map[string]*bool // pointers, so that a null is told from false
	dec := json.NewDecoder(strings.NewReader(raw))
	if err := dec.Decode(&in); err != nil || in == nil {
		return "", badRequest("invalid_tags", "tags must be a JSON object whose values are true or false")
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return "", badRequest("invalid_tags", "tags has data after the JSON object")
	}
	out := make(map[string]bool, len(in))
	for tag, v := range in {
		if v == nil {
			return "", badRequest("invalid_tags", "tags must be a JSON object whose values are true or false")
		}
		out[tag] = *v
	}
	enc, _ := json.Marshal(out)
	return string(enc), nil
}
