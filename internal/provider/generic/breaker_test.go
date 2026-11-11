package generic

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	dto "github.com/prometheus/client_model/go"

	"github.com/lajstrom/lajstrom/internal/config"
	"github.com/lajstrom/lajstrom/internal/metrics"
	"github.com/lajstrom/lajstrom/internal/provider"
)

// clock is a time a test moves by hand.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) add(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// lowerBreaker sets the breaker threshold to n for the test.
func lowerBreaker(t *testing.T, n int) {
	t.Helper()
	threshold, cooldown := breakerThreshold, breakerCooldown
	breakerThreshold, breakerCooldown = n, time.Minute
	t.Cleanup(func() { breakerThreshold, breakerCooldown = threshold, cooldown })
}

// circuitGauge reads lajstrom_provider_circuit_open of the registry.
func circuitGauge(t *testing.T, registry string) float64 {
	t.Helper()
	var m dto.Metric
	if err := metrics.ProviderCircuitOpen.WithLabelValues(registry).Write(&m); err != nil {
		t.Fatal(err)
	}
	return m.GetGauge().GetValue()
}

// TestBreakerOpensAndCloses drives the breaker of a provider through a registry that fails,
// stays down and comes back: it opens after breakerThreshold failures in a row, refuses
// requests without sending them, lets the ping through as the trial after the cooldown, opens
// again when the trial fails, and closes at the first answer.
func TestBreakerOpensAndCloses(t *testing.T) {
	lowerBreaker(t, 3)
	ctx := t.Context()
	f := newFakeRegistry(t)
	log, buf := testLogger()
	p := f.provider(t, log, func(c *config.Registry) { c.Name, c.Retries = "breaker-test", -1 })
	clk := &clock{t: time.Unix(1_700_000_000, 0)}
	p.clients.breaker.now = clk.now
	repo, err := p.Repository(ctx, "raw/scratch/notes")
	if err != nil {
		t.Fatal(err)
	}
	desc := pushManifest(t, repo, "v1", nil)
	rc, err := repo.Fetch(ctx, desc)
	if err != nil {
		t.Fatal(err)
	}
	content, _ := io.ReadAll(rc)
	_ = rc.Close()

	var down sync.Mutex // held while the registry is down
	isDown := true
	f.hook = func(w http.ResponseWriter, r *http.Request) bool {
		down.Lock()
		defer down.Unlock()
		if !isDown {
			return false
		}
		_, _ = io.Copy(io.Discard, r.Body)
		writeErr(w, http.StatusServiceUnavailable, "UNAVAILABLE", "down")
		return true
	}
	tags := func() error { _, err := listTags(ctx, repo); return err }
	ping := func() error { return p.Ping(ctx) }
	put := func() error { return repo.PushReference(ctx, desc, bytes.NewReader(content), "v2") }

	for range 3 {
		_ = wantCall(t, f, "Tags against a failing registry", tags, false, 1)
	}
	if g := circuitGauge(t, "breaker-test"); g != 1 || !strings.Contains(buf.String(), "circuit open") {
		t.Errorf("circuit gauge %v after 3 failures, want 1, and a log line:\n%s", g, buf)
	}
	err = wantCall(t, f, "Tags while open", tags, true, 0)
	if after, ok := provider.RetryAfter(err); !ok || after != time.Minute {
		t.Errorf("RetryAfter of a refusal at the opening = %v, %v; want the whole cooldown, 1m", after, ok)
	}
	_ = wantCall(t, f, "Ping while open", ping, true, 0)

	clk.add(time.Minute) // the cooldown is over: half-open
	_ = wantCall(t, f, "PUT while half-open, never the trial", put, true, 0)
	_ = wantCall(t, f, "Ping as the trial, the registry still down", ping, false, 1)
	_ = wantCall(t, f, "Tags after a failed trial", tags, true, 0)

	down.Lock()
	isDown = false
	down.Unlock()
	clk.add(time.Minute)
	if err := wantCall(t, f, "Ping as the trial, the registry back", ping, false, 1); err != nil {
		t.Fatalf("Ping: %v", err)
	}
	if err := wantCall(t, f, "Tags after the circuit closed", tags, false, 1); err != nil {
		t.Errorf("Tags: %v", err)
	}
	if g := circuitGauge(t, "breaker-test"); g != 0 || !strings.Contains(buf.String(), "circuit closed") {
		t.Errorf("circuit gauge %v after it closed, want 0, and a log line:\n%s", g, buf)
	}
}

