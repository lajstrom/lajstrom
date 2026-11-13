package proxy

import (
	"bytes"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeMetadata is the GCP metadata server as far as an upstream asks it: the default service
// account's token, token-<n> for lifetime, to a request with Metadata-Flavor: Google. status,
// while set, answers that instead; flavorless leaves out the answer's Metadata-Flavor.
type fakeMetadata struct {
	srv *httptest.Server

	mu         sync.Mutex
	calls      int
	issued     int
	lifetime   time.Duration
	status     []int // the statuses of the next answers, then 200
	flavorless bool
	body       string // answers it instead of the token when set
	gate       chan struct{}
}

func newFakeMetadata(t *testing.T) *fakeMetadata {
	f := &fakeMetadata{lifetime: time.Hour}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeMetadata) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	gate := f.gate
	f.mu.Unlock()
	if gate != nil {
		<-gate
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if r.Header.Get("Metadata-Flavor") != "Google" || r.URL.Path != gcpTokenPath {
		http.Error(w, "missing Metadata-Flavor or a path elsewhere", http.StatusForbidden)
		return
	}
	if !f.flavorless {
		w.Header().Set("Metadata-Flavor", "Google")
	}
	if len(f.status) > 0 {
		status := f.status[0]
		f.status = f.status[1:]
		if status == http.StatusFound {
			w.Header().Set("Location", "http://elsewhere.example/token")
		}
		w.WriteHeader(status)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	if f.body != "" {
		_, _ = io.WriteString(w, f.body)
		return
	}
	f.issued++
	_, _ = fmt.Fprintf(w, `{"access_token":"token-%d","expires_in":%d,"token_type":"Bearer"}`, f.issued, int(f.lifetime.Seconds()))
}

func (f *fakeMetadata) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func (f *fakeMetadata) set(edit func(*fakeMetadata)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	edit(f)
}

// gcpClock is a clock that the test moves.
type gcpClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *gcpClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *gcpClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// newTestGCPTokens is newGCPTokens of the fake, with retries, the clock and no backoff.
func newTestGCPTokens(t *testing.T, f *fakeMetadata, retries int, c *gcpClock, log *slog.Logger) *gcpTokens {
	t.Helper()
	g, err := newGCPTokens(strings.TrimPrefix(f.srv.URL, "http://"), time.Second, retries, log)
	if err != nil {
		t.Fatal(err)
	}
	g.backoff, g.now = time.Millisecond, c.now
	return g
}

// wantGCPToken gets the token of g and checks it, and how many requests the fake has had.
func wantGCPToken(t *testing.T, g *gcpTokens, f *fakeMetadata, token string, calls int) {
	t.Helper()
	user, pass, err := g.basic(t.Context())
	if err != nil || user != "oauth2accesstoken" || pass != token {
		t.Fatalf("basic = %s:%s, %v; want oauth2accesstoken:%s", user, pass, err, token)
	}
	if n := f.count(); n != calls {
		t.Errorf("%d requests to the metadata server, want %d", n, calls)
	}
}

// TestGCPTokensRefresh: an hour's token is used for 56 minutes and then replaced; a refresh that
// fails, after its retries, keeps it in use until it expires, asking again at most every 10 s;
// without a token that works, the failure is the error, which never shows a token. A token that
// lives less than 8 minutes is replaced at half its life.
func TestGCPTokensRefresh(t *testing.T) {
	f := newFakeMetadata(t)
	c := &gcpClock{t: time.Date(2026, 11, 13, 8, 0, 0, 0, time.UTC)}
	var logs bytes.Buffer
	g := newTestGCPTokens(t, f, 1, c, slog.New(slog.NewTextHandler(&logs, nil)))

	wantGCPToken(t, g, f, "token-1", 1)
	c.advance(56*time.Minute - time.Second)
	wantGCPToken(t, g, f, "token-1", 1)
	c.advance(time.Second)
	wantGCPToken(t, g, f, "token-2", 2)

	c.advance(56 * time.Minute) // token-2 is due, and the metadata server fails twice
	f.set(func(f *fakeMetadata) { f.status = []int{500, 503, 500, 500} })
	wantGCPToken(t, g, f, "token-2", 4)
	c.advance(gcpRetryRefresh - time.Second)
	wantGCPToken(t, g, f, "token-2", 4)
	c.advance(time.Second)
	wantGCPToken(t, g, f, "token-2", 6)
	if !strings.Contains(logs.String(), "refresh failed") || strings.Contains(logs.String(), "token-") {
		t.Errorf("want one warning about the failed refresh, without a token:\n%s", logs.String())
	}

	c.advance(4 * time.Minute) // token-2 has expired
	f.set(func(f *fakeMetadata) { f.status = []int{404} })
	if _, _, err := g.basic(t.Context()); err == nil || !strings.Contains(err.Error(), "404") || strings.Contains(err.Error(), "token-") {
		t.Errorf("basic without a token that works = %v, want the 404 without a token", err)
	}
	if n := f.count(); n != 7 {
		t.Errorf("%d requests, want 7: a 404 is not retried", n)
	}

	f.set(func(f *fakeMetadata) { f.lifetime = 5 * time.Minute })
	wantGCPToken(t, g, f, "token-3", 8)
	c.advance(150*time.Second - time.Second)
	wantGCPToken(t, g, f, "token-3", 8)
	c.advance(time.Second)
	wantGCPToken(t, g, f, "token-4", 9)
}

// TestGCPTokensAnswers: what is not a token of the metadata server is an error, retried only
// after a 5xx or a 429; a redirect is not followed.
func TestGCPTokensAnswers(t *testing.T) {
	for _, tt := range []struct {
		name  string
		edit  func(*fakeMetadata)
		want  string // in the error; "" for a token
		calls int
	}{
		{"a 503 then the token", func(f *fakeMetadata) { f.status = []int{503} }, "", 2},
		{"a 429 then the token", func(f *fakeMetadata) { f.status = []int{429} }, "", 2},
		{"5xx past the retries", func(f *fakeMetadata) { f.status = []int{500, 500, 500} }, "500", 3},
		{"a 403", func(f *fakeMetadata) { f.status = []int{403} }, "403", 1},
		{"a redirect", func(f *fakeMetadata) { f.status = []int{302} }, "302", 1},
		{"no Metadata-Flavor", func(f *fakeMetadata) { f.flavorless = true }, "not the metadata server", 1},
		{"not JSON", func(f *fakeMetadata) { f.body = "<html>" }, "not an access token", 1},
		{"no expiry", func(f *fakeMetadata) { f.body = `{"access_token":"t0ken"}` }, "not an access token", 1},
	} {
		t.Run(tt.name, func(t *testing.T) {
			f := newFakeMetadata(t)
			f.set(tt.edit)
			g := newTestGCPTokens(t, f, 2, &gcpClock{t: time.Now()}, slog.New(slog.DiscardHandler))
			_, pass, err := g.basic(t.Context())
			switch {
			case tt.want == "" && (err != nil || pass != "token-1"):
				t.Errorf("basic = %q, %v; want token-1", pass, err)
			case tt.want != "" && (err == nil || !strings.Contains(err.Error(), tt.want) || strings.Contains(err.Error(), "t0ken")):
				t.Errorf("basic error = %v, want %q", err, tt.want)
			}
			if n := f.count(); n != tt.calls {
				t.Errorf("%d requests, want %d", n, tt.calls)
			}
		})
	}
}

// TestGCPTokensConcurrent: requests that find no token wait for one request to the metadata
// server.
func TestGCPTokensConcurrent(t *testing.T) {
	f := newFakeMetadata(t)
	gate := make(chan struct{})
	f.set(func(f *fakeMetadata) { f.gate = gate })
	g := newTestGCPTokens(t, f, 0, &gcpClock{t: time.Now()}, slog.New(slog.DiscardHandler))
	var wg sync.WaitGroup
	errs := make(chan error, 16)
	for range 16 {
		wg.Go(func() {
			_, _, err := g.basic(t.Context())
			errs <- err
		})
	}
	time.Sleep(10 * time.Millisecond)
	close(gate)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Errorf("basic: %v", err)
		}
	}
	if n := f.count(); n != 1 {
		t.Errorf("%d requests to the metadata server, want 1", n)
	}
}

