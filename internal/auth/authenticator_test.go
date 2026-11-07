package auth

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lajstrom/lajstrom/internal/index"
)

// fakeUsers is an index.Reader that has users and nothing else, and counts the reads.
type fakeUsers struct {
	index.Reader
	mu    sync.Mutex
	users map[string]index.User
	err   error // the answer of every GetUser when set
	reads atomic.Int64
}

func (f *fakeUsers) GetUser(_ context.Context, name string) (*index.User, error) {
	f.reads.Add(1)
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return nil, f.err
	}
	u, ok := f.users[name]
	if !ok {
		return nil, fmt.Errorf("%w: user %s", index.ErrNotFound, name)
	}
	return &u, nil
}

// set stores u.
func (f *fakeUsers) set(u index.User) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.users[u.Name] = u
}

// cheap are argon2id parameters that cost next to nothing, for the users of the tests whose
// subject is not the cost.
var cheap = argonParams{memory: 64, time: 1, threads: 1, saltLen: 16, keyLen: 32}

const (
	alicePassword = "alice-password-1"
	bobPassword   = "bob-password-22"
)

// clock is a settable clock.
type clock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *clock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// authFixture is an Authenticator over fakeUsers with alice (a password), bob (disabled) and
// carol (no password), a settable clock, a JSON debug log, and the verifications counted by
// the hash they were made against.
type authFixture struct {
	users    *fakeUsers
	a        *Authenticator
	clock    *clock
	logs     *bytes.Buffer
	mu       sync.Mutex
	verified []string // the hash of each verification, in order
}

func newAuthFixture(t *testing.T, o Options) *authFixture {
	t.Helper()
	f := &authFixture{
		users: &fakeUsers{users: map[string]index.User{
			"alice": {ID: 1, Name: "alice", PasswordHash: hashWith(cheap, alicePassword), Kind: KindLocal},
			"bob":   {ID: 2, Name: "bob", PasswordHash: hashWith(cheap, bobPassword), Kind: KindLocal, Disabled: true},
			"carol": {ID: 3, Name: "carol", Kind: KindLocal},
		}},
		clock: &clock{now: time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)},
		logs:  &bytes.Buffer{},
	}
	o.Index, o.Now = f.users, f.clock.Now
	o.Log = slog.New(slog.NewJSONHandler(f.logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	f.a = NewAuthenticator(o)
	f.a.verify = func(hash, password string) (bool, error) {
		f.mu.Lock()
		f.verified = append(f.verified, hash)
		f.mu.Unlock()
		return VerifyPassword(hash, password)
	}
	return f
}

// verifications returns the number of verifications so far.
func (f *authFixture) verifications() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.verified)
}

// lastVerified returns the hash of the last verification.
func (f *authFixture) lastVerified() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.verified[len(f.verified)-1]
}

// wantOK checks that user and secret authenticate as user:<user> with n more verifications.
func (f *authFixture) wantOK(t *testing.T, user, secret string, n int) {
	t.Helper()
	before := f.verifications()
	p, err := f.a.Authenticate(context.Background(), user, secret)
	if err != nil || p != (Principal{Kind: KindUser, Name: user}) {
		t.Fatalf("Authenticate(%s) = %v, %v; want user:%s", user, p, err, user)
	}
	if got := f.verifications() - before; got != n {
		t.Errorf("Authenticate(%s): %d verifications, want %d", user, got, n)
	}
}

// wantRefused checks that user and secret are refused with ErrBadCredentials after n more
// verifications.
func (f *authFixture) wantRefused(t *testing.T, user, secret string, n int) {
	t.Helper()
	before := f.verifications()
	p, err := f.a.Authenticate(context.Background(), user, secret)
	if !errors.Is(err, ErrBadCredentials) || p != (Principal{}) {
		t.Fatalf("Authenticate(%s) = %v, %v; want ErrBadCredentials", user, p, err)
	}
	if got := f.verifications() - before; got != n {
		t.Errorf("Authenticate(%s): %d verifications, want %d", user, got, n)
	}
}

