package generic

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/lajstrom/lajstrom/internal/config"
	"github.com/lajstrom/lajstrom/internal/egress"
	"github.com/lajstrom/lajstrom/internal/provider"
)

// TestBreakerOneRepository is the acceptance of #972: a registry answers every request of one
// repository with 500, as Zot does for a name it cannot store (zot#4695) and as any registry
// may for a repository damaged in its storage. That repository is asked 100 times in a row, by
// every kind of request a reader or a writer sends, and then 30 times at once while another
// repository is read. The breaker stays closed: every request reaches the registry, the other
// repository keeps working, and the log says once that the registry fails one repository only.
// The fifth failure sets off the breaker's probe, GET /v2/, which the registry answers (#1119);
// the clock does not move, so it is the only one.
func TestBreakerOneRepository(t *testing.T) {
	lowerBreaker(t, 5)
	b := newBrokenRepository(t, "breaker-one-repository")
	for i := range 100 {
		c := b.calls[i%len(b.calls)]
		sent := 1
		if i == breakerThreshold-1 {
			sent = 2
		}
		if err := wantCall(t, b.f, c.name+" of the broken repository", func() error { return c.call(i) }, false, sent); err == nil {
			t.Fatalf("%s of the broken repository succeeded", c.name)
		}
	}
	if err := wantCall(t, b.f, "a read of another repository after 100 failures", b.readGood, false, 2); err != nil {
		t.Fatalf("a read of another repository: %v", err)
	}

	var wg sync.WaitGroup // a client that asks for the broken repository 30 times at once
	for i := range 30 {
		wg.Go(func() {
			if err := b.calls[i%len(b.calls)].call(i); err == nil || byBreaker(err) {
				t.Errorf("a request of the broken repository: %v, want the registry's 500", err)
			}
		})
	}
	for range 10 {
		if err := b.readGood(); err != nil {
			t.Errorf("a read of another repository while the broken one is asked: %v", err)
		}
	}
	wg.Wait()
	if err := b.readGood(); err != nil {
		t.Errorf("a read of another repository after the burst: %v", err)
	}

	logs := b.logs.String()
	if g := circuitGauge(t, "breaker-one-repository"); g != 0 || strings.Contains(logs, "circuit open") {
		t.Errorf("circuit gauge %v, want 0, and no circuit open:\n%s", g, logs)
	}
	if n := strings.Count(logs, "fails the requests of one repository only"); n != 1 ||
		!strings.Contains(logs, "repositories=[art/raw/scratch/broken]") {
		t.Errorf("logged %d times that the registry fails one repository only, want once, naming it:\n%s", n, logs)
	}
}

// brokenRepository is a provider, without retries and with a clock that does not move, of a
// fake registry that answers every request of the repository raw/scratch/broken with 500 and
// serves raw/scratch/notes, which holds the manifest desc.
type brokenRepository struct {
	f     *fakeRegistry
	logs  *syncBuffer
	good  provider.Repository
	desc  ocispec.Descriptor
	calls []brokenCall // the requests of the broken repository, of every kind; i varies the tag
}

// brokenCall is one kind of request of the broken repository.
type brokenCall struct {
	name string
	call func(i int) error
}

