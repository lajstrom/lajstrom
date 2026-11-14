package generic

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/iotest"
	"time"

	"github.com/opencontainers/go-digest"
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
// stays down and comes back: it opens after breakerThreshold failures in a row, of two
// repositories (#972), refuses requests without sending them, lets the ping through as the trial
// after the cooldown, opens again when the trial fails, and closes at the first answer.
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
	other, err := p.Repository(ctx, "raw/scratch/other")
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
	otherTags := func() error { _, err := listTags(ctx, other); return err }
	ping := func() error { return p.Ping(ctx) }
	put := func() error { return repo.PushReference(ctx, desc, bytes.NewReader(content), "v2") }

	_ = wantCall(t, f, "Tags against a failing registry", tags, false, 1)
	_ = wantCall(t, f, "Tags of another repository", otherTags, false, 1)
	_ = wantCall(t, f, "Tags against a failing registry", tags, false, 1)
	if g := circuitGauge(t, "breaker-test"); g != 1 || !strings.Contains(buf.String(), "circuit open") ||
		!strings.Contains(buf.String(), "repositories=\"[art/raw/scratch/notes art/raw/scratch/other]\"") {
		t.Errorf("circuit gauge %v after 3 failures, want 1, and a log line that names both repositories:\n%s", g, buf)
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
// answer such as a 404 resets the count, a 429 and a cancelled request count neither way. Each
// request is of another repository, but in the rows of one repository: failures of one
// repository alone never open the breaker, however many (#972).
func TestBreakerCounts(t *testing.T) {
	tests := []struct {
		name    string
		retries int
		answers []int // status of each request, in order; 0 is a request whose caller cancels
		repos   []int // the repository of each request, by index; nil: each its own
		open    bool
	}{
		{name: "three 503 in a row", retries: -1, answers: []int{503, 503, 503}, open: true},
		{name: "a retried request counts once", retries: 3, answers: []int{503, 503}, open: false},
		{name: "a 404 resets the count", retries: -1, answers: []int{503, 503, 404, 503, 503}, open: false},
		{name: "a 429 counts neither way", retries: -1, answers: []int{503, 503, 429, 503}, open: true},
		{name: "a cancelled request counts neither way", retries: -1, answers: []int{503, 503, 0, 503}, open: true},
		{name: "a 501 is an answer", retries: -1, answers: []int{503, 503, 501, 503}, open: false},
		{name: "three 503 of one repository", retries: -1, answers: []int{503, 503, 503}, repos: []int{0, 0, 0}, open: false},
		{name: "ten 500 of one repository", retries: -1, answers: []int{500, 500, 500, 500, 500, 500, 500, 500, 500, 500},
			repos: []int{0, 0, 0, 0, 0, 0, 0, 0, 0, 0}, open: false},
		{name: "one repository, then another", retries: -1, answers: []int{503, 503, 503, 503}, repos: []int{0, 0, 0, 1}, open: true},
		{name: "two repositories in turn", retries: -1, answers: []int{503, 503, 503}, repos: []int{0, 1, 0}, open: true},
	}
	for i, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			lowerBreaker(t, 3)
			f := newFakeRegistry(t)
			log, _ := testLogger()
			name := fmt.Sprintf("breaker-counts-%d", i)
			p := f.provider(t, log, func(c *config.Registry) { c.Name, c.Retries = name, tt.retries })
			repoOf := func(k int) provider.Repository {
				t.Helper()
				if tt.repos != nil {
					k = tt.repos[k]
				}
				repo, err := p.Repository(t.Context(), fmt.Sprintf("raw/scratch/r%d", k))
				if err != nil {
					t.Fatal(err)
				}
				return repo
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
			for k, s := range tt.answers {
				status.Store(int64(s))
				ctx := t.Context()
				if s == 0 {
					var cancel context.CancelFunc
					ctx, cancel = context.WithTimeout(ctx, 50*time.Millisecond)
					defer cancel()
				}
				_, _ = listTags(ctx, repoOf(k))
			}
			_, err := listTags(t.Context(), repoOf(0))
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
	b := newBreaker("breaker-trial", "registry", log)
	clk := &clock{t: time.Unix(1_700_000_000, 0)}
	b.now = clk.now
	get, _ := http.NewRequest(http.MethodGet, "http://registry/v2/", nil)
	if _, err := b.allow(get); err != nil {
		t.Fatal(err)
	}
	b.done(t.Context(), false, -1, b.blame(get, nil)) // opens: GET /v2/ is the registry's
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
	b.done(t.Context(), true, 0, blame{}) // the trial's caller went away
	if trial, err := b.allow(get); err != nil || !trial {
		t.Errorf("allow after a trial that counted neither way = %v, %v; want another trial", trial, err)
	}
}

// failingBody gives "abc" and then fails with err.
func failingBody(err error) io.Reader {
	return io.MultiReader(strings.NewReader("abc"), iotest.ErrReader(err))
}

// TestBreakerBodyFails sends uploads whose body fails to read as it streams to the registry, as
// the body of a /v2 client does that stalls past server.stallTimeout where net/http does not
// cancel the request (HTTP/2), sends a broken chunked encoding, breaks off, or passes the
// upload's bound (#638). Each counts neither way, so that one client cannot open the breaker for
// all: a chunk or a closing PUT, threshold times in a row, leaves it closed; and none is
// provider.ErrUnavailable, though a stall is a timeout and a body that breaks off an EOF (#670).
// A registry that drops the connection while the body streams still counts as a failure, and is
// provider.ErrUnavailable. One that closes it on its own body timeout while the client paused
// is provider.ErrUnavailable too, but counts neither way (#648). The sends alternate between two
// repositories, so that the registry's failures may open the breaker (#972).
func TestBreakerBodyFails(t *testing.T) {
	lowerClientWait(t, 50*time.Millisecond)
	stalled := &net.OpError{Op: "read", Net: "tcp", Err: os.ErrDeadlineExceeded}
	tests := []struct {
		name    string
		body    func() io.Reader
		drop    bool          // the registry closes the connection without an answer
		timeout time.Duration // the registry's body timeout: it closes the connection when no byte came for that long
		open    bool
	}{
		{name: "stalled", body: func() io.Reader { return failingBody(stalled) }},
		{name: "broke off", body: func() io.Reader { return failingBody(io.ErrUnexpectedEOF) }},
		{name: "broken chunked encoding", body: func() io.Reader { return failingBody(errors.New("invalid byte in chunk length")) }},
		{name: "past the bound", body: func() io.Reader { return failingBody(&http.MaxBytesError{Limit: 3}) }},
		{name: "the registry drops the connection", body: func() io.Reader { return strings.NewReader("abcdef") }, drop: true, open: true},
		{name: "the client pauses past the registry's body timeout", body: func() io.Reader {
			return &pausingBody{steps: []pause{{0, "abc"}, {150 * time.Millisecond, "def"}}}
		}, timeout: 60 * time.Millisecond},
	}
	sends := []struct {
		name string
		call func(ctx context.Context, repo provider.Repository, u provider.Upload, body io.Reader) error
	}{
		{"chunk", func(ctx context.Context, repo provider.Repository, u provider.Upload, body io.Reader) error {
			_, err := repo.AppendBlobChunk(ctx, u, body, -1)
			return err
		}},
		{"closing PUT", func(ctx context.Context, repo provider.Repository, u provider.Upload, body io.Reader) error {
			_, err := repo.FinishBlobUpload(ctx, u, digest.FromString("abcdef"), body, 6)
			return err
		}},
	}
	for i, tt := range tests {
		for j, send := range sends {
			t.Run(tt.name+"/"+send.name, func(t *testing.T) {
				lowerBreaker(t, 3)
				ctx := t.Context()
				f := newFakeRegistry(t)
				log, _ := testLogger()
				p := f.provider(t, log, func(c *config.Registry) { c.Name, c.Retries = fmt.Sprintf("breaker-body-%d-%d", i, j), -1 })
				var repos []provider.Repository
				for _, name := range []string{"raw/scratch/notes", "raw/scratch/other"} {
					repo, err := p.Repository(ctx, name)
					if err != nil {
						t.Fatal(err)
					}
					repos = append(repos, repo)
				}
				var sessions []provider.Upload // opened first, so that no answer comes between the sends
				for k := range breakerThreshold {
					u, err := repos[k%2].StartBlobUpload(ctx)
					if err != nil {
						t.Fatal(err)
					}
					sessions = append(sessions, u)
				}
				dropped := tt.drop || tt.timeout > 0
				if dropped {
					f.hook = dropUploads(tt.timeout)
				}
				for k, u := range sessions {
					err := send.call(ctx, repos[k%2], u, tt.body())
					if err == nil || byBreaker(err) {
						t.Fatalf("%s: %v, want it to fail as it streams", send.name, err)
					}
					// The registry that dropped the connection is unavailable (#670); after a
					// pause, the write of the next bytes meets the closed connection, as after a
					// reset (#593). A body that stalled or broke off is the client's, never the
					// registry's.
					if got := errors.Is(err, provider.ErrUnavailable); got != dropped {
						t.Errorf("%s: %v; provider.ErrUnavailable = %v, want %v", send.name, err, got, dropped)
					}
				}
				_, err := repos[0].StartBlobUpload(ctx)
				if got := byBreaker(err); got != tt.open {
					t.Errorf("after %d failed sends: circuit open = %v (%v), want %v", breakerThreshold, got, err, tt.open)
				}
			})
		}
	}
}

// lowerClientWait sets clientWait to d for the test.
func lowerClientWait(t *testing.T, d time.Duration) {
	t.Helper()
	old := clientWait
	clientWait = d
	t.Cleanup(func() { clientWait = old })
}

// pause is one step of a client's body: wait, then data in one read.
type pause struct {
	wait time.Duration
	data string
}

// pausingBody is a client's body that waits before each of its steps.
type pausingBody struct{ steps []pause }

func (b *pausingBody) Read(p []byte) (int, error) {
	if len(b.steps) == 0 {
		return 0, io.EOF
	}
	s := &b.steps[0]
	if s.wait > 0 {
		time.Sleep(s.wait)
		s.wait = 0
	}
	n := copy(p, s.data)
	if s.data = s.data[n:]; s.data == "" {
		b.steps = b.steps[1:]
	}
	return n, nil
}

// TestBreakerClientWait sends uploads straight through breakerTransport to a registry over
// HTTP/1.1 and HTTP/2, chunked and with a Content-Length, and drops them there (#648). A
// registry, or a proxy in front of it, whose body timeout is shorter than server.stallTimeout
// drops the upload of a client that paused while Lajstrom still waits for that client: no read
// of the body fails, and net/http returns the failure once the client sends again. That counts
// neither way, so threshold such uploads leave the breaker closed. A registry that drops an
// upload while its body moves is a failure, also when the client paused earlier in the same
// body and when each read waits a little, as for a client behind a slow link; so is one that
// drops it a while after the whole body came, though the client paused before its end:
// threshold of them open the breaker, the outage that slow uploads must not hide. The uploads are
// of two repositories, so that the registry's failures may open it (#972).
func TestBreakerClientWait(t *testing.T) {
	lowerBreaker(t, 3)
	lowerClientWait(t, 50*time.Millisecond)
	const paused = 150 * time.Millisecond
	tests := []struct {
		name     string
		body     []pause
		registry droppingRegistry
		open     bool
	}{
		{name: "the client pauses past the registry's body timeout", body: []pause{{0, "abc"}, {paused, "def"}},
			registry: droppingRegistry{timeout: 60 * time.Millisecond}},
		{name: "a fast client", body: []pause{{0, "abcdef"}}, registry: droppingRegistry{dropAt: 3}, open: true},
		{name: "a client whose reads wait a little", body: []pause{{0, "abc"}, {2 * time.Millisecond, "d"},
			{2 * time.Millisecond, "e"}, {2 * time.Millisecond, "f"}}, registry: droppingRegistry{dropAt: 4}, open: true},
		{name: "the client paused earlier", body: []pause{{0, "abc"}, {paused, "d"}, {2 * time.Millisecond, "e"},
			{2 * time.Millisecond, "f"}}, registry: droppingRegistry{timeout: time.Second, dropAt: 5}, open: true},
		{name: "the registry drops after the body", body: []pause{{0, "abc"}, {paused, "def"}},
			registry: droppingRegistry{timeout: time.Second, dropAfter: paused}, open: true},
	}
	for _, proto := range []string{"HTTP/1.1", "HTTP/2.0"} {
		for _, size := range []int64{-1, 6} {
			for i, tt := range tests {
				t.Run(fmt.Sprintf("%s/size %d/%s", proto, size, tt.name), func(t *testing.T) {
					srv := startRegistry(t, proto, tt.registry)
					log, buf := testLogger()
					b := newBreaker(fmt.Sprintf("breaker-wait-%s-%d-%d", proto, size, i), srv.Listener.Addr().String(), log)
					rt := &breakerTransport{b: b, next: srv.Client().Transport}
					var wg sync.WaitGroup // threshold clients at once
					for k := range breakerThreshold {
						wg.Go(func() {
							req, err := http.NewRequestWithContext(t.Context(), http.MethodPatch, fmt.Sprintf("%s/v2/x%d/blobs/uploads/1", srv.URL, k%2),
								&pausingBody{steps: slices.Clone(tt.body)})
							if err != nil {
								t.Error(err)
								return
							}
							req.ContentLength = size
							if resp, err := rt.RoundTrip(req); err == nil {
								_ = resp.Body.Close()
								t.Errorf("the upload got %s, want it dropped", resp.Status)
							}
						})
					}
					wg.Wait()
					get, _ := http.NewRequest(http.MethodGet, srv.URL+"/v2/", nil)
					_, err := b.allow(get)
					if got := byBreaker(err); got != tt.open {
						t.Errorf("after %d dropped uploads: circuit open = %v, want %v", breakerThreshold, got, tt.open)
					}
					if logged := strings.Contains(buf.String(), "waited on its body"); logged == tt.open {
						t.Errorf("logged that a request waited on its body: %v, want %v\n%s", logged, !tt.open, buf)
					}
				})
			}
		}
	}
}

// droppingRegistry answers no upload: it drops the request once it read dropAt bytes of the
// body (0: no such bound), when no byte came within timeout, its body timeout between two reads
// (0: none), and else dropAfter after the end of the body.
type droppingRegistry struct {
	timeout   time.Duration
	dropAt    int
	dropAfter time.Duration
}

func (d droppingRegistry) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	rc := http.NewResponseController(w)
	buf := make([]byte, 64)
	for read := 0; ; {
		if d.timeout > 0 {
			_ = rc.SetReadDeadline(time.Now().Add(d.timeout))
		}
		n, err := r.Body.Read(buf)
		read += n
		switch {
		case d.dropAt > 0 && read >= d.dropAt, err != nil && !errors.Is(err, io.EOF):
			dropRequest(w, r)
			return
		case err != nil:
			time.Sleep(d.dropAfter)
			dropRequest(w, r)
			return
		}
	}
}

// startRegistry serves h over proto, HTTP/1.1, or HTTP/2.0 with TLS, and checks that each
// request comes over it.
func startRegistry(t *testing.T, proto string, h http.Handler) *httptest.Server {
	t.Helper()
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Proto != proto {
			t.Errorf("the registry got %s, want %s", r.Proto, proto)
		}
		h.ServeHTTP(w, r)
	}))
	if proto == "HTTP/2.0" {
		srv.EnableHTTP2 = true
		srv.StartTLS()
	} else {
		srv.Start()
	}
	t.Cleanup(srv.Close)
	return srv
}

// dropUploads is a hook of the fake that drops every PATCH and PUT without an answer: at once,
// or, with a body timeout, once no byte of the body came for that long.
func dropUploads(timeout time.Duration) func(http.ResponseWriter, *http.Request) bool {
	return func(w http.ResponseWriter, r *http.Request) bool {
		if r.Method != http.MethodPatch && r.Method != http.MethodPut {
			return false
		}
		rc := http.NewResponseController(w)
		for buf := make([]byte, 64); timeout > 0; {
			_ = rc.SetReadDeadline(time.Now().Add(timeout))
			if _, err := r.Body.Read(buf); err != nil {
				break
			}
		}
		if c, _, err := rc.Hijack(); err == nil {
			_ = c.Close()
		}
		return true
	}
}

// dropRequest ends a request without an answer: it closes the connection of an HTTP/1.1
// request and resets the stream of an HTTP/2 one, the way nginx ends a request whose body
// timed out.
func dropRequest(w http.ResponseWriter, r *http.Request) {
	if r.ProtoMajor == 1 {
		if c, _, err := http.NewResponseController(w).Hijack(); err == nil {
			_ = c.Close()
			return
		}
	}
	panic(http.ErrAbortHandler)
}
