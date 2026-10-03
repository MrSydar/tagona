// Package openapi embeds the OpenAPI description of the public API.
package openapi

import _ "embed"

// V1 is the OpenAPI 3 document (YAML) describing the /v1 API contract.
//
//go:embed v1.yaml
var V1 []byte
