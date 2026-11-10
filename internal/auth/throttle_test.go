package auth

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/lajstrom/lajstrom/internal/index"
)

func TestThrottle(t *testing.T) {
	c := &clock{now: time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)}
	th := newThrottle(3, time.Minute, c.Now)
	steps := []struct {
		advance time.Duration
		add     bool
		want    time.Duration // wait after the step
	}{
		{0, true, 0},
		{10 * time.Second, true, 0},
		{10 * time.Second, true, 40 * time.Second}, // the third in the window opened at 0
		{30 * time.Second, false, 10 * time.Second},
		{10 * time.Second, false, 0}, // the window has closed
		{0, true, 0},                 // a new window, one event
		{0, true, 0},
		{0, true, time.Minute},
	}
	for i, s := range steps {
		c.advance(s.advance)
		if s.add {
			th.add("k")
		}
		if got := th.wait("k"); got != s.want {
			t.Errorf("step %d: wait = %v, want %v", i, got, s.want)
		}
	}
	if got := th.wait("other"); got != 0 {
		t.Errorf("wait of a key never seen = %v", got)
	}
}

// TestLoginThrottle is X-04 of docs/acceptance.md on one replica: 20 wrong passwords for alice,
// the 11th and later refused with ErrThrottled and no verification, while the right password of
// another user still works; within the window alice's right password is refused too, a
// remembered one included, and after it it works.
func TestLoginThrottle(t *testing.T) {
	f := newTokenFixture(t, Options{})
	f.users.set(index.User{ID: 4, UID: testUID(4), Name: "dave", PasswordHash: hashWith(cheap, bobPassword), Kind: KindLocal})
	f.wantOK(t, "alice", alicePassword, 1) // remembered from now on
	alice := userP("alice").String()
	for i := 1; i <= 20; i++ {
		f.clock.advance(time.Second)
		if i <= LoginFailures {
			f.wantRefused(t, "alice", "wrong", 1)
			continue
		}
		f.wantThrottled(t, "alice", "wrong", alice, LoginWindow-time.Duration(i-1)*time.Second)
	}
	f.wantOK(t, "dave", bobPassword, 1)
	f.wantThrottled(t, "alice", alicePassword, alice, LoginWindow-19*time.Second)
	p, _, err := f.a.Authenticate(context.Background(), "alice", f.secrets["alice"])
	if err != nil || p.String() != alice {
		t.Errorf("a personal access token of alice under throttle = %v, %v; want %s", p, err, alice)
	}

	f.clock.advance(LoginWindow - 19*time.Second)
	f.wantOK(t, "alice", alicePassword, 0)
	f.wantRefused(t, "alice", "wrong", 1) // a new window
	f.wantOK(t, "alice", alicePassword, 0)
}

// TestLoginThrottleAlike checks that the throttle does not tell users apart: a wrong password,
// a disabled user, a user without a password and a name that no user has are each refused
// LoginFailures times after one verification, then throttled for the same time without one.
// The account of the log is the uid, "unknown" for a name, and a rename keeps the throttle.
func TestLoginThrottleAlike(t *testing.T) {
	tests := []struct{ user, secret, account string }{
		{"alice", "wrong", userP("alice").String()},
		{"bob", bobPassword, userP("bob").String()},
		{"carol", "anything", userP("carol").String()},
		{"mallory", "anything", "unknown"},
	}
	for _, tt := range tests {
		t.Run(tt.user, func(t *testing.T) {
			f := newAuthFixture(t, Options{})
			for range LoginFailures {
				f.wantRefused(t, tt.user, tt.secret, 1)
			}
			f.clock.advance(time.Second)
			f.wantThrottled(t, tt.user, tt.secret, tt.account, LoginWindow-time.Second)
			if tt.user == "alice" {
				f.users.rename("alice", "alice2")
				f.wantThrottled(t, "alice2", alicePassword, tt.account, LoginWindow-time.Second)
				f.wantRefused(t, "alice", alicePassword, 1) // a name no user has now: its own window
			}
		})
	}
}

// wantThrottled checks that user and secret are refused with a *ThrottledError of account and
// retryAfter, without a verification.
func (f *authFixture) wantThrottled(t *testing.T, user, secret, account string, retryAfter time.Duration) {
	t.Helper()
	before := f.verifications()
	p, _, err := f.a.Authenticate(context.Background(), user, secret)
	var te *ThrottledError
	if !errors.As(err, &te) || !errors.Is(err, ErrThrottled) || p != (Principal{}) {
		t.Fatalf("Authenticate(%s) = %v, %v; want ErrThrottled", user, p, err)
	}
	if te.Account != account || te.RetryAfter != retryAfter {
		t.Errorf("Authenticate(%s): throttled %s for %v, want %s for %v", user, te.Account, te.RetryAfter, account, retryAfter)
	}
	if got := f.verifications() - before; got != 0 {
		t.Errorf("Authenticate(%s) under throttle: %d verifications", user, got)
	}
}