// newBrokenRepository returns the brokenRepository of a registry named name.
func newBrokenRepository(t *testing.T, name string) *brokenRepository {
	t.Helper()
	ctx := t.Context()
	f := newFakeRegistry(t)
	log, buf := testLogger()
	p := f.provider(t, log, func(c *config.Registry) { c.Name, c.Retries = name, -1 })
	p.clients.breaker.now = (&clock{t: time.Unix(1_700_000_000, 0)}).now
	if _, err := p.Capabilities(ctx); err != nil { // the probe, which a blob's HEAD would send first
		t.Fatal(err)
	}
	good, err := p.Repository(ctx, "raw/scratch/notes")
	if err != nil {
		t.Fatal(err)
	}
	desc := pushManifest(t, good, "v1", nil)
	rc, err := good.Fetch(ctx, desc)
	if err != nil {
		t.Fatal(err)
	}
	content, _ := io.ReadAll(rc)
	_ = rc.Close()
	broken, err := p.Repository(ctx, "raw/scratch/broken")
	if err != nil {
		t.Fatal(err)
	}
	f.hook = func(w http.ResponseWriter, r *http.Request) bool {
		if !strings.HasPrefix(r.URL.Path, "/v2/art/raw/scratch/broken/") {
			return false
		}
		_, _ = io.Copy(io.Discard, r.Body)
		writeErr(w, http.StatusInternalServerError, "UNKNOWN", "invalid repository layout")
		return true
	}
	blob := ocispec.Descriptor{MediaType: "application/octet-stream", Digest: digest.FromString("blob"), Size: 4}
	calls := []brokenCall{
		{"manifest HEAD of a tag", func(i int) error { _, err := broken.Resolve(ctx, fmt.Sprintf("v%d", i)); return err }},
		{"manifest GET", func(int) error {
			rc, err := broken.Fetch(ctx, desc)
			if err == nil {
				_ = rc.Close()
			}
			return err
		}},
		{"manifest HEAD by digest", func(int) error { _, err := broken.Exists(ctx, desc); return err }},
		{"tag list", func(int) error { _, err := listTags(ctx, broken); return err }},
		{"blob HEAD", func(int) error { _, err := broken.Exists(ctx, blob); return err }},
		{"upload POST", func(int) error { _, err := broken.StartBlobUpload(ctx); return err }},
		{"manifest PUT", func(i int) error {
			return broken.PushReference(ctx, desc, bytes.NewReader(content), fmt.Sprintf("v%d", i))
		}},
	}
	return &brokenRepository{f: f, logs: buf, good: good, desc: desc, calls: calls}
}

// readGood lists the tags of the repository that works and reads its manifest: two requests.
func (b *brokenRepository) readGood() error {
	ctx := context.Background()
	if _, err := listTags(ctx, b.good); err != nil {
		return err
	}
	rc, err := b.good.Fetch(ctx, b.desc)
	if err != nil {
		return err
	}
	return rc.Close()
}

// TestBreakerOutage checks that a registry that fails every request opens the breaker as fast
// as before #972, counted in requests: after breakerThreshold failures in a row when they are of
// two repositories or more, when they are readiness pings, and when the registry refuses the
// connections, which no request can cause, also of one repository. One repository's 503s open
// it at the breaker's probe, GET /v2/, which fails too (#1119), before the readiness ping would.
// A connection closed without an answer once the request was sent may be the request's doing:
// one repository's open it at the probe, whose connection closes as well; two at once.
func TestBreakerOutage(t *testing.T) {
	const never = 20 // calls without a refusal for a row that does not open
	tags := func(p *Provider, repo int) error {
		r, err := p.Repository(context.Background(), fmt.Sprintf("raw/scratch/r%d", repo))
		if err != nil {
			return err
		}
		_, err = listTags(context.Background(), r)
		return err
	}
	tests := []struct {
		name   string
		addr   func(t *testing.T) string // the registry; nil: the fake, which answers every request 503
		call   func(p *Provider, i int) error
		opens  int // the calls that reach the registry before the breaker refuses one
		probes int // the breaker's probes that the fake got besides
	}{
		{name: "503, a repository each", call: func(p *Provider, i int) error { return tags(p, i) }, opens: 5},
		{name: "503, two repositories in turn", call: func(p *Provider, i int) error { return tags(p, i%2) }, opens: 5},
		{name: "503, the readiness pings", call: func(p *Provider, _ int) error { return p.Ping(context.Background()) }, opens: 5},
		{name: "503 of one repository, then a ping", call: func(p *Provider, i int) error {
			if i < 7 {
				return tags(p, 0)
			}
			return p.Ping(context.Background())
		}, opens: 5, probes: 1},
		{name: "refused, one repository", addr: refusingAddr, call: func(p *Provider, _ int) error { return tags(p, 0) }, opens: 5},
		{name: "closed without an answer, one repository", addr: func(t *testing.T) string { return quietAddr(t, true) },
			call: func(p *Provider, _ int) error { return tags(p, 0) }, opens: 5},
		{name: "closed without an answer, two repositories", addr: func(t *testing.T) string { return quietAddr(t, true) },
			call: func(p *Provider, i int) error { return tags(p, i%2) }, opens: 5},
	}
	for i, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			lowerBreaker(t, 5)
			name := fmt.Sprintf("breaker-outage-%d", i)
			var p *Provider
			var f *fakeRegistry
			if tt.addr == nil {
				f = newFakeRegistry(t)
				f.hook = func(w http.ResponseWriter, _ *http.Request) bool {
					writeErr(w, http.StatusServiceUnavailable, "UNAVAILABLE", "down")
					return true
				}
				p = f.provider(t, slog.New(slog.DiscardHandler), func(c *config.Registry) { c.Name, c.Retries = name, -1 })
			} else {
				var err error
				p, err = New(config.Registry{Name: name, Type: Type, URL: "http://" + tt.addr(t), Retries: -1}, slog.New(slog.DiscardHandler))
				if err != nil {
					t.Fatal(err)
				}
			}
			through := 0
			for ; through < never; through++ {
				err := tt.call(p, through)
				p.clients.breaker.probes.Wait()
				if byBreaker(err) {
					break
				}
				if err == nil {
					t.Fatalf("call %d succeeded against a registry that fails everything", through)
				}
			}
			if through != tt.opens {
				t.Errorf("%d calls reached the registry before the breaker refused one, want %d", through, tt.opens)
			}
			if f != nil {
				if n := len(f.log()); n != through+tt.probes {
					t.Errorf("the registry got %d requests, want %d", n, through+tt.probes)
				}
			}
		})
	}
}

