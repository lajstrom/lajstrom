package generic

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/lajstrom/lajstrom/internal/config"
	"github.com/lajstrom/lajstrom/internal/provider"
)

// failFirst answers the first n requests that match with status, and lets the rest through to
// the fake registry.
func failFirst(n, status int, match func(r *http.Request) bool) func(http.ResponseWriter, *http.Request) bool {
	failed := 0
	return func(w http.ResponseWriter, r *http.Request) bool {
		if !match(r) || failed >= n {
			return false
		}
		failed++
		_, _ = io.Copy(io.Discard, r.Body)
		writeErr(w, status, "UNAVAILABLE", "try later")
		return true
	}
}

// TestRetryOnlyIdempotent runs each operation against a registry that fails its request with
// 503 more often than the retries allow, and counts how often the request was sent: 1 +
// retries for an idempotent request, 1 for the others.
func TestRetryOnlyIdempotent(t *testing.T) {
	const retries = config.DefaultRegistryRetries
	blob := []byte("chunk")
	blobDesc := ocispec.Descriptor{MediaType: "application/octet-stream", Digest: digest.FromBytes(blob), Size: int64(len(blob))}
	tests := []struct {
		name   string
		method string
		path   string // a part of the path of the failing request
		run    func(ctx context.Context, repo provider.Repository, setup setupResult) error
		want   int
	}{
		{name: "GET tags", method: http.MethodGet, path: "/tags/list", want: 1 + retries,
			run: func(ctx context.Context, repo provider.Repository, _ setupResult) error {
				_, err := listTags(ctx, repo)
				return err
			}},
		{name: "HEAD manifest", method: http.MethodHead, path: "/manifests/", want: 1 + retries,
			run: func(ctx context.Context, repo provider.Repository, s setupResult) error {
				_, err := repo.Exists(ctx, s.manifest)
				return err
			}},
		{name: "PUT manifest", method: http.MethodPut, path: "/manifests/", want: 1 + retries,
			run: func(ctx context.Context, repo provider.Repository, s setupResult) error {
				return repo.PushReference(ctx, s.manifest, bytes.NewReader(s.content), "v2")
			}},
		{name: "DELETE manifest", method: http.MethodDelete, path: "/manifests/", want: 1 + retries,
			run: func(ctx context.Context, repo provider.Repository, s setupResult) error {
				return repo.Delete(ctx, s.manifest)
			}},
		{name: "POST upload", method: http.MethodPost, path: "/blobs/uploads/", want: 1,
			run: func(ctx context.Context, repo provider.Repository, _ setupResult) error {
				_, err := repo.StartBlobUpload(ctx)
				return err
			}},
		{name: "POST mount", method: http.MethodPost, path: "/blobs/uploads/", want: 1,
			run: func(ctx context.Context, repo provider.Repository, _ setupResult) error {
				_, _, err := repo.MountBlob(ctx, blobDesc.Digest, "raw/other/x")
				return err
			}},
		{name: "PATCH chunk", method: http.MethodPatch, path: "/blobs/uploads/", want: 1,
			run: func(ctx context.Context, repo provider.Repository, s setupResult) error {
				_, err := repo.AppendBlobChunk(ctx, s.upload, bytes.NewReader(blob), int64(len(blob)))
				return err
			}},
		{name: "PUT closing a session", method: http.MethodPut, path: "/blobs/uploads/", want: 1,
			run: func(ctx context.Context, repo provider.Repository, s setupResult) error {
				_, err := repo.FinishBlobUpload(ctx, s.upload, blobDesc.Digest, nil, 0) // no body: nothing stops a resend but the method
				return err
			}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := t.Context()
			f, repo := uploadRepo(t, nil)
			s := setup(t, repo)
			f.hook = failFirst(100, http.StatusServiceUnavailable, func(r *http.Request) bool {
				return r.Method == tt.method && strings.Contains(r.URL.Path, tt.path)
			})
			before := countMatching(f, tt.method, tt.path)
			if err := tt.run(ctx, repo, s); err == nil {
				t.Fatal("no error from a registry that keeps failing")
			}
			if n := countMatching(f, tt.method, tt.path) - before; n != tt.want {
				t.Errorf("%d %s requests, want %d", n, tt.method, tt.want)
			}
		})
	}
}