func TestGCPRegistry(t *testing.T) {
	for u, want := range map[string]bool{
		"https://europe-west8-docker.pkg.dev": true,
		"https://gcr.io":                      true,
		"https://EU.GCR.IO.":                  true,
		"https://ghcr.io":                     false,
		"https://pkg.dev.example.com":         false,
		"http://127.0.0.1:5000":               false,
	} {
		if got := gcpRegistry(u); got != want {
			t.Errorf("gcpRegistry(%s) = %t, want %t", u, got, want)
		}
	}
}

// TestNewGCPTokensHost: GCE_METADATA_HOST is a host and an optional port; empty is 169.254.169.254.
func TestNewGCPTokensHost(t *testing.T) {
	for host, want := range map[string]string{
		"":                         "http://169.254.169.254" + gcpTokenPath,
		"metadata.google.internal": "http://metadata.google.internal" + gcpTokenPath,
		"127.0.0.1:8080":           "http://127.0.0.1:8080" + gcpTokenPath,
		"http://127.0.0.1":         "",
		"127.0.0.1/x":              "",
		"user@127.0.0.1":           "",
	} {
		g, err := newGCPTokens(host, 0, 0, slog.New(slog.DiscardHandler))
		switch {
		case want == "" && err == nil:
			t.Errorf("newGCPTokens(%q) = %s, want an error", host, g.url)
		case want != "" && (err != nil || g.url != want):
			t.Errorf("newGCPTokens(%q) = %v; want %s", host, err, want)
		}
	}
}
