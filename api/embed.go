// Package api contém a especificação OpenAPI da API HTTP, embutida no binário.
package api

import _ "embed"

// OpenAPI é a especificação em YAML (openapi.yaml).
//
//go:embed openapi.yaml
var OpenAPI []byte
