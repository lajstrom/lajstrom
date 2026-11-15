package generic

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/lajstrom/lajstrom/internal/provider"
)

// TestRepoBreaker drives the breaker of a registry, without its probe, through the breakers of
// its repositories (#1117): five failures in a row of one repository refuse its requests for the
// cooldown, and reach nothing; those refusals do not count toward the registry's breaker, and
// the registry's failures that were all that repository's are forgotten, so that one failure of
// another repository does not open it. After the cooldown one GET or HEAD of the repository is
// its trial: a failure refuses it for another cooldown, an answer forgets it. An answer of the
// repository ends its failures in a row, another repository's does not; a failure no request
// can cause is not the repository's. The registry's breaker opening forgets the repositories.
func TestRepoBreaker(t *testing.T) {
	lowerBreaker(t, 5)
	ctx := t.Context()
	log, buf := testLogger()
	b := newBreaker("repo-breaker", "registry", log)
	clk := &clock{t: time.Unix(1_700_000_000, 0)}
	b.now = clk.now
	req := func(method, repo string) *http.Request {
		r, err := http.NewRequest(method, "http://registry/v2/"+repo+"/manifests/v1", nil)
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	refused := func(what string, r *http.Request, after time.Duration) {
		t.Helper()
		if _, err := b.allow(r); !errors.Is(err, provider.ErrUnavailable) {
			t.Errorf("%s: %v, want provider.ErrUnavailable", what, err)
		} else if got, _ := provider.RetryAfter(err); got != after {
			t.Errorf("%s: RetryAfter %v, want %v", what, got, after)
		}
	}
	allowed := func(what string, r *http.Request) pass {
		t.Helper()
		p, err := b.allow(r)
		if err != nil {
			t.Fatalf("%s: %v, want it through", what, err)
		}
		return p
	}
	fail := func(repo string, n int) {
		t.Helper()
		for range n {
			r := req(http.MethodGet, repo)
			b.done(ctx, allowed("a GET of "+repo, r), -1, b.blame(r, nil))
		}
	}
	answer := func(repo string) {
		t.Helper()
		b.done(ctx, allowed("a GET of "+repo, req(http.MethodGet, repo)), +1, blame{})
	}
	ping := func() { // an answer of no repository: it ends the registry's failures in a row
		r, _ := http.NewRequest(http.MethodGet, "http://registry/v2/", nil)
		b.done(ctx, allowed("the ping", r), +1, blame{})
	}
	registry := func(when string, state, failures int) {
		t.Helper()
		b.mu.Lock()
		s, n := b.state, b.failures
		b.mu.Unlock()
		if s != state || n != failures {
			t.Errorf("%s: the registry's breaker in state %d with %d failures in a row, want %d with %d", when, s, n, state, failures)
		}
	}

	fail("a", 5)
	refused("a GET of a after five failures", req(http.MethodGet, "a"), time.Minute)
	registry("after a's fifth failure", breakerClosed, 0)
	for range 100 {
		refused("a GET of a", req(http.MethodGet, "a"), time.Minute)
	}
	fail("b", 1)
	registry("after a's refusals and one failure of b", breakerClosed, 1)
	if want := "registry fails the requests of one repository: they fail at once"; !contains(buf.String(), want, "repository=a") {
		t.Errorf("no log line %q naming the repository:\n%s", want, buf)
	}
	ping()
	fail("c", 4)
	answer("b")
	fail("c", 1) // the fifth with no answer of c: b's answer is not c's
	refused("a GET of c after five failures, b answered among them", req(http.MethodGet, "c"), time.Minute)

	clk.add(20 * time.Second)
	refused("a GET of a 20 s into the cooldown", req(http.MethodGet, "a"), 40*time.Second)
	clk.add(40 * time.Second)
	trial := req(http.MethodHead, "a")
	p := allowed("a HEAD of a after the cooldown, its trial", trial)
	refused("a second GET of a during its trial", req(http.MethodGet, "a"), 0)
	refused("a PUT of a during its trial", req(http.MethodPut, "a"), 0)
	b.done(ctx, p, 0, blame{}) // the trial's caller went away
	p = allowed("a HEAD of a, its trial again", trial)
	b.done(ctx, p, -1, b.blame(trial, nil))
	refused("a GET of a after its trial failed", req(http.MethodGet, "a"), time.Minute)
	clk.add(time.Minute)
	p = allowed("a GET of a after the second cooldown, its trial", req(http.MethodGet, "a"))
	b.done(ctx, p, +1, blame{})
	fail("a", 4)
	answer("a")
	fail("a", 4)
	allowed("a GET of a after an answer of a between two runs of four failures", req(http.MethodGet, "a"))

	ping()
	refusedConn := &net.OpError{Op: "dial", Net: "tcp", Err: os.NewSyscallError("connect", syscall.ECONNREFUSED)}
	r := req(http.MethodGet, "a")
	b.done(ctx, allowed("a GET of a", r), -1, b.blame(r, refusedConn)) // the registry's, whatever the request
	allowed("a GET of a after four failures and a refused connection", req(http.MethodGet, "a"))

	ping()
	fail("a", 1)
	refused("a GET of a after its fifth failure", req(http.MethodGet, "a"), time.Minute)
	for _, repo := range []string{"d", "e", "d", "e", "d"} {
		fail(repo, 1)
	}
	registry("after five failures of two repositories", breakerOpen, 0)
	clk.add(time.Minute)
	answer("d") // the trial
	registry("after the trial was answered", breakerClosed, 0)
	allowed("a GET of a after the registry's breaker opened and closed", req(http.MethodGet, "a"))
}

// TestRepoBreakerBounded checks that the breaker keeps the state of repoBreakers repositories at
// most, a client that makes names up whatever it asks, and forgets the one used longest ago
// first: a repository whose requests are refused, and asked meanwhile, stays refused.
func TestRepoBreakerBounded(t *testing.T) {
	lowerBreaker(t, 5)
	ctx := t.Context()
	log, _ := testLogger()
	b := newBreaker("repo-breaker-bounded", "registry", log)
	clk := &clock{t: time.Unix(1_700_000_000, 0)}
	b.now = clk.now
	get := func(repo string) *http.Request {
		r, err := http.NewRequest(http.MethodGet, "http://registry/v2/"+repo+"/tags/list", nil)
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	ping, _ := http.NewRequest(http.MethodGet, "http://registry/v2/", nil)
	fail := func(repo string) {
		t.Helper()
		r := get(repo)
		p, err := b.allow(r)
		if err != nil {
			t.Fatalf("a GET of %s: %v", repo, err)
		}
		b.done(ctx, p, -1, b.blame(r, nil))
		p, _ = b.allow(ping)
		b.done(ctx, p, +1, blame{}) // the registry answers: its failures in a row end
	}
	for range breakerThreshold {
		fail("broken")
	}
	for i := range 3 * repoBreakers {
		fail(fmt.Sprintf("made-up/%d", i))
		if i%100 == 0 {
			if _, err := b.allow(get("broken")); !byBreaker(err) {
				t.Fatalf("a GET of the broken repository after %d made-up names: %v, want refused", i, err)
			}
		}
	}
	b.mu.Lock()
	n, order := len(b.repoIndex), b.repoOrder.Len()
	_, first := b.repoIndex["made-up/0"]
	_, last := b.repoIndex[fmt.Sprintf("made-up/%d", 3*repoBreakers-1)]
	b.mu.Unlock()
	if n != repoBreakers || order != repoBreakers || first || !last {
		t.Errorf("%d repositories kept (%d in order), the first made-up name kept %v, the last %v; want %d, %d, false, true",
			n, order, first, last, repoBreakers, repoBreakers)
	}
	if _, err := b.allow(get("broken")); !byBreaker(err) {
		t.Errorf("a GET of the broken repository at the end: %v, want refused", err)
	}
}

// contains reports whether s contains a line that has every one of parts.
func contains(s string, parts ...string) bool {
	for line := range strings.Lines(s) {
		ok := true
		for _, p := range parts {
			ok = ok && strings.Contains(line, p)
		}
		if ok {
			return true
		}
	}
	return false
}
