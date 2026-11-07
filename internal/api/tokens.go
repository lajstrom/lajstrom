package api

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/lajstrom/lajstrom/internal/auth"
	"github.com/lajstrom/lajstrom/internal/format"
	"github.com/lajstrom/lajstrom/internal/index"
)

// TokenList is the answer of GET /api/v1/users/{name}/tokens and of GET
// /api/v1/repos/{format}/{repo}/deploy-tokens: the tokens in the order of their creation, the
// revoked and expired ones included, never a secret.
type TokenList struct {
	Tokens []auth.Token `json:"tokens"`
}

// TokenRequest is the body of POST /api/v1/users/{name}/tokens and of POST
// /api/v1/repos/{format}/{repo}/deploy-tokens. ExpiresAt is RFC 3339; ExpiresIn a number of
// days (30d) or a duration (12h); one of them at most.
type TokenRequest struct {
	Name      string   `json:"name"`
	Scopes    []string `json:"scopes"`
	ExpiresAt *string  `json:"expiresAt"`
	ExpiresIn *string  `json:"expiresIn"`
}

// expiry returns the auth.Expiry of the request, or an error naming the field that does not
// parse; the service checks the rest.
func (req *TokenRequest) expiry() (auth.Expiry, error) {
	var exp auth.Expiry
	if req.ExpiresAt != nil {
		at, err := time.Parse(time.RFC3339, *req.ExpiresAt)
		if err != nil {
			return exp, invalid("expiresAt: %q is not an RFC 3339 time, like 2027-01-31T00:00:00Z", *req.ExpiresAt)
		}
		exp.At = at
	}
	if req.ExpiresIn != nil {
		d, err := auth.ParseLifetime(*req.ExpiresIn)
		if err != nil {
			return exp, invalid("expiresIn: %s", err)
		}
		exp.In = d
	}
	return exp, nil
}

// allowTokens decides whether the request may list, create and revoke the personal access
// tokens of the user called name: the user itself, or the system admin (manage on */*). Until
// RBAC (#45) sets the Authorizer, every principal may, as on every other endpoint. Either way
// the credential limits first (format.Authorize): a deploy token and a personal access token
// with scopes never manage tokens, 403 (docs/acceptance.md X-14).
func (h *handler) allowTokens(r *http.Request, name string) error {
	if p := auth.PrincipalFrom(r.Context()); p.Kind == auth.KindUser && p.Name == name {
		return h.limit(r, auth.ActionManage, systemMask, systemMask)
	}
	return h.allow(r, auth.ActionManage, systemMask, systemMask)
}

// listTokens answers the personal access tokens of a user.
func (h *handler) listTokens(_ http.ResponseWriter, r *http.Request) (int, any, error) {
	name := r.PathValue("name")
	if err := h.allowTokens(r, name); err != nil {
		return 0, nil, err
	}
	list, err := h.tokens.List(r.Context(), name)
	if err != nil {
		return 0, nil, serviceError(err)
	}
	return http.StatusOK, TokenList{Tokens: list}, nil
}

// createToken creates a personal access token of a user from the TokenRequest of the body: 201
// with the token, its secret (the one time it is shown) and its Location.
func (h *handler) createToken(w http.ResponseWriter, r *http.Request) (int, any, error) {
	name := r.PathValue("name")
	if err := h.allowTokens(r, name); err != nil {
		return 0, nil, err
	}
	req, exp, err := tokenRequest(w, r)
	if err != nil {
		return 0, nil, err
	}
	t, secret, err := h.tokens.CreatePAT(r.Context(), name, req.Name, req.Scopes, exp, auth.Origin(origin(r)))
	if err != nil {
		return 0, nil, serviceError(err)
	}
	w.Header().Set("Location", fmt.Sprintf("/api/v1/users/%s/tokens/%d", name, t.ID))
	return http.StatusCreated, auth.CreatedToken{Token: *t, Secret: secret}, nil
}

// revokeToken revokes a personal access token of a user: 204, also for a token revoked
// already; 404 for an id that is not a token of that user.
func (h *handler) revokeToken(_ http.ResponseWriter, r *http.Request) (int, any, error) {
	name := r.PathValue("name")
	if err := h.allowTokens(r, name); err != nil {
		return 0, nil, err
	}
	return h.revoke(r, func(t *auth.Token) bool { return t.Kind == auth.TokenPAT && t.User == name }, "user "+name)
}

