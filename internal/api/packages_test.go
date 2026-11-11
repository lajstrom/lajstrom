package api

import (
	"net/http"
	"testing"
)

// TestRetentionEndpointsLater: the lists and the batch delete of #72 are routes that answer
// 501 naming the issue until they come.
func TestRetentionEndpointsLater(t *testing.T) {
	f := newFixture(t, Options{})
	for _, tt := range []struct{ method, target string }{
		{http.MethodGet, "/api/v1/repos/raw/files/packages"},
		{http.MethodGet, "/api/v1/repos/raw/files/packages/a.txt/versions"},
		{http.MethodPost, "/api/v1/repos/raw/files/versions:delete"},
	} {
		wantError(t, f.do(tt.method, tt.target, "", admin), http.StatusNotImplemented, codeUnavailable, "comes with #72")
	}
}
