package auth

import (
	"errors"
	"fmt"
	"time"
)

// The throttle of failed logins (docs/auth.md, "Failed logins"). Constants, not settings: a
// knob comes when someone needs one.
const (
	// LoginFailures is how many failed password verifications an account may have within
	// LoginWindow. Beyond it, the account's passwords are refused with ErrThrottled, a correct
	// one too, until the window has passed.
	LoginFailures = 10
	// LoginWindow is the window of LoginFailures. It starts with the first failure that
	// finds no window open.
	LoginWindow = time.Minute
	// throttleSize bounds the keys a throttle remembers, the one written longest ago dropped
	// first.
	throttleSize = 10000
)

// ErrThrottled means that the credentials were not checked because their account had too many
// failed logins lately. Errors that wrap it are a *ThrottledError, which says how long to wait.
var ErrThrottled = errors.New("auth: too many failed logins")

// ThrottledError is a refusal of the throttle.
type ThrottledError struct {
	// Throttle is the throttle that refused: "account", for the failed logins of an account.
	Throttle string
	// Account is the account under throttle: user:<uid>, or "unknown" for a name that no user
	// has. It is for the server's log, never for the answer.
	Account string
	// RetryAfter is how long until the window has passed.
	RetryAfter time.Duration
}

func (e *ThrottledError) Error() string {
	return fmt.Sprintf("%s: %s %s, retry after %s", ErrThrottled, e.Throttle, e.Account, e.RetryAfter)
}

// Is makes errors.Is(err, ErrThrottled) true.
func (e *ThrottledError) Is(target error) bool { return target == ErrThrottled }

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
