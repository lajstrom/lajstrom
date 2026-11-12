package facade

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/lajstrom/lajstrom/internal/index"
	"github.com/lajstrom/lajstrom/internal/metrics"
)

// The live upload sessions of a principal (U-4 of the threat model, #246). Each session holds a
// backend session and a row for uploadTTL after its last request, so a writer that opens them
// without end fills the registry's upload area and the index. Clients do not cancel the sessions
// of a failed push (docker, containerd, go-containerregistry and oras send no DELETE) and each
// retry opens new ones, so what counts is a live session: one with a request running, or whose
// last request ended less than liveWindow ago. An idle session takes no slot and stays resumable
// until it expires; a request on a session that exists is never refused.
//
// A replica refuses a new session beyond Options.UploadSessionsPerRepo live sessions of the
// principal in the repo, or Options.UploadSessionsPerPrincipal in all repos: per replica, as the
// throttles of failed logins are, and without a write to the index. It counts the sessions it has
// answered a request of. One that another replica ended counts here until a bound is reached;
// then the replica asks the index about the sessions it counts toward that bound and forgets
// those that are gone. The principal is the request's: a user, whose personal access tokens act
// as the user, or a deploy token. The anonymous principal never writes (rbac.md §1); without an
// Authorizer, in the tests, it is one principal like the others.

const (
	// liveWindow is how long a session counts after its last request ended.
	liveWindow = 5 * time.Minute
	// recheckAfter is how long what the index said of a session counted at a bound holds, before
	// a refused session asks again: a principal at its bound does not make each POST read every
	// row it holds.
	recheckAfter = 10 * time.Second
	// retryRunning is the Retry-After of a refused session while a request on a session counted
	// runs: it may be the closing PUT.
	retryRunning = 5 * time.Second
	// maxUploadPrincipals bounds the principals counted at once, as the throttles bound their
	// keys. When it is reached and none can be forgotten, a new one is not counted.
	maxUploadPrincipals = 10000
)

// liveUpload is an upload session that a replica counts.
type liveUpload struct {
	repoID  int64
	running int       // requests on it that run on this replica
	last    time.Time // when its last request ended: here, or on another replica as its row says
	checked time.Time // when the index was last asked about it
}

// live reports whether u still counts at now.
func (u *liveUpload) live(now time.Time) bool { return u.running > 0 || now.Sub(u.last) < liveWindow }

// liveUploads counts the live upload sessions of each principal on this replica. perRepo and
// perPrincipal are the bounds; zero or less turns one off, and both off counts nothing.
type liveUploads struct {
	perRepo, perPrincipal int

	mu   sync.Mutex
	held map[string]map[string]*liveUpload // principal → upload id → session
}

// sessionsFull is a bound that a new session would pass: the repo's when perRepo, else the
// principal's in all repos; retry is how long until one of the sessions it counts stops counting.
type sessionsFull struct {
	perRepo bool
	limit   int
	retry   time.Duration
}

// off reports whether no bound is set.
func (q *liveUploads) off() bool { return q.perRepo <= 0 && q.perPrincipal <= 0 }

// admit counts the new upload id of principal in repoID as a running request, or returns the
// bound that the principal's live sessions have reached.
func (q *liveUploads) admit(principal string, repoID int64, id string, now time.Time) *sessionsFull {
	q.mu.Lock()
	defer q.mu.Unlock()
	ids := q.sessionsOf(principal, now)
	if ids == nil {
		return nil // fail open: the bound of the memory wins over the bound of the principal
	}
	if full := q.full(ids, repoID, now); full != nil {
		return full
	}
	ids[id] = &liveUpload{repoID: repoID, running: 1, last: now}
	return nil
}

// full forgets the sessions of ids that no longer count, and returns the bound that the others
// reach in repoID, or nil. The caller holds the lock.
func (q *liveUploads) full(ids map[string]*liveUpload, repoID int64, now time.Time) *sessionsFull {
	inRepo, all := 0, 0
	for id, u := range ids {
		if !u.live(now) {
			delete(ids, id)
			continue
		}
		all++
		if u.repoID == repoID {
			inRepo++
		}
	}
	f := &sessionsFull{retry: liveWindow}
	switch {
	case q.perRepo > 0 && inRepo >= q.perRepo:
		f.perRepo, f.limit = true, q.perRepo
	case q.perPrincipal > 0 && all >= q.perPrincipal:
		f.limit = q.perPrincipal
	default:
		return nil
	}
	for _, u := range ids {
		if f.perRepo && u.repoID != repoID {
			continue
		}
		wait := retryRunning
		if u.running == 0 {
			wait = u.last.Add(liveWindow).Sub(now)
		}
		f.retry = min(f.retry, wait)
	}
	return f
}

// sessionsOf returns the sessions counted for principal, a new map for one not counted yet, or
// nil when maxUploadPrincipals are counted and none can be forgotten. The caller holds the lock.
func (q *liveUploads) sessionsOf(principal string, now time.Time) map[string]*liveUpload {
	if ids, ok := q.held[principal]; ok {
		return ids
	}
	if q.held == nil {
		q.held = map[string]map[string]*liveUpload{}
	}
	if len(q.held) >= maxUploadPrincipals && !q.sweep(now) {
		return nil
	}
	ids := map[string]*liveUpload{}
	q.held[principal] = ids
	return ids
}

