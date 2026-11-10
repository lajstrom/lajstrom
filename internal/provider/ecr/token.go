package ecr

import (
	"context"
	"encoding/base64"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/ecr"
	"oras.land/oras-go/v2/registry/remote/auth"
)

const (
	// refreshMargin is how long before its expiry the authorization token is replaced: ECR
	// issues tokens for 12 hours, so the next one is fetched after 11. A refresh that fails
	// leaves the token in use until it expires.
	refreshMargin = time.Hour
	// retryRefresh is the least time between two refreshes that fail while the token still
	// works: requests in the meantime use it without asking ECR again.
	retryRefresh = time.Minute
	// minLifetime is how long a token whose expiry is already past, or this close, is used:
	// only a clock far from ECR's gives one (or moto, which dates every token 2015-01-01).
	minLifetime = time.Minute
)

// token is an authorization token of ECR: the Basic credentials of the registry's /v2 API.
type token struct {
	cred    auth.Credential
	expires time.Time
	refresh time.Time // when the next token is fetched
}

func (t token) usable(now time.Time) bool { return t.cred.Password != "" && now.Before(t.expires) }

// tokens caches the authorization token of the registry and replaces it refreshMargin before
// it expires (GetAuthorizationToken). One request fetches a token at a time: while a token
// works, the others use it; without one, they wait. It is safe for concurrent use.
type tokens struct {
	api tokenAPI
	now func() time.Time
	log *slog.Logger

	fetching sync.Mutex // held for the GetAuthorizationToken call

	mu       sync.Mutex
	cur      token
	retryAt  time.Time // after a refresh failed, the next one waits until then
	warnedAt time.Time // the expiry of the last token whose lifetime was warned about
}

// tokenAPI is the part of the ECR API that tokens uses.
type tokenAPI interface {
	GetAuthorizationToken(ctx context.Context, in *ecr.GetAuthorizationTokenInput, optFns ...func(*ecr.Options)) (*ecr.GetAuthorizationTokenOutput, error)
}

// credential is the auth.CredentialFunc of the data path: the current token as the Basic
// username and password.
func (s *tokens) credential(ctx context.Context, _ string) (auth.Credential, error) {
	t, err := s.get(ctx)
	if err != nil {
		return auth.EmptyCredential, err
	}
	return t.cred, nil
}

// get returns a token that works now, fetching one when there is none or the current one is
// due for its refresh. A refresh that fails returns the current token while it works.
func (s *tokens) get(ctx context.Context) (token, error) {
	if t, ok := s.cached(); ok {
		return t, nil
	}
	s.fetching.Lock()
	defer s.fetching.Unlock()
	if t, ok := s.cached(); ok { // another request fetched it while this one waited
		return t, nil
	}
	t, err := s.fetch(ctx)
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	if err != nil {
		if s.cur.usable(now) {
			s.retryAt = now.Add(retryRefresh)
			s.log.WarnContext(ctx, "ECR authorization token: refresh failed; the current token is used until it expires",
				"expires", s.cur.expires, "error", err.Error())
			return s.cur, nil
		}
		return token{}, err
	}
	s.cur, s.retryAt = t, time.Time{}
	return t, nil
}

// cached returns the current token when no fetch is needed: it is not due, or it works and a
// refresh failed less than retryRefresh ago.
func (s *tokens) cached() (token, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	t := s.cur
	if t.usable(now) && (now.Before(t.refresh) || now.Before(s.retryAt)) {
		return t, true
	}
	return token{}, false
}

// fetch asks ECR for a token and decides when to replace it.
func (s *tokens) fetch(ctx context.Context) (token, error) {
	out, err := s.api.GetAuthorizationToken(ctx, &ecr.GetAuthorizationTokenInput{})
	if err != nil {
		return token{}, translate("get authorization token", err)
	}
	if len(out.AuthorizationData) == 0 {
		return token{}, errors.New("ecr: get authorization token: the answer has no token")
	}
	d := out.AuthorizationData[0]
	cred, err := decodeToken(d.AuthorizationToken)
	if err != nil {
		return token{}, err
	}
	now := s.now()
	t := token{cred: cred, expires: now.Add(minLifetime)}
	if d.ExpiresAt != nil && d.ExpiresAt.After(t.expires) {
		t.expires = *d.ExpiresAt
	} else {
		s.warnLifetime(ctx, d.ExpiresAt)
	}
	// The margin, or half the lifetime of a token that lives less than two margins.
	t.refresh = t.expires.Add(-min(refreshMargin, t.expires.Sub(now)/2))
	return t, nil
}

// warnLifetime logs once per expiry that a token was issued with an expiry in the past.
func (s *tokens) warnLifetime(ctx context.Context, expires *time.Time) {
	var at time.Time
	if expires != nil {
		at = *expires
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.warnedAt.IsZero() && s.warnedAt.Equal(at) {
		return
	}
	s.warnedAt = at
	s.log.WarnContext(ctx, "ECR authorization token expires before it can be used; it is used for at most a minute at a time (check the clock)",
		"expires", at, "lifetime", minLifetime)
}

// decodeToken splits an authorization token, base64 of "<user>:<password>", into its parts.
// The error never shows the token.
func decodeToken(s *string) (auth.Credential, error) {
	if s == nil {
		return auth.EmptyCredential, errors.New("ecr: get authorization token: the answer has no token")
	}
	raw, err := base64.StdEncoding.DecodeString(*s)
	user, pass, ok := strings.Cut(string(raw), ":")
	if err != nil || !ok || user == "" || pass == "" {
		return auth.EmptyCredential, errors.New("ecr: get authorization token: the token is not base64 of <user>:<password>")
	}
	return auth.Credential{Username: user, Password: pass}, nil
}
