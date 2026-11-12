package auth

import (
	"container/list"
	"context"
	"errors"
	"fmt"
	"net/netip"
	"sync"
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
	// throttleSize bounds the keys a throttle remembers. A new key takes the place of a window
	// that has closed, else of the open window opened first that does not throttle its key
	// (throttle.makeRoom).
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
// counts in the per-address throttles: the failed logins (AddressFailures), the token
// requests (TokenRequests) and the wrong bootstrap tokens (BootstrapFailures). Without it, or with "", the request counts in no per-address
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
// It remembers at most size keys (throttleSize), in memory, per replica; makeRoom says which
// one a new key replaces. Its methods may be called concurrently.
type throttle struct {
	budget int
	window time.Duration
	size   int
	now    func() time.Time

	mu    sync.Mutex
	keys  map[string]*list.Element // of *throttleWindow
	order *list.List               // the windows by the time they opened, the first first
}

// throttleWindow is the window of a key: when it opened and the events since.
type throttleWindow struct {
	key   string
	start time.Time
	n     int
}

func newThrottle(budget int, window time.Duration, now func() time.Time) *throttle {
	return &throttle{budget: budget, window: window, size: throttleSize, now: now,
		keys: map[string]*list.Element{}, order: list.New()}
}

// wait returns how long key is throttled: until its window closes when the window holds budget
// events, zero otherwise.
func (t *throttle) wait(key string) time.Duration {
	t.mu.Lock()
	defer t.mu.Unlock()
	w := t.lookup(key)
	if w == nil || w.n < t.budget {
		return 0
	}
	return max(w.start.Add(t.window).Sub(t.now()), 0)
}

// take counts an event of key and returns zero when key is not throttled; when it is, it
// counts nothing and returns how long it is throttled.
func (t *throttle) take(key string) time.Duration {
	now := t.now()
	t.mu.Lock()
	defer t.mu.Unlock()
	switch w := t.lookup(key); {
	case w == nil || t.closed(w, now):
		t.open(key, now)
	case w.n >= t.budget:
		return w.start.Add(t.window).Sub(now)
	default:
		w.n++
	}
	return 0
}

// add counts an event of key, in its window, or in a new one when its window has closed.
func (t *throttle) add(key string) {
	now := t.now()
	t.mu.Lock()
	defer t.mu.Unlock()
	if w := t.lookup(key); w != nil && !t.closed(w, now) {
		w.n++
		return
	}
	t.open(key, now)
}

// lookup returns the window of key, nil when it has none; t.mu is held.
func (t *throttle) lookup(key string) *throttleWindow {
	if el, ok := t.keys[key]; ok {
		return el.Value.(*throttleWindow)
	}
	return nil
}

// closed reports whether w has closed at now.
func (t *throttle) closed(w *throttleWindow, now time.Time) bool {
	return !now.Before(w.start.Add(t.window))
}

// open opens a window of key at now with one event, in place of its closed one. A new key
// needs room; when makeRoom finds none, the key is not tracked. t.mu is held.
func (t *throttle) open(key string, now time.Time) {
	if el, ok := t.keys[key]; ok {
		t.order.Remove(el)
	} else if len(t.keys) >= t.size && !t.makeRoom(now) {
		return
	}
	t.keys[key] = t.order.PushBack(&throttleWindow{key: key, start: now, n: 1})
}

// makeRoom drops the windows that have closed, or, when none has, the open window opened first
// that does not hold budget events, and reports whether it dropped one. It never drops a
// window that throttles its key while the window is open: failures of other keys must not
// end a throttle (threat model A-2, #251). When every window is open and full, nothing is
// dropped and the new key goes untracked: the throttle fails open. Reaching that takes size ×
// budget events within one window, 100 000 failed logins a minute on one replica (each an
// argon2id verification), and failing closed would let them lock every other account out.
// The windows are in the order they opened and all last as long, so the closed ones come
// first. t.mu is held.
func (t *throttle) makeRoom(now time.Time) bool {
	dropped := false
	for el := t.order.Front(); el != nil && t.closed(el.Value.(*throttleWindow), now); el = t.order.Front() {
		t.drop(el)
		dropped = true
	}
	if dropped {
		return true
	}
	for el := t.order.Front(); el != nil; el = el.Next() {
		if el.Value.(*throttleWindow).n < t.budget {
			t.drop(el)
			return true
		}
	}
	return false
}

// drop forgets the window el; t.mu is held.
func (t *throttle) drop(el *list.Element) {
	t.order.Remove(el)
	delete(t.keys, el.Value.(*throttleWindow).key)
}

// len returns the number of keys.
func (t *throttle) len() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return len(t.keys)
}
