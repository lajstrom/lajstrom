package api

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"net/http"
	"strconv"
	"time"

	"github.com/lajstrom/lajstrom/internal/auth"
	"github.com/lajstrom/lajstrom/internal/format"
	"github.com/lajstrom/lajstrom/internal/index"
)

// DefaultLoginLifetime is the lifetime of the token of POST /api/v1/login when
// Options.LoginLifetime is zero: auth.login.tokenLifetime's default.
const DefaultLoginLifetime = 12 * time.Hour

// Passwords checks the password of a sign-in (auth.Authenticator.AuthenticatePassword): the
// principal and a CredPassword, ErrBadCredentials for a refusal, a *ThrottledError for a
// throttled account or address.
type Passwords interface {
	AuthenticatePassword(ctx context.Context, username, password string) (auth.Principal, auth.Credential, error)
}

// LoginRequest is the body of POST /api/v1/login (docs/spec/http-routing.md §4): a local
// user's name and password.
type LoginRequest struct {
	Username *string `json:"username"`
	Password *string `json:"password"`
}

// login answers POST /api/v1/login (docs/spec/rbac.md §10): the password of the body checked as
// Basic credentials are, with the throttles of failed logins, then a personal access token of
// the user without scopes, named login <time> <4 hex>, expiring after the login lifetime: 201
// with the token, its secret and its Location. The request takes no credential: the server's
// middleware leaves it anonymous. A refusal is 401 whatever its reason, a throttled account or
// address 429 with Retry-After, a body without the name or the password 400.
func (h *handler) login(w http.ResponseWriter, r *http.Request) (int, any, error) {
	var req LoginRequest
	if err := decodeBody(w, r, &req); err != nil {
		return 0, nil, err
	}
	if req.Username == nil || req.Password == nil {
		return 0, nil, invalid(`body: must be {"username", "password"}`)
	}
	if h.passwords == nil {
		return 0, nil, &clientError{msg: "this server checks no passwords", err: format.ErrUnsupported}
	}
	p, cred, err := h.passwords.AuthenticatePassword(r.Context(), *req.Username, *req.Password)
	var te *auth.ThrottledError
	switch {
	case errors.As(err, &te):
		w.Header().Set("Retry-After", strconv.Itoa(max(int(math.Ceil(te.RetryAfter.Seconds())), 1)))
		return http.StatusTooManyRequests, envelope{Error: envelopeError{Code: codeUnavailable,
			Message: "too many failed logins: try again after Retry-After seconds"}}, nil
	case errors.Is(err, auth.ErrBadCredentials):
		return 0, nil, &clientError{msg: "the user name or the password is wrong", err: format.ErrUnauthenticated}
	case err != nil:
		return 0, nil, err
	}
	ctx := auth.WithCredential(auth.WithPrincipal(r.Context(), p), cred)
	return h.loginToken(ctx, w, *req.Username, "login", h.loginLifetime, func(ctx context.Context, name string, exp auth.Expiry) (*auth.Token, string, error) {
		return h.tokens.CreatePAT(ctx, *req.Username, name, nil, exp, auth.Origin(origin(r)))
	})
}

// loginToken mints the token of a sign-in with create, named <label> <time> <4 hex> and
// expiring after lifetime, another 4 hex when the name is taken, and answers it as
// POST …/tokens answers a creation: 201, the token with its secret, and its Location.
func (h *handler) loginToken(ctx context.Context, w http.ResponseWriter, user, label string, lifetime time.Duration,
	create func(ctx context.Context, name string, exp auth.Expiry) (*auth.Token, string, error)) (int, any, error) {
	var (
		t      *auth.Token
		secret string
		err    error
	)
	for range 3 { // two sign-ins of the user in one second with the same 4 hex: another name
		b := make([]byte, 2)
		_, _ = rand.Read(b) // crypto/rand.Read does not return errors; it crashes the program instead
		name := label + " " + h.repos.now().UTC().Format(time.RFC3339) + " " + hex.EncodeToString(b)
		if t, secret, err = create(ctx, name, auth.Expiry{In: lifetime}); !errors.Is(err, index.ErrConflict) {
			break
		}
	}
	if err != nil {
		return 0, nil, serviceError(fmt.Errorf("sign-in of %s: %w", user, err))
	}
	w.Header().Set("Location", fmt.Sprintf("/api/v1/users/%s/tokens/%d", t.User, t.ID))
	return http.StatusCreated, auth.CreatedToken{Token: *t, Secret: secret}, nil
}
