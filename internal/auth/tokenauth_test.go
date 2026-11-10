package auth

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lajstrom/lajstrom/internal/index"
)

// tokenStore is the token side of fakeUsers: tokens by the hash of their secret, repos by id,
// the use of tokens, and counters of the reads.
type tokenStore struct {
	tmu       sync.Mutex
	tokens    map[string]index.Token // by hash
	repos     map[int64]index.Repo
	touched   map[int64][]time.Time
	tokenErr  error // the answer of every GetTokenByHash when set
	repoErr   error // of every GetRepoByID
	listErr   error // of every ListUsers
	touchErr  error // of every TouchToken
	tokenRead atomic.Int64
	listRead  atomic.Int64
}

func (f *fakeUsers) GetTokenByHash(_ context.Context, hash string) (*index.Token, error) {
	f.tokenRead.Add(1)
	f.tmu.Lock()
	defer f.tmu.Unlock()
	if f.tokenErr != nil {
		return nil, f.tokenErr
	}
	t, ok := f.tokens[hash]
	if !ok {
		return nil, fmt.Errorf("%w: token", index.ErrNotFound)
	}
	return &t, nil
}

func (f *fakeUsers) GetRepoByID(_ context.Context, id int64) (*index.Repo, error) {
	f.tmu.Lock()
	defer f.tmu.Unlock()
	if f.repoErr != nil {
		return nil, f.repoErr
	}
	r, ok := f.repos[id]
	if !ok {
		return nil, fmt.Errorf("%w: repo %d", index.ErrNotFound, id)
	}
	return &r, nil
}

func (f *fakeUsers) GetUserByID(_ context.Context, id int64) (*index.User, error) {
	f.reads.Add(1)
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return nil, f.err
	}
	for _, u := range f.users {
		if u.ID == id {
			return &u, nil
		}
	}
	for _, u := range f.deleted {
		if u.ID == id {
			return &u, nil
		}
	}
	return nil, fmt.Errorf("%w: user %d", index.ErrNotFound, id)
}

