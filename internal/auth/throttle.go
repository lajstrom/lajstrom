package auth

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"time"
)

// The throttle of failed logins (docs/auth.md, "Failed logins"). Constants, not settings: a
// knob comes when someone needs one.
const (
	// LoginFailures is how many failed password verifications an account may have within
	// LoginWindow. Beyond it, the account's passwords are refused with ErrThrottled, a correct
	// one too, until the window has passed.
	LoginFailures = 10
	// LoginWindow is the window of LoginFailures and of AddressFailures. It starts with the
	// first failure that finds no window open.
	LoginWindow = time.Minute
	// AddressFailures is how many failed password verifications a client address may have
	// within LoginWindow, whatever the accounts: a wide guess from one address is bounded too.
	// It is above LoginFailures, so that one account under throttle does not throttle its
	// address.
	AddressFailures = 30
	// TokenRequests is how many requests to the token endpoint (/v2/token) a client address
	// may make within TokenWindow, with credentials or without.
	TokenRequests = 100
	// TokenWindow is the window of TokenRequests.
	TokenWindow = time.Second
	// throttleSize bounds the keys a throttle remembers, the one written longest ago dropped
	// first.
	throttleSize = 10000
)

// ErrThrottled means that the credentials were not checked because their account or the client
// address had too many failed logins lately, or that the client address made too many requests
// to the token endpoint. Errors that wrap it are a *ThrottledError, which says how long to wait.
var ErrThrottled = errors.New("auth: throttled")

// ThrottledError is a refusal of the throttle.
type ThrottledError struct {
	// Throttle is the throttle that refused: "account" or "address", for the failed logins of
	// an account or of the client address, or "token", for the token requests of the address.
	Throttle string
	// Account is the account under throttle: user:<uid>, or "unknown" for a name that no user
	// has; empty for the throttles of the address. It is for the server's log, never for the
	// answer.
	Account string
	// RetryAfter is how long until the window has passed.
	RetryAfter time.Duration
}

func (e *ThrottledError) Error() string {
	return fmt.Sprintf("%s: %s %s, retry after %s", ErrThrottled, e.Throttle, e.Account, e.RetryAfter)
}

// Is makes errors.Is(err, ErrThrottled) true.
func (e *ThrottledError) Is(target error) bool { return target == ErrThrottled }

// addressKey is the context key of the client address of the per-address throttles.
type addressKey struct{}

// WithClientAddress returns a copy of ctx in which addr, the client address of the request,
// counts in the per-address throttles: the failed logins (AddressFailures) and the token
// requests (TokenRequests). Without it, or with "", the request counts in no per-address
// throttle: the server leaves it out when it cannot tell clients apart (ADR 0011).
func WithClientAddress(ctx context.Context, addr string) context.Context {
	return context.WithValue(ctx, addressKey{}, addr)
}

// throttleAddress returns the key of the client address of ctx in the per-address throttles,
// "" for none. An IPv6 address counts as its /64, which one client usually has whole.
func throttleAddress(ctx context.Context) string {
	addr, _ := ctx.Value(addressKey{}).(string)
	if a, err := netip.ParseAddr(addr); err == nil && a.Is6() && !a.Is4In6() {
		p, _ := a.WithZone("").Prefix(64)
		return p.String()
	}
	return addr
}

// AllowTokenRequest counts a request to the token endpoint from the client address of ctx
// (WithClientAddress), and returns nil, or a *ThrottledError of the throttle "token" when the
// address has made TokenRequests of them within TokenWindow; the refused ones do not count.
// It allows every request of a context without an address.
func (a *Authenticator) AllowTokenRequest(ctx context.Context) error {
	addr := throttleAddress(ctx)
	if addr == "" {
		return nil
	}
	if wait := a.tokens.take(addr); wait > 0 {
		return &ThrottledError{Throttle: "token", RetryAfter: wait}
	}
	return nil
}

// throttle counts events per key within a window that opens with the first event of the key
// and lasts window; a key with budget events in its open window is throttled until it closes.
// It remembers at most throttleSize keys, in memory, per replica. Its methods may be called
// concurrently.
type throttle struct {
	budget int
	window time.Duration
	now    func() time.Time
	keys   *bounded[string, throttleWindow]
}

// throttleWindow is the window of a key: when it opened and the events since.
type throttleWindow struct {
	start time.Time
	n     int
}

func newThrottle(budget int, window time.Duration, now func() time.Time) *throttle {
	return &throttle{budget: budget, window: window, now: now, keys: newBounded[string, throttleWindow](throttleSize)}
}

// wait returns how long key is throttled: until its window closes when the window holds budget
// events, zero otherwise.
func (t *throttle) wait(key string) time.Duration {
	w, ok := t.keys.get(key)
	if !ok || w.n < t.budget {
		return 0
	}
	return max(w.start.Add(t.window).Sub(t.now()), 0)
}

// take counts an event of key and returns zero when key is not throttled; when it is, it
// counts nothing and returns how long it is throttled.
func (t *throttle) take(key string) (wait time.Duration) {
	now := t.now()
	t.keys.update(key, func(w throttleWindow, ok bool) throttleWindow {
		switch {
		case !ok || !now.Before(w.start.Add(t.window)):
			return throttleWindow{start: now, n: 1}
		case w.n >= t.budget:
			wait = w.start.Add(t.window).Sub(now)
			return w
		}
		w.n++
		return w
	})
	return wait
}

// add counts an event of key, in its window, or in a new one when its window has closed.
func (t *throttle) add(key string) {
	now := t.now()
	t.keys.update(key, func(w throttleWindow, ok bool) throttleWindow {
		if !ok || !now.Before(w.start.Add(t.window)) {
			return throttleWindow{start: now, n: 1}
		}
		w.n++
		return w
	})
}