// TestAuthenticate checks the outcomes: a user with the right password is its principal;
// unknown, disabled, without a password and with a wrong secret are ErrBadCredentials after
// one verification each, against the dummy hash when the user's own is not to be used; a name
// or a secret that no user can have is refused without the index and without argon2id. The log
// names the user and the reason at debug level, and never the secret.
func TestAuthenticate(t *testing.T) {
	tests := []struct {
		name, user, secret string
		ok                 bool
		verify             int    // verifications
		dummy              bool   // the verification is against the dummy hash
		reads              int64  // index reads
		reason             string // of the debug line, for a refusal
	}{
		{name: "right password", user: "alice", secret: alicePassword, ok: true, verify: 1, reads: 1},
		{name: "wrong password", user: "alice", secret: bobPassword, verify: 1, reads: 1, reason: "wrong password"},
		{name: "unknown user", user: "dave", secret: alicePassword, verify: 1, dummy: true, reads: 1, reason: "unknown user"},
		{name: "disabled", user: "bob", secret: bobPassword, verify: 1, dummy: true, reads: 1, reason: "disabled"},
		{name: "no password", user: "carol", secret: alicePassword, verify: 1, dummy: true, reads: 1, reason: "no password"},
		{name: "upper case", user: "Alice", secret: alicePassword, reason: "malformed user name"},
		{name: "empty user", user: "", secret: alicePassword, reason: "malformed user name"},
		{name: "password as the user", user: "Pass Word!", secret: alicePassword, reason: "malformed user name"},
		{name: "empty secret", user: "alice", secret: "", reason: "malformed password"},
		{name: "secret over the bound", user: "alice", secret: strings.Repeat("x", MaxPasswordLen+1), reason: "malformed password"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newAuthFixture(t, Options{})
			if tt.ok {
				f.wantOK(t, tt.user, tt.secret, tt.verify)
			} else {
				f.wantRefused(t, tt.user, tt.secret, tt.verify)
			}
			if got := f.users.reads.Load(); got != tt.reads {
				t.Errorf("index reads = %d, want %d", got, tt.reads)
			}
			if tt.dummy && f.lastVerified() != dummyHash() {
				t.Errorf("verified against %q, want the dummy hash", f.lastVerified())
			}
			logs := f.logs.String()
			if tt.secret != "" && strings.Contains(logs, tt.secret) {
				t.Errorf("the log contains the secret:\n%s", logs)
			}
			if tt.ok {
				if logs != "" {
					t.Errorf("a success is logged:\n%s", logs)
				}
				return
			}
			want := fmt.Sprintf(`"level":"DEBUG","msg":"authentication refused","user":%q,"reason":%q`, tt.user, tt.reason)
			if CheckName(tt.user) != nil { // an invalid name is not logged
				want = fmt.Sprintf(`"level":"DEBUG","msg":"authentication refused","reason":%q}`, tt.reason)
			}
			if !strings.Contains(logs, want) {
				t.Errorf("log %s does not have %s", logs, want)
			}
		})
	}
}

// TestDummyHash checks that the dummy hash costs what a new hash costs: the parameters of
// hashParams, so that an unknown user takes as long as a wrong password.
func TestDummyHash(t *testing.T) {
	if h := dummyHash(); !phc.MatchString(h) || h != dummyHash() {
		t.Errorf("dummy hash %q: want one hash with the parameters of new hashes", h)
	}
	if ok, err := VerifyPassword(dummyHash(), alicePassword); ok || err != nil {
		t.Errorf("VerifyPassword(dummy) = %v, %v; want false", ok, err)
	}
}