func (f *fakeUsers) ListUsers(context.Context) ([]index.User, error) {
	f.listRead.Add(1)
	if f.listErr != nil {
		return nil, f.listErr
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []index.User
	for _, u := range f.users {
		out = append(out, u)
	}
	return out, nil
}

func (f *fakeUsers) TouchToken(_ context.Context, id int64, at time.Time) error {
	f.tmu.Lock()
	defer f.tmu.Unlock()
	if f.touchErr != nil {
		return f.touchErr
	}
	if f.touched == nil {
		f.touched = map[int64][]time.Time{}
	}
	f.touched[id] = append(f.touched[id], at)
	return nil
}

// addToken stores t with a new secret of prefix as its hash and returns the secret.
func (f *fakeUsers) addToken(prefix string, t index.Token) string {
	secret := newSecret(prefix)
	f.tmu.Lock()
	defer f.tmu.Unlock()
	if f.tokens == nil {
		f.tokens = map[string]index.Token{}
	}
	t.Hash = HashSecret(secret)
	f.tokens[t.Hash] = t
	return secret
}

// touches returns the recorded uses of token id.
func (f *fakeUsers) touches(id int64) []time.Time {
	f.tmu.Lock()
	defer f.tmu.Unlock()
	return slices.Clone(f.touched[id])
}

func ptr[T any](v T) *T { return &v }

// tokenFixture is an authFixture with tokens: alice's (full rights, read only, revoked,
// expired, without an expiry, of a deploy row), bob's (bob is disabled), carol's (no
// password), one of a user that does not exist, and deploy tokens of raw/scratch (repo 10:
// read and write, revoked, expired, never expiring, of a pat row) and of a repo that is gone.
type tokenFixture struct {
	*authFixture
	secrets map[string]string // by name
}

func newTokenFixture(t *testing.T, o Options) *tokenFixture {
	t.Helper()
	f := &tokenFixture{authFixture: newAuthFixture(t, o), secrets: map[string]string{}}
	u := f.users
	u.repos = map[int64]index.Repo{10: {ID: 10, Format: "raw", Name: "scratch"}}
	now := f.clock.Now()
	hour, past := ptr(now.Add(time.Hour)), ptr(now.Add(-time.Second))
	for _, tt := range []struct {
		name, prefix string
		row          index.Token
	}{
		{"alice", PATPrefix, index.Token{ID: 1, Kind: TokenPAT, UserID: ptr(int64(1)), ExpiresAt: hour}},
		{"alice-read", PATPrefix, index.Token{ID: 2, Kind: TokenPAT, UserID: ptr(int64(1)), Scopes: []string{"read"}, ExpiresAt: hour}},
		{"alice-revoked", PATPrefix, index.Token{ID: 3, Kind: TokenPAT, UserID: ptr(int64(1)), ExpiresAt: hour, RevokedAt: past}},
		{"alice-expired", PATPrefix, index.Token{ID: 4, Kind: TokenPAT, UserID: ptr(int64(1)), ExpiresAt: past}},
		{"alice-no-expiry", PATPrefix, index.Token{ID: 5, Kind: TokenPAT, UserID: ptr(int64(1))}},
		{"alice-as-deploy", PATPrefix, index.Token{ID: 6, Kind: TokenDeploy, RepoID: ptr(int64(10)), Scopes: []string{"read"}}},
		{"bob", PATPrefix, index.Token{ID: 7, Kind: TokenPAT, UserID: ptr(int64(2)), ExpiresAt: hour}},
		{"carol", PATPrefix, index.Token{ID: 8, Kind: TokenPAT, UserID: ptr(int64(3)), ExpiresAt: hour}},
		{"nobody", PATPrefix, index.Token{ID: 9, Kind: TokenPAT, UserID: ptr(int64(42)), ExpiresAt: hour}},
		{"deploy", DeployPrefix, index.Token{ID: 20, Kind: TokenDeploy, RepoID: ptr(int64(10)), Scopes: []string{"read", "write"}, ExpiresAt: hour}},
		{"deploy-revoked", DeployPrefix, index.Token{ID: 21, Kind: TokenDeploy, RepoID: ptr(int64(10)), Scopes: []string{"read"}, RevokedAt: past}},
		{"deploy-expired", DeployPrefix, index.Token{ID: 22, Kind: TokenDeploy, RepoID: ptr(int64(10)), Scopes: []string{"read"}, ExpiresAt: past}},
		{"deploy-never", DeployPrefix, index.Token{ID: 23, Kind: TokenDeploy, RepoID: ptr(int64(10)), Scopes: []string{"read"}}},
		{"deploy-as-pat", DeployPrefix, index.Token{ID: 24, Kind: TokenPAT, UserID: ptr(int64(1)), ExpiresAt: hour}},
		{"deploy-gone", DeployPrefix, index.Token{ID: 25, Kind: TokenDeploy, RepoID: ptr(int64(99)), Scopes: []string{"read"}}},
	} {
		f.secrets[tt.name] = u.addToken(tt.prefix, tt.row)
	}
	f.secrets["unknown"] = newSecret(PATPrefix)
	f.secrets["unknown-deploy"] = newSecret(DeployPrefix)
	return f
}

// noSecrets fails the test when the log holds any secret of the fixture or its hash.
func (f *tokenFixture) noSecrets(t *testing.T) {
	t.Helper()
	logs := f.logs.String()
	for name, s := range f.secrets {
		if strings.Contains(logs, s) || strings.Contains(logs, HashSecret(s)) {
			t.Errorf("the log holds the secret %s or its hash", name)
		}
		if strings.Contains(logs, s[len(PATPrefix):]) {
			t.Errorf("the log holds the random part of the secret %s", name)
		}
	}
}

// TestAuthenticateTokens is the table of Basic credentials with a token: a personal access
// token is its user and only its user's, whatever else its owner is (carol has no password);
// a deploy token is token:<id> whatever the user name; the refusals say why at debug, with the
// token id, after one GetTokenByHash and, for a personal access token, one GetUser, and never
// an argon2id verification; a name or a secret of the wrong shape needs no index at all.
func TestAuthenticateTokens(t *testing.T) {
	deploy := Principal{Kind: KindToken, Name: "20"}
	tests := []struct {
		name, user, which string
		want              Principal // zero: refused
		cred              Credential
		reason            string
		token             int64 // in the log line, 0 for none
		tokenReads, reads int64 // GetTokenByHash, GetUser
	}{
		{name: "pat", user: "alice", which: "alice", want: userP("alice"),
			cred: Credential{Kind: CredPAT, TokenID: 1, UserID: 1, Scopes: []string{}}, tokenReads: 1, reads: 1},
		{name: "pat with scopes", user: "alice", which: "alice-read", want: userP("alice"),
			cred: Credential{Kind: CredPAT, TokenID: 2, UserID: 1, Scopes: []string{"read"}}, tokenReads: 1, reads: 1},
		{name: "pat of a user without a password", user: "carol", which: "carol", want: userP("carol"),
			cred: Credential{Kind: CredPAT, TokenID: 8, UserID: 3, Scopes: []string{}}, tokenReads: 1, reads: 1},
		{name: "pat of another user", user: "carol", which: "alice", reason: "the token of another user", token: 1, tokenReads: 1, reads: 1},
		{name: "pat with an unknown user", user: "dave", which: "alice", reason: "unknown user", token: 1, tokenReads: 1, reads: 1},
		{name: "pat as deploy", user: "deploy", which: "alice", reason: "malformed user name"},
		{name: "pat with a bad user name", user: "Alice", which: "alice", reason: "malformed user name"},
		{name: "pat revoked", user: "alice", which: "alice-revoked", reason: "revoked", token: 3, tokenReads: 1, reads: 1},
		{name: "pat expired", user: "alice", which: "alice-expired", reason: "expired", token: 4, tokenReads: 1, reads: 1},
		{name: "pat without an expiry", user: "alice", which: "alice-no-expiry", reason: "a personal access token without an expiry or an owner", token: 5, tokenReads: 1, reads: 1},
		{name: "pat of a disabled owner", user: "bob", which: "bob", reason: "disabled", token: 7, tokenReads: 1, reads: 1},
		{name: "pat unknown", user: "alice", which: "unknown", reason: "unknown token", tokenReads: 1, reads: 1},
		{name: "pat secret on a deploy row", user: "alice", which: "alice-as-deploy", reason: "a token of kind deploy", token: 6, tokenReads: 1, reads: 1},
		{name: "pat malformed", user: "alice", which: PATPrefix + "short", reason: "malformed token"},
		{name: "pat with a character off the alphabet", user: "alice", which: PATPrefix + strings.Repeat("a", 42) + "-", reason: "malformed token"},
		{name: "deploy", user: "deploy", which: "deploy", want: deploy,
			cred: Credential{Kind: CredDeploy, TokenID: 20, RepoID: 10, Repo: "raw/scratch", Scopes: []string{"read", "write"}}, tokenReads: 1},
		{name: "deploy with any user name", user: "__token__", which: "deploy", want: deploy,
			cred: Credential{Kind: CredDeploy, TokenID: 20, RepoID: 10, Repo: "raw/scratch", Scopes: []string{"read", "write"}}, tokenReads: 1},
		{name: "deploy without a user name", user: "", which: "deploy", want: deploy,
			cred: Credential{Kind: CredDeploy, TokenID: 20, RepoID: 10, Repo: "raw/scratch", Scopes: []string{"read", "write"}}, tokenReads: 1},
		{name: "deploy never expiring", user: "deploy", which: "deploy-never", want: Principal{Kind: KindToken, Name: "23"},
			cred: Credential{Kind: CredDeploy, TokenID: 23, RepoID: 10, Repo: "raw/scratch", Scopes: []string{"read"}}, tokenReads: 1},
		{name: "deploy revoked", user: "deploy", which: "deploy-revoked", reason: "revoked", token: 21, tokenReads: 1},
		{name: "deploy expired", user: "deploy", which: "deploy-expired", reason: "expired", token: 22, tokenReads: 1},
		{name: "deploy secret on a pat row", user: "deploy", which: "deploy-as-pat", reason: "a token of kind pat", token: 24, tokenReads: 1},
		{name: "deploy of a repo gone", user: "deploy", which: "deploy-gone", reason: "the repo is gone", token: 25, tokenReads: 1},
		{name: "deploy unknown", user: "deploy", which: "unknown-deploy", reason: "unknown token", tokenReads: 1},
		{name: "deploy malformed", user: "deploy", which: DeployPrefix + "short", reason: "malformed token"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newTokenFixture(t, Options{})
			secret := tt.which
			if s, ok := f.secrets[secret]; ok {
				secret = s
			}
			p, c, err := f.a.Authenticate(context.Background(), tt.user, secret)
			if tt.want != (Principal{}) {
				f.wantSuccess(t, p, c, err, tt.want, tt.cred)
			} else {
				f.wantRefusal(t, p, c, err, tt.token, tt.reason)
			}
			reads := fmt.Sprintf("GetTokenByHash %d, GetUser %d, argon2id %d", f.users.tokenRead.Load(), f.users.reads.Load(), f.verifications())
			if want := fmt.Sprintf("GetTokenByHash %d, GetUser %d, argon2id 0", tt.tokenReads, tt.reads); reads != want {
				t.Errorf("%s, want %s", reads, want)
			}
			f.noSecrets(t)
		})
	}
}