// TestThrottleEviction checks what a throttle that holds its size of keys drops to make room
// for a new key (#251): the windows that have closed, else the open window opened first that
// is not full, and never a full one. When every window is open and full, the new key is not
// tracked: the throttle fails open, as before.
func TestThrottleEviction(t *testing.T) {
	type step struct {
		advance time.Duration
		keys    []string // one event each, in order
	}
	names := make([]string, throttleSize)
	for i := range names {
		names[i] = fmt.Sprintf("name:nobody-%d", i)
	}
	tests := []struct {
		name      string
		size      int
		budget    int
		steps     []step
		throttled []string // under throttle after the steps
		kept      []string // tracked after the steps
		gone      []string // not tracked after the steps
	}{
		{name: "closed windows first", size: 3, budget: 2,
			steps: []step{{0, []string{"a", "a", "b"}}, {30 * time.Second, []string{"c"}}, {31 * time.Second, []string{"d"}}},
			kept:  []string{"c", "d"}, gone: []string{"a", "b"}},
		{name: "an open window that is not full before a full one", size: 3, budget: 2,
			steps:     []step{{0, []string{"a", "a"}}, {time.Second, []string{"b"}}, {time.Second, []string{"c"}}, {time.Second, []string{"d"}}},
			throttled: []string{"a"}, kept: []string{"a", "c", "d"}, gone: []string{"b"}},
		{name: "of the open windows that are not full, the one opened first", size: 3, budget: 3,
			steps: []step{{0, []string{"a"}}, {time.Second, []string{"b"}}, {time.Second, []string{"a"}}, {time.Second, []string{"c"}}, {time.Second, []string{"d"}}},
			kept:  []string{"b", "c", "d"}, gone: []string{"a"}},
		{name: "every window open and full: the new key is not tracked", size: 3, budget: 2,
			steps:     []step{{0, []string{"a", "a", "b", "b", "c", "c"}}, {time.Second, []string{"d", "d", "d"}}},
			throttled: []string{"a", "b", "c"}, gone: []string{"d"}},
		{name: "a throttled account through a failure of each of throttleSize other names", size: throttleSize, budget: LoginFailures,
			steps:     []step{{0, slices.Repeat([]string{"user:x"}, LoginFailures)}, {time.Second, names}},
			throttled: []string{"user:x"}, kept: names[1:], gone: names[:1]},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := &clock{now: time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)}
			th := newThrottle(tt.budget, time.Minute, c.Now)
			th.size = tt.size
			for _, s := range tt.steps {
				c.advance(s.advance)
				for _, k := range s.keys {
					th.add(k)
				}
			}
			for _, k := range tt.throttled {
				if th.wait(k) == 0 {
					t.Errorf("%s is not throttled", k)
				}
			}
			for _, k := range tt.kept {
				if !th.tracked(k) {
					t.Errorf("%s is not tracked", k)
					break
				}
			}
			for _, k := range tt.gone {
				if th.tracked(k) {
					t.Errorf("%s is still tracked", k)
				}
			}
			if n := th.len(); n > tt.size {
				t.Errorf("%d keys, more than %d", n, tt.size)
			}
		})
	}
}

// tracked reports whether the throttle has a window of key.
func (t *throttle) tracked(key string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	_, ok := t.keys[key]
	return ok
}

// TestLoginThrottleFlood is A-2 of the threat model (#251): alice under throttle stays under
// throttle through a failed login of each of throttleSize names that no user has, from a
// context without a client address, so without the per-address throttle, as a server without
// server.trustedProxies has it.
func TestLoginThrottleFlood(t *testing.T) {
	f := newAuthFixture(t, Options{})
	verify := f.a.verify
	f.a.verify = func(hash, password string) (bool, error) {
		if hash == dummyHash() {
			return false, nil // the flood without the cost of argon2id
		}
		return verify(hash, password)
	}
	for range LoginFailures {
		f.wantRefused(t, "alice", "wrong", 1)
	}
	alice := userP("alice").String()
	f.wantThrottled(t, "alice", alicePassword, alice, LoginWindow)
	for i := range throttleSize {
		f.clock.advance(time.Millisecond)
		if _, _, err := f.a.Authenticate(context.Background(), fmt.Sprintf("nobody-%d", i), "wrong"); !errors.Is(err, ErrBadCredentials) {
			t.Fatalf("Authenticate(nobody-%d) = %v, want ErrBadCredentials", i, err)
		}
	}
	f.wantThrottled(t, "alice", alicePassword, alice, LoginWindow-throttleSize*time.Millisecond)
}
