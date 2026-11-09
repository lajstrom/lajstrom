package facade

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/lajstrom/lajstrom/internal/auth"
	"github.com/lajstrom/lajstrom/internal/auth/token"
	"github.com/lajstrom/lajstrom/internal/index"
	"github.com/lajstrom/lajstrom/internal/server"
)

// The docker bearer flow (docs/spec/rbac.md §4, ADR 0010): GET /v2/token turns the principal
// of the request, from HTTP Basic or anonymous, into a JWT that grants the scopes it asked for
// and may have; every other /v2 request may carry it as a bearer, and is then decided by its
// access claims rather than by the Authorizer.

// grantKey is the context key of the access a request's bearer grants.
type grantKey struct{}

// withGrant returns a copy of ctx that carries g, the grant of the request's bearer.
func withGrant(ctx context.Context, g *token.Grant) context.Context {
	return context.WithValue(ctx, grantKey{}, g)
}

// grantFrom returns the grant of the request's bearer, and false for a request without one.
func grantFrom(ctx context.Context) (*token.Grant, bool) {
	g, ok := ctx.Value(grantKey{}).(*token.Grant)
	return g, ok
}

// tokenResponse is the answer of GET /v2/token: the token twice, as docker reads token and
// OAuth2 clients access_token, its lifetime in seconds and when it was issued.
type tokenResponse struct {
	Token       string `json:"token"`
	AccessToken string `json:"access_token"`
	ExpiresIn   int    `json:"expires_in"`
	IssuedAt    string `json:"issued_at"`
}

// maxTokenScopes is the most scopes a token request may ask for: each costs a lookup of its
// repo and a question to the Authorizer, and a request is a few kilobytes of query, so without
// a bound one anonymous request made thousands of each (#63, T-7 of the threat model). docker
// asks for one or two (a mount names its source), oras and helm for a few.
const maxTokenScopes = 100

// issueToken answers GET /v2/token?service=&scope=…: a token for the principal of the
// request, authenticated by the middleware from Basic credentials (refused ones never get
// here: the middleware answers them 401 with the Basic challenge) or anonymous. The scopes are
// parsed (token.ParseScopes; more than maxTokenScopes is 400 UNSUPPORTED), those of repos that
// do not exist left without actions (unknownScopes), and reduced to what the Authorizer allows
// the principal, every action when there is none (token.Reduce): every repository asked for is
// in the token, with no action when it does not exist or the principal may not read it, alike
// (docs/spec/rbac.md §3). The service is the configured one: a service parameter that names
// another is a client of another registry, not an error, and is ignored. No audit row is
// written: a token is issued for every docker pull and oras push, one row each would be noise,
// and the operations the token then allows are audited themselves. Only GET: the OAuth2 POST
// form is not implemented.
func (h *handler) issueToken(w http.ResponseWriter, r *http.Request) error {
	if r.Method != http.MethodGet {
		return &apiError{status: http.StatusMethodNotAllowed, code: codeUnsupported,
			msg: fmt.Sprintf("%s is not supported on /v2/token: clients get a token with GET", r.Method)}
	}
	ctx := r.Context()
	q := r.URL.Query()
	_, service := token.Realm(h.o.ExternalURL, r)
	if s := q.Get("service"); s != "" && s != service {
		h.o.Log.DebugContext(ctx, "token for another service: the token is for this one", "asked", s, "service", service)
	}
	p := auth.PrincipalFrom(ctx)
	asked := token.ParseScopes(q["scope"])
	if len(asked) > maxTokenScopes {
		return &apiError{status: http.StatusBadRequest, code: codeUnsupported,
			msg: fmt.Sprintf("a token request may name at most %d repositories, this one names %d", maxTokenScopes, len(asked))}
	}
	scopes, err := h.unknownScopes(ctx, asked)
	if err != nil {
		return err
	}
	granted, err := token.Reduce(ctx, h.o.Auth, p, scopes)
	if err != nil {
		return fmt.Errorf("facade: token: %w", err)
	}
	t, err := h.issuer.Issue(ctx, p, service, granted)
	if err != nil {
		return fmt.Errorf("facade: token: %w", err)
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, tokenResponse{Token: t.Raw, AccessToken: t.Raw,
		ExpiresIn: int(t.ExpiresAt.Sub(t.IssuedAt) / time.Second), IssuedAt: t.IssuedAt.Format(time.RFC3339)})
	return nil
}