// wantSuccess checks an authentication that succeeded as want with the credential cred, and
// logged nothing.
func (f *tokenFixture) wantSuccess(t *testing.T, p Principal, c Credential, err error, want Principal, cred Credential) {
	t.Helper()
	if err != nil || p != want || fmt.Sprint(c) != fmt.Sprint(cred) {
		t.Fatalf("Authenticate = %v, %+v, %v; want %v, %+v", p, c, err, want, cred)
	}
	if f.logs.Len() != 0 {
		t.Errorf("a success is logged:\n%s", f.logs)
	}
}

// wantRefusal checks a refusal, logged at debug with the reason and the token's id when it is
// not 0.
func (f *tokenFixture) wantRefusal(t *testing.T, p Principal, c Credential, err error, token int64, reason string) {
	t.Helper()
	if !errors.Is(err, ErrBadCredentials) || p != (Principal{}) || c.Kind != "" {
		t.Fatalf("Authenticate = %v, %+v, %v; want ErrBadCredentials", p, c, err)
	}
	want := fmt.Sprintf(`"reason":%q`, reason)
	if token != 0 {
		want = fmt.Sprintf(`"token":%d,"reason":%q`, token, reason)
	}
	if !strings.Contains(f.logs.String(), `"level":"DEBUG","msg":"authentication refused"`) || !strings.Contains(f.logs.String(), want) {
		t.Errorf("log %s does not have %s", f.logs, want)
	}
}

