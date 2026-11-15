package generic

import (
	"container/list"
	"context"
	"net/http"
	"time"

	"github.com/lajstrom/lajstrom/internal/provider"
)

// repoBreakers bounds how many repositories the breaker of a registry keeps a state of (#1117):
// the one used longest ago is forgotten first, so that a client that makes names up costs
// bounded memory. A forgotten repository starts again from no failure.
const repoBreakers = 1024

// repoState is the breaker of one repository under the registry's (#1117), Envoy's outlier
// ejection mapped onto a registry: the repository is ejected, not the registry. After
// breakerThreshold failures in a row of the repository, with no answer of it in between, its
// requests fail at once for breakerCooldown, and reach nothing; then one GET or HEAD of it is
// its trial, whose answer closes it and whose failure opens it for another cooldown. It counts
// only while the registry's breaker is closed: when that opens, the registry's failure explains
// its repositories', and they are forgotten.
type repoState struct {
	key      string    // the repository (repoKey)
	failures int       // in a row, with no answer of the repository in between
	opened   time.Time // when its requests began to fail at once; zero while they go through
	trial    bool      // its trial is in flight, after the cooldown
}

// repoKey returns the repository that req names, for its breaker: that of a request of the
// distribution API to the registry's host (repositoryOf), or "" for any other request, the ping,
// the catalog, a token realm, the storage a blob redirects to.
func (b *breaker) repoKey(req *http.Request) string {
	if req.URL.Host != b.host {
		return ""
	}
	return repositoryOf(req.URL.Path)
}

// allowRepo reports whether a request of the repository key with method may go to the registry,
// and with the repository's state, whether it is the repository's trial; b.mu is held. While
// the repository's breaker is open, its requests fail with the *provider.UnavailableError of an
// open circuit breaker, with the time until its trial: the frontends answer it 503 with
// Retry-After and the status text alone, as they answer the registry's 500 with the status text
// alone. After the cooldown one GET or HEAD is the trial, and the others fail at once.
func (b *breaker) allowRepo(key, method string) (*repoState, bool, error) {
	el := b.repoIndex[key]
	if el == nil {
		return nil, false, nil
	}
	b.repoOrder.MoveToFront(el)
	st := el.Value.(*repoState)
	if st.opened.IsZero() {
		return st, false, nil
	}
	if left := breakerCooldown - b.now().Sub(st.opened); left > 0 {
		return st, false, &provider.UnavailableError{RetryAfter: left}
	}
	if st.trial || method != http.MethodGet && method != http.MethodHead {
		return st, false, &provider.UnavailableError{} // its trial is under way: the answer is near
	}
	st.trial = true
	return st, true, nil
}

// doneRepo records, after the registry's breaker, the outcome of a request of the repository
// p.repo for that repository's breaker; b.mu is held. An answer of the repository forgets it. A
// failure counts while the registry's breaker is closed, unless it is the registry's whatever
// the request (blame.wide): breakerThreshold in a row open the repository's breaker, and so
// does its failed trial (failedRepo).
func (b *breaker) doneRepo(ctx context.Context, p pass, result int, against blame) {
	el := b.repoIndex[p.repo]
	var st *repoState
	if el != nil {
		st = el.Value.(*repoState)
	}
	trial := p.repoTrial && st != nil && st == p.rs
	if trial {
		st.trial = false
	}
	switch {
	case result > 0 && st != nil:
		if !st.opened.IsZero() {
			b.log.InfoContext(ctx, "registry answers the repository again: its requests go through", "repository", p.repo)
		}
		b.repoOrder.Remove(el)
		delete(b.repoIndex, p.repo)
	case result < 0 && !against.wide && b.state == breakerClosed:
		if st == nil {
			st = b.newRepo(p.repo)
		} else {
			b.repoOrder.MoveToFront(el)
		}
		b.failedRepo(ctx, st, trial)
	}
}

// failedRepo counts a failure of the repository st, of its trial when trial; b.mu is held. When
// the repository's breaker opens, the registry's failures in a row that are all the
// repository's are forgotten: its refused requests do not count, so that a failure of another
// repository starts from zero and does not open the registry's breaker with them.
func (b *breaker) failedRepo(ctx context.Context, st *repoState, trial bool) {
	st.failures++
	switch {
	case trial:
		st.opened = b.now()
	case st.opened.IsZero() && st.failures >= breakerThreshold:
		st.opened = b.now()
		if b.now().Sub(b.ejected) >= breakerCooldown {
			b.ejected = b.now()
			b.log.WarnContext(ctx, "registry fails the requests of one repository: they fail at once for a while, "+
				"the repository may be broken in the registry", "repository", st.key, "failures", st.failures, "cooldown", breakerCooldown)
		}
		if b.wide == 0 && len(b.repos) == 1 && b.repos[0] == st.key {
			b.reset()
		}
	}
}

// newRepo adds the state of the repository key, used now, and forgets the one used longest ago
// past repoBreakers; b.mu is held.
func (b *breaker) newRepo(key string) *repoState {
	if b.repoIndex == nil {
		b.repoIndex = map[string]*list.Element{}
	}
	st := &repoState{key: key}
	b.repoIndex[key] = b.repoOrder.PushFront(st)
	for b.repoOrder.Len() > repoBreakers {
		last := b.repoOrder.Back()
		b.repoOrder.Remove(last)
		delete(b.repoIndex, last.Value.(*repoState).key)
	}
	return st
}

// forgetRepos forgets the state of every repository; b.mu is held.
func (b *breaker) forgetRepos() {
	b.repoOrder.Init()
	clear(b.repoIndex)
}