// wantCall runs one call of a breaker scenario and checks whether the breaker refused it and
// how many requests reached the registry. It returns the call's error.
func wantCall(t *testing.T, f *fakeRegistry, what string, call func() error, refused bool, sent int) error {
	t.Helper()
	before := len(f.log())
	err := call()
	if got := byBreaker(err); got != refused {
		t.Errorf("%s: %v; refused by the breaker: %v, want %v", what, err, got, refused)
	}
	if n := len(f.log()) - before; n != sent {
		t.Errorf("%s: %d requests reached the registry, want %d", what, n, sent)
	}
	return err
}

// byBreaker reports whether err is the breaker's refusal, an *UnavailableError: the registry's
// own 503 is an ErrUnavailable too, a *RejectedError.
func byBreaker(err error) bool {
	var u *provider.UnavailableError
	return errors.As(err, &u)
}

// TestBreakerCounts checks what counts as a failure in a row: a retried request once, an
// answer such as a 404 resets the count, a 429 and a cancelled request count neither way.
func TestBreakerCounts(t *testing.T) {
	tests := []struct {
		name    string
		retries int
		answers []int // status of each request, in order; 0 is a request whose caller cancels
		open    bool
	}{
		{name: "three 503 in a row", retries: -1, answers: []int{503, 503, 503}, open: true},
		{name: "a retried request counts once", retries: 3, answers: []int{503, 503}, open: false},
		{name: "a 404 resets the count", retries: -1, answers: []int{503, 503, 404, 503, 503}, open: false},
		{name: "a 429 counts neither way", retries: -1, answers: []int{503, 503, 429, 503}, open: true},
		{name: "a cancelled request counts neither way", retries: -1, answers: []int{503, 503, 0, 503}, open: true},
		{name: "a 501 is an answer", retries: -1, answers: []int{503, 503, 501, 503}, open: false},
	}
	for i, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			lowerBreaker(t, 3)
			f := newFakeRegistry(t)
			log, _ := testLogger()
			name := fmt.Sprintf("breaker-counts-%d", i)
			p := f.provider(t, log, func(c *config.Registry) { c.Name, c.Retries = name, tt.retries })
			repo, err := p.Repository(t.Context(), "raw/scratch/notes")
			if err != nil {
				t.Fatal(err)
			}
			var status atomic.Int64
			f.hook = func(w http.ResponseWriter, r *http.Request) bool {
				s := int(status.Load())
				if s == 0 {
					<-r.Context().Done() // never answers; the caller cancels
					return true
				}
				writeErr(w, s, "X", "x")
				return true
			}
			for _, s := range tt.answers {
				status.Store(int64(s))
				ctx := t.Context()
				if s == 0 {
					var cancel context.CancelFunc
					ctx, cancel = context.WithTimeout(ctx, 50*time.Millisecond)
					defer cancel()
				}
				_, _ = listTags(ctx, repo)
			}
			_, err = listTags(t.Context(), repo)
			if got := byBreaker(err); got != tt.open {
				t.Errorf("circuit open = %v (last error %v), want %v", got, err, tt.open)
			}
		})
	}
}

// TestBreakerOneTrial checks that a half-open breaker lets one GET through at a time, and
// another one when the trial counted neither way.
func TestBreakerOneTrial(t *testing.T) {
	lowerBreaker(t, 1)
	log, _ := testLogger()
	b := newBreaker("breaker-trial", log)
	clk := &clock{t: time.Unix(1_700_000_000, 0)}
	b.now = clk.now
	get, _ := http.NewRequest(http.MethodGet, "http://registry/v2/", nil)
	if _, err := b.allow(get); err != nil {
		t.Fatal(err)
	}
	b.done(t.Context(), false, -1) // opens
	clk.add(20 * time.Second)
	if _, err := b.allow(get); !errors.Is(err, provider.ErrUnavailable) {
		t.Fatalf("allow while open = %v", err)
	} else if after, _ := provider.RetryAfter(err); after != 40*time.Second {
		t.Errorf("RetryAfter 20 s into a 1 min cooldown = %v, want 40s", after)
	}
	clk.add(40 * time.Second)
	trial, err := b.allow(get)
	if err != nil || !trial {
		t.Fatalf("allow after the cooldown = %v, %v; want the trial", trial, err)
	}
	if _, err := b.allow(get); !errors.Is(err, provider.ErrUnavailable) {
		t.Errorf("a second GET during the trial = %v, want provider.ErrUnavailable", err)
	} else if after, _ := provider.RetryAfter(err); after != 0 {
		t.Errorf("RetryAfter during the trial = %v, want 0", after)
	}
	b.done(t.Context(), true, 0) // the trial's caller went away
	if trial, err := b.allow(get); err != nil || !trial {
		t.Errorf("allow after a trial that counted neither way = %v, %v; want another trial", trial, err)
	}
}
