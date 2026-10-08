package e2e_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type collectionInfo struct {
	ID            string `json:"id"`
	Name          string `json:"name"`
	TaggerVersion string `json:"tagger_version"`
}

// newKey creates an API key that is deleted when the test ends.
func newKey(t *testing.T) apiKeyCreds {
	t.Helper()
	k := createAPIKey(t)
	t.Cleanup(func() { deleteAPIKey(t, k.ID) })
	return k
}

// createCollectionWith creates a collection with a body, answers its status and removes it afterwards.
func createCollectionWith(t *testing.T, key, body string) (int, []byte) {
	t.Helper()
	status, out := do(t, http.MethodPost, "/v1/collections", body, asKey(key))
	if status == http.StatusCreated {
		var c collectionInfo
		require.NoError(t, json.Unmarshal(out, &c))
		t.Cleanup(func() { do(t, http.MethodDelete, "/v1/collections/"+c.Name, "", asKey(key)) })
	}
	return status, out
}

func TestCollectionsArePaged(t *testing.T) {
	key := newKey(t).Key
	prefix := fmt.Sprintf("e2e-page-%d-", time.Now().UnixNano()%1_000_000_000)
	for i := 0; i < 5; i++ {
		status, body := createCollectionWith(t, key, fmt.Sprintf(`{"name":"%s%d"}`, prefix, i))
		require.Equal(t, http.StatusCreated, status, string(body))
	}

	// walk the whole list two collections at a time: each of ours shows up once, newest first
	seen := map[string]int{}
	var order []string
	query, pages := "?limit=2", 0
	for {
		status, body := do(t, http.MethodGet, "/v1/collections"+query, "", asKey(key))
		require.Equal(t, http.StatusOK, status, string(body))
		var page struct {
			Collections []collectionInfo
			Next        string
		}
		require.NoError(t, json.Unmarshal(body, &page))
		require.LessOrEqual(t, len(page.Collections), 2)
		for _, c := range page.Collections {
			assert.NotEmpty(t, c.TaggerVersion, "collection %s", c.Name)
			seen[c.Name]++
			if strings.HasPrefix(c.Name, prefix) {
				order = append(order, c.Name)
			}
		}
		pages++
		require.Less(t, pages, 1000, "paging does not end")
		if page.Next == "" {
			break
		}
		query = "?limit=2&cursor=" + url.QueryEscape(page.Next)
	}
	for name, n := range seen {
		assert.Equal(t, 1, n, "collection %s listed %d times", name, n)
	}
	require.Len(t, order, 5)
	for i, name := range order {
		assert.Equal(t, fmt.Sprintf("%s%d", prefix, 4-i), name, "newest first")
	}

	for _, tt := range []struct{ query, code string }{
		{"?limit=0", "invalid_limit"}, {"?limit=1001", "invalid_limit"}, {"?limit=x", "invalid_limit"},
		{"?cursor=!!", "invalid_cursor"}, {"?cursor=bm9uc2Vuc2U", "invalid_cursor"},
	} {
		status, body := do(t, http.MethodGet, "/v1/collections"+tt.query, "", asKey(key))
		assert.Equal(t, http.StatusBadRequest, status, tt.query)
		assert.Equal(t, tt.code, errorCode(t, body), tt.query)
	}
}

