package api

import (
	"context"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/lajstrom/lajstrom/internal/auth"
	"github.com/lajstrom/lajstrom/internal/index"
	"github.com/lajstrom/lajstrom/internal/index/reindex"
	"github.com/lajstrom/lajstrom/internal/provider"
)

// failingReindexer fails every Start with err: no endpoint of the API waits for a registry
// today, so the error is handed in as the provider or the store makes it.
type failingReindexer struct{ err error }

func (f failingReindexer) Start(context.Context, reindex.Request) (*index.ReindexRun, error) {
	return nil, fmt.Errorf("reindex: %w", f.err)
}

// TestUnavailable checks that the API answers a registry whose circuit breaker is open, and a
// database that does not answer, with 503 UNAVAILABLE in its envelope and Retry-After: the
// seconds left rounded up, and 5 for the database.
func TestUnavailable(t *testing.T) {
	for _, tt := range []struct {
		name       string
		err        error
		retryAfter string
	}{
		{"registry", &provider.UnavailableError{RetryAfter: 12*time.Second + 300*time.Millisecond}, "13"},
		{"database", fmt.Errorf("%w: reindex_runs: failed to connect: connection refused", index.ErrUnavailable), "5"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			f := newFixture(t, Options{Reindex: failingReindexer{tt.err}})
			f.create(t, "raw", "assets")
			rr := f.do(http.MethodPost, "/api/v1/repos/raw/assets/reindex", "", auth.Principal{Kind: auth.KindUser, Name: "admin"})
			wantError(t, rr, http.StatusServiceUnavailable, codeUnavailable, "Service Unavailable")
			if got := rr.Header().Get("Retry-After"); got != tt.retryAfter {
				t.Errorf("Retry-After %q, want %s", got, tt.retryAfter)
			}
		})
	}
}