// setupResult is what setup made: a pushed manifest, its content, and an open upload session.
type setupResult struct {
	manifest ocispec.Descriptor
	content  []byte
	upload   provider.Upload
}

func setup(t *testing.T, repo provider.Repository) setupResult {
	t.Helper()
	desc := pushManifest(t, repo, "v1", nil)
	rc, err := repo.Fetch(t.Context(), desc)
	if err != nil {
		t.Fatal(err)
	}
	content, err := io.ReadAll(rc)
	_ = rc.Close()
	if err != nil {
		t.Fatal(err)
	}
	u, err := repo.StartBlobUpload(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	return setupResult{manifest: desc, content: content, upload: u}
}

// countMatching counts the requests of f with method whose path contains part.
func countMatching(f *fakeRegistry, method, part string) int {
	n := 0
	for _, line := range f.log() {
		m, rest, _ := strings.Cut(line, " ")
		if m == method && strings.Contains(rest, part) {
			n++
		}
	}
	return n
}

// TestRetryStatuses checks which answers of the registry are retried: 429 and the 5xx but 501
// and 505; never another 4xx.
func TestRetryStatuses(t *testing.T) {
	tests := []struct {
		status int
		retry  bool
	}{
		{http.StatusTooManyRequests, true},
		{http.StatusInternalServerError, true},
		{http.StatusBadGateway, true},
		{http.StatusServiceUnavailable, true},
		{http.StatusGatewayTimeout, true},
		{http.StatusNotImplemented, false},
		{http.StatusHTTPVersionNotSupported, false},
		{http.StatusBadRequest, false},
		{http.StatusForbidden, false},
		{http.StatusNotFound, false},
		{http.StatusRequestTimeout, false},
		{http.StatusConflict, false},
	}
	for _, tt := range tests {
		t.Run(fmt.Sprint(tt.status), func(t *testing.T) {
			f, repo := uploadRepo(t, nil)
			pushManifest(t, repo, "v1", nil)
			f.hook = failFirst(1, tt.status, func(r *http.Request) bool { return strings.HasSuffix(r.URL.Path, "/tags/list") })
			_, err := listTags(t.Context(), repo)
			if (err == nil) != tt.retry {
				t.Errorf("Tags error = %v, want a retry to succeed: %v", err, tt.retry)
			}
			want := 1
			if tt.retry {
				want = 2
			}
			if n := countMatching(f, http.MethodGet, "/tags/list"); n != want {
				t.Errorf("%d requests, want %d", n, want)
			}
		})
	}
}

// answer is what stubRegistry answers one attempt: an error, or a status with header.
type answer struct {
	err    error
	status int
	header http.Header
}

// stubRegistry answers the attempts of a retryTransport with answers, in order, the last one
// for every attempt after; it counts the attempts and keeps the bodies it was sent.
type stubRegistry struct {
	answers  []answer
	attempts int
	bodies   []string
}

func (s *stubRegistry) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.Body != nil {
		b, _ := io.ReadAll(r.Body)
		s.bodies = append(s.bodies, string(b))
	}
	a := s.answers[min(s.attempts, len(s.answers)-1)]
	s.attempts++
	if a.err != nil {
		return nil, a.err
	}
	h := a.header
	if h == nil {
		h = http.Header{}
	}
	return &http.Response{StatusCode: a.status, Header: h, Body: io.NopCloser(strings.NewReader("")), Request: r}, nil
}

// roundTrip sends req through a retryTransport over next with 3 retries and returns the status,
// 0 for an error.
func roundTrip(next http.RoundTripper, log *slog.Logger, req *http.Request) (int, error) {
	resp, err := (&retryTransport{next: next, retries: 3, log: log}).RoundTrip(req)
	if err != nil {
		return 0, err
	}
	_ = resp.Body.Close()
	return resp.StatusCode, nil
}

