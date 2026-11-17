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
	"github.com/lajstrom/lajstrom/internal/auth/dpop"
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
	// RefusalMessage is the message of the 401 of a refusal, fallback when the authenticator has
	// none of its own (*auth.Authenticator.RefusalMessage).
	RefusalMessage(err error, fallback string) string
}

// Handoffs redeems the code that an OIDC login hands a web UI (oidc.Handoffs.Redeem) with its
// verifier and the thumbprint of the request's DPoP key: the token of the login, bound to that
// key, and its secret; an error wrapping auth.ErrInvalid for a code that is unknown, used,
// expired, of another verifier or of another key, or auth.ErrBadCredentials for a user disabled
// or deleted since the login.
type Handoffs interface {
	Redeem(ctx context.Context, code, verifier, jkt string, o auth.Origin) (*auth.Token, string, error)
}

// LoginRequest is the body of POST /api/v1/login (docs/spec/http-routing.md §4): a local
// user's name and password, or the code an OIDC login handed a web UI and the PKCE verifier
// whose challenge started that login.
type LoginRequest struct {
	Username     *string `json:"username"`
	Password     *string `json:"password"`
	Code         *string `json:"code"`
	CodeVerifier *string `json:"codeVerifier"`
}

// loginBody is the message of a body that is neither form of LoginRequest.
const loginBody = `body: must be {"username", "password"} or {"code", "codeVerifier"}`

// Proofs checks the DPoP proof of a sign-in (dpop.Verifier, RFC 9449): Verify checks the proof
// of the request, Spend records it as used, on any replica.
type Proofs interface {
	Verify(r *http.Request, token string) (*dpop.Proof, error)
	Spend(ctx context.Context, p *dpop.Proof) error
}

// LoginToken is the answer of POST /api/v1/login: the token as POST …/tokens answers a
// creation, bound to the key of the request's proof (dpopJkt), and tokenType DPoP, the
// token_type of RFC 9449 §5.
type LoginToken struct {
	auth.CreatedToken
	TokenType string `json:"tokenType"`
}

// login answers POST /api/v1/login (docs/spec/rbac.md §10), the sign-in of a web UI, for a
// password (passwordLogin) or for the code of an OIDC login (codeLogin), with the DPoP proof of
// the request checked and used first (loginProof): 201 with a personal access token of the user
// without scopes, bound to the proof's key, its secret and its Location. The request takes no
// credential: the server's middleware leaves it anonymous. A body with neither form, or with
// both, is 400, and so is a missing or refused proof.
func (h *handler) login(w http.ResponseWriter, r *http.Request) (int, any, error) {
	var req LoginRequest
	if err := decodeBody(w, r, &req); err != nil {
		return 0, nil, err
	}
	password := req.Username != nil && req.Password != nil && req.Code == nil && req.CodeVerifier == nil
	code := req.Code != nil && req.CodeVerifier != nil && req.Username == nil && req.Password == nil
	if !password && !code {
		return 0, nil, invalid(loginBody)
	}
	jkt, err := h.loginProof(r)
	if err != nil {
		return 0, nil, err
	}
	if code {
		return h.codeLogin(w, r, *req.Code, *req.CodeVerifier, jkt)
	}
	return h.passwordLogin(w, r, *req.Username, *req.Password, jkt)
}

// codeLogin redeems the code of an OIDC login with its verifier and the DPoP key of the
// thumbprint jkt (Options.Handoffs): 201 with the token of the login, bound to the key, named
// oidc <time> <4 hex>; 400 for a code that does not redeem, which is every code without Handoffs
// (OIDC is off); 401 for a user disabled since the login.
func (h *handler) codeLogin(w http.ResponseWriter, r *http.Request, code, verifier, jkt string) (int, any, error) {
	if h.handoffs == nil {
		return 0, nil, invalid("code: unknown, used, expired, or of another verifier or key: sign in again")
	}
	t, secret, err := h.handoffs.Redeem(r.Context(), code, verifier, jkt, auth.Origin(origin(r)))
	switch {
	case errors.Is(err, auth.ErrBadCredentials):
		return 0, nil, &clientError{msg: err.Error(), err: format.ErrUnauthenticated}
	case err != nil:
		return 0, nil, serviceError(err)
	}
	return answerToken(w, t, secret)
}