// TestAuthenticateBearer is the table of Authorization: Bearer with a token: a personal access
// token is its owner, a deploy token token:<id> (ADR 0012), anything else is refused, and so are
// the refusals of the Basic path.
func TestAuthenticateBearer(t *testing.T) {
	tests := []struct {
		name, which string
		want        Principal // zero: refused
		reason      string
	}{
		{name: "pat", which: "alice", want: userP("alice")},
		{name: "pat with scopes", which: "alice-read", want: userP("alice")},
		{name: "deploy token", which: "deploy", want: Principal{Kind: KindToken, Name: "20"}},
		{name: "deploy token revoked", which: "deploy-revoked", reason: "revoked"},
		{name: "deploy token unknown", which: "unknown-deploy", reason: "unknown token"},
		{name: "malformed", which: "eyJhbGciOi.x.y", reason: "malformed token"},
		{name: "unknown", which: "unknown", reason: "unknown token"},
		{name: "revoked", which: "alice-revoked", reason: "revoked"},
		{name: "expired", which: "alice-expired", reason: "expired"},
		{name: "owner disabled", which: "bob", reason: "disabled"},
		{name: "owner unknown", which: "nobody", reason: "unknown user"},
		{name: "a deploy row", which: "alice-as-deploy", reason: "a token of kind deploy"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newTokenFixture(t, Options{})
			secret := tt.which
			if s, ok := f.secrets[secret]; ok {
				secret = s
			}
			p, c, err := f.a.AuthenticateBearer(context.Background(), secret)
			want := map[Kind]Credential{KindUser: {Kind: CredPAT, UserID: 1}, KindToken: {Kind: CredDeploy, TokenID: 20, RepoID: 10}}[tt.want.Kind]
			if tt.want != (Principal{}) {
				if err != nil || p != tt.want || c.Kind != want.Kind || c.UserID != want.UserID || c.RepoID != want.RepoID {
					t.Fatalf("AuthenticateBearer = %v, %+v, %v; want %v, %+v", p, c, err, tt.want, want)
				}
			} else {
				if !errors.Is(err, ErrBadCredentials) || p != (Principal{}) {
					t.Fatalf("AuthenticateBearer = %v, %v; want ErrBadCredentials", p, err)
				}
				if want := fmt.Sprintf(`"reason":%q`, tt.reason); !strings.Contains(f.logs.String(), want) {
					t.Errorf("log %s does not have %s", f.logs, want)
				}
			}
			f.noSecrets(t)
		})
	}
}

