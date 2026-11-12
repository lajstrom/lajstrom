// Package oidc is the OpenID Connect relying party of auth.oidc (docs/spec/rbac.md §8, #69):
// discovery, the IdP's signing keys (JWKS), the authorization code exchange with PKCE, and the
// verification of the ID token. The login flows use it through Provider only, so that a library
// may implement it instead (decision D5 on #69).
//
// It is written on net/http and encoding/json; JWS verification is github.com/golang-jwt/jwt/v5
// (docs/spec/layout.md §3) over keys of crypto/rsa, crypto/ecdsa and crypto/ed25519 decoded from
// the JWKS. No secret it handles (a code, a verifier, an ID token, the client secret) is logged
// or put in an error.
package oidc

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"
)

// ErrRefused means the IdP refused the code, or the ID token or its claims did not verify. The
// error's message is for the log, never for the client.
var ErrRefused = errors.New("oidc: refused")

// ErrUnavailable means the IdP did not answer: its discovery document, its keys or its token
// endpoint could not be read.
var ErrUnavailable = errors.New("oidc: the IdP does not answer")

// Leeway is the clock skew the verification tolerates on exp, iat and nbf: the leeway of the
// bearer tokens (docs/spec/rbac.md §4).
const Leeway = 60 * time.Second

// DefaultTimeout bounds each request to the IdP when Config.HTTPClient is nil.
const DefaultTimeout = 10 * time.Second

// Provider is the relying party as the login flows use it (docs/spec/layout.md, auth/oidc).
type Provider interface {
	// AuthCodeURL is the IdP's authorization endpoint with the parameters of the code flow:
	// state, nonce and the S256 challenge of the PKCE verifier. Errors wrap ErrUnavailable when
	// the discovery document cannot be read.
	AuthCodeURL(ctx context.Context, state, nonce, challenge string) (string, error)
	// Exchange redeems code with the verifier at the token endpoint and verifies the ID token
	// against nonce. Errors wrap ErrRefused or ErrUnavailable.
	Exchange(ctx context.Context, code, verifier, nonce string) (*Identity, error)
}

// Identity is what a verified ID token says of its person.
type Identity struct {
	// Issuer and Subject are iss and sub: the identity (index-schema.md, identities).
	Issuer, Subject string
	// Username is the claim Config.UsernameClaim, a string, unchecked; "" when missing.
	Username string
	// Groups are the strings of the claim Config.GroupsClaim that are names of groups by
	// docs/spec/rbac.md §8, in the IdP's order without repeats; the others are dropped and
	// counted in a debug line.
	Groups []string
}

// Config configures a Client: the keys of auth.oidc and how to reach the IdP.
type Config struct {
	// Issuer is auth.oidc.issuer, the exact iss of the IdP's tokens; https, or http with
	// AllowHTTP.
	Issuer string
	// ClientID and ClientSecret are the client's credentials, sent with client_secret_basic.
	ClientID, ClientSecret string
	// RedirectURL is <server.externalURL>/-/oidc/callback.
	RedirectURL string
	// Scopes are auth.oidc.scopes; openid is added when missing.
	Scopes []string
	// UsernameClaim and GroupsClaim name the claims of the user's name and of its groups;
	// GroupsClaim "" reads no groups.
	UsernameClaim, GroupsClaim string
	// KeepGroup reports whether a string of the groups claim is a group's name; nil keeps every
	// string. serve sets auth.CheckGroupName.
	KeepGroup func(name string) error
	// AllowHTTP accepts an issuer and endpoints on http: server.insecureHTTP, the dev stack.
	AllowHTTP bool
	// HTTPClient sends the requests to the IdP; nil is a client with DefaultTimeout that
	// follows no redirect.
	HTTPClient *http.Client
	// Now is the clock of the verification and of the caches; nil is time.Now.
	Now func() time.Time
	// Log gets the reloads of the keys and of the discovery document at debug level; nil is
	// slog.Default().
	Log *slog.Logger
}

// Client is the relying party over net/http. Create it with New; its methods may be called
// concurrently.
type Client struct {
	cfg  Config
	disc *discovery
	keys *keySet
}

var _ Provider = (*Client)(nil)

// New checks cfg and returns a Client. It reads nothing from the IdP: the discovery document is
// read at the first login, so that serve starts while the IdP is down.
func New(cfg Config) (*Client, error) {
	if err := checkURL("issuer", cfg.Issuer, cfg.AllowHTTP); err != nil {
		return nil, err
	}
	if cfg.ClientID == "" {
		return nil, errors.New("oidc: clientID must not be empty")
	}
	if err := checkURL("redirect URL", cfg.RedirectURL, true); err != nil {
		return nil, err
	}
	if !slices.Contains(cfg.Scopes, "openid") {
		cfg.Scopes = append([]string{"openid"}, cfg.Scopes...)
	}
	if cfg.HTTPClient == nil {
		cfg.HTTPClient = &http.Client{
			Timeout:       DefaultTimeout,
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		}
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}
	c := &Client{cfg: cfg}
	c.disc = &discovery{c: c}
	c.keys = &keySet{c: c}
	return c, nil
}

// checkURL checks that s is an absolute URL without a query or fragment, on https, or on http
// when allowHTTP.
func checkURL(what, s string, allowHTTP bool) error {
	u, err := url.Parse(s)
	switch {
	case s == "":
		return fmt.Errorf("oidc: the %s must not be empty", what)
	case err != nil || u.Host == "" || u.RawQuery != "" || u.Fragment != "":
		return fmt.Errorf("oidc: the %s %q is not an absolute URL without a query", what, s)
	case u.Scheme == "https", u.Scheme == "http" && allowHTTP:
		return nil
	}
	return fmt.Errorf("oidc: the %s %q is not https (http only with server.insecureHTTP)", what, s)
}

// AuthCodeURL returns the authorization endpoint of the discovery document with the parameters
// of the code flow, its own query parameters kept.
func (c *Client) AuthCodeURL(ctx context.Context, state, nonce, challenge string) (string, error) {
	m, err := c.disc.get(ctx)
	if err != nil {
		return "", err
	}
	u, _ := url.Parse(m.AuthorizationEndpoint) // checked when the document loaded
	q := u.Query()
	q.Set("response_type", "code")
	q.Set("client_id", c.cfg.ClientID)
	q.Set("redirect_uri", c.cfg.RedirectURL)
	q.Set("scope", strings.Join(c.cfg.Scopes, " "))
	q.Set("state", state)
	q.Set("nonce", nonce)
	q.Set("code_challenge", challenge)
	q.Set("code_challenge_method", "S256")
	u.RawQuery = q.Encode()
	return u.String(), nil
}

// Exchange redeems code at the token endpoint and verifies the ID token of the answer.
func (c *Client) Exchange(ctx context.Context, code, verifier, nonce string) (*Identity, error) {
	m, err := c.disc.get(ctx)
	if err != nil {
		return nil, err
	}
	raw, err := c.redeem(ctx, m.TokenEndpoint, code, verifier)
	if err != nil {
		return nil, err
	}
	return c.verify(ctx, raw, nonce)
}
