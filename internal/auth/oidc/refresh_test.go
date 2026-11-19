package oidc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// refreshIDP is the refresh grant of the fake IdP (#1282), guarded by idp.mu: the refresh tokens
// it issued and still takes, and how it answers a refresh.
type refreshIDP struct {
	issue   bool            // the code exchange answers a refresh token
	live    map[string]bool // the refresh tokens the IdP takes
	issued  int
	rotate  bool          // a refresh answers a new token and spends the one sent, as Okta, Entra ID and Dex do
	idToken string        // the ID token of a refresh's answer; "" answers none
	status  int           // not 0: a refresh answers this status with body
	body    string        // the body of status
	delay   time.Duration // a refresh waits this long before it answers
	calls   int
}

// refreshSecret is in every refresh token of the fake IdP, so that a test finds one anywhere.
const refreshSecret = "rt-SECRET-7f3a" //nolint:gosec // a fake IdP's token, which the tests look for

// issueRefresh returns a new refresh token the IdP takes when it issues them, else "".
func (p *idp) issueRefresh() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.refresh.issue {
		return ""
	}
	return p.newRefresh()
}

// newRefresh makes a refresh token the IdP takes; p.mu is held.
func (p *idp) newRefresh() string {
	p.refresh.issued++
	tok := fmt.Sprintf("%s-%d", refreshSecret, p.refresh.issued)
	if p.refresh.live == nil {
		p.refresh.live = map[string]bool{}
	}
	p.refresh.live[tok] = true
	return tok
}

// refreshGrant answers grant_type=refresh_token: the status set, or invalid_grant for a token it
// does not take (with the token in its description, which must surface nowhere), or 200 with a
// new token when it rotates and the ID token set.
func (p *idp) refreshGrant(w http.ResponseWriter, r *http.Request) {
	rt := r.PostForm.Get("refresh_token")
	p.mu.Lock()
	rf := &p.refresh
	rf.calls++
	status, body, delay, idTok, ok := rf.status, rf.body, rf.delay, rf.idToken, rf.live[rt]
	var next string
	if ok && status == 0 && rf.rotate {
		delete(rf.live, rt)
		next = p.newRefresh()
	}
	p.mu.Unlock()
	if delay > 0 {
		select {
		case <-time.After(delay):
		case <-r.Context().Done():
			return
		}
	}
	switch {
	case status != 0:
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
		return
	case !ok:
		w.WriteHeader(http.StatusBadRequest)
		//nolint:gosec // a fake IdP's answer, which puts the token and markup where Lajstrom must never show them
		_, _ = fmt.Fprintf(w, `{"error":"invalid_grant","error_description":"the token %s is <b>spent</b>"}`, rt)
		return
	}
	answer := map[string]string{"access_token": "at2", "token_type": "Bearer"}
	if next != "" {
		answer["refresh_token"] = next
	}
	if idTok != "" {
		answer["id_token"] = idTok
	}
	_ = json.NewEncoder(w).Encode(answer)
}

// setRefresh changes the refresh grant of the IdP under its lock.
func (p *idp) setRefresh(change func(*refreshIDP)) {
	p.mu.Lock()
	defer p.mu.Unlock()
	change(&p.refresh)
}

// refreshedClaims are the claims of the ID token of a refresh: as a login's, without a nonce.
func (p *idp) refreshedClaims() jwt.MapClaims {
	c := p.claims("")
	delete(c, "nonce")
	c["groups"] = []any{"ops"}
	return c
}

// loginWithRefresh exchanges the code of a login whose IdP issues refresh tokens, and returns
// the refresh token of the exchange.
func loginWithRefresh(t *testing.T, p *idp, c *Client) string {
	t.Helper()
	rs, _, _ := keys(t)
	p.setRefresh(func(r *refreshIDP) { r.issue = true })
	p.answer(sign(t, rs, p.claims("n-1")))
	id, err := c.Exchange(context.Background(), testCode, testVerifier, "n-1")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(id.RefreshToken, refreshSecret) {
		t.Fatalf("the exchange's refresh token = %q", id.RefreshToken)
	}
	return id.RefreshToken
}

// TestExchangeRefreshToken: the exchange keeps the answer's refresh token, and "" without one.
func TestExchangeRefreshToken(t *testing.T) {
	rs, _, _ := keys(t)
	p := newIDP(t, rs)
	c := p.client(t, clockAt(testNow))
	p.answer(sign(t, rs, p.claims("n-1")))
	id, err := c.Exchange(context.Background(), testCode, testVerifier, "n-1")
	if err != nil || id.RefreshToken != "" {
		t.Fatalf("Exchange without a refresh token = %+v, %v", id, err)
	}
	loginWithRefresh(t, p, c)
}

