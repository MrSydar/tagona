package e2e_test

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The api does not forward requests: it validates them and builds new ones for storage. Anything
// the endpoint does not take is refused by the api itself, with the codes documented in the OpenAPI
// document, and the stack still works for what is valid.
func TestGatewayRefusesUnexpectedInput(t *testing.T) {
	creds := createAPIKey(t)
	defer deleteAPIKey(t, creds.ID)
	coll := "e2e_sanitize_" + strings.ReplaceAll(time.Now().Format("150405.000000"), ".", "")
	createCollection(t, creds.Key, coll)
	obj := uploadObject(t, creds.Key, coll, []byte("a golang job"))
	defer func() {
		do(t, http.MethodDelete, "/v1/collections/"+coll, "", asKey(creds.Key))
	}()

	tests := []struct {
		name, method, path, body string
		status                   int
		code                     string
	}{
		{"unknown query parameter", http.MethodGet, "/v1/collections?admin=true", "", 400, "invalid_parameter"},
		{"repeated query parameter", http.MethodGet, "/v1/collections/" + coll + "/tags?limit=1&limit=2", "", 400, "invalid_parameter"},
		{"parameter on a delete", http.MethodDelete, "/v1/collections/" + coll + "?force=1", "", 400, "invalid_parameter"},
		{"body on a get", http.MethodGet, "/v1/collections", `{"x":1}`, 400, "unexpected_body"},
		{"unknown field in a query", http.MethodPost, "/v1/collections/" + coll + "/objects/query", `{"limit":1,"sql":"x"}`, 400, "invalid_json"},
		{"trailing data after a query", http.MethodPost, "/v1/collections/" + coll + "/objects/query", `{} {}`, 400, "invalid_json"},
		{"unknown field when creating a collection", http.MethodPost, "/v1/collections", `{"name":"x","owner":"me"}`, 400, "invalid_json"},
		{"malformed cursor", http.MethodGet, "/v1/collections/" + coll + "/tags?cursor=not%20valid", "", 400, "invalid_cursor"},
		{"object id that is not a uuid", http.MethodGet, "/v1/collections/" + coll + "/objects/not-a-uuid", "", 404, "not_found"},
		{"unknown parameter when uploading", http.MethodPost, "/v1/collections/" + coll + "/objects?owner=x", "data", 400, "invalid_parameter"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			status, body := do(t, tt.method, tt.path, tt.body, asKey(creds.Key))
			assert.Equal(t, tt.status, status, string(body))
			assert.Equal(t, tt.code, errorCode(t, body))
		})
	}

	// None of that left a mark, and valid requests with normalized values still work.
	status, body := do(t, http.MethodGet, "/v1/collections/"+coll+"/objects/"+obj.ID+"/tags?tags=golang&evaluate=1", "", asKey(creds.Key))
	require.Equal(t, http.StatusOK, status, string(body))
	assert.Contains(t, string(body), `"golang":true`)
	status, body = do(t, http.MethodPost, "/v1/collections/"+coll+"/objects/query", `{"tags":{"golang":true},"limit":5}`, asKey(creds.Key))
	require.Equal(t, http.StatusOK, status, string(body))
	assert.Contains(t, string(body), obj.ID)
}
