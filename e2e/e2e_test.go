package e2e_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const storageURL = "http://localhost:8080"

var httpClient = &http.Client{Timeout: 30 * time.Second}

var adminUsername = envOrDefault("API_ADMIN_USERNAME", "admin")
var adminPassword = envOrDefault("API_ADMIN_PASSWORD", "tagona")

func envOrDefault(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

type tagQueryReq struct {
	Tags       map[string]bool `json:"tags,omitempty"`
	Limit      int             `json:"limit"`
	Cursor     string          `json:"cursor,omitempty"`
	TimeoutMs  int             `json:"timeout_ms,omitempty"`
	BestEffort bool            `json:"best_effort,omitempty"`
}

type tagQueryResp struct {
	Objects []objMeta `json:"objects"`
	Next    string    `json:"next,omitempty"`
}

type objMeta struct {
	ID          string    `json:"id"`
	Collection  string    `json:"collection"`
	DataType    string    `json:"data_type"`
	Date        time.Time `json:"date"`
	SizeBytes   int64     `json:"size_bytes"`
	ContentHash string    `json:"content_hash"`
}

type apiErrorResp struct {
	Error struct {
		Code string `json:"code"`
	} `json:"error"`
}

func errorCode(t *testing.T, body []byte) string {
	t.Helper()
	var e apiErrorResp
	require.NoError(t, json.Unmarshal(body, &e), "decode error body: %s", string(body))
	return e.Error.Code
}

func setBearer(req *http.Request, apiKey string) {
	req.Header.Set("Authorization", "Bearer "+apiKey)
}

type apiKeyCreds struct {
	ID  string `json:"id"`
	Key string `json:"key"`
}

func createAPIKey(t *testing.T) apiKeyCreds {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, storageURL+"/v1/admin/api-keys", bytes.NewReader([]byte(`{"name":"e2e"}`)))
	require.NoError(t, err)
	req.SetBasicAuth(adminUsername, adminPassword)
	resp, err := httpClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	require.Equal(t, http.StatusCreated, resp.StatusCode, "create api key failed: %s", string(b))
	var creds apiKeyCreds
	require.NoError(t, json.Unmarshal(b, &creds))
	require.NotEmpty(t, creds.Key)
	return creds
}

func deleteAPIKey(t *testing.T, id string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodDelete, storageURL+"/v1/admin/api-keys/"+id, nil)
	require.NoError(t, err)
	req.SetBasicAuth(adminUsername, adminPassword)
	resp, err := httpClient.Do(req)
	require.NoError(t, err)
	resp.Body.Close()
}

func createCollection(t *testing.T, apiKey, name string) {
	t.Helper()
	createBody := []byte(fmt.Sprintf(`{"name":"%s","data_type":"txt"}`, name))
	req, err := http.NewRequest(http.MethodPost, storageURL+"/v1/collections", bytes.NewReader(createBody))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	setBearer(req, apiKey)
	resp, err := httpClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	require.Equal(t, http.StatusCreated, resp.StatusCode, "create collection failed: %s", string(b))
}

