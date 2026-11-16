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

// idleFixture is a tokenFixture with auth.login.idleTimeout 15m and alice's tokens bound to a key
// at the edges of the bound, by name: used-idle, used once more than the bound ago (id 50);
// used-at-bound, used the bound ago (51); new-idle, never used and made more than the bound ago
// (52); new-fresh, made a minute ago (53); revoked-idle and expired-idle, idle but revoked or
// expired (54, 55); and plain-idle, bound to no key and unused for an hour (56).
func idleFixture(t *testing.T) *tokenFixture {
	t.Helper()
	f := newTokenFixture(t, Options{IdleTimeout: 15 * time.Minute})
	now := f.clock.Now()
	for _, tt := range []struct {
		name string
		row  index.Token
	}{
		{"used-idle", index.Token{ID: 50, DPoPJKT: boundJKT, CreatedAt: now.Add(-time.Hour), LastUsedAt: ptr(now.Add(-15*time.Minute - time.Second))}},
		{"used-at-bound", index.Token{ID: 51, DPoPJKT: boundJKT, CreatedAt: now.Add(-time.Hour), LastUsedAt: ptr(now.Add(-15 * time.Minute))}},
		{"new-idle", index.Token{ID: 52, DPoPJKT: boundJKT, CreatedAt: now.Add(-16 * time.Minute)}},
		{"new-fresh", index.Token{ID: 53, DPoPJKT: boundJKT, CreatedAt: now.Add(-time.Minute)}},
		{"revoked-idle", index.Token{ID: 54, DPoPJKT: boundJKT, CreatedAt: now.Add(-time.Hour), RevokedAt: ptr(now.Add(-time.Minute))}},
		{"expired-idle", index.Token{ID: 55, DPoPJKT: boundJKT, CreatedAt: now.Add(-time.Hour), ExpiresAt: ptr(now.Add(-time.Second))}},
		{"plain-idle", index.Token{ID: 56, CreatedAt: now.Add(-time.Hour), LastUsedAt: ptr(now.Add(-time.Hour))}},
	} {
		row := tt.row
		row.Kind, row.UserID = TokenPAT, ptr(int64(1))
		if row.ExpiresAt == nil {
			row.ExpiresAt = ptr(now.Add(time.Hour))
		}
		f.secrets[tt.name] = f.users.addToken(PATPrefix, row)
	}
	return f
}

// TestAuthenticateIdle checks the idle bound of a web UI's sign-in (auth.login.idleTimeout,
// docs/spec/rbac.md §10, #1277): a token bound to a key whose last use, its last_used_at or else
// its created_at, is more than the bound before now is refused with an *IdleError that wraps
// ErrIdle and ErrBadCredentials, carries the token, its last use and the bound, is logged at debug
// and records no use.
func TestAuthenticateIdle(t *testing.T) {
	f := idleFixture(t)
	now := f.clock.Now()
	for _, tt := range []struct {
		name string
		id   int64
		last time.Time
	}{
		{"used-idle", 50, now.Add(-15*time.Minute - time.Second)},
		{"new-idle", 52, now.Add(-16 * time.Minute)},
	} {
		f.logs.Reset()
		_, _, err := f.a.AuthenticateDPoP(context.Background(), f.secrets[tt.name], boundJKT)
		var idle *IdleError
		if !errors.As(err, &idle) || !errors.Is(err, ErrIdle) || !errors.Is(err, ErrBadCredentials) {
			t.Fatalf("%s: %v, want an *IdleError", tt.name, err)
		}
		if *idle != (IdleError{TokenID: tt.id, LastUse: tt.last, Timeout: 15 * time.Minute}) {
			t.Errorf("%s: %+v, want token %d, last use %v, 15m", tt.name, idle, tt.id, tt.last)
		}
		if got := f.users.touches(tt.id); len(got) != 0 {
			t.Errorf("%s: its use is recorded: %v", tt.name, got)
		}
		if want := fmt.Sprintf(`"token":%d,"reason":"a sign-in idle for longer than auth.login.idleTimeout"`, tt.id); !strings.Contains(f.logs.String(), want) {
			t.Errorf("%s: log %s does not have %s", tt.name, f.logs, want)
		}
	}
	f.noSecrets(t)
}

