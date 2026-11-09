package facade

import (
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"testing"
)

// TestTokenScopesBound: a token request names at most maxTokenScopes repositories; one more is
// 400 UNSUPPORTED before any repo is looked up, so that one request without credentials cannot
// make thousands of lookups (#63, T-7 of the threat model).
func TestTokenScopesBound(t *testing.T) {
	e := newEnv(t, envOptions{})
	target := func(n int) string {
		q := url.Values{}
		for i := range n {
			q.Add("scope", fmt.Sprintf("repository:raw/assets/p%d:pull", i))
		}
		return "/v2/token?" + q.Encode()
	}
	wantStatus(t, "at the bound", e.do(e.h, http.MethodGet, target(maxTokenScopes), nil), http.StatusOK)
	wantError(t, "over the bound", e.do(e.h, http.MethodGet, target(maxTokenScopes+1), nil), http.StatusBadRequest, codeUnsupported)
	// An index whose GetRepo fails answers 500 to any lookup: over the bound, none is made.
	h := New(Options{Index: reposDown{e.st}, Tokens: e.tokens, Log: slog.New(slog.NewTextHandler(e.logs, nil))})
	wantError(t, "over the bound, no lookup", e.do(h, http.MethodGet, target(maxTokenScopes+1), nil), http.StatusBadRequest, codeUnsupported)
}
