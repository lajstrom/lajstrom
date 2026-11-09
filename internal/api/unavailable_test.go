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

// unavailableReindexer fails every Start as a registry whose circuit breaker is open: no
// endpoint of the API waits for a registry today, so the error is handed in as the provider
// makes it.
type unavailableReindexer struct{}

func (unavailableReindexer) Start(context.Context, reindex.Request) (*index.ReindexRun, error) {
	return nil, fmt.Errorf("reindex: %w", &provider.UnavailableError{RetryAfter: 12*time.Second + 300*time.Millisecond})
}

// TestRegistryUnavailable checks that the API answers a registry whose circuit breaker is open
// with 503 UNAVAILABLE in its envelope and Retry-After, the seconds left rounded up.
func TestRegistryUnavailable(t *testing.T) {
	f := newFixture(t, Options{Reindex: unavailableReindexer{}})
	f.create(t, "raw", "assets")
	rr := f.do(http.MethodPost, "/api/v1/repos/raw/assets/reindex", "", auth.Principal{Kind: auth.KindUser, Name: "admin"})
	wantError(t, rr, http.StatusServiceUnavailable, codeUnavailable, "Service Unavailable")
	if got := rr.Header().Get("Retry-After"); got != "13" {
		t.Errorf("Retry-After %q, want 13", got)
	}
}
