package auth

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
)

// TestLoginThrottleParallel checks that tries in parallel do not overrun the budgets: of 50
// wrong passwords for one account at once, exactly LoginFailures are verified and the rest are
// throttled; of 50 unknown names from one address at once, exactly AddressFailures; and 20
// right passwords at once, with nothing remembered yet, cost one verification and all succeed.
func TestLoginThrottleParallel(t *testing.T) {
	tests := []struct {
		name       string
		ctx        context.Context
		user       func(i int) string
		secret     string
		verified   int
		throttled  int
		authorized int
	}{
		{name: "one account", ctx: context.Background(), user: func(int) string { return "alice" }, secret: "wrong",
			verified: LoginFailures, throttled: 50 - LoginFailures},
		{name: "one address", ctx: WithClientAddress(context.Background(), "198.51.100.7"),
			user: func(i int) string { return fmt.Sprintf("u%d", i) }, secret: "x-password",
			verified: AddressFailures, throttled: 50 - AddressFailures},
		{name: "the right password", ctx: context.Background(), user: func(int) string { return "alice" }, secret: alicePassword,
			verified: 1, authorized: 20},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newAuthFixture(t, Options{})
			n := 50
			if tt.authorized > 0 {
				n = tt.authorized
			}
			var mu sync.Mutex
			throttled, authorized := 0, 0
			var wg sync.WaitGroup
			for i := range n {
				wg.Go(func() {
					_, _, err := f.a.Authenticate(tt.ctx, tt.user(i), tt.secret)
					mu.Lock()
					defer mu.Unlock()
					switch {
					case err == nil:
						authorized++
					case errors.Is(err, ErrThrottled):
						throttled++
					case !errors.Is(err, ErrBadCredentials):
						t.Errorf("Authenticate: %v", err)
					}
				})
			}
			wg.Wait()
			if got := f.verifications(); got != tt.verified || throttled != tt.throttled || authorized != tt.authorized {
				t.Errorf("%d verifications, %d throttled, %d authorized; want %d, %d, %d",
					got, throttled, authorized, tt.verified, tt.throttled, tt.authorized)
			}
		})
	}
}