// passwordLogin checks the password of user as Basic credentials are, with the throttles of
// failed logins, then mints a personal access token of the user without scopes, bound to the
// DPoP key of the thumbprint jkt, named login <time> <4 hex>, expiring after the login lifetime.
// A refusal is 401 whatever its reason, a throttled account or address 429 with Retry-After, a
// right password whose user must change it 403 (auth.ErrMustChange), without a token, and 501
// without Options.Passwords.
func (h *handler) passwordLogin(w http.ResponseWriter, r *http.Request, user, password, jkt string) (int, any, error) {
	if h.passwords == nil {
		return 0, nil, &clientError{msg: "this server checks no passwords", err: format.ErrUnsupported}
	}
	p, cred, err := h.passwords.AuthenticatePassword(r.Context(), user, password)
	var te *auth.ThrottledError
	switch {
	case errors.As(err, &te):
		w.Header().Set("Retry-After", strconv.Itoa(max(int(math.Ceil(te.RetryAfter.Seconds())), 1)))
		return http.StatusTooManyRequests, envelope{Error: envelopeError{Code: codeUnavailable,
			Message: "too many failed logins: try again after Retry-After seconds"}}, nil
	case errors.Is(err, auth.ErrBadCredentials): // the authenticator's message, when it has one
		return 0, nil, &clientError{msg: h.passwords.RefusalMessage(err, "the user name or the password is wrong"), err: format.ErrUnauthenticated}
	case errors.Is(err, auth.ErrMustChange): // the one 403 of a sign-in: the UI offers the change
		return 0, nil, &clientError{msg: auth.MsgMustChange, err: format.ErrForbidden}
	case err != nil:
		return 0, nil, err
	}
	ctx := auth.WithCredential(auth.WithPrincipal(r.Context(), p), cred)
	return h.loginToken(ctx, w, user, "login", h.loginLifetime, func(ctx context.Context, name string, exp auth.Expiry) (*auth.Token, string, error) {
		return h.tokens.CreateBoundPAT(ctx, user, name, jkt, exp, auth.Origin(origin(r)))
	})
}

// loginProof checks the DPoP proof of a sign-in (Options.DPoP; docs/spec/http-routing.md §4),
// without a token and so without ath, and records it as used: the thumbprint of its key, which
// the token is bound to. A missing, refused or used proof is 400 saying why, before the
// credential is checked; 501 without Options.DPoP.
func (h *handler) loginProof(r *http.Request) (string, error) {
	if h.proofs == nil {
		return "", &clientError{msg: "this server checks no DPoP proofs", err: format.ErrUnsupported}
	}
	p, err := h.proofs.Verify(r, "")
	if err == nil {
		err = h.proofs.Spend(r.Context(), p)
	}
	switch why := dpop.Why(err); {
	case why != "":
		return "", invalid("DPoP: %s", why)
	case err != nil:
		return "", err
	}
	return p.JKT, nil
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
	return answerToken(w, t, secret)
}

// answerToken answers the token of a sign-in as POST …/tokens answers a creation: 201, the token
// with its secret, and its Location.
func answerToken(w http.ResponseWriter, t *auth.Token, secret string) (int, any, error) {
	w.Header().Set("Location", fmt.Sprintf("/api/v1/users/%s/tokens/%d", t.User, t.ID))
	return http.StatusCreated, LoginToken{CreatedToken: auth.CreatedToken{Token: *t, Secret: secret}, TokenType: "DPoP"}, nil
}
