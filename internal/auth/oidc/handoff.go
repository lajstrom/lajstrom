package oidc

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/lajstrom/lajstrom/internal/auth"
	"github.com/lajstrom/lajstrom/internal/auth/dpop"
	"github.com/lajstrom/lajstrom/internal/index"
)

// The sign-in of a web UI through the OIDC login (docs/spec/rbac.md §10): the login takes the
// page of the UI to return to, the S256 challenge of the UI's PKCE verifier and the thumbprint
// of the UI's DPoP key, its callback hands the UI a code in the fragment of that page, and POST
// /api/v1/login redeems the code with the verifier and a proof of that key (Handoffs.Redeem).
const (
	// maxReturnTo bounds return_to, in bytes.
	maxReturnTo = 2048
	// challengeLen is the length of an S256 challenge: base64url of 32 bytes, without padding.
	challengeLen = 43
)

// uiParams are the parameters of a web UI's login: the page to return to, the S256 challenge of
// its PKCE verifier, and the JWK SHA-256 thumbprint of its DPoP key (RFC 9449 §10).
type uiParams struct {
	returnTo, challenge, jkt string
}

// uiLogin returns the parameters of a web UI's login from the query q, all "" for a login
// without them, or why the login cannot start: return_to on an origin that is not one of
// origins, a challenge that is not an S256 one, a method that is not S256, a dpop_jkt that is no
// thumbprint, one of the three without the others, any with user_code.
func uiLogin(q url.Values, origins []string) (uiParams, string) {
	if !q.Has("return_to") && !q.Has("code_challenge") && !q.Has("code_challenge_method") && !q.Has("dpop_jkt") {
		return uiParams{}, ""
	}
	ui := uiParams{returnTo: q.Get("return_to"), challenge: q.Get("code_challenge"), jkt: q.Get("dpop_jkt")}
	switch {
	case q.Has("user_code"):
		return uiParams{}, "the login of a terminal takes no return_to"
	case ui.returnTo == "" || ui.challenge == "" || ui.jkt == "":
		return uiParams{}, "return_to, code_challenge and dpop_jkt go together"
	case q.Get("code_challenge_method") != "S256":
		return uiParams{}, "the code challenge method must be S256"
	case !s256Challenge(ui.challenge):
		return uiParams{}, "the code challenge is not an S256 challenge"
	case !dpop.ValidThumbprint(ui.jkt):
		return uiParams{}, "dpop_jkt is not a JWK SHA-256 thumbprint"
	}
	if why := returnToProblem(ui.returnTo, origins); why != "" {
		return uiParams{}, why
	}
	return ui, ""
}

// returnToProblem returns why returnTo is not a page a web UI's login may return to, or "": an
// absolute http(s) URL of at most maxReturnTo bytes, without credentials or a fragment, on one of
// origins.
func returnToProblem(returnTo string, origins []string) string {
	u, err := url.Parse(returnTo)
	switch {
	case len(returnTo) > maxReturnTo:
		return fmt.Sprintf("return_to is longer than %d bytes", maxReturnTo)
	case err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || u.User != nil:
		return "return_to is not an absolute URL"
	case strings.Contains(returnTo, "#"):
		return "return_to has a fragment"
	case !slices.Contains(origins, OriginOf(u)):
		return "return_to is not on the origin of this server or of a web UI it lists"
	}
	return ""
}

// OriginOf returns the origin of u as a browser sends it in Origin: the scheme and the host in
// lower case, without the default port.
func OriginOf(u *url.URL) string {
	scheme, host := strings.ToLower(u.Scheme), strings.ToLower(u.Host)
	switch {
	case scheme == "https" && u.Port() == "443", scheme == "http" && u.Port() == "80":
		host = strings.TrimSuffix(host, ":"+u.Port())
	}
	return scheme + "://" + host
}

// s256Challenge reports whether c has the shape of an S256 challenge: 43 characters of
// base64url.
func s256Challenge(c string) bool {
	return len(c) == challengeLen && strings.Trim(c, base64URL) == ""
}

// verifierChars are the characters of a PKCE verifier (RFC 7636 §4.1), base64URL those of
// base64url.
const (
	base64URL     = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_"
	verifierChars = base64URL + ".~"
)

// validVerifier reports whether v is a PKCE verifier: 43 to 128 characters of verifierChars.
func validVerifier(v string) bool {
	return len(v) >= 43 && len(v) <= 128 && strings.Trim(v, verifierChars) == ""
}

// Handoffs redeems the codes that the callback of a web UI's login hands the UI, for POST
// /api/v1/login (docs/spec/rbac.md §10). Create it as a struct literal; Index and Tokens are
// required. Its methods may be called concurrently.
type Handoffs struct {
	Index  index.Store
	Tokens *auth.Tokens
	// TokenLifetime is the lifetime of the token: the shorter of auth.login.tokenLifetime and
	// auth.oidc.tokenLifetime.
	TokenLifetime time.Duration
	// Now is the clock; nil is time.Now.
	Now func() time.Time
	// Log gets the refusals at info level; nil is slog.Default().
	Log *slog.Logger
}