// tagsRequest is a GET with a query and credentials, which the log must not show.
func tagsRequest(ctx context.Context) *http.Request {
	r, _ := http.NewRequestWithContext(ctx, http.MethodGet, "http://registry/v2/raw/x/tags/list?n=1&secret=s3cr3t", nil)
	r.Header.Set("Authorization", "Bearer t0ken")
	return r
}

// TestRetryTransportErrors checks which transport errors are retried.
func TestRetryTransportErrors(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want int // attempts
	}{
		{name: "connection reset", err: fmt.Errorf("read: %w", syscall.ECONNRESET), want: 2},
		{name: "connection refused", err: fmt.Errorf("dial: %w", syscall.ECONNREFUSED), want: 2},
		{name: "unexpected EOF", err: io.ErrUnexpectedEOF, want: 2},
		{name: "timeout", err: timeoutErr{}, want: 2},
		{name: "context canceled", err: context.Canceled, want: 1},
		{name: "certificate", err: errors.New("x509: certificate signed by unknown authority"), want: 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			stub := &stubRegistry{answers: []answer{{err: tt.err}, {status: http.StatusOK}}}
			log, _ := testLogger()
			_, _ = roundTrip(stub, log, tagsRequest(t.Context()))
			if stub.attempts != tt.want {
				t.Errorf("%d attempts, want %d", stub.attempts, tt.want)
			}
		})
	}
}

// TestRetryTransportBody checks that a retried PUT of a manifest sends its body again.
func TestRetryTransportBody(t *testing.T) {
	stub := &stubRegistry{answers: []answer{{status: http.StatusBadGateway}, {status: http.StatusCreated}}}
	req, _ := http.NewRequestWithContext(t.Context(), http.MethodPut, "http://registry/v2/raw/x/manifests/v1", strings.NewReader("{manifest}"))
	log, _ := testLogger()
	if status, err := roundTrip(stub, log, req); err != nil || status != http.StatusCreated {
		t.Fatalf("RoundTrip = %d, %v; want 201", status, err)
	}
	if got := strings.Join(stub.bodies, "|"); got != "{manifest}|{manifest}" {
		t.Errorf("bodies %q, want the manifest twice", got)
	}
}

// TestRetryTransportContext checks that a cancelled context ends the wait between attempts.
// The context is cancelled once the transport logs the retry, right before it waits: cancelled
// as the first attempt returns, before the transport looks at the context, it gets the 503 as
// the answer instead (#566).
func TestRetryTransportContext(t *testing.T) {
	defer func(w struct{ min, max time.Duration }) { retryWait = w }(retryWait)
	retryWait.min, retryWait.max = time.Hour, time.Hour
	ctx, cancel := context.WithCancel(t.Context())
	stub := &stubRegistry{answers: []answer{{status: http.StatusServiceUnavailable}}}
	retried := make(chan struct{}, 4)
	log := slog.New(signalHandler{msg: "registry request retried", c: retried})
	done := make(chan error, 1)
	go func() {
		_, err := roundTrip(stub, log, tagsRequest(ctx))
		done <- err
	}()
	<-retried // the first attempt failed; the transport waits an hour now
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) || stub.attempts != 1 {
			t.Errorf("RoundTrip error = %v after %d attempts, want context.Canceled after 1", err, stub.attempts)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("RoundTrip still waits after the context ended")
	}
}

// signalHandler is a slog.Handler that sends on c for each record whose message is msg and
// drops every record.
type signalHandler struct {
	msg string
	c   chan<- struct{}
}

func (h signalHandler) Enabled(context.Context, slog.Level) bool { return true }

func (h signalHandler) Handle(_ context.Context, r slog.Record) error {
	if r.Message == h.msg {
		h.c <- struct{}{}
	}
	return nil
}

func (h signalHandler) WithAttrs([]slog.Attr) slog.Handler { return h }

func (h signalHandler) WithGroup(string) slog.Handler { return h }

