package oidc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
)

// The refresh of a login (docs/spec/rbac.md §8, Asking the IdP again, #1282; ADR 0037): serve
// redeems each login's refresh token again, every auth.oidc.refresh.interval, and ends the login
// when the IdP refuses it. Only the IdP's invalid_grant is a refusal; anything else ends nothing.

// The kinds of a RefreshError, for the log and the metric of the refreshes.
const (
	// RefreshUnavailable: the IdP did not answer, in time or at all, or answered 429 or 5xx, or
	// its discovery document or its keys did not load.
	RefreshUnavailable = "unavailable"
	// RefreshRejected: the IdP answered an OAuth error that is not invalid_grant, such as
	// invalid_client, or another status than 200, 400 and 401 without one.
	RefreshRejected = "rejected"
	// RefreshMalformed: an answer of 200 that is not the JSON of a token answer.
	RefreshMalformed = "malformed"
	// RefreshIDToken: the answer's ID token does not verify.
	RefreshIDToken = "id_token"
)

// RefreshError is the error of a refresh that ends nothing: every failure but the IdP's
// invalid_grant. Kind says which; the message names the endpoint, the status and an OAuth error
// code, never the IdP's description nor the token. It matches ErrUnavailable for the kind
// RefreshUnavailable, and never ErrRefused.
type RefreshError struct {
	Kind string
	msg  string
}

func (e *RefreshError) Error() string { return "oidc: refresh: " + e.msg }

// Unwrap returns ErrUnavailable for the kind RefreshUnavailable, nil otherwise.
func (e *RefreshError) Unwrap() error {
	if e.Kind == RefreshUnavailable {
		return ErrUnavailable
	}
	return nil
}

// refreshFailed is a RefreshError of kind with the message of format and args.
func refreshFailed(kind, format string, args ...any) error {
	return &RefreshError{Kind: kind, msg: fmt.Sprintf(format, args...)}
}

// Refresh posts grant_type=refresh_token with the refresh token to the token endpoint, with the
// client's credentials and no scope (RFC 6749 §6), and returns the answer: its new refresh token,
// if any, and its ID token verified as at a login but for the nonce (OpenID Connect Core §12.2).
func (c *Client) Refresh(ctx context.Context, refreshToken string) (*Refreshed, error) {
	m, err := c.disc.get(ctx)
	if err != nil {
		return nil, refreshFailed(RefreshUnavailable, "%v", err)
	}
	resp, err := c.postToken(ctx, m.TokenEndpoint, url.Values{"grant_type": {"refresh_token"}, "refresh_token": {refreshToken}})
	if err != nil {
		return nil, refreshFailed(RefreshUnavailable, "%v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	var a tokenAnswer
	decodeErr := json.NewDecoder(io.LimitReader(resp.Body, maxDocument)).Decode(&a)
	switch {
	case (resp.StatusCode == http.StatusBadRequest || resp.StatusCode == http.StatusUnauthorized) && a.Error == "invalid_grant":
		return nil, fmt.Errorf("%w: the token endpoint answered %s, error \"invalid_grant\"", ErrRefused, resp.Status)
	case resp.StatusCode == http.StatusBadRequest || resp.StatusCode == http.StatusUnauthorized:
		return nil, refreshFailed(RefreshRejected, "the token endpoint answered %s, error %q", resp.Status, errorCode(a.Error))
	case resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= http.StatusInternalServerError:
		return nil, refreshFailed(RefreshUnavailable, "the token endpoint answered %s", resp.Status)
	case resp.StatusCode != http.StatusOK:
		return nil, refreshFailed(RefreshRejected, "the token endpoint answered %s", resp.Status)
	case decodeErr != nil:
		return nil, refreshFailed(RefreshMalformed, "the token endpoint's answer is not JSON")
	case a.Error != "":
		return nil, refreshFailed(RefreshRejected, "the token endpoint answered 200 with error %q", errorCode(a.Error))
	}
	out := &Refreshed{RefreshToken: a.RefreshToken}
	if a.IDToken == "" {
		return out, nil
	}
	if out.Identity, err = c.verifyRefreshed(ctx, a.IDToken); err != nil {
		return nil, err
	}
	return out, nil
}

// verifyRefreshed verifies the ID token raw of a refresh: as at a login (parse, identity) but for
// the nonce. A JWKS that never loaded is RefreshUnavailable, any other failure RefreshIDToken.
func (c *Client) verifyRefreshed(ctx context.Context, raw string) (*Identity, error) {
	claims, err := c.parse(ctx, raw, c.cfg.ClientID)
	if err == nil {
		var id *Identity
		if id, err = c.identity(ctx, claims, "", true); err == nil {
			return id, nil
		}
	}
	if errors.Is(err, ErrUnavailable) {
		return nil, refreshFailed(RefreshUnavailable, "the refreshed ID token: %v", err)
	}
	return nil, refreshFailed(RefreshIDToken, "the refreshed ID token does not verify: %v", err)
}