// TestRefreshRotation: a refresh posts grant_type=refresh_token with the token and the client's
// credentials, and no scope; an IdP that rotates answers a new token and refuses the one it
// replaced with invalid_grant (its reuse detection), the new one refreshes again; the ID token of
// the answer is verified without a nonce, and read as a login's.
func TestRefreshRotation(t *testing.T) {
	rs, _, _ := keys(t)
	p := newIDP(t, rs)
	c := p.client(t, clockAt(testNow))
	first := loginWithRefresh(t, p, c)
	p.setRefresh(func(r *refreshIDP) { r.rotate, r.idToken = true, sign(t, rs, p.refreshedClaims()) })

	got, err := c.Refresh(context.Background(), first)
	if err != nil {
		t.Fatal(err)
	}
	if got.RefreshToken == "" || got.RefreshToken == first {
		t.Errorf("the rotated refresh token = %q", got.RefreshToken)
	}
	want := Identity{Issuer: p.srv.URL, Subject: "user-1", Username: "alice", Groups: []string{"ops"}}
	if got.Identity == nil || fmt.Sprint(*got.Identity) != fmt.Sprint(want) {
		t.Errorf("the refreshed identity = %+v, want %+v", got.Identity, want)
	}
	p.mu.Lock()
	form, auth := p.lastForm, p.lastAuth
	p.mu.Unlock()
	if form.Get("grant_type") != "refresh_token" || form.Get("refresh_token") != first || form.Has("scope") ||
		form.Has("client_secret") || len(form) != 2 {
		t.Errorf("the refresh's form: %v", form)
	}
	if auth != [2]string{url.QueryEscape(testClient), url.QueryEscape(testSecret)} {
		t.Errorf("the refresh's Basic credentials: %q", auth)
	}
	if _, err := c.Refresh(context.Background(), first); !errors.Is(err, ErrRefused) {
		t.Errorf("the replaced token again = %v, want ErrRefused", err)
	}
	if again, err := c.Refresh(context.Background(), got.RefreshToken); err != nil || again.RefreshToken == "" {
		t.Errorf("the rotated token = %+v, %v", again, err)
	}
}

// TestRefreshWithoutRotation: an IdP that does not rotate answers no refresh token, and the one
// sent stays good; an answer without an ID token has no identity.
func TestRefreshWithoutRotation(t *testing.T) {
	rs, _, _ := keys(t)
	p := newIDP(t, rs)
	c := p.client(t, clockAt(testNow))
	rt := loginWithRefresh(t, p, c)
	for range 2 {
		got, err := c.Refresh(context.Background(), rt)
		if err != nil || got.RefreshToken != "" || got.Identity != nil {
			t.Fatalf("Refresh = %+v, %v; want no new token and no identity", got, err)
		}
	}
	// An ID token with a nonce, which OpenID Connect Core §12.2 allows when it is the login's.
	p.setRefresh(func(r *refreshIDP) { r.idToken = sign(t, rs, p.claims("n-1")) })
	if got, err := c.Refresh(context.Background(), rt); err != nil || got.Identity == nil || got.Identity.Subject != "user-1" {
		t.Errorf("Refresh with an ID token that has a nonce = %+v, %v", got, err)
	}
}

// TestRefreshRefused: invalid_grant, and nothing else, wraps ErrRefused: a person removed at the
// IdP, whose refresh tokens it takes no more.
func TestRefreshRefused(t *testing.T) {
	rs, _, _ := keys(t)
	p := newIDP(t, rs)
	c := p.client(t, clockAt(testNow))
	rt := loginWithRefresh(t, p, c)
	p.setRefresh(func(r *refreshIDP) { clear(r.live) })
	_, err := c.Refresh(context.Background(), rt)
	var re *RefreshError
	if !errors.Is(err, ErrRefused) || errors.As(err, &re) || errors.Is(err, ErrUnavailable) {
		t.Fatalf("Refresh of a removed person = %v, want ErrRefused only", err)
	}
	if strings.Contains(err.Error(), refreshSecret) || strings.Contains(err.Error(), "spent") {
		t.Errorf("the refusal shows the token or the IdP's words: %v", err)
	}
	// invalid_grant on 401 is a refusal too.
	p.setRefresh(func(r *refreshIDP) { r.status, r.body = http.StatusUnauthorized, `{"error":"invalid_grant"}` })
	if _, err := c.Refresh(context.Background(), rt); !errors.Is(err, ErrRefused) {
		t.Errorf("invalid_grant with 401 = %v, want ErrRefused", err)
	}
}

