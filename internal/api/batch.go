package api

import (
	"fmt"
	"net/http"

	"github.com/lajstrom/lajstrom/internal/format"
)

// deleteVersions answers POST /api/v1/repos/{format}/{repo}/versions:delete: the batch delete
// (docs/api.md, Delete versions in a batch). It comes with #72.
func (h *handler) deleteVersions(_ http.ResponseWriter, r *http.Request) (int, any, error) {
	return 0, nil, later(r, "#72")
}

// later is the 501 of an endpoint that the issue named comes with.
func later(r *http.Request, issue string) error {
	return &clientError{msg: fmt.Sprintf("%s %s comes with %s (docs/api.md)", r.Method, r.URL.Path, issue), err: format.ErrUnsupported}
}