// TestAuthenticateCache checks the verification cache: a hit skips argon2id but not the index,
// a wrong secret is never remembered, a new hash and a disabled user count at the next request,
// entries expire after the TTL, and the oldest go beyond the size bound.
func TestAuthenticateCache(t *testing.T) {
	t.Run("hit", func(t *testing.T) {
		f := newAuthFixture(t, Options{})
		f.wantOK(t, "alice", alicePassword, 1)
		f.wantOK(t, "alice", alicePassword, 0)
		f.wantOK(t, "alice", alicePassword, 0)
		if got := f.users.reads.Load(); got != 3 {
			t.Errorf("index reads = %d, want one per request", got)
		}
	})
	t.Run("wrong secret is not remembered", func(t *testing.T) {
		f := newAuthFixture(t, Options{})
		f.wantOK(t, "alice", alicePassword, 1)
		f.wantRefused(t, "alice", bobPassword, 1)
		f.wantRefused(t, "alice", bobPassword, 1)
		f.wantRefused(t, "alice", alicePassword+"\x00", 1)
		f.wantOK(t, "alice", alicePassword, 0)
	})
	t.Run("new password", func(t *testing.T) {
		f := newAuthFixture(t, Options{})
		f.wantOK(t, "alice", alicePassword, 1)
		f.users.set(index.User{ID: 1, Name: "alice", PasswordHash: hashWith(cheap, bobPassword), Kind: KindLocal})
		f.wantRefused(t, "alice", alicePassword, 1)
		f.wantOK(t, "alice", bobPassword, 1)
		f.wantOK(t, "alice", bobPassword, 0)
	})
	t.Run("new parameters", func(t *testing.T) {
		f := newAuthFixture(t, Options{})
		f.wantOK(t, "alice", alicePassword, 1)
		f.users.set(index.User{ID: 1, Name: "alice", PasswordHash: hashWith(argonParams{memory: 128, time: 2, threads: 1, saltLen: 16, keyLen: 32}, alicePassword), Kind: KindLocal})
		f.wantOK(t, "alice", alicePassword, 1)
		f.wantOK(t, "alice", alicePassword, 0)
	})
	t.Run("disabled at the next request", func(t *testing.T) {
		f := newAuthFixture(t, Options{})
		f.wantOK(t, "alice", alicePassword, 1)
		u := f.users.users["alice"]
		u.Disabled = true
		f.users.set(u)
		f.wantRefused(t, "alice", alicePassword, 1)
		if f.lastVerified() != dummyHash() {
			t.Error("a disabled user's own hash was verified")
		}
		u.Disabled = false
		f.users.set(u)
		f.wantOK(t, "alice", alicePassword, 0) // the entry was verified against the same hash
	})
	t.Run("deleted", func(t *testing.T) {
		f := newAuthFixture(t, Options{})
		f.wantOK(t, "alice", alicePassword, 1)
		f.users.mu.Lock()
		delete(f.users.users, "alice")
		f.users.mu.Unlock()
		f.wantRefused(t, "alice", alicePassword, 1)
	})
	t.Run("TTL", func(t *testing.T) {
		f := newAuthFixture(t, Options{CacheTTL: time.Minute})
		f.wantOK(t, "alice", alicePassword, 1)
		f.clock.advance(time.Minute - time.Nanosecond)
		f.wantOK(t, "alice", alicePassword, 0)
		f.clock.advance(time.Nanosecond)
		f.wantOK(t, "alice", alicePassword, 1)
		f.clock.advance(time.Minute - time.Nanosecond)
		f.wantOK(t, "alice", alicePassword, 0)
	})
	t.Run("default TTL", func(t *testing.T) {
		f := newAuthFixture(t, Options{})
		f.wantOK(t, "alice", alicePassword, 1)
		f.clock.advance(DefaultCacheTTL - time.Second)
		f.wantOK(t, "alice", alicePassword, 0)
		f.clock.advance(time.Second)
		f.wantOK(t, "alice", alicePassword, 1)
	})
	t.Run("size bound", func(t *testing.T) {
		f := newAuthFixture(t, Options{CacheSize: 2})
		for i := range 3 {
			name := fmt.Sprintf("user%d", i)
			f.users.set(index.User{Name: name, PasswordHash: hashWith(cheap, name+"-password"), Kind: KindLocal})
			f.wantOK(t, name, name+"-password", 1)
		}
		if n := f.a.cacheLen(); n != 2 {
			t.Errorf("cache holds %d entries, want 2", n)
		}
		f.wantOK(t, "user2", "user2-password", 0)
		f.wantOK(t, "user1", "user1-password", 0)
		f.wantOK(t, "user0", "user0-password", 1) // the oldest was evicted; now user1 is
		f.wantOK(t, "user1", "user1-password", 1)
		if n := f.a.cacheLen(); n != 2 {
			t.Errorf("cache holds %d entries, want 2", n)
		}
	})
	t.Run("default size", func(t *testing.T) {
		if a := NewAuthenticator(Options{Index: &fakeUsers{}}); a.size != DefaultCacheSize || a.ttl != DefaultCacheTTL {
			t.Errorf("size %d, TTL %v; want %d and %v", a.size, a.ttl, DefaultCacheSize, DefaultCacheTTL)
		}
	})
}

