package auth

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/lajstrom/lajstrom/internal/index"
)

func TestThrottleTake(t *testing.T) {
	c := &clock{now: time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)}
	th := newThrottle(2, time.Second, c.Now)
	steps := []struct {
		advance time.Duration
		want    time.Duration
	}{
		{0, 0}, {0, 0},
		{0, time.Second}, // refused, not counted
		{600 * time.Millisecond, 400 * time.Millisecond},
		{400 * time.Millisecond, 0}, // a new window
		{0, 0},
		{0, time.Second},
	}
	for i, s := range steps {
		c.advance(s.advance)
		if got := th.take("k"); got != s.want {
			t.Errorf("step %d: take = %v, want %v", i, got, s.want)
		}
	}
}

// authenticateFrom authenticates user and secret from ctx and returns the number of
// verifications and of user reads it cost, and the error.
func (f *authFixture) authenticateFrom(ctx context.Context, user, secret string) (verified, reads int, err error) {
	v, r := f.verifications(), f.users.reads.Load()
	_, _, err = f.a.Authenticate(ctx, user, secret)
	return f.verifications() - v, int(f.users.reads.Load() - r), err
}

// TestAddressThrottle checks the failed logins of a client address. X-04 from one address: alice
// under throttle does not throttle the address, and dave logs in from it. Then failures across
// other names make AddressFailures, and every password from the address is refused with the
// throttle "address" before the user is read, while another address and a context without an
// address go on. An IPv6 address counts as its /64.
func TestAddressThrottle(t *testing.T) {
	f := newAuthFixture(t, Options{})
	f.users.set(index.User{ID: 4, UID: testUID(4), Name: "dave", PasswordHash: hashWith(cheap, bobPassword), Kind: KindLocal})
	from := WithClientAddress(context.Background(), "2001:db8:1:2::7")
	for i := range 20 {
		want := ErrBadCredentials
		if i >= LoginFailures {
			want = ErrThrottled
		}
		if _, _, err := f.authenticateFrom(from, "alice", "wrong"); !errors.Is(err, want) {
			t.Fatalf("wrong password %d from one address: %v", i+1, err)
		}
	}
	if _, _, err := f.authenticateFrom(from, "dave", bobPassword); err != nil {
		t.Fatalf("dave from the address of alice's 20 failures: %v", err)
	}

	near := WithClientAddress(context.Background(), "2001:db8:1:2::8") // the same /64
	for i := range AddressFailures - LoginFailures {
		if _, _, err := f.authenticateFrom(near, fmt.Sprintf("u%d", i), "x-password"); !errors.Is(err, ErrBadCredentials) {
			t.Fatalf("unknown user %d: %v", i, err)
		}
	}
	f.clock.advance(time.Second)
	verified, reads, err := f.authenticateFrom(from, "dave", bobPassword)
	want := &ThrottledError{Throttle: "address", RetryAfter: LoginWindow - time.Second}
	if te := (*ThrottledError)(nil); !errors.As(err, &te) || *te != *want || verified+reads != 0 {
		t.Errorf("dave from a throttled /64: %v, %d verifications, %d reads; want %v and none", err, verified, reads, want)
	}
	for _, ctx := range []context.Context{WithClientAddress(context.Background(), "2001:db8:1:3::7"),
		WithClientAddress(context.Background(), "198.51.100.7"), context.Background()} {
		if _, _, err := f.authenticateFrom(ctx, "dave", bobPassword); err != nil {
			t.Errorf("dave from elsewhere: %v", err)
		}
	}
	f.clock.advance(LoginWindow)
	if _, _, err := f.authenticateFrom(from, "dave", bobPassword); err != nil {
		t.Errorf("dave after the window: %v", err)
	}
}

// TestAllowTokenRequest checks the token requests of a client address: TokenRequests of 200 in
// one instant are allowed, the rest refused with the throttle "token" for TokenWindow, until the
// window has passed; another address and a context without one are not counted.
func TestAllowTokenRequest(t *testing.T) {
	f := newAuthFixture(t, Options{})
	ctx := WithClientAddress(context.Background(), "198.51.100.7")
	allowed := 0
	for range 200 {
		err := f.a.AllowTokenRequest(ctx)
		var te *ThrottledError
		switch {
		case err == nil:
			allowed++
		case !errors.As(err, &te) || te.Throttle != "token" || te.RetryAfter != TokenWindow:
			t.Fatalf("AllowTokenRequest = %v, want the token throttle for %v", err, TokenWindow)
		}
	}
	if allowed != TokenRequests {
		t.Errorf("%d of 200 allowed, want %d", allowed, TokenRequests)
	}
	for range 200 {
		if err := f.a.AllowTokenRequest(context.Background()); err != nil {
			t.Fatalf("AllowTokenRequest without an address: %v", err)
		}
	}
	if err := f.a.AllowTokenRequest(WithClientAddress(context.Background(), "198.51.100.8")); err != nil {
		t.Errorf("AllowTokenRequest from another address: %v", err)
	}
	f.clock.advance(TokenWindow)
	if err := f.a.AllowTokenRequest(ctx); err != nil {
		t.Errorf("AllowTokenRequest after the window: %v", err)
	}
}