// unknownScopes returns the scopes with no action on each repository that is not
// <format>/<repo>/<path> of a repo of the control plane; the others, and the catalog scope,
// stay as they are.
func (h *handler) unknownScopes(ctx context.Context, scopes []token.Access) ([]token.Access, error) {
	for i, s := range scopes {
		if s.Type != token.TypeRepository {
			continue
		}
		formatName, rest, _ := strings.Cut(s.Name, "/")
		repoName, _, _ := strings.Cut(rest, "/")
		_, err := h.o.Index.GetRepo(ctx, formatName, repoName)
		switch {
		case errors.Is(err, index.ErrNotFound):
			scopes[i].Actions = nil
		case err != nil:
			return nil, fmt.Errorf("facade: token: repo of %s: %w", s.Name, err)
		}
	}
	return scopes, nil
}

// bearer verifies the bearer token of r, when it has one, and returns r with the token's
// principal (auth.WithPrincipal) and grant in its context; the principal is also the access
// log's (server.HoldPrincipal). A token that does not verify is 401 with the challenge. A
// request without a bearer is returned as it is: its principal is the middleware's.
func (h *handler) bearer(r *http.Request) (*http.Request, error) {
	scheme, raw, _ := strings.Cut(r.Header.Get("Authorization"), " ")
	if !strings.EqualFold(scheme, "Bearer") {
		return r, nil
	}
	ctx := r.Context()
	_, service := token.Realm(h.o.ExternalURL, r)
	g, err := h.verifier.Verify(ctx, strings.TrimSpace(raw), service)
	switch {
	case errors.Is(err, token.ErrInvalidToken):
		return r, &apiError{status: http.StatusUnauthorized, msg: "the bearer token is not valid: get another from the realm of the challenge"}
	case err != nil:
		return r, fmt.Errorf("facade: bearer token: %w", err)
	}
	server.HoldPrincipal(ctx, g.Principal)
	return r.WithContext(withGrant(auth.WithPrincipal(ctx, g.Principal), g)), nil
}

// namedByToken decides by the access claims of g what does not depend on the action nor on
// whether the repo of the repository name exists, and so comes before the repo is looked up
// (docs/spec/rbac.md §3, §4): 401 with error="insufficient_scope" when the token does not name
// the repository, so that the client fetches a token with the scope of the challenge; when it
// names it without any action, which the token endpoint grants on a repo that does not exist
// and on one the principal may not read alike, the answer of a repo that does not exist
// (repoUnknown).
func (h *handler) namedByToken(ctx context.Context, g *token.Grant, name string) error {
	access, ok := g.Repository(name)
	switch {
	case !ok:
		return &apiError{status: http.StatusUnauthorized, insufficient: true,
			msg: fmt.Sprintf("the bearer token grants nothing on %s: get a token with its scope", name)}
	case len(access.Actions) == 0:
		return h.repoUnknown(ctx, name)
	}
	return nil
}

// allowedByToken decides by the access claims of g whether the request may do a on the
// repository name: namedByToken first; then 403 DENIED when the token names the repository
// without the action, which the token endpoint already refused (O-12).
func (h *handler) allowedByToken(ctx context.Context, g *token.Grant, a auth.Action, name string) error {
	if err := h.namedByToken(ctx, g, name); err != nil {
		return err
	}
	if want := token.ScopeAction(a); !g.Allows(name, want) {
		return &apiError{status: http.StatusForbidden, msg: fmt.Sprintf("the bearer token does not grant %s on %s", want, name)}
	}
	return nil
}

// requestScope is the scope of the challenge of a 401 to r: the repository the request
// addresses with the actions it needs, pull for GET and HEAD, pull,push for the uploads and a
// manifest PUT, delete for DELETE; a mount from another repository adds that one with pull;
// the catalog scope for /v2/_catalog; none for /v2/ and for a path that is no route.
func requestScope(r *http.Request) string {
	rest, _ := strings.CutPrefix(r.URL.Path, "/v2/")
	switch rest {
	case "", "/v2", "token":
		return ""
	case "_catalog":
		return token.CatalogScope
	}
	rt, ok := parseRoute(rest)
	if !ok || !token.ValidName(rt.name) {
		return ""
	}
	actions := token.ActionPull
	switch {
	case rt.kind == routeUploads, rt.kind == routeUpload, rt.kind == routeManifest && r.Method == http.MethodPut:
		actions = token.ActionPull + "," + token.ActionPush
	case r.Method == http.MethodDelete:
		actions = token.ActionDelete
	}
	scope := token.TypeRepository + ":" + rt.name + ":" + actions
	q := r.URL.Query()
	if from := q.Get("from"); rt.kind == routeUploads && q.Has("mount") && from != rt.name && token.ValidName(from) {
		scope += " " + token.TypeRepository + ":" + from + ":" + token.ActionPull
	}
	return scope
}
