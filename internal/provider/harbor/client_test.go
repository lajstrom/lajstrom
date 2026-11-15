package harbor

import (
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/lajstrom/lajstrom/internal/provider"
)

func TestTranslate(t *testing.T) {
	tests := []struct {
		status int
		header http.Header
		want   error
		busy   bool
	}{
		{http.StatusUnauthorized, nil, provider.ErrUnauthorized, false},
		{http.StatusForbidden, nil, provider.ErrUnauthorized, false},
		{http.StatusNotFound, nil, provider.ErrNotFound, false},
		{http.StatusConflict, nil, provider.ErrConflict, false},
		{http.StatusPreconditionFailed, nil, provider.ErrConflict, false},
		{http.StatusBadRequest, nil, provider.ErrRejected, false},
		{http.StatusTooManyRequests, http.Header{"Retry-After": {"7"}}, provider.ErrUnavailable, true},
		{http.StatusServiceUnavailable, nil, provider.ErrUnavailable, true},
		{http.StatusInternalServerError, nil, nil, false},
	}
	c := &client{registry: "harbor"}
	for _, tt := range tests {
		t.Run(http.StatusText(tt.status), func(t *testing.T) {
			resp := &http.Response{StatusCode: tt.status, Header: tt.header,
				Body: http.NoBody}
			err := c.translate("list repositories raw/", resp, "repository:list")
			if tt.want != nil && !errors.Is(err, tt.want) || tt.want == nil && (errors.Is(err, provider.ErrRejected) || err == nil) {
				t.Fatalf("translate %d: %v; want %v", tt.status, err, tt.want)
			}
			if d, ok := provider.RetryAfter(err); ok != tt.busy || tt.status == http.StatusTooManyRequests && d.Seconds() != 7 {
				t.Errorf("RetryAfter = %v, %v", d, ok)
			}
			if !strings.HasPrefix(err.Error(), "harbor: list repositories raw/: ") {
				t.Errorf("message %q", err)
			}
		})
	}
}