// TestRetryAfter checks that a Retry-After within retryWait.max is waited and one above it is
// not: the 429 is the answer then.
func TestRetryAfter(t *testing.T) {
	defer func(w struct{ min, max time.Duration }) { retryWait = w }(retryWait)
	retryWait.min, retryWait.max = 0, 2*time.Second
	for after, want := range map[string]int{"0": 2, "1": 2, "60": 1} {
		t.Run(after, func(t *testing.T) {
			stub := &stubRegistry{answers: []answer{
				{status: http.StatusTooManyRequests, header: http.Header{"Retry-After": {after}}}, {status: http.StatusOK},
			}}
			log, _ := testLogger()
			_, _ = roundTrip(stub, log, tagsRequest(t.Context()))
			if stub.attempts != want {
				t.Errorf("%d attempts, want %d", stub.attempts, want)
			}
		})
	}
}

// TestRetryLog checks that each retry is a debug line that names the method, the path and the
// cause, and shows no header and no query.
func TestRetryLog(t *testing.T) {
	stub := &stubRegistry{answers: []answer{{status: http.StatusServiceUnavailable}, {err: syscall.ECONNRESET}, {status: http.StatusOK}}}
	log, buf := testLogger()
	if status, err := roundTrip(stub, log, tagsRequest(t.Context())); err != nil || status != http.StatusOK {
		t.Fatalf("RoundTrip = %d, %v; want 200", status, err)
	}
	out := buf.String()
	if n := strings.Count(out, "registry request retried"); n != 2 {
		t.Errorf("%d retry lines, want 2:\n%s", n, out)
	}
	for _, want := range []string{"method=GET", "path=/v2/raw/x/tags/list", "attempt=1", "cause=503", "attempt=2", "cause=error"} {
		if !strings.Contains(out, want) {
			t.Errorf("log has no %q:\n%s", want, out)
		}
	}
	for _, secret := range []string{"s3cr3t", "t0ken", "n=1"} {
		if strings.Contains(out, secret) {
			t.Errorf("log shows %q:\n%s", secret, out)
		}
	}
}

// timeoutErr is a net.Error that timed out.
type timeoutErr struct{}

func (timeoutErr) Error() string   { return "i/o timeout" }
func (timeoutErr) Timeout() bool   { return true }
func (timeoutErr) Temporary() bool { return true }

// TestPushRetry checks that a blob push that fails is done again from the start, in a new
// session, when its content can be rewound, and once when it is a stream.
func TestPushRetry(t *testing.T) {
	tests := []struct {
		name      string
		content   func(s string) io.Reader
		failures  int // 503 answers to the PUT that closes the session
		wantPosts int
		wantErr   bool
	}{
		{name: "rewindable, the registry recovers", content: func(s string) io.Reader { return strings.NewReader(s) }, failures: 2, wantPosts: 3},
		{name: "rewindable, the registry keeps failing", content: func(s string) io.Reader { return strings.NewReader(s) }, failures: 100, wantPosts: 1 + config.DefaultRegistryRetries, wantErr: true},
		{name: "stream", content: func(s string) io.Reader { return struct{ io.Reader }{strings.NewReader(s)} }, failures: 1, wantPosts: 1, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f, repo := uploadRepo(t, nil)
			f.hook = failFirst(tt.failures, http.StatusServiceUnavailable, func(r *http.Request) bool {
				return r.Method == http.MethodPut && strings.Contains(r.URL.Path, "/blobs/uploads/")
			})
			desc := ocispec.Descriptor{MediaType: "application/octet-stream", Digest: digest.FromString("hello"), Size: 5}
			err := repo.Push(t.Context(), desc, tt.content("hello"))
			if (err != nil) != tt.wantErr {
				t.Fatalf("Push error = %v, want error %v", err, tt.wantErr)
			}
			if n := countMatching(f, http.MethodPost, "/blobs/uploads/"); n != tt.wantPosts {
				t.Errorf("%d upload sessions, want %d", n, tt.wantPosts)
			}
			if !tt.wantErr {
				if ok, err := repo.Exists(t.Context(), desc); err != nil || !ok {
					t.Errorf("Exists after the push = %v, %v; want true", ok, err)
				}
			}
		})
	}
}
