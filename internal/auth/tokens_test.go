package auth

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/lajstrom/lajstrom/internal/index"
	"github.com/lajstrom/lajstrom/internal/index/sqlite"
)

// tokensFixture is the tokens service over a migrated in-memory SQLite index with the users
// alice (id 1) and bob (disabled) and the repos raw/scratch and raw/other, its clock at
// usersNow, logging JSON at debug.
type tokensFixture struct {
	tokens  *Tokens
	st      *sqlite.Store
	logs    *bytes.Buffer
	secrets []string // every secret made, for noSecrets
}

func newTokensFixture(t *testing.T) *tokensFixture {
	t.Helper()
	users, st, logs := newUsers(t)
	ctx := context.Background()
	for _, u := range []index.User{{Name: "alice", Kind: KindLocal}, {Name: "bob", Kind: KindLocal, Disabled: true}} {
		if err := st.CreateUser(ctx, &u); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{"scratch", "other"} {
		r := index.Repo{Format: "raw", Name: name, Registry: "local", Prefix: "raw/" + name, Kind: "hosted"}
		if err := st.CreateRepo(ctx, &r); err != nil {
			t.Fatal(err)
		}
	}
	return &tokensFixture{tokens: &Tokens{Index: st, Now: users.Now, Log: users.Log}, st: st, logs: logs}
}

// pat creates a personal access token of user and fails the test on an error.
func (f *tokensFixture) pat(t *testing.T, user, name string, scopes []string, exp Expiry) *Token {
	t.Helper()
	tok, secret, err := f.tokens.CreatePAT(context.Background(), user, name, scopes, exp, cli)
	if err != nil {
		t.Fatalf("CreatePAT(%s, %s): %v", user, name, err)
	}
	f.secrets = append(f.secrets, secret)
	return tok
}

// deploy creates a deploy token of repo and fails the test on an error.
func (f *tokensFixture) deploy(t *testing.T, repo, name string, scopes []string, exp Expiry) *Token {
	t.Helper()
	tok, secret, err := f.tokens.CreateDeploy(context.Background(), repo, name, scopes, exp, cli)
	if err != nil {
		t.Fatalf("CreateDeploy(%s, %s): %v", repo, name, err)
	}
	f.secrets = append(f.secrets, secret)
	return tok
}

// noSecrets fails the test when a secret made by the fixture, or its hash, is in the log or in
// the audit log.
func (f *tokensFixture) noSecrets(t *testing.T) {
	t.Helper()
	evs, err := f.st.ListAudit(context.Background(), index.AuditFilter{})
	if err != nil {
		t.Fatal(err)
	}
	audit, _ := json.Marshal(evs)
	text := f.logs.String() + string(audit)
	for _, s := range f.secrets {
		if strings.Contains(text, s) || strings.Contains(text, HashSecret(s)) {
			t.Error("the log or the audit log holds a secret or its hash")
		}
	}
}

// tokenAudit returns the audit events, newest first, each "<principal> <action> <repo>
// <subject> <details>".
func tokenAudit(t *testing.T, st index.Reader) []string {
	t.Helper()
	evs, err := st.ListAudit(context.Background(), index.AuditFilter{})
	if err != nil {
		t.Fatal(err)
	}
	out := make([]string, len(evs))
	for i, ev := range evs {
		out[i] = named(t, st, fmt.Sprintf("%s %s %s %s %s", ev.Principal, ev.Action, ev.Repo, ev.Subject, ev.Details))
	}
	return out
}

// wantEvents checks the audit events, newest first.
func wantEvents(t *testing.T, st index.Reader, want ...string) {
	t.Helper()
	if got := tokenAudit(t, st); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("audit events:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

// rowSummary is what the tests compare of a tokens row.
func rowSummary(r *index.Token) string {
	owner := func(id *int64) string {
		if id == nil {
			return "-"
		}
		return fmt.Sprint(*id)
	}
	stamp := func(t *time.Time) string {
		if t == nil {
			return "-"
		}
		return t.Format(time.RFC3339)
	}
	return fmt.Sprintf("%s user %s repo %s %q %v expires %s used %s revoked %s", r.Kind, owner(r.UserID), owner(r.RepoID), r.Name,
		r.Scopes, stamp(r.ExpiresAt), stamp(r.LastUsedAt), stamp(r.RevokedAt))
}

// TestTokensCreatePAT creates a personal access token with the defaults: the secret, its hash
// in the index and its prefix in the resource, no scopes, an expiry 90 days ahead, one
// token.create event without the secret or the hash.
func TestTokensCreatePAT(t *testing.T) {
	f := newTokensFixture(t)
	ctx := WithPrincipal(context.Background(), Principal{Kind: KindUser, Name: "admin"})
	tok, secret, err := f.tokens.CreatePAT(ctx, "alice", "laptop", nil, Expiry{}, Origin{Source: "api", IP: "192.0.2.7", RequestID: "req-1"})
	if err != nil {
		t.Fatal(err)
	}
	f.secrets = append(f.secrets, secret)
	if !tokenShaped(secret, PATPrefix) {
		t.Errorf("the secret %.8s… of %d characters is not a personal access token", secret, len(secret))
	}
	row, err := f.st.GetTokenByHash(ctx, HashSecret(secret))
	if err != nil {
		t.Fatalf("the index has no token with the hash of the secret: %v", err)
	}
	if got, want := rowSummary(row), `pat user 1 repo - "laptop" [] expires 2027-02-05T12:00:00Z used - revoked -`; got != want {
		t.Errorf("row = %s\nwant  %s", got, want)
	}
	b, _ := json.Marshal(tok)
	want := fmt.Sprintf(`{"id":%d,"kind":"pat","user":"alice","name":"laptop","scopes":[],"expiresAt":"2027-02-05T12:00:00Z",`+
		`"lastUsedAt":null,"createdAt":"2026-11-07T12:00:00Z","revokedAt":null,"hashPrefix":%q}`, row.ID, row.Hash[:8])
	if string(b) != want {
		t.Errorf("token = %s\nwant    %s", b, want)
	}
	var created any = CreatedToken{Token: *tok, Secret: secret} // the one answer that carries the secret
	if b, _ := json.Marshal(created); !bytes.HasSuffix(b, []byte(`,"secret":"`+secret+`"}`)) {
		t.Error("the created token does not end with its secret")
	}
	wantEvents(t, f.st, fmt.Sprintf(`user:admin token.create  user:alice {"expiresAt":"2027-02-05T12:00:00Z","id":%d,"kind":"pat","name":"laptop","requestId":"req-1","scopes":[],"source":"api"}`, row.ID))
	if evs, _ := f.st.ListAudit(ctx, index.AuditFilter{}); evs[0].IP != "192.0.2.7" {
		t.Errorf("audit IP = %q", evs[0].IP)
	}
	f.noSecrets(t)
}

// TestTokensBounds checks the rules of a new token: the expiry (the 90-day default, at most 365
// days for a personal access token, in the future, one of At and In), the scopes (a subset of
// read and write; required, and never delete or manage, for a deploy token: X-14), the name
// (printable, 1 to 64 bytes, unique among the owner's live tokens) and the owner.
func TestTokensBounds(t *testing.T) {
	f := newTokensFixture(t)
	ctx := context.Background()
	year := usersNow.Add(MaxPATLifetime)
	f.pat(t, "alice", "a year", []string{"write", "read", "read"}, Expiry{At: year})
	f.pat(t, "alice", "thirty days", []string{"read"}, Expiry{In: 30 * 24 * time.Hour})
	f.pat(t, "bob", "of a disabled user", nil, Expiry{})
	f.pat(t, "alice", strings.Repeat("é", 32), nil, Expiry{})
	f.deploy(t, "raw/scratch", "forever", []string{"read"}, Expiry{})
	f.deploy(t, "raw/scratch", "long", []string{"write", "read"}, Expiry{In: 400 * 24 * time.Hour})
	f.deploy(t, "raw/scratch", "scanner", []string{"attach", "read", "attach"}, Expiry{})
	group := index.Repo{Format: "raw", Name: "all", Prefix: "raw/all", Kind: index.KindGroup}
	if err := f.st.CreateRepo(ctx, &group); err != nil {
		t.Fatal(err)
	}
	f.deploy(t, "raw/all", "reader", []string{"read"}, Expiry{})
	for _, tt := range []struct {
		name     string
		deploy   bool
		owner    string
		token    string
		scopes   []string
		exp      Expiry
		sentinel error
		msg      string
	}{
		{"over a year", false, "alice", "x", nil, Expiry{At: year.Add(time.Second)}, ErrInvalid,
			"expiresAt: a personal access token expires at most 365 days ahead, at 2027-11-07T12:00:00Z at the latest"},
		{"over a year in days", false, "alice", "x", nil, Expiry{In: 366 * 24 * time.Hour}, ErrInvalid, "expiresIn: a personal access token expires at most 365 days ahead"},
		{"in the past", false, "alice", "x", nil, Expiry{At: usersNow.Add(-time.Second)}, ErrInvalid, "expiresAt: must be in the future"},
		{"now", true, "raw/scratch", "x", []string{"read"}, Expiry{At: usersNow}, ErrInvalid, "expiresAt: must be in the future"},
		{"both", false, "alice", "x", nil, Expiry{At: year, In: time.Hour}, ErrInvalid, "expiresAt and expiresIn: give one of them, not both"},
		{"negative", false, "alice", "x", nil, Expiry{In: -time.Hour}, ErrInvalid, "expiresIn: must be positive"},
		{"pat scope delete", false, "alice", "x", []string{"read", "delete"}, Expiry{}, ErrInvalid, `scopes: "delete" is not a scope: a personal access token narrows`},
		{"deploy without scopes", true, "raw/scratch", "x", nil, Expiry{}, ErrInvalid, "scopes: a deploy token needs read, write or attach"},
		{"deploy scope delete", true, "raw/scratch", "x", []string{"read", "delete"}, Expiry{}, ErrInvalid,
			`scopes: "delete" is not a scope of a deploy token: it gets read, write and attach, one or more, never delete or manage`},
		{"deploy scope manage", true, "raw/scratch", "x", []string{"manage"}, Expiry{}, ErrInvalid, `scopes: "manage" is not a scope of a deploy token`},
		{"deploy write on a group", true, "raw/all", "x", []string{"read", "write"}, Expiry{}, ErrInvalid,
			"scopes: raw/all is a group, and its deploy token reads only"},
		{"deploy attach on a group", true, "raw/all", "x", []string{"read", "attach"}, Expiry{}, ErrInvalid,
			"scopes: raw/all is a group, and its deploy token reads only"},
		{"empty name", false, "alice", "", nil, Expiry{}, ErrInvalid, "name: must not be empty"},
		{"long name", false, "alice", strings.Repeat("x", 65), nil, Expiry{}, ErrInvalid, "name: must be at most 64 bytes"},
		{"control character", false, "alice", "a\tb", nil, Expiry{}, ErrInvalid, `name: "a\tb" must be printable UTF-8`},
		{"not UTF-8", true, "raw/scratch", "\xff", []string{"read"}, Expiry{}, ErrInvalid, `name: "\xff" must be printable UTF-8`},
		{"name taken", false, "alice", "a year", nil, Expiry{}, index.ErrConflict, `name: "a year" is taken by a live token of user alice`},
		{"deploy name taken", true, "raw/scratch", "forever", []string{"read"}, Expiry{}, index.ErrConflict, `name: "forever" is taken by a live token of repo raw/scratch`},
		{"unknown user", false, "dave", "x", nil, Expiry{}, index.ErrNotFound, "user dave does not exist"},
		{"unknown repo", true, "raw/absent", "x", []string{"read"}, Expiry{}, index.ErrNotFound, "repo raw/absent does not exist"},
		{"not a repo", true, "scratch", "x", []string{"read"}, Expiry{}, ErrInvalid, `repo: "scratch" is not <format>/<name>`},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var err error
			if tt.deploy {
				_, _, err = f.tokens.CreateDeploy(ctx, tt.owner, tt.token, tt.scopes, tt.exp, cli)
			} else {
				_, _, err = f.tokens.CreatePAT(ctx, tt.owner, tt.token, tt.scopes, tt.exp, cli)
			}
			if !errors.Is(err, tt.sentinel) || !strings.HasPrefix(err.Error(), tt.msg) {
				t.Errorf("err = %v, want %v saying %q", err, tt.sentinel, tt.msg)
			}
		})
	}
	wantTokens(t, f, "alice", []string{
		`pat "a year" [read write] expires 2027-11-07T12:00:00Z live`,
		`pat "thirty days" [read] expires 2026-12-07T12:00:00Z live`,
		`pat "éééééééééééééééééééééééééééééééé" [] expires 2027-02-05T12:00:00Z live`,
	})
	wantTokens(t, f, "raw/scratch", []string{
		`deploy "forever" [read] expires never live`,
		`deploy "long" [read write] expires 2027-12-12T12:00:00Z live`,
		`deploy "scanner" [read attach] expires never live`,
	})
	wantTokens(t, f, "raw/all", []string{`deploy "reader" [read] expires never live`})
	if n := len(tokenAudit(t, f.st)); n != 8 {
		t.Errorf("%d audit events, want the 8 creations: a refusal writes nothing", n)
	}
	f.noSecrets(t)
}

// wantTokens checks the tokens of owner, a user or <format>/<name>, in their order, by kind,
// name, scopes, expiry and whether they are revoked.
func wantTokens(t *testing.T, f *tokensFixture, owner string, want []string) {
	t.Helper()
	var (
		list []Token
		err  error
	)
	if strings.Contains(owner, "/") {
		list, err = f.tokens.ListDeploy(context.Background(), owner)
	} else {
		list, err = f.tokens.List(context.Background(), owner)
	}
	if err != nil {
		t.Fatal(err)
	}
	got := make([]string, len(list))
	for i, tok := range list {
		expires, state := "never", "live"
		if tok.ExpiresAt != nil {
			expires = tok.ExpiresAt.Format(time.RFC3339)
		}
		if tok.RevokedAt != nil {
			state = "revoked at " + tok.RevokedAt.Format(time.RFC3339)
		}
		got[i] = fmt.Sprintf("%s %q %v expires %s %s", tok.Kind, tok.Name, tok.Scopes, expires, state)
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("tokens of %s:\n%s\nwant:\n%s", owner, strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

// TestTokensRevoke covers the life of tokens: list in the order of creation with the revoked
// ones, revoke with one event, revoke again without one, the name free again once its token is
// revoked or expired, Get of both kinds, and the unknown id.
func TestTokensRevoke(t *testing.T) {
	f := newTokensFixture(t)
	ctx := context.Background()
	first := f.pat(t, "alice", "ci", []string{"read"}, Expiry{})
	f.pat(t, "alice", "short", nil, Expiry{In: time.Hour})
	dep := f.deploy(t, "raw/scratch", "ci", []string{"write"}, Expiry{})
	f.deploy(t, "raw/other", "ci", []string{"read"}, Expiry{})
	for range 2 {
		if err := errors.Join(f.tokens.Revoke(ctx, first.ID, cli), f.tokens.Revoke(ctx, dep.ID, cli)); err != nil {
			t.Fatal(err)
		}
	}
	f.pat(t, "alice", "ci", nil, Expiry{}) // the name is free again
	f.deploy(t, "raw/scratch", "ci", []string{"read"}, Expiry{})
	later := usersNow.Add(2 * time.Hour)
	f.tokens.Now = func() time.Time { return later }
	f.pat(t, "alice", "short", nil, Expiry{}) // its namesake expired
	wantTokens(t, f, "alice", []string{
		`pat "ci" [read] expires 2027-02-05T12:00:00Z revoked at 2026-11-07T12:00:00Z`,
		`pat "short" [] expires 2026-11-07T13:00:00Z live`,
		`pat "ci" [] expires 2027-02-05T12:00:00Z live`,
		`pat "short" [] expires 2027-02-05T14:00:00Z live`,
	})
	wantTokens(t, f, "raw/scratch", []string{
		`deploy "ci" [write] expires never revoked at 2026-11-07T12:00:00Z`,
		`deploy "ci" [read] expires never live`,
	})

	pat, err := f.tokens.Get(ctx, first.ID)
	if err != nil {
		t.Fatal(err)
	}
	deploy, err := f.tokens.Get(ctx, dep.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got := fmt.Sprintf("%s %q %q / %s %q %q", pat.Kind, pat.User, pat.Repo, deploy.Kind, deploy.User, deploy.Repo); got != `pat "alice" "" / deploy "" "raw/scratch"` {
		t.Errorf("Get = %s", got)
	}
	for _, err := range []error{f.tokens.Revoke(ctx, 999, cli), second(f.tokens.Get(ctx, 999))} {
		if !errors.Is(err, index.ErrNotFound) || err.Error() != "token 999 does not exist" {
			t.Errorf("unknown id: %v", err)
		}
	}
	for owner, sentinel := range map[string]error{"dave": index.ErrNotFound, "raw/absent": index.ErrNotFound, "raw": ErrInvalid} {
		var err error
		if owner == "dave" {
			_, err = f.tokens.List(ctx, owner)
		} else {
			_, err = f.tokens.ListDeploy(ctx, owner)
		}
		if !errors.Is(err, sentinel) {
			t.Errorf("the tokens of %s: %v, want %v", owner, err, sentinel)
		}
	}
	wantRevokeEvents(t, f.st,
		fmt.Sprintf(`anonymous token.revoke raw/scratch raw/scratch {"id":%d,"kind":"deploy","name":"ci","source":"cli"}`, dep.ID),
		fmt.Sprintf(`anonymous token.revoke  user:alice {"id":%d,"kind":"pat","name":"ci","source":"cli"}`, first.ID))
	f.noSecrets(t)
}

// wantRevokeEvents checks the token.revoke events, newest first: a second revocation writes
// none.
func wantRevokeEvents(t *testing.T, st index.Reader, want ...string) {
	t.Helper()
	var got []string
	for _, ev := range tokenAudit(t, st) {
		if strings.Contains(ev, " "+ActionTokenRevoke+" ") {
			got = append(got, ev)
		}
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("token.revoke events:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

func second[T any](_ T, err error) error { return err }

func third[A, B any](_ A, _ B, err error) error { return err }

// TestTokensAuditWriter checks that with an audit.Writer the events go to it after the commit,
// not to the index, and that a writer that fails is logged and undoes nothing.
func TestTokensAuditWriter(t *testing.T) {
	f := newTokensFixture(t)
	w := &recordingAudit{}
	f.tokens.Audit = w
	tok := f.pat(t, "alice", "laptop", nil, Expiry{})
	if err := f.tokens.Revoke(context.Background(), tok.ID, cli); err != nil {
		t.Fatal(err)
	}
	if len(w.evs) != 2 || w.evs[0].Action != ActionTokenCreate || w.evs[1].Action != ActionTokenRevoke {
		t.Errorf("writer events = %+v", w.evs)
	}
	if n := len(tokenAudit(t, f.st)); n != 0 {
		t.Errorf("%d events in the index, want none", n)
	}
	w.fail = errors.New("audit: gone")
	f.pat(t, "alice", "desktop", nil, Expiry{})
	if !strings.Contains(named(t, f.st, f.logs.String()), `"msg":"auth: audit event of a token change not written","action":"token.create","subject":"user:alice","error":"audit: gone"`) {
		t.Errorf("log = %s", f.logs)
	}
	f.noSecrets(t)
}

// TestTokensFailures checks that a failure of the index is an error of its own, named by the
// operation, never one of the sentinels.
func TestTokensFailures(t *testing.T) {
	f := newTokensFixture(t)
	tok := f.pat(t, "alice", "laptop", nil, Expiry{})
	_ = f.st.Close()
	ctx := context.Background()
	for name, err := range map[string]error{
		"create pat":    third(f.tokens.CreatePAT(ctx, "alice", "x", nil, Expiry{}, cli)),
		"create deploy": third(f.tokens.CreateDeploy(ctx, "raw/scratch", "x", []string{"read"}, Expiry{}, cli)),
		"list":          second(f.tokens.List(ctx, "alice")),
		"list deploy":   second(f.tokens.ListDeploy(ctx, "raw/scratch")),
		"get":           second(f.tokens.Get(ctx, tok.ID)),
		"revoke":        f.tokens.Revoke(ctx, tok.ID, cli),
	} {
		if err == nil || errors.Is(err, index.ErrNotFound) || errors.Is(err, ErrInvalid) || !strings.HasPrefix(err.Error(), "auth: ") {
			t.Errorf("%s: %v, want a failure of the index", name, err)
		}
	}
}

// TestParseLifetime is the table of expiresIn and --expires.
func TestParseLifetime(t *testing.T) {
	for in, want := range map[string]time.Duration{"30d": 30 * 24 * time.Hour, "1d": 24 * time.Hour, "12h": 12 * time.Hour, "90m": 90 * time.Minute, "2s": 2 * time.Second} {
		if got, err := ParseLifetime(in); err != nil || got != want {
			t.Errorf("ParseLifetime(%q) = %v, %v; want %v", in, got, err, want)
		}
	}
	for _, in := range []string{"", "0d", "-1d", "1.5d", "d", "30", "0s", "-1h", "100001d", "soon"} {
		if _, err := ParseLifetime(in); !errors.Is(err, ErrInvalid) {
			t.Errorf("ParseLifetime(%q) = %v, want ErrInvalid", in, err)
		}
	}
}

// TestTokensCreatedBy checks the credential rule of a new personal access token (#249): a
// password, or no credential (the command line), creates one; a personal access token, with
// scopes or without, and a deploy token do not, with an error wrapping ErrScope, and nothing is
// written. The token.create event names the credential that created the token in createdBy:
// password, or token:<id> for a token (here a deploy token created with a personal access
// token); nothing from the command line.
func TestTokensCreatedBy(t *testing.T) {
	f := newTokensFixture(t)
	admin := WithPrincipal(context.Background(), Principal{Kind: KindUser, Name: "admin"})
	pat := Credential{Kind: CredPAT, TokenID: 7, UserID: 1}
	tests := []struct {
		name    string
		c       Credential
		deploy  bool   // create a deploy token of raw/scratch, else a PAT of alice
		refused bool   // ErrScope, and no event
		by      string // createdBy of the event, "" for none
	}{
		{name: "a password", c: Credential{Kind: CredPassword}, by: "password"},
		{name: "the command line", by: ""},
		{name: "a pat without scopes", c: pat, refused: true},
		{name: "a pat with scopes", c: Credential{Kind: CredPAT, TokenID: 8, UserID: 1, Scopes: []string{"read", "write"}}, refused: true},
		{name: "a deploy token", c: Credential{Kind: CredDeploy, TokenID: 9, RepoID: 1, Repo: "raw/scratch", Scopes: []string{"write"}}, refused: true},
		{name: "a deploy token by a pat", c: pat, deploy: true, by: "token:7"},
	}
	create := func(ctx context.Context, deploy bool, name string) error {
		var (
			secret string
			err    error
		)
		if deploy {
			_, secret, err = f.tokens.CreateDeploy(ctx, "raw/scratch", name, []string{ScopeRead}, Expiry{}, cli)
		} else {
			_, secret, err = f.tokens.CreatePAT(ctx, "alice", name, nil, Expiry{}, cli)
		}
		if secret != "" {
			f.secrets = append(f.secrets, secret)
		}
		return err
	}
	for i, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := WithCredential(admin, tt.c)
			before := len(tokenAudit(t, f.st))
			err := create(ctx, tt.deploy, fmt.Sprintf("t%d", i))
			added := len(tokenAudit(t, f.st)) - before
			switch {
			case tt.refused && (!errors.Is(err, ErrScope) || added != 0 || !strings.Contains(err.Error(), "password")):
				t.Fatalf("create = %v with %d new events; want ErrScope saying a password creates one, and no event", err, added)
			case tt.refused:
				return
			case err != nil || added != 1:
				t.Fatalf("create = %v with %d new events, want one", err, added)
			}
			if by := createdByOf(t, f.st); by != tt.by {
				t.Errorf("createdBy = %q, want %q", by, tt.by)
			}
		})
	}
	f.noSecrets(t)
}

// createdByOf returns the createdBy of the newest audit event, "" when it has none.
func createdByOf(t *testing.T, st index.Reader) string {
	t.Helper()
	evs, err := st.ListAudit(context.Background(), index.AuditFilter{})
	if err != nil || len(evs) == 0 {
		t.Fatalf("ListAudit = %d events, %v", len(evs), err)
	}
	var details map[string]any
	if err := json.Unmarshal(evs[0].Details, &details); err != nil {
		t.Fatal(err)
	}
	by, _ := details["createdBy"].(string)
	return by
}

// TestTokensDefaults checks the clock and the logger of a zero Tokens.
func TestTokensDefaults(t *testing.T) {
	var s Tokens
	if s.now().Location() != time.UTC || s.log() != slog.Default() {
		t.Error("a zero Tokens has no UTC clock or default logger")
	}
}

// mask creates a deploy token over mask and fails the test on an error.
func (f *tokensFixture) mask(t *testing.T, mask, name string, scopes []string, exp Expiry) *Token {
	t.Helper()
	tok, secret, err := f.tokens.CreateMaskDeploy(context.Background(), mask, name, scopes, exp, cli)
	if err != nil {
		t.Fatalf("CreateMaskDeploy(%s, %s): %v", mask, name, err)
	}
	f.secrets = append(f.secrets, secret)
	return tok
}

// wantMaskTokens checks ListMaskDeploy(mask), by mask, kind, name, scopes, expiry and state.
func wantMaskTokens(t *testing.T, f *tokensFixture, mask string, want ...string) {
	t.Helper()
	list, err := f.tokens.ListMaskDeploy(context.Background(), mask)
	if err != nil {
		t.Fatal(err)
	}
	got := make([]string, len(list))
	for i, tok := range list {
		expires, state := "never", "live"
		if tok.ExpiresAt != nil {
			expires = tok.ExpiresAt.Format(time.RFC3339)
		}
		if tok.RevokedAt != nil {
			state = "revoked"
		}
		got[i] = fmt.Sprintf("%s %s %q %v expires %s %s", tok.RepoMask, tok.Kind, tok.Name, tok.Scopes, expires, state)
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("ListMaskDeploy(%q):\n%s\nwant:\n%s", mask, strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

// maskFixture is the tokens fixture with the formats raw and oci served and the group raw/all,
// and deploy tokens over masks (#911): oci/* nodes (read), oci/* ci (read and write, 400 days),
// */scratch nodes, raw/scratch nodes (an exact mask), raw/* all (read and write: raw/all is a
// group), besides the deploy token nodes of the repo raw/scratch. It returns the first.
func maskFixture(t *testing.T) (*tokensFixture, *Token) {
	t.Helper()
	f := newTokensFixture(t)
	ctx := context.Background()
	f.tokens.Formats = func(name string) bool { return name == "raw" || name == "oci" }
	group := index.Repo{Format: "raw", Name: "all", Prefix: "raw/all", Kind: index.KindGroup}
	if err := f.st.CreateRepo(ctx, &group); err != nil {
		t.Fatal(err)
	}
	nodes := f.mask(t, "oci/*", "nodes", []string{"read"}, Expiry{})
	b, _ := json.Marshal(nodes)
	row, err := f.st.GetTokenByHash(ctx, HashSecret(f.secrets[0]))
	if err != nil {
		t.Fatal(err)
	}
	want := fmt.Sprintf(`{"id":%d,"kind":"deploy","repoMask":"oci/*","name":"nodes","scopes":["read"],"expiresAt":null,`+
		`"lastUsedAt":null,"createdAt":"2026-11-07T12:00:00Z","revokedAt":null,"hashPrefix":%q}`, row.ID, row.Hash[:8])
	if string(b) != want {
		t.Errorf("token = %s\nwant    %s", b, want)
	}
	if row.RepoID != nil || row.RepoMask != "oci/*" || !strings.HasPrefix(f.secrets[0], DeployPrefix) {
		t.Errorf("row = %s, mask %q; want no repo and the mask oci/*, and a deploy token's secret", rowSummary(row), row.RepoMask)
	}
	f.mask(t, "oci/*", "ci", []string{"write", "read"}, Expiry{In: 400 * 24 * time.Hour})
	f.mask(t, "*/scratch", "nodes", []string{"read"}, Expiry{}) // the same name over another mask
	f.mask(t, "raw/scratch", "nodes", []string{"read"}, Expiry{})
	f.mask(t, "raw/*", "all", []string{"read", "write"}, Expiry{}) // raw/all is a group: write all the same
	f.deploy(t, "raw/scratch", "nodes", []string{"read"}, Expiry{})
	return f, nodes
}

// TestTokensMaskDeploy creates deploy tokens over a mask (#911, docs/spec/rbac.md §1): the masks
// of a binding's grammar, an exact one included, with the formats the server serves; the
// scopes and expiry of a deploy token, write too where the mask matches a group; the name unique
// among the live tokens of the same mask; the row and the resource (maskFixture), and the
// refusals.
func TestTokensMaskDeploy(t *testing.T) {
	f, _ := maskFixture(t)
	ctx := context.Background()
	for _, tt := range []struct {
		name, mask, token string
		scopes            []string
		sentinel          error
		msg               string
	}{
		{"one segment", "oci", "x", []string{"read"}, ErrInvalid, `repoMask: mask "oci": want <format>/<repo>, each a name or *`},
		{"three segments", "oci/a/b", "x", []string{"read"}, ErrInvalid, `repoMask: mask "oci/a/b": want <format>/<repo>`},
		{"empty", "", "x", []string{"read"}, ErrInvalid, `repoMask: mask "": want <format>/<repo>`},
		{"a format not served", "maven/*", "x", []string{"read"}, ErrInvalid, `repoMask: "maven" is not a format of this server, nor *`},
		{"not a repo name", "oci/Talos", "x", []string{"read"}, ErrInvalid, `repoMask: "Talos" is not a repo name`},
		{"part of a segment", "oci/ta*", "x", []string{"read"}, ErrInvalid, `repoMask: "ta*" is not a repo name`},
		{"no scopes", "oci/*", "x", nil, ErrInvalid, "scopes: a deploy token needs read, write or attach"},
		{"delete", "oci/*", "x", []string{"read", "delete"}, ErrInvalid, `scopes: "delete" is not a scope of a deploy token`},
		{"manage", "*/*", "x", []string{"manage"}, ErrInvalid, `scopes: "manage" is not a scope of a deploy token`},
		{"empty name", "oci/*", "", []string{"read"}, ErrInvalid, "name: must not be empty"},
		{"name taken", "oci/*", "nodes", []string{"read"}, index.ErrConflict, `name: "nodes" is taken by a live token of mask oci/*`},
	} {
		t.Run(tt.name, func(t *testing.T) {
			_, _, err := f.tokens.CreateMaskDeploy(ctx, tt.mask, tt.token, tt.scopes, Expiry{}, cli)
			if !errors.Is(err, tt.sentinel) || !strings.HasPrefix(err.Error(), tt.msg) {
				t.Errorf("err = %v, want %v saying %q", err, tt.sentinel, tt.msg)
			}
		})
	}
	if n := len(tokenAudit(t, f.st)); n != 6 {
		t.Errorf("%d audit events, want the 6 creations: a refusal writes nothing", n)
	}
	f.noSecrets(t)
}

// TestTokensMaskDeployCredential is the credential that creates a deploy token over a mask
// (decided on #911, 2026-11-14): a password, whose token.create row says createdBy password, or
// none, from the command line; a personal access token, with scopes or without, a deploy token
// and an OIDC login are refused with ErrScope naming the rule, and write nothing.
func TestTokensMaskDeployCredential(t *testing.T) {
	f := newTokensFixture(t)
	admin := WithPrincipal(context.Background(), Principal{Kind: KindUser, Name: "admin"})
	for _, c := range []Credential{
		{Kind: CredPAT, TokenID: 1, UserID: 1},
		{Kind: CredPAT, TokenID: 2, UserID: 1, Scopes: []string{"read", "write"}},
		{Kind: CredDeploy, TokenID: 3, Mask: "*/*", Scopes: []string{"read", "write"}},
		{Kind: CredDeploy, TokenID: 4, RepoID: 1, Repo: "raw/scratch", Scopes: []string{"write"}},
		{Kind: CredOIDC},
	} {
		_, _, err := f.tokens.CreateMaskDeploy(WithCredential(admin, c), "raw/*", "minted", []string{"read"}, Expiry{}, cli)
		if !errors.Is(err, ErrScope) || err.Error() != "a deploy token over a mask is created with the admin's password or a web UI's "+
			"fresh sign-in through OIDC, not with a token" {
			t.Errorf("with %+v: %v, want ErrScope naming the rule", c, err)
		}
	}
	if n := len(tokenAudit(t, f.st)); n != 0 {
		t.Errorf("%d audit events after the refusals, want none", n)
	}
	f.mask(t, "raw/*", "cli", []string{"read"}, Expiry{}) // no credential: the command line
	if _, secret, err := f.tokens.CreateMaskDeploy(WithCredential(admin, Credential{Kind: CredPassword}), "raw/*", "api", []string{"read"}, Expiry{}, cli); err != nil {
		t.Fatalf("with a password: %v", err)
	} else {
		f.secrets = append(f.secrets, secret)
	}
	if by := createdByOf(t, f.st); by != "password" {
		t.Errorf("createdBy = %q, want password", by)
	}
	f.noSecrets(t)
}

// TestTokensMaskDeployLife lists, keeps and revokes deploy tokens over a mask (#911): the lists,
// which keep them apart from the repos' own tokens; a repo deleted, which leaves them; Get; the
// revocation; the audit rows, whose subject is the mask and which have no repo.
func TestTokensMaskDeployLife(t *testing.T) {
	f, nodes := maskFixture(t)
	ctx := context.Background()
	wantMaskTokens(t, f, "",
		`oci/* deploy "nodes" [read] expires never live`,
		`oci/* deploy "ci" [read write] expires 2027-12-12T12:00:00Z live`,
		`*/scratch deploy "nodes" [read] expires never live`,
		`raw/scratch deploy "nodes" [read] expires never live`,
		`raw/* deploy "all" [read write] expires never live`)
	wantMaskTokens(t, f, "oci/*", `oci/* deploy "nodes" [read] expires never live`, `oci/* deploy "ci" [read write] expires 2027-12-12T12:00:00Z live`)
	wantMaskTokens(t, f, "*/*")
	if _, err := f.tokens.ListMaskDeploy(ctx, "oci"); !errors.Is(err, ErrInvalid) {
		t.Errorf("ListMaskDeploy(oci) = %v, want ErrInvalid", err)
	}
	wantTokens(t, f, "raw/scratch", []string{`deploy "nodes" [read] expires never live`}) // not the masks that match it

	// A repo deleted takes its own tokens, not those over a mask that matches it.
	scratch, err := f.st.GetRepo(ctx, "raw", "scratch")
	if err != nil {
		t.Fatal(err)
	}
	if err := f.st.DeleteRepo(ctx, scratch.ID); err != nil {
		t.Fatal(err)
	}
	wantMaskTokens(t, f, "raw/scratch", `raw/scratch deploy "nodes" [read] expires never live`)

	got, err := f.tokens.Get(ctx, nodes.ID)
	if err != nil || got.RepoMask != "oci/*" || got.Repo != "" || got.User != "" {
		t.Errorf("Get = %+v, %v; want the mask oci/* and no other owner", got, err)
	}
	for range 2 {
		if err := f.tokens.Revoke(ctx, nodes.ID, cli); err != nil {
			t.Fatal(err)
		}
	}
	f.mask(t, "oci/*", "nodes", []string{"read"}, Expiry{}) // the name is free again
	wantRevokeEvents(t, f.st, fmt.Sprintf(`anonymous token.revoke  oci/* {"id":%d,"kind":"deploy","name":"nodes","source":"cli"}`, nodes.ID))
	creations := slices.DeleteFunc(tokenAudit(t, f.st), func(ev string) bool { return !strings.Contains(ev, " token.create ") })
	if first := creations[len(creations)-1]; first != fmt.Sprintf(`anonymous token.create  oci/* {"expiresAt":null,"id":%d,"kind":"deploy","name":"nodes","scopes":["read"],"source":"cli"}`, nodes.ID) {
		t.Errorf("the first token.create = %s", first)
	}
	f.noSecrets(t)
}

// TestTokensDeployLifetime: a deploy token, of a repo and over a mask, is bounded by
// DeployMaxLifetime (docs/spec/rbac.md §1, #1286): created without an expiry it gets the bound,
// one asked to expire later is refused naming the field given, or --expires; zero bounds
// nothing, and a token without an expiry then never expires.
func TestTokensDeployLifetime(t *testing.T) {
	ctx := context.Background()
	quarter := 90 * 24 * time.Hour
	decade := Expiry{In: 3650 * 24 * time.Hour}
	latest := "at 2027-02-05T12:00:00Z at the latest"
	for i, tt := range []struct {
		max  time.Duration
		mask bool // a token over raw/*, else one of raw/scratch
		exp  Expiry
		want time.Duration // the lifetime given; 0 for none
		err  string
	}{
		{max: 0, exp: Expiry{}},
		{max: 0, exp: decade, want: decade.In},
		{max: 0, mask: true, exp: Expiry{}},
		{max: quarter, exp: Expiry{}, want: quarter},
		{max: quarter, mask: true, exp: Expiry{}, want: quarter},
		{max: quarter, exp: Expiry{In: 30 * 24 * time.Hour}, want: 30 * 24 * time.Hour},
		{max: quarter, exp: Expiry{At: usersNow.Add(quarter)}, want: quarter},
		{max: quarter, exp: Expiry{In: quarter + time.Hour},
			err: "expiresIn: a deploy token expires at most 2160h0m0s ahead (auth.deployTokens.maxTTL), " + latest},
		{max: quarter, exp: Expiry{At: usersNow.Add(quarter + time.Second)},
			err: "expiresAt: a deploy token expires at most 2160h0m0s ahead (auth.deployTokens.maxTTL), " + latest},
		{max: quarter, mask: true, exp: Expiry{In: decade.In, Field: "--expires"},
			err: "--expires: a deploy token expires at most 2160h0m0s ahead (auth.deployTokens.maxTTL), " + latest},
		{max: quarter, exp: Expiry{At: usersNow.Add(-time.Hour), Field: "--expires"}, err: "--expires: must be in the future"},
	} {
		f := newTokensFixture(t)
		f.tokens.DeployMaxLifetime = tt.max
		var (
			tok *Token
			err error
		)
		if tt.mask {
			tok, _, err = f.tokens.CreateMaskDeploy(ctx, "raw/*", "nodes", []string{ScopeRead}, tt.exp, cli)
		} else {
			tok, _, err = f.tokens.CreateDeploy(ctx, "raw/scratch", "ci", []string{ScopeRead}, tt.exp, cli)
		}
		switch {
		case tt.err != "":
			if !errors.Is(err, ErrInvalid) || err.Error() != tt.err {
				t.Errorf("%d: %v, want %q", i, err, tt.err)
			}
			if n := len(tokenAudit(t, f.st)); n != 0 {
				t.Errorf("%d: %d audit events, want none for a refusal", i, n)
			}
		case err != nil:
			t.Errorf("%d: %v", i, err)
		case tt.want == 0 && tok.ExpiresAt != nil, tt.want != 0 && (tok.ExpiresAt == nil || !tok.ExpiresAt.Equal(usersNow.Add(tt.want))):
			t.Errorf("%d: expiresAt %v, want %v from now", i, tok.ExpiresAt, tt.want)
		}
	}
}

// TestTokensMaxTTLAudit: the token.create event of a deploy token or of a service account's token
// created under a bound of the configuration has maxTTL, the seconds of the bound; one created
// without a bound has none, nor has a person's, whose bound is a constant.
func TestTokensMaxTTLAudit(t *testing.T) {
	ctx := context.Background()
	f, _ := newServiceFixture(t)
	tokens := &Tokens{Index: f.st, Now: f.users.Now, Log: f.users.Log}
	r := index.Repo{Format: "raw", Name: "scratch", Registry: "local", Prefix: "raw/scratch", Kind: "hosted"}
	if err := f.st.CreateRepo(ctx, &r); err != nil {
		t.Fatal(err)
	}
	for _, tt := range []struct {
		name            string
		deploy, service time.Duration
		create          func(name string) error
		want            any // maxTTL in the details; nil for none
	}{
		{name: "deploy under a bound", deploy: 90 * 24 * time.Hour, want: float64(7776000), create: func(name string) error {
			return third(tokens.CreateDeploy(ctx, "raw/scratch", name, []string{ScopeRead}, Expiry{}, cli))
		}},
		{name: "mask under a bound", deploy: time.Hour, want: float64(3600), create: func(name string) error {
			return third(tokens.CreateMaskDeploy(ctx, "raw/*", name, []string{ScopeRead}, Expiry{In: time.Minute}, cli))
		}},
		{name: "deploy without a bound", create: func(name string) error {
			return third(tokens.CreateDeploy(ctx, "raw/scratch", name, []string{ScopeRead}, Expiry{}, cli))
		}},
		{name: "service account under a bound", service: 720 * time.Hour, want: float64(2592000), create: func(name string) error {
			return third(tokens.CreatePAT(ctx, "robot", name, nil, Expiry{}, cli))
		}},
		{name: "service account without a bound", create: func(name string) error {
			return third(tokens.CreatePAT(ctx, "robot", name, nil, Expiry{}, cli))
		}},
		{name: "a person's", deploy: time.Hour, service: time.Hour, create: func(name string) error {
			return third(tokens.CreatePAT(ctx, "alice", name, nil, Expiry{}, cli))
		}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			tokens.DeployMaxLifetime, tokens.ServiceMaxLifetime = tt.deploy, tt.service
			if err := tt.create(tt.name); err != nil {
				t.Fatal(err)
			}
			evs, err := f.st.ListAudit(ctx, index.AuditFilter{})
			if err != nil || len(evs) == 0 || evs[0].Action != ActionTokenCreate {
				t.Fatalf("ListAudit = %d events, %v; want a token.create first", len(evs), err)
			}
			var details map[string]any
			if err := json.Unmarshal(evs[0].Details, &details); err != nil {
				t.Fatal(err)
			}
			if got := details["maxTTL"]; got != tt.want {
				t.Errorf("maxTTL = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestTokensBeyondMaxTTL: once a bound is set, every view marks the live tokens made before it
// beyond it, a deploy token's and a service account's, and CountBeyondMaxTTL counts them
// (docs/spec/rbac.md §1, #1286); a token made under the bound, a token within it, a revoked or
// expired one and a person's are never marked, and without a bound none is.
func TestTokensBeyondMaxTTL(t *testing.T) {
	ctx := context.Background()
	tokens, forever, within := beyondFixture(t)
	if got, want := markedBeyond(t, tokens, forever.ID), "repo: \nmasks: \nrobot: \nalice: \nlive: \nall: \nget: "; got != want {
		t.Errorf("without a bound, marked:\n%s", got)
	}
	if d, s, err := tokens.CountBeyondMaxTTL(ctx); d != 0 || s != 0 || err != nil {
		t.Errorf("CountBeyondMaxTTL without a bound = %d, %d, %v; want none", d, s, err)
	}

	tokens.DeployMaxLifetime, tokens.ServiceMaxLifetime = 90*24*time.Hour, 30*24*time.Hour
	under, _, err := tokens.CreateDeploy(ctx, "raw/scratch", "under", []string{ScopeRead}, Expiry{}, cli)
	if err != nil || under.BeyondMaxTTL || under.ExpiresAt == nil {
		t.Errorf("a token made under the bound: %+v, %v", under, err)
	}
	want := "repo: forever,long\nmasks: nodes\nrobot: ci\nalice: \nlive: forever,long,nodes,ci\nall: forever,long,nodes,ci\nget: forever"
	if got := markedBeyond(t, tokens, forever.ID); got != want {
		t.Errorf("marked:\n%s\nwant:\n%s", got, want)
	}
	if d, s, err := tokens.CountBeyondMaxTTL(ctx); d != 3 || s != 1 || err != nil {
		t.Errorf("CountBeyondMaxTTL = %d deploy, %d service, %v; want 3 and 1", d, s, err)
	}
	if b, _ := json.Marshal(within); strings.Contains(string(b), "beyondMaxTTL") {
		t.Errorf("a token within the bound answers %s, want no beyondMaxTTL", b)
	}
}

// beyondFixture is the service-account fixture with the repo raw/scratch, tokens made without a
// bound, and its clock two hours later: deploy tokens forever (no expiry), long (400 days),
// within (30 days), expired (an hour) and revoked, nodes over raw/* (no expiry), robot's ci (no
// expiry) and alice's tablet (300 days). It returns the service, forever and within.
func beyondFixture(t *testing.T) (*Tokens, *Token, *Token) {
	t.Helper()
	ctx := context.Background()
	f, _ := newServiceFixture(t)
	r := index.Repo{Format: "raw", Name: "scratch", Registry: "local", Prefix: "raw/scratch", Kind: "hosted"}
	if err := f.st.CreateRepo(ctx, &r); err != nil {
		t.Fatal(err)
	}
	clock := usersNow
	tokens := &Tokens{Index: f.st, Now: func() time.Time { return clock }, Log: f.users.Log}
	day, read := 24*time.Hour, []string{ScopeRead}
	made := func(tok *Token, _ string, err error) *Token {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
		return tok
	}
	forever := made(tokens.CreateDeploy(ctx, "raw/scratch", "forever", read, Expiry{}, cli))
	made(tokens.CreateDeploy(ctx, "raw/scratch", "long", read, Expiry{In: 400 * day}, cli))
	within := made(tokens.CreateDeploy(ctx, "raw/scratch", "within", read, Expiry{In: 30 * day}, cli))
	made(tokens.CreateDeploy(ctx, "raw/scratch", "expired", read, Expiry{In: time.Hour}, cli))
	gone := made(tokens.CreateDeploy(ctx, "raw/scratch", "revoked", read, Expiry{}, cli))
	made(tokens.CreateMaskDeploy(ctx, "raw/*", "nodes", read, Expiry{}, cli))
	made(tokens.CreatePAT(ctx, "robot", "ci", nil, Expiry{}, cli))
	made(tokens.CreatePAT(ctx, "alice", "tablet", nil, Expiry{In: 300 * day}, cli))
	if err := tokens.Revoke(ctx, gone.ID, cli); err != nil {
		t.Fatal(err)
	}
	clock = usersNow.Add(2 * time.Hour)
	return tokens, forever, within
}

// markedBeyond returns the names of the tokens each view of tokens marks BeyondMaxTTL, a line per
// view: the repo raw/scratch, every mask, robot's, alice's, every live token, every token, and
// the token id.
func markedBeyond(t *testing.T, tokens *Tokens, id int64) string {
	t.Helper()
	ctx := context.Background()
	var out []string
	mark := func(view string) func([]Token, error) {
		return func(list []Token, err error) {
			if err != nil {
				t.Fatalf("%s: %v", view, err)
			}
			names := []string{}
			for _, tok := range list {
				if tok.BeyondMaxTTL {
					names = append(names, tok.Name)
				}
			}
			out = append(out, view+": "+strings.Join(names, ","))
		}
	}
	mark("repo")(tokens.ListDeploy(ctx, "raw/scratch"))
	mark("masks")(tokens.ListMaskDeploy(ctx, ""))
	mark("robot")(tokens.List(ctx, "robot"))
	mark("alice")(tokens.List(ctx, "alice"))
	mark("live")(tokens.Page(ctx, TokenQuery{Live: true, Limit: 100}))
	mark("all")(tokens.Page(ctx, TokenQuery{Limit: 100}))
	got, err := tokens.Get(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	mark("get")([]Token{*got}, nil)
	return strings.Join(out, "\n")
}