// TestAuthenticateNotIdle checks what the idle bound leaves alone: a sign-in used the bound ago
// or made a minute ago passes; a revoked or an expired one, or one sent with another key, is
// refused for that first, no IdleError; a token bound to no key, as a bearer or in Basic
// credentials, has no idle bound; and with the bound off nothing is idle.
func TestAuthenticateNotIdle(t *testing.T) {
	ctx := context.Background()
	f := idleFixture(t)
	for _, name := range []string{"used-at-bound", "new-fresh"} {
		if _, _, err := f.a.AuthenticateDPoP(ctx, f.secrets[name], boundJKT); err != nil {
			t.Errorf("%s: %v, want it to pass", name, err)
		}
	}
	for _, tt := range []struct{ name, jkt string }{{"revoked-idle", boundJKT}, {"expired-idle", boundJKT}, {"used-idle", "another-thumbprint"}} {
		if _, _, err := f.a.AuthenticateDPoP(ctx, f.secrets[tt.name], tt.jkt); !errors.Is(err, ErrBadCredentials) || errors.Is(err, ErrIdle) {
			t.Errorf("%s with %s: %v, want ErrBadCredentials and no IdleError", tt.name, tt.jkt, err)
		}
	}
	plain := f.secrets["plain-idle"]
	if _, _, err := f.a.AuthenticateBearer(ctx, plain); err != nil {
		t.Errorf("a token bound to no key, unused for an hour, as a bearer: %v, want it to pass", err)
	}
	if _, _, err := f.a.Authenticate(ctx, "alice", plain); err != nil {
		t.Errorf("a token bound to no key, unused for an hour, in Basic credentials: %v, want it to pass", err)
	}

	off := newTokenFixture(t, Options{})
	off.secrets["idle"] = off.users.addToken(PATPrefix, index.Token{ID: 50, Kind: TokenPAT, UserID: ptr(int64(1)),
		ExpiresAt: ptr(off.clock.Now().Add(time.Hour)), DPoPJKT: boundJKT, CreatedAt: off.clock.Now().Add(-11 * time.Hour)})
	if _, _, err := off.a.AuthenticateDPoP(ctx, off.secrets["idle"], boundJKT); err != nil {
		t.Errorf("without a bound, a sign-in unused for 11 hours: %v, want it to pass", err)
	}
	f.noSecrets(t)
	off.noSecrets(t)
}

// revokeRows returns the token.revoke rows of the audit log of st.
func revokeRows(t *testing.T, st index.Store) []index.AuditEvent {
	t.Helper()
	evs, err := st.ListAudit(context.Background(), index.AuditFilter{})
	if err != nil {
		t.Fatal(err)
	}
	var out []index.AuditEvent
	for _, ev := range evs {
		if ev.Action == ActionTokenRevoke {
			out = append(out, ev)
		}
	}
	return out
}

// TestRevokeIdle checks the revocation of a sign-in refused as idle (Tokens.RevokeIdle,
// docs/spec/rbac.md §5 and §10): revoked at once, with one token.revoke row by anonymous whatever
// the context says, the subject the owner, source idle and the request's id and address, reason
// idle, lastUsedAt and idleTimeout in its details; a second revocation, as of another replica,
// writes nothing; an unknown token is index.ErrNotFound.
func TestRevokeIdle(t *testing.T) {
	f := newTokensFixture(t)
	ctx := WithCredential(context.Background(), Credential{Kind: CredPassword})
	tok, secret, err := f.tokens.CreateBoundPAT(ctx, "alice", "login", boundJKT, Expiry{In: time.Hour}, cli)
	if err != nil {
		t.Fatal(err)
	}
	f.secrets = append(f.secrets, secret)
	e := &IdleError{TokenID: tok.ID, LastUse: time.Date(2026, 11, 16, 9, 0, 0, 0, time.UTC), Timeout: 15 * time.Minute}
	o := Origin{IP: "192.0.2.7", RequestID: "req-1"}
	asAlice := WithPrincipal(context.Background(), userP("alice"))
	for range 2 {
		if err := f.tokens.RevokeIdle(asAlice, e, o); err != nil {
			t.Fatalf("RevokeIdle: %v", err)
		}
	}
	if row, err := f.st.GetTokenByHash(ctx, HashSecret(secret)); err != nil || row.RevokedAt == nil {
		t.Fatalf("the token: %+v, %v; want it revoked", row, err)
	}
	revokes := revokeRows(t, f.st)
	if len(revokes) != 1 {
		t.Fatalf("token.revoke rows: %+v, want one", revokes)
	}
	alice, err := f.st.GetUser(ctx, "alice")
	if err != nil {
		t.Fatal(err)
	}
	if ev := revokes[0]; ev.Principal != Anonymous.String() || ev.Subject != "user:"+alice.UID || ev.IP != "192.0.2.7" {
		t.Errorf("row %+v, want by anonymous, subject user:%s, ip 192.0.2.7", ev, alice.UID)
	}
	want := fmt.Sprintf(`{"id":%d,"idleTimeout":900,"kind":"pat","lastUsedAt":"2026-11-16T09:00:00Z","name":"login","reason":"idle","requestId":"req-1","source":"idle"}`, tok.ID)
	if got := string(revokes[0].Details); got != want {
		t.Errorf("details %s, want %s", got, want)
	}
	if err := f.tokens.RevokeIdle(ctx, &IdleError{TokenID: 999999, Timeout: time.Minute}, o); !errors.Is(err, index.ErrNotFound) {
		t.Errorf("RevokeIdle of an unknown token: %v, want ErrNotFound", err)
	}
	f.noSecrets(t)
}
