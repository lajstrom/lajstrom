package generic

import (
	"errors"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"

	"github.com/lajstrom/lajstrom/internal/config"
	"github.com/lajstrom/lajstrom/internal/metrics"
	"github.com/lajstrom/lajstrom/internal/provider"
)

// TestBreakerProbeHangingUpstream is the acceptance of #1119: an upstream of an oci proxy repo,
// which is never pinged, hangs while one image is asked of it, as one image pulled on every
// node of a rollout. The breaker sends a probe of its own, GET /v2/, when the image's failures
// reach the threshold; the upstream does not answer that either within its timeout, and the
// breaker opens: after the threshold and one probe, counted in requests.
func TestBreakerProbeHangingUpstream(t *testing.T) {
	lowerBreaker(t, 5)
	const name = "breaker-probe-hang"
	f := newFakeRegistry(t)
	f.hook = func(_ http.ResponseWriter, r *http.Request) bool {
		<-r.Context().Done() // never answers; Lajstrom gives up after the timeout
		return true
	}
	p := newUpstream(t, f, name, Credential{Username: f.user, Password: f.pass}, func(c *config.Upstream) {
		c.Timeout, c.Retries = 100*time.Millisecond, -1
	})
	f.watch(p)
	opens := circuitCount(t, metrics.ProviderCircuitOpens, name)
	pull := func() error { _, err := resolve(p, "library/nginx", "1.27"); return err }
	for i := range breakerThreshold {
		sent := 1
		if i == breakerThreshold-1 {
			sent = 2 // and the probe
		}
		if err := wantCall(t, f, "a pull of one image from a hanging upstream", pull, false, sent); !errors.Is(err, provider.ErrUnavailable) {
			t.Errorf("a pull from a hanging upstream: %v, want provider.ErrUnavailable", err)
		}
	}
	_ = wantCall(t, f, "a pull after the probe failed", pull, true, 0)
	log := f.log()
	if probe := log[len(log)-1]; probe != "GET /v2/" {
		t.Errorf("the last request is %q, want the probe, GET /v2/", probe)
	}
	if g, o := circuitGauge(t, name), circuitCount(t, metrics.ProviderCircuitOpens, name)-opens; g != 1 || o != 1 {
		t.Errorf("circuit gauge %v, opens_total rose by %v; want 1, 1", g, o)
	}
}

// TestBreakerProbe checks the probe (#1119) against a registry that answers every request of the
// repositories raw/scratch/broken-* 500 and takes Basic credentials; each step asks another of
// them. The probe is anonymous: the registry answers it 401, an answer, which ends the run, and
// the breaker stays closed; it is recorded as the readiness ping is, operation ping. Another run
// within a second sends none, a run a second later one.
func TestBreakerProbe(t *testing.T) {
	r := newProbeRegistry(t, "breaker-probe")
	pings0 := r.pings(t)
	r.run(t, "tags of a broken repository, then the probe", "raw/scratch/broken-1", true)
	r.f.mu.Lock()
	auths := slices.Clone(r.f.auths)
	last := r.f.requests[len(r.f.requests)-1]
	r.f.mu.Unlock()
	if probe, broken := auths[len(auths)-1], auths[len(auths)-2]; last != "GET /v2/" || probe != "" || broken == "" {
		t.Errorf("the last requests carried %q, %q (%s): want the broken repository's with the credentials, then the probe, GET /v2/, without",
			broken, probe, last)
	}
	r.b.mu.Lock()
	failures := r.b.failures
	r.b.mu.Unlock()
	if g := circuitGauge(t, r.name); failures != 0 || g != 0 {
		t.Errorf("%d failures in a row after the probe was answered, circuit gauge %v; want 0, 0", failures, g)
	}
	if n := r.pings(t) - pings0; n != 1 {
		t.Errorf("lajstrom_provider_request_duration_seconds{operation=\"ping\",status=\"401\"} rose by %v, want the probe's 1", n)
	}
	_ = wantCall(t, r.f, "tags of a repository that works", r.good, false, 1)
	r.run(t, "tags of another broken repository within the second: no probe", "raw/scratch/broken-2", false)
	_ = wantCall(t, r.f, "tags of a repository that works", r.good, false, 1)
	r.clk.add(time.Second)
	r.run(t, "tags of a third broken repository a second later, then the probe", "raw/scratch/broken-3", true)
}

