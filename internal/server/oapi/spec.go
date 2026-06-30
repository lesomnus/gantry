// Package oapi holds the generated OpenAPI 3.1 document, embedded so the server
// can serve the schema without any swag runtime dependency. Regenerate with
// `go generate ./...`.
package oapi

import _ "embed"

//go:embed swagger.json
var JSON []byte

//go:embed swagger.yaml
var YAML []byte
