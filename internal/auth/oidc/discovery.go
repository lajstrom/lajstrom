package oidc

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// maxDocument bounds a document of the IdP: the discovery document, the JWKS, a token answer.
const maxDocument = 1 << 20

// discoveryMaxAge is how long a discovery document is used before it is read again; a read
// that fails keeps the last one.
const discoveryMaxAge = time.Hour

// metadata is what Lajstrom reads of the discovery document (OpenID Connect Discovery 1.0 §3).
type metadata struct {
	Issuer                string `json:"issuer"`
	AuthorizationEndpoint string `json:"authorization_endpoint"`
	TokenEndpoint         string `json:"token_endpoint"`
	JWKSURI               string `json:"jwks_uri"`
}

// discovery caches the discovery document.
type discovery struct {
	c *Client

	mu   sync.Mutex
	meta *metadata
	at   time.Time // when meta was read
}

// get returns the discovery document, read again when it is discoveryMaxAge old. A read that
// fails returns the last document when there is one, and an error wrapping ErrUnavailable when
// there is none.
func (d *discovery) get(ctx context.Context) (*metadata, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	now := d.c.cfg.Now()
	if d.meta != nil && now.Sub(d.at) < discoveryMaxAge {
		return d.meta, nil
	}
	m, err := d.read(ctx)
	if err != nil {
		if d.meta != nil {
			d.c.cfg.Log.WarnContext(ctx, "oidc: the discovery document did not load again; the last one stays", "error", err.Error())
			return d.meta, nil
		}
		return nil, err
	}
	d.meta, d.at = m, now
	d.c.cfg.Log.DebugContext(ctx, "oidc: discovery document loaded", "issuer", m.Issuer)
	return m, nil
}

// read reads and checks the discovery document: its issuer is the configured one exactly, and
// its endpoints are absolute URLs of the allowed schemes.
func (d *discovery) read(ctx context.Context) (*metadata, error) {
	var m metadata
	if err := d.c.getJSON(ctx, strings.TrimSuffix(d.c.cfg.Issuer, "/")+"/.well-known/openid-configuration", &m); err != nil {
		return nil, err
	}
	if m.Issuer != d.c.cfg.Issuer {
		return nil, fmt.Errorf("%w: the discovery document names the issuer %q, not %q", ErrUnavailable, m.Issuer, d.c.cfg.Issuer)
	}
	for _, e := range []struct{ what, u string }{
		{"authorization_endpoint", m.AuthorizationEndpoint},
		{"token_endpoint", m.TokenEndpoint},
		{"jwks_uri", m.JWKSURI},
	} {
		if err := checkEndpoint(e.u, d.c.cfg.AllowHTTP); err != nil {
			return nil, fmt.Errorf("%w: the discovery document's %s: %w", ErrUnavailable, e.what, err)
		}
	}
	return &m, nil
}

// checkEndpoint checks that s is an absolute https URL, or http when allowHTTP; it may have a
// query (an authorization endpoint may).
func checkEndpoint(s string, allowHTTP bool) error {
	u, err := url.Parse(s)
	switch {
	case err != nil || u.Host == "" || u.Fragment != "":
		return fmt.Errorf("%q is not an absolute URL", s)
	case u.Scheme == "https", u.Scheme == "http" && allowHTTP:
		return nil
	}
	return fmt.Errorf("%q is not https", s)
}

// getJSON reads the JSON document at u into v. Any failure is an error wrapping ErrUnavailable
// that names the URL, which holds no secret.
func (c *Client) getJSON(ctx context.Context, u string, v any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return fmt.Errorf("%w: %s: %w", ErrUnavailable, u, err)
	}
	req.Header.Set("Accept", "application/json")
	resp, err := c.cfg.HTTPClient.Do(req)
	if err != nil {
		return fmt.Errorf("%w: %s: %w", ErrUnavailable, u, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxDocument))
		return fmt.Errorf("%w: %s answered %s", ErrUnavailable, u, resp.Status)
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxDocument)).Decode(v); err != nil {
		return fmt.Errorf("%w: %s: not a JSON document: %w", ErrUnavailable, u, err)
	}
	return nil
}

// tokenAnswer is what Lajstrom reads of the token endpoint's answer (RFC 6749 §5.1 and §5.2;
// OpenID Connect Core §3.1.3.3): the ID token, or the error code. The access token is not read.
type tokenAnswer struct {
	IDToken string `json:"id_token"`
	Error   string `json:"error"`
}

// redeem posts the code and the verifier to the token endpoint with the client's credentials
// (client_secret_basic, RFC 6749 §2.3.1) and returns the raw ID token. A refusal (400 or 401,
// or an answer without an ID token) wraps ErrRefused with the IdP's error code, never its
// description; another failure wraps ErrUnavailable.
func (c *Client) redeem(ctx context.Context, endpoint, code, verifier string) (string, error) {
	form := url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"redirect_uri":  {c.cfg.RedirectURL},
		"code_verifier": {verifier},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return "", fmt.Errorf("%w: the token endpoint: %w", ErrUnavailable, err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	req.SetBasicAuth(url.QueryEscape(c.cfg.ClientID), url.QueryEscape(c.cfg.ClientSecret))
	resp, err := c.cfg.HTTPClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("%w: the token endpoint: %w", ErrUnavailable, err)
	}
	defer func() { _ = resp.Body.Close() }()
	var a tokenAnswer
	decodeErr := json.NewDecoder(io.LimitReader(resp.Body, maxDocument)).Decode(&a)
	switch {
	case resp.StatusCode == http.StatusBadRequest || resp.StatusCode == http.StatusUnauthorized:
		return "", fmt.Errorf("%w: the token endpoint answered %s, error %q", ErrRefused, resp.Status, errorCode(a.Error))
	case resp.StatusCode != http.StatusOK:
		return "", fmt.Errorf("%w: the token endpoint answered %s", ErrUnavailable, resp.Status)
	case decodeErr != nil:
		return "", fmt.Errorf("%w: the token endpoint's answer is not JSON: %w", ErrUnavailable, decodeErr)
	case a.IDToken == "":
		return "", fmt.Errorf("%w: the token endpoint's answer has no id_token (is the scope openid allowed?)", ErrRefused)
	}
	return a.IDToken, nil
}

// errorCode returns an OAuth error code for a log line: the code when it has the characters of
// one (RFC 6749 §5.2), else a placeholder, so that a log line never carries what an IdP put there.
func errorCode(s string) string {
	if s == "" || len(s) > 64 || strings.IndexFunc(s, func(r rune) bool { return r < 0x20 || r > 0x7e || r == '"' || r == '\\' }) >= 0 {
		return "?"
	}
	return s
}