// TestAuthenticateBearerOwner checks that the owner of a bearer is read by id on every call
// (GetUserByID), so that a renamed owner keeps its token, the principal staying its uid, and a
// deleted one is refused at once.
func TestAuthenticateBearerOwner(t *testing.T) {
	f := newTokenFixture(t, Options{CacheTTL: time.Minute})
	bearer := func() (Principal, error) {
		p, _, err := f.a.AuthenticateBearer(context.Background(), f.secrets["alice"])
		return p, err
	}
	p, err := bearer()
	if err != nil {
		t.Fatal(err)
	}
	alice := f.users.byName("alice")
	if p != UserPrincipal(&alice) {
		t.Fatalf("principal %s, want alice's user:<uid>", p)
	}
	f.users.rename("alice", "alicia")
	if p, err := bearer(); err != nil || p != UserPrincipal(&alice) {
		t.Errorf("after a rename: %s, %v; want the same principal", p, err)
	}
	f.users.markDeleted("alicia")
	if _, err := bearer(); !errors.Is(err, ErrBadCredentials) {
		t.Errorf("a deleted owner: %v, want ErrBadCredentials", err)
	}
}

// TestTouch checks that the use of a token is recorded at most once a minute per token, at the
// time of the clock, for both kinds and both paths; that a record that fails is logged at debug
// and refuses nothing; and that the remembered uses are bounded.
func TestTouch(t *testing.T) {
	f := newTokenFixture(t, Options{})
	ctx := context.Background()
	start := f.clock.Now()
	use := func(name string) {
		t.Helper()
		var err error
		switch {
		case strings.HasPrefix(name, "deploy"):
			_, _, err = f.a.Authenticate(ctx, "deploy", f.secrets[name])
		case strings.HasSuffix(name, "-bearer"):
			_, _, err = f.a.AuthenticateBearer(ctx, f.secrets[strings.TrimSuffix(name, "-bearer")])
		default:
			_, _, err = f.a.Authenticate(ctx, "alice", f.secrets[name])
		}
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
	}
	use("alice")
	f.clock.advance(30 * time.Second)
	use("alice-bearer")
	use("deploy")
	f.clock.advance(30*time.Second - time.Nanosecond)
	use("alice")
	f.clock.advance(time.Nanosecond)
	use("alice")
	use("deploy")
	if got, want := fmt.Sprint(f.users.touches(1)), fmt.Sprint([]time.Time{start, start.Add(time.Minute)}); got != want {
		t.Errorf("touches of token 1 = %s, want %s", got, want)
	}
	if got := f.users.touches(20); len(got) != 1 || !got[0].Equal(start.Add(30*time.Second)) {
		t.Errorf("touches of token 20 = %v, want one at +30s", got)
	}

	f.users.touchErr = errors.New("index: database is read-only")
	f.clock.advance(time.Minute)
	use("alice")
	if !strings.Contains(f.logs.String(), `"level":"DEBUG","msg":"the use of a token is not recorded","token":1,"error":"index: database is read-only"`) {
		t.Errorf("the failed record is not logged at debug:\n%s", f.logs)
	}
	f.noSecrets(t)

	small := newTokenFixture(t, Options{CacheSize: 2})
	for _, name := range []string{"alice", "alice-read", "carol"} {
		user := "alice"
		if name == "carol" {
			user = "carol"
		}
		if _, _, err := small.a.Authenticate(ctx, user, small.secrets[name]); err != nil {
			t.Fatal(err)
		}
	}
	if n := small.a.touches.len(); n != 2 {
		t.Errorf("%d uses remembered, want the bound 2", n)
	}
}