// TestBreakerBlame checks what a failed request is held against: the repository it names in
// the distribution API, whatever its method, reference and query; the path of a request to the
// registry of none; the host of a request to another host. A failure is the registry's
// whatever the request (wide) for GET /v2/ and for a request to the registry's host that never
// reached it; not for a dial a guard refused, a timeout or a reset once it was sent, or another
// host's.
func TestBreakerBlame(t *testing.T) {
	const reg = "http://registry:5000"
	refused := &net.OpError{Op: "dial", Net: "tcp", Err: os.NewSyscallError("connect", syscall.ECONNREFUSED)}
	tests := []struct {
		method, url string
		err         error
		want        blame
	}{
		{"GET", reg + "/v2/art/raw/a/manifests/v1", nil, blame{repo: "art/raw/a"}},
		{"HEAD", reg + "/v2/art/raw/a/manifests/sha256:ab", nil, blame{repo: "art/raw/a"}},
		{"PUT", reg + "/v2/art/raw/a/manifests/v2", nil, blame{repo: "art/raw/a"}},
		{"DELETE", reg + "/v2/art/raw/a/manifests/sha256:ab", nil, blame{repo: "art/raw/a"}},
		{"GET", reg + "/v2/art/raw/a/blobs/sha256:ab", nil, blame{repo: "art/raw/a"}},
		{"POST", reg + "/v2/art/raw/a/blobs/uploads/", nil, blame{repo: "art/raw/a"}},
		{"POST", reg + "/v2/art/raw/a/blobs/uploads/?mount=sha256:ab&from=art/raw/b", nil, blame{repo: "art/raw/a"}},
		{"PATCH", reg + "/v2/art/raw/a/blobs/uploads/1f2e?_state=xyz", nil, blame{repo: "art/raw/a"}},
		{"GET", reg + "/v2/art/raw/a/tags/list?n=100&last=v9", nil, blame{repo: "art/raw/a"}},
		{"GET", reg + "/v2/art/raw/a/referrers/sha256:ab", nil, blame{repo: "art/raw/a"}},
		{"GET", reg + "/v2/oci/r/blobs/manifests/uploads", nil, blame{repo: "oci/r/blobs"}},
		{"POST", reg + "/v2/oci/r/blobs/blobs/uploads/", nil, blame{repo: "oci/r/blobs"}},
		{"GET", reg + "/v2/library/nginx/manifests/latest", nil, blame{repo: "library/nginx"}},
		{"GET", reg + "/v2/", nil, blame{repo: "/v2/", wide: true}},
		{"GET", reg + "/v2/_catalog?n=1", nil, blame{repo: "/v2/_catalog"}},
		{"GET", reg + "/v2/a/b", nil, blame{repo: "/v2/a/b"}},
		{"GET", "https://auth.example/token?scope=repository:art/raw/a:pull", nil, blame{repo: "auth.example"}},
		{"GET", "https://storage.example/bucket/docker/registry/v2/blobs/sha256/ab/ab/data", nil, blame{repo: "storage.example"}},
		{"GET", reg + "/v2/art/raw/a/manifests/v1", refused, blame{repo: "art/raw/a", wide: true}},
		{"GET", reg + "/v2/art/raw/a/manifests/v1", &url.Error{Op: "Get", URL: reg, Err: refused}, blame{repo: "art/raw/a", wide: true}},
		{"GET", reg + "/v2/art/raw/a/manifests/v1", &net.OpError{Op: "dial", Net: "tcp", Err: &net.DNSError{Err: "no such host", Name: "registry", IsNotFound: true}},
			blame{repo: "art/raw/a", wide: true}},
		{"GET", reg + "/v2/art/raw/a/manifests/v1", &net.DNSError{Err: "no such host", Name: "registry"}, blame{repo: "art/raw/a", wide: true}},
		{"GET", reg + "/v2/art/raw/a/manifests/v1", &net.OpError{Op: "dial", Net: "tcp", Err: os.ErrDeadlineExceeded},
			blame{repo: "art/raw/a", wide: true}},
		{"GET", reg + "/v2/art/raw/a/manifests/v1", &net.OpError{Op: "proxyconnect", Net: "tcp", Err: refused}, blame{repo: "art/raw/a", wide: true}},
		{"GET", reg + "/v2/_catalog", refused, blame{repo: "/v2/_catalog", wide: true}},
		{"GET", reg + "/v2/art/raw/a/manifests/v1", &net.OpError{Op: "read", Net: "tcp", Err: os.NewSyscallError("read", syscall.ECONNRESET)},
			blame{repo: "art/raw/a"}},
		{"GET", reg + "/v2/art/raw/a/manifests/v1", &noAnswerError{err: io.EOF}, blame{repo: "art/raw/a"}},
		{"GET", reg + "/v2/art/raw/a/manifests/v1", &noAnswerError{err: &net.OpError{Op: "read", Err: os.ErrDeadlineExceeded}},
			blame{repo: "art/raw/a"}},
		{"GET", "https://storage.example/bucket/data", refused, blame{repo: "storage.example"}},
		{"GET", "https://cdn.example/blob", &net.OpError{Op: "dial", Net: "tcp", Err: fmt.Errorf("%w: cdn.example resolves to 10.0.0.1", egress.ErrRefused)},
			blame{repo: "cdn.example"}},
		{"GET", reg + "/v2/art/raw/a/manifests/v1", &net.OpError{Op: "dial", Net: "tcp", Err: fmt.Errorf("%w: x", egress.ErrRefused)},
			blame{repo: "art/raw/a"}},
	}
	b := newBreaker("breaker-blame", "registry:5000", slog.New(slog.DiscardHandler))
	for _, tt := range tests {
		req, err := http.NewRequest(tt.method, tt.url, nil)
		if err != nil {
			t.Fatal(err)
		}
		if got := b.blame(req, tt.err); got != tt.want {
			t.Errorf("blame(%s %s, %v) = %+v, want %+v", tt.method, tt.url, tt.err, got, tt.want)
		}
	}
	if connectFailed(nil) {
		t.Error("connectFailed(nil) = true")
	}
}