func TestEndToEnd(t *testing.T) {
	creds := createAPIKey(t)
	defer deleteAPIKey(t, creds.ID)
	coll := fmt.Sprintf("e2e_end2end_%d", time.Now().UnixNano())

	// 1. Create collection
	createCollection(t, creds.Key, coll)

	defer func() {
		req, _ := http.NewRequest("DELETE", storageURL+"/v1/collections/"+coll, nil)
		setBearer(req, creds.Key)
		r, _ := httpClient.Do(req)
		if r != nil {
			r.Body.Close()
		}
	}()

	// 2. Upload first object
	payload1 := []byte("a golang job description")
	obj1 := uploadObject(t, creds.Key, coll, "txt", payload1)

	// 3. Query without tags — should return obj1
	result := queryObjects(t, creds.Key, coll, tagQueryReq{Limit: 10})
	require.Len(t, result.Objects, 1)
	assert.Equal(t, obj1.ID, result.Objects[0].ID)

	// 4. Query with "golang" — should return obj1
	result = queryObjects(t, creds.Key, coll, tagQueryReq{Tags: map[string]bool{"golang": true}, Limit: 10})
	require.Len(t, result.Objects, 1)
	assert.Equal(t, obj1.ID, result.Objects[0].ID)

	// 5. Query with "java" — should return nothing
	result = queryObjects(t, creds.Key, coll, tagQueryReq{Tags: map[string]bool{"java": true}, Limit: 10})
	assert.Empty(t, result.Objects)

	// 6. Upload second object
	payload2 := []byte("a golang and java job description")
	obj2 := uploadObject(t, creds.Key, coll, "txt", payload2)

	// 7. Query no tags — both present
	result = queryObjects(t, creds.Key, coll, tagQueryReq{Limit: 10})
	ids := extractIDs(result.Objects)
	assert.Len(t, result.Objects, 2)
	assert.Contains(t, ids, obj1.ID)
	assert.Contains(t, ids, obj2.ID)

	// 8. Query "golang" — both present
	result = queryObjects(t, creds.Key, coll, tagQueryReq{Tags: map[string]bool{"golang": true}, Limit: 10})
	ids = extractIDs(result.Objects)
	assert.Len(t, result.Objects, 2)
	assert.Contains(t, ids, obj1.ID)
	assert.Contains(t, ids, obj2.ID)

	// 9. Query "java" — only obj2 present
	result = queryObjects(t, creds.Key, coll, tagQueryReq{Tags: map[string]bool{"java": true}, Limit: 10})
	ids = extractIDs(result.Objects)
	assert.Len(t, result.Objects, 1)
	assert.Contains(t, ids, obj2.ID)
	assert.NotContains(t, ids, obj1.ID)

	// 10. Query "c++" — nothing present
	result = queryObjects(t, creds.Key, coll, tagQueryReq{Tags: map[string]bool{"c++": true}, Limit: 10})
	assert.Empty(t, result.Objects)
}

func TestPagination(t *testing.T) {
	creds := createAPIKey(t)
	defer deleteAPIKey(t, creds.ID)
	coll := fmt.Sprintf("e2e_pagination_%d", time.Now().UnixNano())

	// 1. Create collection
	createCollection(t, creds.Key, coll)

	defer func() {
		req, _ := http.NewRequest("DELETE", storageURL+"/v1/collections/"+coll, nil)
		setBearer(req, creds.Key)
		r, _ := httpClient.Do(req)
		if r != nil {
			r.Body.Close()
		}
	}()

	// 2. Upload 4 unique objects
	payloads := [][]byte{
		[]byte("object one"),
		[]byte("object two"),
		[]byte("object three"),
		[]byte("object four"),
	}
	objs := make([]objMeta, len(payloads))
	for i, p := range payloads {
		objs[i] = uploadObject(t, creds.Key, coll, "txt", p)
	}

	// 3. Query page 1 with limit=2 (no tag filter -> all objects)
	page1 := queryObjects(t, creds.Key, coll, tagQueryReq{Limit: 2})
	require.Len(t, page1.Objects, 2, "page 1 should contain 2 objects")
	require.NotEmpty(t, page1.Next, "page 1 should have a next cursor")

	// 4. Query page 2 using cursor from page 1
	page2 := queryObjects(t, creds.Key, coll, tagQueryReq{Limit: 2, Cursor: page1.Next})
	require.Len(t, page2.Objects, 2, "page 2 should contain 2 objects")
	require.Empty(t, page2.Next, "page 2 should not have a next cursor")

	// 5. Verify all fetched objects are unique
	allIDs := extractIDs(append(page1.Objects, page2.Objects...))
	seen := make(map[string]struct{}, len(allIDs))
	for _, id := range allIDs {
		_, exists := seen[id]
		require.False(t, exists, "duplicate object id found: %s", id)
		seen[id] = struct{}{}
	}
	require.Len(t, seen, 4, "should have fetched 4 unique objects in total")

	// 6. Ensure all uploaded objects were returned
	for _, obj := range objs {
		assert.Contains(t, allIDs, obj.ID, "uploaded object %s should be present in results", obj.ID)
	}

	// 7. Third page should return empty
	if page2.Next != "" {
		page3 := queryObjects(t, creds.Key, coll, tagQueryReq{Limit: 2, Cursor: page2.Next})
		assert.Empty(t, page3.Objects, "page 3 should be empty")
		assert.Empty(t, page3.Next, "page 3 should not have a next cursor")
	}
}

