package api

import (
	_ "embed" // the OpenAPI document
	"net/http"
)

// openAPI is the OpenAPI 3.1 document of the management API (openapi.yaml, docs/api.md).
//
//go:embed openapi.yaml
var openAPI []byte

// serveOpenAPI answers GET /api/v1/openapi.yaml with the document, to anyone: it describes the
// API, not what is in it.
func serveOpenAPI(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/yaml")
	_, _ = w.Write(openAPI)
}
