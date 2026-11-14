package npm

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"testing"

	"github.com/lajstrom/lajstrom/internal/auth"
	"github.com/lajstrom/lajstrom/internal/format"
	"github.com/lajstrom/lajstrom/internal/index"
	"github.com/lajstrom/lajstrom/internal/provider"
)

// failingRepos is an index whose repo lookups fail.
type failingRepos struct{ index.Store }

func (failingRepos) GetRepo(context.Context, string, string) (*index.Repo, error) {
	return nil, errors.New("index: database is unreachable")
}

// TestRepoMiddleware: a repo that does not exist is answered as the router's repo middleware
// answers it, 401 with the Basic challenge to the anonymous principal when Deps.Auth is set and
// 404 otherwise, with its message, but as npm's JSON error, as Authorize's denial of a repo that
// exists is answered (docs/spec/rbac.md §3, #1091); the web login is 404 whatever the repo; a
// lookup that fails is 500 without its cause.
func TestRepoMiddleware(t *testing.T) {
	const challenge = `Basic realm="lajstrom"`
	tests := []struct {
		name, method, path string
		authz              auth.Authorizer
		failing            bool // the index's repo lookups fail
		anonymous          bool
		status             int
		body, challenge    string
	}{
		{name: "anonymous", method: http.MethodGet, path: "/npm/missing/lodash", authz: deny{}, anonymous: true, status: http.StatusUnauthorized,
			body: `{"error":"format: authentication required: repository npm/missing"}`, challenge: challenge},
		{name: "anonymous on the repo that exists", method: http.MethodGet, path: "/npm/internal/lodash", authz: deny{}, anonymous: true,
			status: http.StatusUnauthorized, body: `{"error":"format: authentication required: repository npm/internal"}`, challenge: challenge},
		{name: "anonymous without Auth", method: http.MethodGet, path: "/npm/missing/lodash", anonymous: true, status: http.StatusNotFound,
			body: `{"error":"index: not found: repository npm/missing"}`},
		{name: "a user", method: http.MethodPut, path: "/npm/missing/lodash", authz: deny{}, status: http.StatusNotFound,
			body: `{"error":"index: not found: repository npm/missing"}`},
		{name: "a user on the repo that exists", method: http.MethodPut, path: "/npm/internal/lodash", authz: deny{}, status: http.StatusNotFound,
			body: `{"error":"index: not found: repository npm/internal"}`},
		{name: "the web login", method: http.MethodPost, path: "/npm/missing/-/v1/login", authz: deny{}, anonymous: true, status: http.StatusNotFound,
			body: `{"error":"index: not found: /npm/missing/-/v1/login"}`},
		{name: "a lookup that fails", method: http.MethodGet, path: "/npm/internal/lodash", authz: deny{}, failing: true,
			status: http.StatusInternalServerError, body: `{"error":"Internal Server Error"}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := newPipeline(t)
			var st index.Store = p.st
			if tt.failing {
				st = failingRepos{p.st}
			}
			deps := format.Deps{Index: st, Ingest: p.pipe, Providers: provider.Map{"local": p.fake}, Auth: tt.authz, Log: slog.New(slog.DiscardHandler)}
			mux := http.NewServeMux()
			Plugin{}.Routes(format.NewRouter(mux, Name, deps), deps)
			rr := do(mux, tt.method, tt.path, []byte("{}"), tt.anonymous)
			if rr.Code != tt.status || strings.TrimSuffix(rr.Body.String(), "\n") != tt.body {
				t.Errorf("answer = %d %s, want %d %s", rr.Code, rr.Body, tt.status, tt.body)
			}
			if got := rr.Header().Get("Content-Type"); got != "application/json" {
				t.Errorf("Content-Type = %q, want application/json", got)
			}
			if got := rr.Header().Get("WWW-Authenticate"); got != tt.challenge {
				t.Errorf("WWW-Authenticate = %q, want %q", got, tt.challenge)
			}
		})
	}
}
