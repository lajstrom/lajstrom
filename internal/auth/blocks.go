package auth

import (
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/lajstrom/lajstrom/internal/index"
)

// ActionLoginBlock is the audit action of a throttle of failed logins that trips: the account
// or the client address whose credentials are refused unchecked until the window closes
// (docs/spec/rbac.md §5, #1284), and a lock of the lockout (#1280), the limit LimitLockout.
const ActionLoginBlock = "login.block"

// The limits of a login.block row: the throttle that tripped, or the lockout.
const (
	LimitAccount   = "account"
	LimitAddress   = "address"
	LimitBootstrap = "bootstrap"
	LimitLockout   = "lockout"
)

// BlockRows is how many login.block rows a replica writes within BlockWindow, whatever the keys:
// beyond, a trip is counted, and the next row written carries the count as suppressed. A flood
// from many addresses costs the database at most one row a second; the throttled lines and
// lajstrom_auth_throttled_total have every one.
const (
	BlockRows   = 60
	BlockWindow = time.Minute
)

// blockWriteTimeout bounds the write of a login.block row, which outlives the request that
// tripped the throttle.
const blockWriteTimeout = 5 * time.Second

// Blocks writes the login.block rows of a replica: the Authenticator's and the Bootstrap's
// (docs/spec/rbac.md §5). Give both the same one, so that BlockRows bounds them together; a nil
// *Blocks writes nothing. Create it as a struct literal; its methods may be called concurrently.
type Blocks struct {
	// Index is where the rows are written, each in a transaction of its own; required.
	Index index.Store
	// Log receives a row that cannot be written, at error; nil means slog.Default().
	Log *slog.Logger
	// Now is the clock of the rows and of BlockWindow; nil means time.Now.
	Now func() time.Time

	mu         sync.Mutex
	start      time.Time // the window of BlockRows
	n          int       // rows written in it
	suppressed int       // trips not written since the last row
}

// block is a throttle that tripped, or a lock of the lockout, which has no window.
type block struct {
	limit, credential, subject string
	failures                   int
	window                     time.Duration
	until                      time.Time
}

// record writes the row of b, unless the replica wrote BlockRows within BlockWindow; then it only
// counts it. A row that cannot be written is logged at error and changes no answer.
func (bs *Blocks) record(ctx context.Context, b block) {
	if bs == nil {
		return
	}
	suppressed, ok := bs.take()
	if !ok {
		return
	}
	r := RequestFrom(ctx)
	ev := b.event(ctx, bs.now(), suppressed)
	wctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), blockWriteTimeout)
	defer cancel()
	if err := bs.Index.AppendAudit(wctx, &ev); err != nil {
		bs.log().ErrorContext(ctx, "auth: the audit row of a blocked login not written", "limit", b.limit, "subject", b.subject,
			"error", err.Error(), "requestId", r.ID)
	}
}

// event is the login.block row of b at at, by anonymous, with the client address and the request
// id of ctx (WithRequest) and the count of the trips held back before it, suppressed, when it is
// not 0; without a window when b has none.
func (b block) event(ctx context.Context, at time.Time, suppressed int) index.AuditEvent {
	r := RequestFrom(ctx)
	details := map[string]any{"limit": b.limit, "credential": b.credential, "failures": b.failures,
		"until": b.until.UTC().Format(time.RFC3339)}
	if b.window > 0 {
		details["window"] = b.window.String()
	}
	if r.ID != "" {
		details["requestId"] = r.ID
	}
	if suppressed > 0 {
		details["suppressed"] = suppressed
	}
	raw, _ := json.Marshal(details) // strings and ints: never an error
	return index.AuditEvent{At: at, Principal: Anonymous.String(), Action: ActionLoginBlock, Subject: b.subject,
		IP: r.Address, Details: raw, State: index.AuditOK}
}

// take reports whether a row may be written now and how many trips were held back since the last
// row, which it then forgets.
func (bs *Blocks) take() (int, bool) {
	now := bs.now()
	bs.mu.Lock()
	defer bs.mu.Unlock()
	if now.Sub(bs.start) >= BlockWindow {
		bs.start, bs.n = now, 0
	}
	if bs.n >= BlockRows {
		bs.suppressed++
		return 0, false
	}
	bs.n++
	s := bs.suppressed
	bs.suppressed = 0
	return s, true
}

func (bs *Blocks) now() time.Time {
	if bs.Now == nil {
		return time.Now()
	}
	return bs.Now()
}

func (bs *Blocks) log() *slog.Logger {
	if bs.Log == nil {
		return slog.Default()
	}
	return bs.Log
}

// accountSubject is the subject of the row of an account's throttle, its key: user:<uid>, or
// unknown for a name that no user has (name:<username>), which is never written.
func accountSubject(account string) string {
	if strings.HasPrefix(account, "name:") {
		return AccountUnknown
	}
	return account
}
