package auth

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/lajstrom/lajstrom/internal/index"
)

// boundJKT is the thumbprint of the key of RFC 9449 Figure 9, the key alice's bound token of
// the tests is bound to.
const boundJKT = "0ZcOCORZNYy-DWpqq30jZyJGHTN0d2HglBV3uiguA4I"

// TestAuthenticateBound checks a personal access token bound to a DPoP key (docs/spec/rbac.md
// §10): AuthenticateDPoP takes it with the key's thumbprint, as alice's; Basic credentials and a
// bearer refuse it with ErrBound, which wraps ErrBadCredentials; AuthenticateDPoP refuses it with
// another key, refuses a token bound to no key, a deploy token and the refusals of a bearer, none
// of them ErrBound.
func TestAuthenticateBound(t *testing.T) {
	ctx := context.Background()
	f := newTokenFixture(t, Options{})
	hour := ptr(f.clock.Now().Add(time.Hour))
	bound := f.users.addToken(PATPrefix, index.Token{ID: 40, Kind: TokenPAT, UserID: ptr(int64(1)), ExpiresAt: hour, DPoPJKT: boundJKT})
	boundRevoked := f.users.addToken(PATPrefix, index.Token{ID: 41, Kind: TokenPAT, UserID: ptr(int64(1)), ExpiresAt: hour,
		RevokedAt: ptr(f.clock.Now().Add(-time.Second)), DPoPJKT: boundJKT})
	f.secrets["bound"], f.secrets["bound-revoked"] = bound, boundRevoked

	p, c, err := f.a.AuthenticateDPoP(ctx, bound, boundJKT)
	if err != nil || p != userP("alice") || c.Kind != CredPAT || c.TokenID != 40 || c.UserID != 1 {
		t.Fatalf("AuthenticateDPoP = %v, %+v, %v; want alice's personal access token 40", p, c, err)
	}
	for name, try := range map[string]func() error{
		"Basic":  func() error { _, _, err := f.a.Authenticate(ctx, "alice", bound); return err },
		"bearer": func() error { _, _, err := f.a.AuthenticateBearer(ctx, bound); return err },
	} {
		if err := try(); !errors.Is(err, ErrBound) || !errors.Is(err, ErrBadCredentials) {
			t.Errorf("%s: %v, want ErrBound", name, err)
		}
	}
	if !strings.Contains(f.logs.String(), `"token":40,"reason":"a token bound to a DPoP key, without its proof"`) {
		t.Errorf("log %s does not say why", f.logs)
	}
	for _, tt := range []struct{ secret, jkt, reason string }{
		{bound, "another-thumbprint", "a token bound to another DPoP key"},
		{bound, "", "a token bound to another DPoP key"},
		{f.secrets["alice"], boundJKT, "a token bound to no DPoP key, sent with a proof"},
		{boundRevoked, boundJKT, "revoked"},
		{f.secrets["deploy"], boundJKT, "malformed token"},
		{f.secrets["unknown"], boundJKT, "unknown token"},
		{f.secrets["bob"], boundJKT, "disabled"},
	} {
		f.logs.Reset()
		_, _, err := f.a.AuthenticateDPoP(ctx, tt.secret, tt.jkt)
		if !errors.Is(err, ErrBadCredentials) || errors.Is(err, ErrBound) {
			t.Errorf("AuthenticateDPoP for %q: %v, want ErrBadCredentials", tt.reason, err)
		}
		if want := fmt.Sprintf(`"reason":%q`, tt.reason); !strings.Contains(f.logs.String(), want) {
			t.Errorf("log %s does not have %s", f.logs, want)
		}
	}
	f.noSecrets(t)
}

// TestCreateBoundPAT checks a token bound to a DPoP key: dpopJkt in the row, the resource and
// the details of its token.create event; a thumbprint of the wrong shape is ErrInvalid, and the
// rule of MayCreatePAT holds.
func TestCreateBoundPAT(t *testing.T) {
	f := newTokensFixture(t)
	ctx := WithCredential(context.Background(), Credential{Kind: CredPassword})
	tok, secret, err := f.tokens.CreateBoundPAT(ctx, "alice", "login", boundJKT, Expiry{In: time.Hour}, cli)
	if err != nil {
		t.Fatalf("CreateBoundPAT: %v", err)
	}
	f.secrets = append(f.secrets, secret)
	row, err := f.st.GetTokenByHash(ctx, HashSecret(secret))
	if err != nil || row.DPoPJKT != boundJKT || tok.DPoPJKT != boundJKT || len(tok.Scopes) != 0 || tok.Kind != TokenPAT {
		t.Fatalf("row %+v, %v, token %+v; want both bound to %s, a pat without scopes", row, err, tok, boundJKT)
	}
	evs, err := f.st.ListAudit(ctx, index.AuditFilter{})
	if err != nil || len(evs) == 0 || evs[0].Action != ActionTokenCreate || !strings.Contains(string(evs[0].Details), `"dpopJkt":"`+boundJKT+`"`) {
		t.Fatalf("audit %v, %v; want token.create with dpopJkt", evs, err)
	}
	if plain := f.pat(t, "alice", "laptop", nil, Expiry{In: time.Hour}); plain.DPoPJKT != "" {
		t.Errorf("CreatePAT: %+v, want a token bound to no key", plain)
	}
	if _, _, err := f.tokens.CreateBoundPAT(ctx, "alice", "bad", "not a thumbprint", Expiry{In: time.Hour}, cli); !errors.Is(err, ErrInvalid) {
		t.Errorf("CreateBoundPAT with a malformed thumbprint: %v, want ErrInvalid", err)
	}
	asToken := WithCredential(ctx, Credential{Kind: CredPAT, TokenID: tok.ID, UserID: 1})
	if _, _, err := f.tokens.CreateBoundPAT(asToken, "alice", "again", boundJKT, Expiry{In: time.Hour}, cli); !errors.Is(err, ErrScope) {
		t.Errorf("CreateBoundPAT with a token: %v, want ErrScope", err)
	}
	f.noSecrets(t)
}