// TestBreakerHalfOpenBrokenRepository is the acceptance of #1116: a registry that fails every
// request opens the breaker; it comes back, but for one repository it answers 500, as for a
// repository damaged in its storage. A client asks for that repository after the cooldown: its
// trials fail, but the breaker stays half-open, and the first trial of another repository the
// registry answers closes it. A trial of the broken repository that hangs does not keep the
// others out either.
func TestBreakerHalfOpenBrokenRepository(t *testing.T) {
	lowerBreaker(t, 5)
	const name = "breaker-half-open"
	f := newFakeRegistry(t)
	log, buf := testLogger()
	p := f.provider(t, log, func(c *config.Registry) { c.Name, c.Retries = name, -1 })
	clk := &clock{t: time.Unix(1_700_000_000, 0)}
	p.clients.breaker.now = clk.now
	broken, good, other := tagsCall(t, p, "raw/scratch/broken"), tagsCall(t, p, "raw/scratch/notes"), tagsCall(t, p, "raw/scratch/other")
	reg := &recoveringRegistry{down: true, hanged: make(chan struct{}, 1)}
	f.hook = reg.serve

	for i := range breakerThreshold {
		_ = wantCall(t, f, "tags of two repositories in turn, the registry down", []func() error{good, other}[i%2], false, 1)
	}
	_ = wantCall(t, f, "tags while open", good, true, 0)
	reg.set(false, nil) // the registry is back, but for the broken repository

	clk.add(time.Minute)
	for range 3 {
		if err := wantCall(t, f, "tags of the broken repository as the trial", broken, false, 1); err == nil {
			t.Fatal("tags of the broken repository succeeded")
		}
	}
	if g := circuitGauge(t, name); g != 1 {
		t.Errorf("circuit gauge %v after the broken repository's trials failed, want 1: half-open", g)
	}
	if err := wantCall(t, f, "tags of another repository as a trial", good, false, 1); err != nil {
		t.Fatalf("tags of another repository after the registry came back: %v", err)
	}
	if g := circuitGauge(t, name); g != 0 || !strings.Contains(buf.String(), "circuit closed") {
		t.Errorf("circuit gauge %v after another repository's trial was answered, want 0, and a log line:\n%s", g, buf)
	}
	_ = wantCall(t, f, "tags of a third repository", other, false, 1)

	reg.set(true, nil)
	for i := range breakerThreshold {
		_ = wantCall(t, f, "tags of two repositories in turn, the registry down again", []func() error{good, other}[i%2], false, 1)
	}
	release := make(chan struct{})
	reg.set(false, release)
	clk.add(time.Minute)
	stuck := make(chan error, 1)
	go func() { stuck <- broken() }()
	<-reg.hanged // the broken repository's trial is in flight
	if err := wantCall(t, f, "tags of another repository beside the broken one's trial", good, false, 1); err != nil {
		t.Errorf("tags of another repository while the broken one's trial hangs: %v", err)
	}
	close(release)
	if err := <-stuck; err == nil || byBreaker(err) {
		t.Errorf("the broken repository's trial: %v, want the registry's 500", err)
	}
	if g := circuitGauge(t, name); g != 0 {
		t.Errorf("circuit gauge %v after the broken repository's trial ended, want 0", g)
	}
}

