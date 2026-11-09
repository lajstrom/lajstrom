package auth

import (
	"context"
	"errors"
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
