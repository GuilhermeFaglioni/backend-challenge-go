// Package api embeds the OpenAPI document, so the binary serves exactly the contract
// the tests validate against.
package api

import _ "embed"

// OpenAPI is the content of openapi.yaml.
//
//go:embed openapi.yaml
var OpenAPI []byte
