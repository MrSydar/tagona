package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
	"go.yaml.in/yaml/v3"
)

// Swagger UI is loaded from a CDN at a pinned version with Subresource
// Integrity hashes, so the browser rejects a file that differs from the one
// reviewed. To upgrade, change the version and recompute both hashes:
//
//	curl -s https://cdn.jsdelivr.net/npm/swagger-ui-dist@<v>/<file> | openssl dgst -sha384 -binary | openssl base64 -A
const (
	swaggerUIVersion  = "5.33.1"
	swaggerUICSSHash  = "sha384-Ov4/wv3j2bmct8cDc5X4ngJZohVPzEmc6uDPH8WeljUxO5vtoykvMEfbu9Vh6RaW"
	swaggerUIBundleSH = "sha384-ZPehFMQommnnuaZ4rpxgkgTT2DKFVp4hZC/7pLit+9Lek9T1YGSo23eHFbvNkXkw"
)

// docsPage is the interactive documentation. The spec URL is relative so the
// page keeps working behind any path prefix.
var docsPage = fmt.Sprintf(`<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>Tagona API v1</title>
<link rel="stylesheet" href="https://cdn.jsdelivr.net/npm/swagger-ui-dist@%[1]s/swagger-ui.css" integrity="%[2]s" crossorigin="anonymous">
</head>
<body>
<div id="swagger-ui"></div>
<script src="https://cdn.jsdelivr.net/npm/swagger-ui-dist@%[1]s/swagger-ui-bundle.js" integrity="%[3]s" crossorigin="anonymous"></script>
<script>
window.onload = function () {
  window.ui = SwaggerUIBundle({ url: "openapi.json", dom_id: "#swagger-ui", deepLinking: true });
};
</script>
</body>
</html>
`, swaggerUIVersion, swaggerUICSSHash, swaggerUIBundleSH)

// docsHandlers serves the OpenAPI document (YAML and JSON) and the interactive
// documentation. They are public: documentation carries no data, and a client
// has to be able to read the contract before it has a key.
type docsHandlers struct {
	yamlSpec []byte
	jsonSpec []byte
	yamlTag  string
	jsonTag  string
	pageTag  string
}

// newDocsHandlers prepares the handlers from the embedded YAML document,
// converting it to JSON once.
func newDocsHandlers(spec []byte) (*docsHandlers, error) {
	var doc any
	if err := yaml.Unmarshal(spec, &doc); err != nil {
		return nil, fmt.Errorf("parse openapi yaml: %w", err)
	}
	jsonSpec, err := json.Marshal(doc)
	if err != nil {
		return nil, fmt.Errorf("convert openapi to json: %w", err)
	}
	return &docsHandlers{
		yamlSpec: spec,
		jsonSpec: jsonSpec,
		yamlTag:  etag(spec),
		jsonTag:  etag(jsonSpec),
		pageTag:  etag([]byte(docsPage)),
	}, nil
}

func etag(b []byte) string {
	sum := sha256.Sum256(b)
	return `"` + hex.EncodeToString(sum[:8]) + `"`
}

// register mounts the documentation under /v1.
func (d *docsHandlers) register(r chi.Router) {
	page := []byte(docsPage)
	routes := []struct {
		path string
		h    http.HandlerFunc
	}{
		{"/v1/openapi.json", d.serve("application/json", &d.jsonSpec, &d.jsonTag)},
		{"/v1/openapi.yaml", d.serve("application/yaml", &d.yamlSpec, &d.yamlTag)},
		{"/v1/docs", d.serve("text/html; charset=utf-8", &page, &d.pageTag)},
	}
	for _, route := range routes {
		r.Get(route.path, route.h)
		r.Head(route.path, route.h) // link checkers and tooling probe with HEAD
	}
}

func (d *docsHandlers) serve(contentType string, body *[]byte, tag *string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", contentType)
		w.Header().Set("ETag", *tag)
		w.Header().Set("Cache-Control", "no-cache") // always revalidate; the ETag makes that cheap
		w.Header().Set("X-Content-Type-Options", "nosniff")
		if inm := r.Header.Get("If-None-Match"); inm != "" && strings.Contains(inm, *tag) {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Write(*body)
	}
}