// errHandoff is the message of every code that does not redeem, for the person.
const errHandoff = "code: unknown, used, expired, or of another verifier or key: sign in again"

// Redeem redeems the code of a web UI's login with the PKCE verifier whose S256 challenge
// started it and jkt, the thumbprint of the key of the request's DPoP proof, which must be the
// login's dpop_jkt (RFC 9449 §10): the login request of the code, approved and not expired, is
// marked redeemed once, on any replica, and the token of the login is minted for its user,
// bound to that key (auth.Tokens.BoundLoginToken), with its token.create event, createdBy oidc.
// A verifier that is malformed, a code that is unknown, used or expired, a verifier of another
// challenge and a key of another thumbprint are errors wrapping auth.ErrInvalid; a wrong
// verifier or key marks the request denied, so that a code is tried once. A user disabled or
// deleted since the callback wraps auth.ErrBadCredentials. Any other error is a failure of the
// index. Nothing about the code or the verifier is logged.
func (h *Handoffs) Redeem(ctx context.Context, code, verifier, jkt string, o auth.Origin) (*auth.Token, string, error) {
	if !validVerifier(verifier) {
		return nil, "", &refusal{msg: "codeVerifier: must be 43 to 128 characters of A-Z a-z 0-9 - . _ ~", err: auth.ErrInvalid}
	}
	row, err := h.approved(ctx, code)
	if err != nil {
		return nil, "", err
	}
	verifierOK := subtle.ConstantTimeCompare([]byte(Challenge(verifier)), []byte(row.CodeChallenge)) == 1
	keyOK := row.DPoPJKT != "" && subtle.ConstantTimeCompare([]byte(jkt), []byte(row.DPoPJKT)) == 1
	if !verifierOK || !keyOK {
		row.Status = index.OIDCDenied
		if err := h.Index.UpdateOIDCLogin(ctx, row, index.OIDCApproved); err != nil && !errors.Is(err, index.ErrConflict) {
			return nil, "", fmt.Errorf("oidc: deny a code: %w", err)
		}
		if !verifierOK {
			return nil, "", h.refuse(ctx, "a code of another verifier")
		}
		return nil, "", h.refuse(ctx, "a code of another DPoP key")
	}
	row.Status = index.OIDCRedeemed
	switch err := h.Index.UpdateOIDCLogin(ctx, row, index.OIDCApproved); {
	case errors.Is(err, index.ErrConflict), errors.Is(err, index.ErrNotFound):
		return nil, "", h.refuse(ctx, "a code used already")
	case err != nil:
		return nil, "", fmt.Errorf("oidc: redeem a code: %w", err)
	}
	u, err := h.Index.GetUserByID(ctx, *row.UserID)
	switch {
	case errors.Is(err, index.ErrNotFound):
		return nil, "", &refusal{msg: "the user of the login is gone", err: auth.ErrBadCredentials}
	case err != nil:
		return nil, "", fmt.Errorf("oidc: redeem a code: its user: %w", err)
	case u.Disabled || u.DeletedAt != nil:
		return nil, "", &refusal{msg: "the user " + u.Name + " is disabled", err: auth.ErrBadCredentials}
	}
	return h.Tokens.BoundLoginToken(ctx, u, h.TokenLifetime, jkt, o)
}

// approved returns the login request of code, a code login approved for a user and not
// expired; else the refusal of Redeem, or a failure of the index.
func (h *Handoffs) approved(ctx context.Context, code string) (*index.OIDCLogin, error) {
	now := time.Now
	if h.Now != nil {
		now = h.Now
	}
	row, err := h.Index.GetOIDCLoginByHandoff(ctx, Hash(code))
	switch {
	case code == "" || errors.Is(err, index.ErrNotFound):
		return nil, h.refuse(ctx, "an unknown code")
	case err != nil:
		return nil, fmt.Errorf("oidc: redeem a code: %w", err)
	case row.Kind != index.OIDCLoginCode || row.Status != index.OIDCApproved || row.UserID == nil:
		return nil, h.refuse(ctx, "a code used already")
	case !now().Before(row.ExpiresAt):
		return nil, h.refuse(ctx, "an expired code")
	}
	return row, nil
}

// refuse logs why a code does not redeem at info and returns the error for the person.
func (h *Handoffs) refuse(ctx context.Context, reason string) error {
	log := h.Log
	if log == nil {
		log = slog.Default()
	}
	log.InfoContext(ctx, "oidc: a web UI's code refused", "reason", reason)
	return &refusal{msg: errHandoff, err: auth.ErrInvalid}
}

// refusal is an error of Redeem whose message is for the person; it wraps auth.ErrInvalid or
// auth.ErrBadCredentials.
type refusal struct {
	msg string
	err error
}

func (e *refusal) Error() string { return e.msg }
func (e *refusal) Unwrap() error { return e.err }