func TestAuthRequired(t *testing.T) {
	// (a) /v1 call without a key → 401 missing_api_key
	resp, err := http.Post(storageURL+"/v1/collections", "application/json", bytes.NewReader([]byte(`{"name":"noauth","data_type":"txt"}`)))
	require.NoError(t, err)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	require.Equal(t, http.StatusUnauthorized, resp.StatusCode, "unauthenticated /v1 call should be rejected: %s", string(body))
	assert.Equal(t, "missing_api_key", errorCode(t, body))

	creds := createAPIKey(t)
	defer deleteAPIKey(t, creds.ID)

	// (b) admin endpoint with a Bearer API key (instead of Basic) → 403
	req, err := http.NewRequest(http.MethodPost, storageURL+"/v1/admin/api-keys", bytes.NewReader([]byte(`{"name":"e2e"}`)))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	setBearer(req, creds.Key)
	resp, err = httpClient.Do(req)
	require.NoError(t, err)
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	require.Equal(t, http.StatusForbidden, resp.StatusCode, "api key on admin endpoint should be rejected: %s", string(body))
	assert.Equal(t, "forbidden", errorCode(t, body))

	// (c) admin endpoint with wrong password → 401
	req, err = http.NewRequest(http.MethodGet, storageURL+"/v1/admin/api-keys", nil)
	require.NoError(t, err)
	req.SetBasicAuth(adminUsername, "wrong-password")
	resp, err = httpClient.Do(req)
	require.NoError(t, err)
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	require.Equal(t, http.StatusUnauthorized, resp.StatusCode, "wrong admin password should be rejected: %s", string(body))
	assert.Equal(t, "invalid_admin_credentials", errorCode(t, body))
}

func uploadObject(t *testing.T, apiKey, collection, dataType string, data []byte) objMeta {
	t.Helper()
	url := fmt.Sprintf("%s/v1/collections/%s/objects?data_type=%s", storageURL, collection, dataType)
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(data))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/octet-stream")
	setBearer(req, apiKey)
	resp, err := httpClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	require.Equal(t, http.StatusCreated, resp.StatusCode, "upload failed: %s", string(b))
	var obj objMeta
	require.NoError(t, json.Unmarshal(b, &obj))
	return obj
}

func queryObjects(t *testing.T, apiKey, collection string, req tagQueryReq) tagQueryResp {
	t.Helper()
	url := fmt.Sprintf("%s/v1/collections/%s/objects/query", storageURL, collection)
	body, _ := json.Marshal(req)
	hreq, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	require.NoError(t, err)
	hreq.Header.Set("Content-Type", "application/json")
	setBearer(hreq, apiKey)
	resp, err := httpClient.Do(hreq)
	require.NoError(t, err)
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	require.Equal(t, http.StatusOK, resp.StatusCode, "query failed: %s", string(b))
	var result tagQueryResp
	require.NoError(t, json.Unmarshal(b, &result))
	return result
}

func extractIDs(objs []objMeta) []string {
	ids := make([]string, len(objs))
	for i, o := range objs {
		ids[i] = o.ID
	}
	return ids
}