// TestCacheKey checks that the key separates the user name from the secret and holds neither.
func TestCacheKey(t *testing.T) {
	if keyOf("ab", "c") == keyOf("a", "bc") {
		t.Error("ab:c and a:bc have the same key")
	}
	k := keyOf("alice", alicePassword)
	if bytes.Contains(k[:], []byte(alicePassword)) || bytes.Contains(k[:], []byte("alice")) {
		t.Error("the key holds the user name or the secret")
	}
}

// TestAuthenticateFailures checks that a failure to decide is an error of its own, not
// ErrBadCredentials: the index is down, or the stored hash is malformed.
func TestAuthenticateFailures(t *testing.T) {
	errDown := errors.New("index: database is gone")
	t.Run("index", func(t *testing.T) {
		f := newAuthFixture(t, Options{})
		f.users.err = errDown
		_, err := f.a.Authenticate(context.Background(), "alice", alicePassword)
		if !errors.Is(err, errDown) || errors.Is(err, ErrBadCredentials) || f.verifications() != 0 {
			t.Errorf("Authenticate = %v after %d verifications; want the index error, no verification", err, f.verifications())
		}
	})
	t.Run("malformed hash", func(t *testing.T) {
		f := newAuthFixture(t, Options{})
		malformed := "$argon2id$v=19$garbage"
		f.users.set(index.User{Name: "alice", PasswordHash: malformed, Kind: KindLocal})
		_, err := f.a.Authenticate(context.Background(), "alice", alicePassword)
		if !errors.Is(err, ErrMalformedHash) || errors.Is(err, ErrBadCredentials) {
			t.Errorf("Authenticate = %v; want ErrMalformedHash", err)
		}
		if strings.Contains(err.Error(), "garbage") {
			t.Errorf("error %q contains the hash", err)
		}
	})
}

// TestAuthenticateRealCost runs the real VerifyPassword with the parameters of new hashes: a
// hit is far cheaper than a verification, and an unknown user costs about what a wrong
// password costs.
func TestAuthenticateRealCost(t *testing.T) {
	hash, err := HashPassword(alicePassword)
	if err != nil {
		t.Fatal(err)
	}
	users := &fakeUsers{users: map[string]index.User{"alice": {Name: "alice", PasswordHash: hash, Kind: KindLocal}}}
	a := NewAuthenticator(Options{Index: users})
	_ = dummyHash()
	timed := func(user, secret string) time.Duration {
		start := time.Now()
		_, _ = a.Authenticate(context.Background(), user, secret)
		return time.Since(start)
	}
	miss := timed("alice", alicePassword)
	hit := timed("alice", alicePassword)
	wrong := timed("alice", bobPassword)
	unknown := timed("dave", bobPassword)
	t.Logf("verification %v, hit %v, wrong password %v, unknown user %v", miss, hit, wrong, unknown)
	if hit*10 > miss {
		t.Errorf("a hit took %v, a verification %v: the hit did not skip argon2id", hit, miss)
	}
	if unknown*3 < wrong {
		t.Errorf("an unknown user took %v, a wrong password %v", unknown, wrong)
	}
}

// TestAuthenticateConcurrent authenticates from many goroutines at once, for the race
// detector.
func TestAuthenticateConcurrent(t *testing.T) {
	f := newAuthFixture(t, Options{CacheSize: 3})
	var wg sync.WaitGroup
	for i := range 8 {
		wg.Go(func() {
			for j := range 6 {
				switch (i + j) % 3 {
				case 0:
					_, _ = f.a.Authenticate(context.Background(), "alice", alicePassword)
				case 1:
					_, _ = f.a.Authenticate(context.Background(), "alice", bobPassword)
				default:
					_, _ = f.a.Authenticate(context.Background(), fmt.Sprintf("u%d", j), alicePassword)
				}
			}
		})
	}
	wg.Wait()
	f.wantOK(t, "alice", alicePassword, 0)
}

func TestNewAuthenticatorWithoutIndex(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("NewAuthenticator without an index did not panic")
		}
	}()
	NewAuthenticator(Options{})
}

// TestAuthenticatorDefaults checks the logger and the clock of a zero Options.
func TestAuthenticatorDefaults(t *testing.T) {
	a := NewAuthenticator(Options{Index: &fakeUsers{}})
	if a.log == nil || a.now == nil {
		t.Error("NewAuthenticator left the logger or the clock nil")
	}
}