func TestCollectionTaggerVersion(t *testing.T) {
	key := newKey(t).Key
	suffix := time.Now().UnixNano() % 1_000_000_000

	// left out: the version of the tagger that runs
	status, body := createCollectionWith(t, key, fmt.Sprintf(`{"name":"e2e-tv-default-%d"}`, suffix))
	require.Equal(t, http.StatusCreated, status, string(body))
	var def collectionInfo
	require.NoError(t, json.Unmarshal(body, &def))
	require.NotEmpty(t, def.TaggerVersion)
	assert.NotEmpty(t, def.ID)

	// given: kept as it is, whatever it looks like
	const custom = "e2e-other:zai-org/GLM-5.3-Flash"
	status, body = createCollectionWith(t, key, fmt.Sprintf(`{"name":"e2e-tv-custom-%d","tagger_version":%q}`, suffix, custom))
	require.Equal(t, http.StatusCreated, status, string(body))
	var pinned collectionInfo
	require.NoError(t, json.Unmarshal(body, &pinned))
	assert.Equal(t, custom, pinned.TaggerVersion)

	for name, bad := range map[string]string{
		"empty":         `{"name":"e2e-tv-bad","tagger_version":""}`,
		"too long":      `{"name":"e2e-tv-bad","tagger_version":"` + strings.Repeat("x", 129) + `"}`,
		"control":       `{"name":"e2e-tv-bad","tagger_version":"a\nb"}`,
		"not a string":  `{"name":"e2e-tv-bad","tagger_version":1}`,
		"old data_type": `{"name":"e2e-tv-bad","data_type":"txt"}`,
	} {
		status, body := do(t, http.MethodPost, "/v1/collections", bad, asKey(key))
		assert.Equal(t, http.StatusBadRequest, status, "%s: %s", name, body)
	}

	// The running tagger is not the collection's: evaluating is refused, what is known still answers.
	uploaded := uploadObject(t, key, pinned.Name, []byte("a golang job"))
	queryPath := "/v1/collections/" + pinned.Name + "/objects/query"
	status, body = do(t, http.MethodPost, queryPath, `{"tags":{"golang":true},"limit":5}`, asKey(key))
	require.Equal(t, http.StatusConflict, status, string(body))
	assert.Equal(t, "tagger_version_mismatch", errorCode(t, body))
	var e struct {
		Error struct{ Details map[string]string }
	}
	require.NoError(t, json.Unmarshal(body, &e))
	assert.Equal(t, custom, e.Error.Details["expected"])
	assert.Equal(t, def.TaggerVersion, e.Error.Details["running"])

	status, body = do(t, http.MethodPost, queryPath, `{"tags":{"golang":true},"limit":5,"evaluate":false}`, asKey(key))
	require.Equal(t, http.StatusOK, status, string(body))
	assert.JSONEq(t, `{"objects":[]}`, string(body))
	status, body = do(t, http.MethodPost, queryPath, `{"limit":5}`, asKey(key))
	require.Equal(t, http.StatusOK, status, "a query without tags needs no tagger: %s", body)
	assert.Contains(t, string(body), uploaded.ID)

	tagsPath := "/v1/collections/" + pinned.Name + "/objects/" + uploaded.ID + "/tags"
	status, body = do(t, http.MethodGet, tagsPath+"?tags=golang", "", asKey(key))
	assert.Equal(t, http.StatusConflict, status, string(body))
	status, body = do(t, http.MethodGet, tagsPath+"?tags=golang&evaluate=false", "", asKey(key))
	assert.Equal(t, http.StatusOK, status, string(body))

	// the collection made with the running version is evaluated normally
	object := uploadObject(t, key, def.Name, []byte("a golang job"))
	status, body = do(t, http.MethodGet, "/v1/collections/"+def.Name+"/objects/"+object.ID+"/tags?tags=golang", "", asKey(key))
	assert.Equal(t, http.StatusOK, status, string(body))
}