// TestRefreshErrorEndsNothing: every other answer, or none, is a *RefreshError of its kind, never
// ErrRefused, and its message holds neither the token nor the IdP's description.
func TestRefreshErrorEndsNothing(t *testing.T) {
	rs, es, _ := keys(t)
	p := newIDP(t, rs)
	c := p.client(t, clockAt(testNow))
	rt := loginWithRefresh(t, p, c)
	bad := func(change func(jwt.MapClaims)) string {
		cl := p.refreshedClaims()
		change(cl)
		return sign(t, rs, cl)
	}
	for _, tt := range []struct {
		name   string
		status int
		body   string
		idTok  string
		kind   string
	}{
		{"5xx", http.StatusInternalServerError, "boom " + rt, "", RefreshUnavailable},
		{"503", http.StatusServiceUnavailable, "", "", RefreshUnavailable},
		{"429", http.StatusTooManyRequests, `{"error":"slow_down"}`, "", RefreshUnavailable},
		{"invalid_client", http.StatusUnauthorized, `{"error":"invalid_client","error_description":"bad <b>secret</b>"}`, "",
			RefreshRejected},
		{"invalid_request", http.StatusBadRequest, `{"error":"invalid_request"}`, "", RefreshRejected},
		{"400 without JSON", http.StatusBadRequest, "nope", "", RefreshRejected},
		{"403", http.StatusForbidden, `{"error":"invalid_grant"}`, "", RefreshRejected},
		{"200 not JSON", http.StatusOK, "<html>", "", RefreshMalformed},
		{"200 with an error", http.StatusOK, `{"error":"invalid_grant"}`, "", RefreshRejected},
		{"an ID token of another issuer", 0, "", bad(func(c jwt.MapClaims) { c["iss"] = "https://evil.example" }), RefreshIDToken},
		{"an ID token of another audience", 0, "", bad(func(c jwt.MapClaims) { c["aud"] = "other" }), RefreshIDToken},
		{"an expired ID token", 0, "", bad(func(c jwt.MapClaims) { c["exp"] = testNow.Add(-time.Hour).Unix() }), RefreshIDToken},
		{"an ID token without sub", 0, "", bad(func(c jwt.MapClaims) { delete(c, "sub") }), RefreshIDToken},
		{"an ID token of an unknown key", 0, "", sign(t, es, p.refreshedClaims()), RefreshIDToken},
		{"an ID token that is no JWT", 0, "", "abc", RefreshIDToken},
	} {
		t.Run(tt.name, func(t *testing.T) {
			p.setRefresh(func(r *refreshIDP) { r.status, r.body, r.idToken = tt.status, tt.body, tt.idTok })
			got, err := c.Refresh(context.Background(), rt)
			var re *RefreshError
			switch {
			case !errors.As(err, &re):
				t.Fatalf("Refresh = %+v, %v; want a *RefreshError", got, err)
			case errors.Is(err, ErrRefused):
				t.Errorf("Refresh = %v: a refusal, which would end the login", err)
			case re.Kind != tt.kind:
				t.Errorf("the kind = %s, want %s (%v)", re.Kind, tt.kind, err)
			case errors.Is(err, ErrUnavailable) != (tt.kind == RefreshUnavailable):
				t.Errorf("errors.Is(%v, ErrUnavailable) = %v", err, errors.Is(err, ErrUnavailable))
			}
			for _, secret := range []string{refreshSecret, "secret</b>", "boom", "<html>"} {
				if err != nil && strings.Contains(err.Error(), secret) {
					t.Errorf("the error shows %q: %v", secret, err)
				}
			}
		})
	}
	// The IdP down, and slower than the client's timeout.
	p.setRefresh(func(r *refreshIDP) { r.status, r.body, r.idToken, r.delay = 0, "", "", time.Second })
	slow := p.client(t, clockAt(testNow))
	slow.cfg.HTTPClient = &http.Client{Timeout: 50 * time.Millisecond}
	if _, err := slow.Refresh(context.Background(), rt); !errors.Is(err, ErrUnavailable) || errors.Is(err, ErrRefused) {
		t.Errorf("Refresh of a slow IdP = %v, want unavailable", err)
	}
	p.srv.Close()
	if _, err := c.Refresh(context.Background(), rt); !errors.Is(err, ErrUnavailable) || strings.Contains(err.Error(), refreshSecret) {
		t.Errorf("Refresh of an IdP down = %v, want unavailable without the token", err)
	}
}

// TestGrantTypes: the grant types of the discovery document, nil when it lists none.
func TestGrantTypes(t *testing.T) {
	rs, _, _ := keys(t)
	p := newIDP(t, rs)
	c := p.client(t, clockAt(testNow))
	got, err := c.GrantTypes(context.Background())
	if err != nil || got != nil {
		t.Errorf("GrantTypes of a document without them = %v, %v", got, err)
	}
	p.mu.Lock()
	p.grantTypes = []string{"authorization_code", "refresh_token"}
	p.mu.Unlock()
	listed := p.client(t, clockAt(testNow))
	if got, err := listed.GrantTypes(context.Background()); err != nil || !slices.Equal(got, []string{"authorization_code", "refresh_token"}) {
		t.Errorf("GrantTypes = %v, %v", got, err)
	}
	p.down.Store(true)
	down := p.client(t, clockAt(testNow))
	if _, err := down.GrantTypes(context.Background()); !errors.Is(err, ErrUnavailable) {
		t.Errorf("GrantTypes of an IdP down = %v", err)
	}
}
