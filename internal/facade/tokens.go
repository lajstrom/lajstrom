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

// issueToken answers GET /v2/token?service=&scope=…: a token for the principal of the
// request, authenticated by the middleware from Basic credentials (refused ones never get
// here: the middleware answers them 401 with the Basic challenge) or anonymous. The scopes are
// parsed (token.ParseScopes), those of repos that do not exist dropped, and reduced to what
// the Authorizer allows the principal, every action when there is none (token.Reduce). The
// service is the configured one: a service parameter that names another is a client of
// another registry, not an error, and is ignored. No audit row is written: a token is issued
// for every docker pull and oras push, one row each would be noise, and the operations the
// token then allows are audited themselves. Only GET: the OAuth2 POST form is not
// implemented.
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
	scopes, err := h.existingScopes(ctx, token.ParseScopes(q["scope"]))
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

// existingScopes returns the scopes whose repository is <format>/<repo>/<path> of a repo of
// the control plane; the catalog scope stays.
func (h *handler) existingScopes(ctx context.Context, scopes []token.Access) ([]token.Access, error) {
	out := scopes[:0]
	for _, s := range scopes {
		if s.Type == token.TypeRepository {
			formatName, rest, _ := strings.Cut(s.Name, "/")
			repoName, _, _ := strings.Cut(rest, "/")
			_, err := h.o.Index.GetRepo(ctx, formatName, repoName)
			if errors.Is(err, index.ErrNotFound) {
				continue
			}
			if err != nil {
				return nil, fmt.Errorf("facade: token: repo of %s: %w", s.Name, err)
			}
		}
		out = append(out, s)
	}
	return out, nil
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

// allowedByToken decides by the access claims of g whether the request may do a on the
// repository name: 401 with error="insufficient_scope" when the token grants nothing on it, so
// that the client fetches a token with the scope of the challenge; 403 DENIED when it grants
// the repository without the action, which the token endpoint already refused (O-12).
func allowedByToken(g *token.Grant, a auth.Action, name string) error {
	if _, ok := g.Repository(name); !ok {
		return &apiError{status: http.StatusUnauthorized, insufficient: true,
			msg: fmt.Sprintf("the bearer token grants nothing on %s: get a token with its scope", name)}
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