func TestObjectMetadata(t *testing.T) {
	key := newKey(t).Key
	coll := fmt.Sprintf("e2e-meta-%d", time.Now().UnixNano()%1_000_000_000)
	status, body := createCollectionWith(t, key, fmt.Sprintf(`{"name":"%s"}`, coll))
	require.Equal(t, http.StatusCreated, status, string(body))
	objects := "/v1/collections/" + coll + "/objects"

	upload := func(payload, metadata string) (int, objMeta) {
		target := objects
		if metadata != "" {
			target += "?metadata=" + url.QueryEscape(metadata)
		}
		status, body := do(t, http.MethodPost, target, payload, asKey(key))
		var o objMeta
		_ = json.Unmarshal(body, &o)
		return status, o
	}
	get := func(id string) map[string]string {
		status, body := do(t, http.MethodGet, objects+"/"+id, "", asKey(key))
		require.Equal(t, http.StatusOK, status, string(body))
		var o objMeta
		require.NoError(t, json.Unmarshal(body, &o))
		require.NotNil(t, o.Metadata, "metadata is never null")
		return o.Metadata
	}

	// upload with metadata; without it the metadata is empty, not missing
	status, dog := upload("woof", `{"name":"cute-dog.png","kind":"photo"}`)
	require.Equal(t, http.StatusCreated, status)
	assert.Equal(t, map[string]string{"name": "cute-dog.png", "kind": "photo"}, dog.Metadata)
	assert.Equal(t, dog.Metadata, get(dog.ID))
	status, plain := upload("meow", "")
	require.Equal(t, http.StatusCreated, status)
	assert.Equal(t, map[string]string{}, plain.Metadata)

	// the same bytes again are the same object, and keep their own metadata
	status, again := upload("woof", `{"name":"other.png"}`)
	assert.Equal(t, http.StatusOK, status)
	assert.Equal(t, dog.ID, again.ID)
	assert.Equal(t, "cute-dog.png", again.Metadata["name"])

	// PUT replaces everything
	status, body = do(t, http.MethodPut, objects+"/"+dog.ID+"/metadata", `{"name":"dog.png","photographer":"Sam"}`, asKey(key))
	require.Equal(t, http.StatusOK, status, string(body))
	assert.Equal(t, map[string]string{"name": "dog.png", "photographer": "Sam"}, get(dog.ID))

	// PATCH sets some keys and removes the null ones
	status, body = do(t, http.MethodPatch, objects+"/"+dog.ID+"/metadata", `{"photographer":null,"mood":"happy"}`, asKey(key))
	require.Equal(t, http.StatusOK, status, string(body))
	var patched objMeta
	require.NoError(t, json.Unmarshal(body, &patched))
	assert.Equal(t, map[string]string{"name": "dog.png", "mood": "happy"}, patched.Metadata)
	assert.Equal(t, patched.Metadata, get(dog.ID))

	status, body = do(t, http.MethodPut, objects+"/"+dog.ID+"/metadata", `{}`, asKey(key))
	require.Equal(t, http.StatusOK, status, string(body))
	assert.Equal(t, map[string]string{}, get(dog.ID))

	// queries return it too
	status, body = do(t, http.MethodPost, objects+"/query", `{"limit":10}`, asKey(key))
	require.Equal(t, http.StatusOK, status, string(body))
	var q tagQueryResp
	require.NoError(t, json.Unmarshal(body, &q))
	require.Len(t, q.Objects, 2)
	for _, o := range q.Objects {
		assert.NotNil(t, o.Metadata)
	}

	// limits and shapes
	many := map[string]string{}
	for i := 0; i < 33; i++ {
		many[fmt.Sprintf("k%d", i)] = "v"
	}
	manyJSON, _ := json.Marshal(many)
	bad := map[string]string{
		"too many entries": string(manyJSON),
		"long value":       `{"a":"` + strings.Repeat("x", 1025) + `"}`,
		"long key":         `{"` + strings.Repeat("k", 65) + `":"v"}`,
		"empty key":        `{"":"v"}`,
	}
	for name, metadata := range bad {
		status, body := do(t, http.MethodPut, objects+"/"+dog.ID+"/metadata", metadata, asKey(key))
		assert.Equal(t, http.StatusBadRequest, status, "PUT %s: %s", name, body)
		assert.Equal(t, "invalid_metadata", errorCode(t, body), name)
		status, _ = upload("fresh "+name, metadata)
		assert.Equal(t, http.StatusBadRequest, status, "upload %s", name)
	}
	for name, metadata := range map[string]string{"not an object": `["a"]`, "a number": `{"a":1}`, "null": `null`, "null value in a PUT": `{"a":null}`} {
		status, body := do(t, http.MethodPut, objects+"/"+dog.ID+"/metadata", metadata, asKey(key))
		assert.Equal(t, http.StatusBadRequest, status, "PUT %s: %s", name, body)
		assert.Equal(t, "invalid_json", errorCode(t, body), name)
	}
	// a merge that would exceed the limit changes nothing
	full := map[string]string{}
	for i := 0; i < 32; i++ {
		full[fmt.Sprintf("k%d", i)] = "v"
	}
	fullJSON, _ := json.Marshal(full)
	status, _ = do(t, http.MethodPut, objects+"/"+dog.ID+"/metadata", string(fullJSON), asKey(key))
	require.Equal(t, http.StatusOK, status)
	status, body = do(t, http.MethodPatch, objects+"/"+dog.ID+"/metadata", `{"one-too-many":"v"}`, asKey(key))
	assert.Equal(t, http.StatusBadRequest, status, string(body))
	assert.Len(t, get(dog.ID), 32)

	// unknown objects and other collections' objects
	status, _ = do(t, http.MethodPut, objects+"/00000000-0000-4000-8000-000000000000/metadata", `{}`, asKey(key))
	assert.Equal(t, http.StatusNotFound, status)
	status, _ = do(t, http.MethodPut, "/v1/collections/e2e-no-such-collection/objects/"+dog.ID+"/metadata", `{}`, asKey(key))
	assert.Equal(t, http.StatusNotFound, status)
	status, _ = do(t, http.MethodPut, objects+"/"+dog.ID+"/metadata", `{}`, nil)
	assert.Equal(t, http.StatusUnauthorized, status, "metadata needs an api key")
}