// TestAuthenticateTokenFailures checks that a failure of the index is an error of its own, not
// ErrBadCredentials, on every read of the token paths, and that it holds no secret.
func TestAuthenticateTokenFailures(t *testing.T) {
	errDown := errors.New("index: database is gone")
	tests := []struct {
		name    string
		breakIt func(*fakeUsers)
		user    string // "" for a bearer
		secret  string
	}{
		{"pat token", func(u *fakeUsers) { u.tokenErr = errDown }, "alice", "alice"},
		{"pat user", func(u *fakeUsers) { u.err = errDown }, "alice", "alice"},
		{"deploy token", func(u *fakeUsers) { u.tokenErr = errDown }, "deploy", "deploy"},
		{"deploy repo", func(u *fakeUsers) { u.repoErr = errDown }, "deploy", "deploy"},
		{"bearer token", func(u *fakeUsers) { u.tokenErr = errDown }, "", "alice"},
		{"bearer owner", func(u *fakeUsers) { u.err = errDown }, "", "alice"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newTokenFixture(t, Options{})
			tt.breakIt(f.users)
			var err error
			if tt.user == "" {
				_, _, err = f.a.AuthenticateBearer(context.Background(), f.secrets[tt.secret])
			} else {
				_, _, err = f.a.Authenticate(context.Background(), tt.user, f.secrets[tt.secret])
			}
			if !errors.Is(err, errDown) || errors.Is(err, ErrBadCredentials) {
				t.Fatalf("err = %v, want the index error", err)
			}
			if strings.Contains(err.Error(), f.secrets[tt.secret]) || strings.Contains(err.Error(), HashSecret(f.secrets[tt.secret])) {
				t.Errorf("error %q holds the secret or its hash", err)
			}
		})
	}
}

// TestAuthenticateTokensConcurrent uses tokens from many goroutines at once, for the race
// detector.
func TestAuthenticateTokensConcurrent(t *testing.T) {
	f := newTokenFixture(t, Options{CacheSize: 2})
	var wg sync.WaitGroup
	for i := range 8 {
		wg.Go(func() {
			for j := range 6 {
				f.clock.advance(time.Duration(i+j) * time.Second)
				switch (i + j) % 3 {
				case 0:
					_, _, _ = f.a.Authenticate(context.Background(), "alice", f.secrets["alice"])
				case 1:
					_, _, _ = f.a.AuthenticateBearer(context.Background(), f.secrets["alice-read"])
				default:
					_, _, _ = f.a.Authenticate(context.Background(), "deploy"+strconv.Itoa(j), f.secrets["deploy"])
				}
			}
		})
	}
	wg.Wait()
	if _, _, err := f.a.Authenticate(context.Background(), "alice", f.secrets["alice"]); err != nil {
		t.Error(err)
	}
}