// TestBreakerProbeFails checks, on the registry of TestBreakerProbe, that at most one probe is in
// flight, that a probe that fails after the registry answered another request while it ran
// opens nothing, and that one that fails, a 503, opens the breaker, sent once though the
// registry's requests are retried.
func TestBreakerProbeFails(t *testing.T) {
	r := newProbeRegistry(t, "breaker-probe-fails")
	release := make(chan struct{})
	r.set(http.StatusServiceUnavailable, release)
	r.clk.add(time.Second)
	r.fail(t, "raw/scratch/broken-1")
	<-r.received // the probe is in flight
	r.clk.add(time.Second)
	before := len(r.f.log())
	if err := r.good(); err != nil { // an answer while the probe runs
		t.Fatal(err)
	}
	r.fail(t, "raw/scratch/broken-2")
	if n := len(r.f.log()) - before; n != 1+breakerThreshold*r.attempts {
		t.Errorf("%d requests while a probe was in flight, want %d: no second probe", n, 1+breakerThreshold*r.attempts)
	}
	r.set(http.StatusServiceUnavailable, nil)
	close(release)
	r.f.settle()
	if g := circuitGauge(t, r.name); g != 0 {
		t.Errorf("circuit gauge %v after a probe failed that the registry answered another request during, want 0", g)
	}

	opens := circuitCount(t, metrics.ProviderCircuitOpens, r.name)
	_ = wantCall(t, r.f, "tags of a repository that works", r.good, false, 1)
	r.clk.add(time.Second)
	r.run(t, "tags of a broken repository, the registry failing GET /v2/: the probe, sent once", "raw/scratch/broken-3", true)
	_ = wantCall(t, r.f, "tags of the repository that works after the probe failed", r.good, true, 0)
	if g, o := circuitGauge(t, r.name), circuitCount(t, metrics.ProviderCircuitOpens, r.name)-opens; g != 1 || o != 1 ||
		!strings.Contains(r.logs.String(), "GET /v2/ failed too") {
		t.Errorf("circuit gauge %v, opens_total rose by %v after the probe failed; want 1, 1, and a log line:\n%s", g, o, r.logs)
	}
}

// probeRegistry is a provider, with retries and a clock a test moves, of a fake registry that
// takes Basic credentials and answers every request of the repositories raw/scratch/broken-*
// 500; GET /v2/ gets ping's status, or the fake's own answer, a 401 without credentials, while
// ping is 0, once hang closes when hang is set.
type probeRegistry struct {
	name     string
	f        *fakeRegistry
	p        *Provider
	b        *breaker
	clk      *clock
	logs     *syncBuffer
	good     func() error // lists the tags of raw/scratch/notes, a repository that works
	attempts int          // what a request of a broken repository costs: the 500 is retried twice
	received chan struct{}

	mu   sync.Mutex
	ping int
	hang chan struct{}
}

// newProbeRegistry returns the probeRegistry of a registry named name, whose provider has met the
// Basic challenge once.
func newProbeRegistry(t *testing.T, name string) *probeRegistry {
	t.Helper()
	lowerBreaker(t, 5)
	r := &probeRegistry{name: name, f: newFakeRegistry(t), clk: &clock{t: time.Unix(1_700_000_000, 0)}, attempts: 1 + 2,
		received: make(chan struct{}, 1)}
	r.f.authMode = "basic"
	var log *slog.Logger
	log, r.logs = testLogger()
	r.p = r.f.provider(t, log, func(c *config.Registry) {
		c.Name, c.Retries = name, 2
		c.Auth.Username, c.Auth.Password = r.f.user, r.f.pass
	})
	r.b = r.p.clients.breaker
	r.b.now = r.clk.now
	r.f.hook = r.serve
	r.good = tagsCall(t, r.p, "raw/scratch/notes")
	if err := wantCall(t, r.f, "tags of a repository that works", r.good, false, 2); err != nil { // the challenge, then with Basic
		t.Fatal(err)
	}
	return r
}

func (r *probeRegistry) serve(w http.ResponseWriter, req *http.Request) bool {
	switch {
	case strings.HasPrefix(req.URL.Path, "/v2/art/raw/scratch/broken-"):
		writeErr(w, http.StatusInternalServerError, "UNKNOWN", "invalid repository layout")
		return true
	case req.URL.Path != "/v2/":
		return false
	}
	r.mu.Lock()
	status, wait := r.ping, r.hang
	r.mu.Unlock()
	if wait != nil {
		r.received <- struct{}{}
		<-wait
	}
	if status == 0 {
		return false
	}
	writeErr(w, status, "UNAVAILABLE", "x")
	return true
}

// set sets the status of GET /v2/ and what it waits for.
func (r *probeRegistry) set(ping int, hang chan struct{}) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.ping, r.hang = ping, hang
}

// run asks the broken repository path breakerThreshold times through wantCall, and expects the
// probe after the last when probe is true.
func (r *probeRegistry) run(t *testing.T, what, path string, probe bool) {
	t.Helper()
	broken := tagsCall(t, r.p, path)
	for i := range breakerThreshold {
		sent := r.attempts
		if probe && i == breakerThreshold-1 {
			sent++
		}
		_ = wantCall(t, r.f, what, broken, false, sent)
	}
}

// fail asks the broken repository path breakerThreshold times, without waiting for a probe.
func (r *probeRegistry) fail(t *testing.T, path string) {
	t.Helper()
	broken := tagsCall(t, r.p, path)
	for range breakerThreshold {
		if err := broken(); err == nil {
			t.Fatalf("tags of %s succeeded", path)
		}
	}
}

// pings reads how many samples lajstrom_provider_request_duration_seconds has of a GET /v2/ of the
// registry answered 401.
func (r *probeRegistry) pings(t *testing.T) float64 {
	t.Helper()
	var m dto.Metric
	if err := metrics.ProviderDuration.WithLabelValues(r.name, "ping", http.MethodGet, "401").(prometheus.Metric).Write(&m); err != nil {
		t.Fatal(err)
	}
	return float64(m.GetHistogram().GetSampleCount())
}
