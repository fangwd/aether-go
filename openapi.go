// Package aether holds the API contract, embedded for serving and request
// validation.
package aether

import _ "embed"

// Spec is openapi.yaml: the source of truth for every route, request and
// response; internal/core/oapi is generated from it.
//
//go:embed openapi.yaml
var Spec []byte

//go:generate go tool oapi-codegen -config oapi-codegen.yaml openapi.yaml
//go:generate go tool sqlc generate