// tagsCall returns a call that lists the tags of the repository path of p; the fake's 404 for a
// repository without tags is an answer, and no error.
func tagsCall(t *testing.T, p *Provider, path string) func() error {
	t.Helper()
	repo, err := p.Repository(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	return func() error {
		if _, err := listTags(t.Context(), repo); !errors.Is(err, provider.ErrNotFound) {
			return err
		}
		return nil
	}
}

// recoveringRegistry is the hook of a fake registry that fails every request while down, and
// otherwise answers every request of the repository raw/scratch/broken 500, once hang closes
// when hang is set; hanged gets a value for each such request that waits.
type recoveringRegistry struct {
	mu     sync.Mutex
	down   bool
	hang   chan struct{}
	hanged chan struct{}
}

// set sets down and hang.
func (r *recoveringRegistry) set(down bool, hang chan struct{}) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.down, r.hang = down, hang
}

func (r *recoveringRegistry) serve(w http.ResponseWriter, req *http.Request) bool {
	r.mu.Lock()
	down, wait := r.down, r.hang
	r.mu.Unlock()
	switch {
	case down:
		writeErr(w, http.StatusServiceUnavailable, "UNAVAILABLE", "down")
		return true
	case !strings.HasPrefix(req.URL.Path, "/v2/art/raw/scratch/broken/"):
		return false
	case wait != nil:
		r.hanged <- struct{}{}
		<-wait
	}
	writeErr(w, http.StatusInternalServerError, "UNKNOWN", "invalid repository layout")
	return true
}