// listDeployTokens answers the deploy tokens of a repo.
func (h *handler) listDeployTokens(_ http.ResponseWriter, r *http.Request) (int, any, error) {
	repo, err := h.allowDeployTokens(r)
	if err != nil {
		return 0, nil, err
	}
	list, err := h.tokens.ListDeploy(r.Context(), repo)
	if err != nil {
		return 0, nil, serviceError(err)
	}
	return http.StatusOK, TokenList{Tokens: list}, nil
}

// createDeployToken creates a deploy token of a repo from the TokenRequest of the body, whose
// scopes are required: 201 with the token, its secret and its Location.
func (h *handler) createDeployToken(w http.ResponseWriter, r *http.Request) (int, any, error) {
	repo, err := h.allowDeployTokens(r)
	if err != nil {
		return 0, nil, err
	}
	req, exp, err := tokenRequest(w, r)
	if err != nil {
		return 0, nil, err
	}
	t, secret, err := h.tokens.CreateDeploy(r.Context(), repo, req.Name, req.Scopes, exp, auth.Origin(origin(r)))
	if err != nil {
		return 0, nil, serviceError(err)
	}
	w.Header().Set("Location", fmt.Sprintf("/api/v1/repos/%s/deploy-tokens/%d", repo, t.ID))
	return http.StatusCreated, auth.CreatedToken{Token: *t, Secret: secret}, nil
}

// revokeDeployToken revokes a deploy token of a repo: 204, also for a token revoked already;
// 404 for an id that is not a deploy token of that repo.
func (h *handler) revokeDeployToken(_ http.ResponseWriter, r *http.Request) (int, any, error) {
	repo, err := h.allowDeployTokens(r)
	if err != nil {
		return 0, nil, err
	}
	return h.revoke(r, func(t *auth.Token) bool { return t.Kind == auth.TokenDeploy && t.Repo == repo }, "repo "+repo)
}

// allowDeployTokens decides whether the request may list, create and revoke the deploy tokens
// of the repo of its path, <format>/<repo>, which it returns: manage on the repo. A deploy
// token on its own repo, and a personal access token with scopes, get 403 (never manage); a
// deploy token on another repo 404.
func (h *handler) allowDeployTokens(r *http.Request) (string, error) {
	f, name := r.PathValue("format"), r.PathValue("repo")
	if err := h.allow(r, auth.ActionManage, f, name); err != nil {
		return "", err
	}
	return f + "/" + name, nil
}

// revoke revokes the token of the path's {id} when owned reports that it belongs to owner:
// 204; any other id is 404, so that the tokens of others are not told apart from none.
func (h *handler) revoke(r *http.Request, owned func(*auth.Token) bool, owner string) (int, any, error) {
	raw := r.PathValue("id")
	notFound := &clientError{msg: fmt.Sprintf("token %s of %s does not exist", raw, owner), err: index.ErrNotFound}
	id, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || id <= 0 {
		return 0, nil, notFound
	}
	t, err := h.tokens.Get(r.Context(), id)
	switch {
	case errors.Is(err, index.ErrNotFound):
		return 0, nil, notFound
	case err != nil:
		return 0, nil, err
	case !owned(t):
		return 0, nil, notFound
	}
	if err := h.tokens.Revoke(r.Context(), id, auth.Origin(origin(r))); err != nil {
		return 0, nil, serviceError(err)
	}
	return http.StatusNoContent, nil, nil
}

// tokenRequest decodes the TokenRequest of the body and its expiry.
func tokenRequest(w http.ResponseWriter, r *http.Request) (TokenRequest, auth.Expiry, error) {
	var req TokenRequest
	if err := decodeBody(w, r, &req); err != nil {
		return req, auth.Expiry{}, err
	}
	exp, err := req.expiry()
	return req, exp, err
}

// serviceError turns an error of auth.Tokens into the one the API answers: auth.ErrInvalid is
// 400 with the service's message, which names the field; index.ErrNotFound and
// index.ErrConflict keep their message and status; anything else is a 500.
func serviceError(err error) error {
	if errors.Is(err, auth.ErrInvalid) {
		return &clientError{msg: err.Error(), err: format.ErrInvalid}
	}
	return err
}