// sweep forgets the sessions that no longer count, and the principals left without one, and
// reports whether there is room for another principal. The caller holds the lock.
func (q *liveUploads) sweep(now time.Time) bool {
	for p, ids := range q.held {
		for id, u := range ids {
			if !u.live(now) {
				delete(ids, id)
			}
		}
		if len(ids) == 0 {
			delete(q.held, p)
		}
	}
	return len(q.held) < maxUploadPrincipals
}

// count returns how many sessions of all principals count at now: lajstrom_upload_sessions_live.
func (q *liveUploads) count(now time.Time) int {
	q.mu.Lock()
	defer q.mu.Unlock()
	n := 0
	for _, ids := range q.held {
		for _, u := range ids {
			if u.live(now) {
				n++
			}
		}
	}
	return n
}

// begin counts a request on the upload id of principal in repoID as running, until end.
func (q *liveUploads) begin(principal string, repoID int64, id string, now time.Time) {
	if q.off() {
		return
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	ids := q.sessionsOf(principal, now)
	if ids == nil {
		return
	}
	u, ok := ids[id]
	if !ok {
		u = &liveUpload{repoID: repoID}
		ids[id] = u
	}
	u.running++
}

// end records that a request on the upload id of principal ended at now.
func (q *liveUploads) end(principal, id string, now time.Time) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if u, ok := q.held[principal][id]; ok {
		u.running = max(u.running-1, 0)
		u.last = now
	}
}

// release stops counting the upload id of principal: the session ended.
func (q *liveUploads) release(principal, id string) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if ids, ok := q.held[principal]; ok {
		delete(ids, id)
		if len(ids) == 0 {
			delete(q.held, principal)
		}
	}
}

// unchecked returns the sessions of principal that count toward the bound f in repoID, have no
// request running here, and that the index was not asked about within recheckAfter: upload id
// → repo.
func (q *liveUploads) unchecked(principal string, repoID int64, f *sessionsFull, now time.Time) map[string]int64 {
	q.mu.Lock()
	defer q.mu.Unlock()
	out := map[string]int64{}
	for id, u := range q.held[principal] {
		if u.running == 0 && now.Sub(u.checked) >= recheckAfter && (!f.perRepo || u.repoID == repoID) {
			out[id] = u.repoID
		}
	}
	return out
}

// recheck applies what the index said of the sessions of principal: the time of the last
// request on each that is open, by id; one that is not in last is gone and forgotten, unless a
// request on it began here meanwhile.
func (q *liveUploads) recheck(principal string, asked map[string]int64, last map[string]time.Time, now time.Time) {
	q.mu.Lock()
	defer q.mu.Unlock()
	ids := q.held[principal]
	for id := range asked {
		u, ok := ids[id]
		if !ok || u.running > 0 {
			continue
		}
		at, open := last[id]
		if !open {
			delete(ids, id)
			continue
		}
		u.last, u.checked = maxTime(u.last, at), now
	}
}

func maxTime(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}

// admitUpload counts a new upload id of principal in t's repo, or answers 429 TOOMANYREQUESTS
// with Retry-After when the principal's live sessions reach a bound. At a bound the index is
// asked first about the sessions counted toward it (recheck): those another replica ended do not
// count.
func (h *handler) admitUpload(ctx context.Context, t *target, principal, id string) error {
	q := &h.uploads
	if q.off() {
		return nil
	}
	full := q.admit(principal, t.repo.ID, id, h.o.Now())
	if full == nil {
		return nil
	}
	if asked := q.unchecked(principal, t.repo.ID, full, h.o.Now()); len(asked) > 0 {
		last := map[string]time.Time{}
		for held, repoID := range asked {
			s, err := h.o.Index.GetSession(ctx, repoID, uploadKind+held)
			switch {
			case errors.Is(err, index.ErrNotFound):
			case err != nil: // cannot tell: it still counts
				last[held] = time.Time{}
			case s.ExpiresAt.After(h.o.Now()):
				last[held] = s.ExpiresAt.Add(-uploadTTL) // saveSession set it uploadTTL after the request
			}
		}
		q.recheck(principal, asked, last, h.o.Now())
		full = q.admit(principal, t.repo.ID, id, h.o.Now())
	}
	if full == nil {
		return nil
	}
	limit := "principal"
	if full.perRepo {
		limit = "repo"
	}
	metrics.UploadSessionsRefused.WithLabelValues(limit).Inc()
	return errTooManyUploads(t, full)
}

// errTooManyUploads is the answer for a new session beyond the bound f.
func errTooManyUploads(t *target, f *sessionsFull) error {
	held, key := fmt.Sprintf("%d live upload sessions of %s/%s", f.limit, t.repo.Format, t.repo.Name), "server.uploadSessions.perRepo"
	if !f.perRepo {
		held, key = fmt.Sprintf("%d live upload sessions in all repos", f.limit), "server.uploadSessions.perPrincipal"
	}
	return &apiError{status: http.StatusTooManyRequests, code: codeTooManyRequests, retryAfter: f.retry,
		msg: fmt.Sprintf("this principal holds %s on this replica, the bound %s: finish or cancel one, or retry later; "+
			"a session stops counting %d minutes after its last request", held, key, int(liveWindow/time.Minute))}
}
