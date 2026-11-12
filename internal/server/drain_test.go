package server

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// uploadHandler reads the whole body, tells entered when the first bytes are in, and answers
// 201 with the number of bytes. ended receives the error of the read and of the request's
// context when the handler returns.
func uploadHandler(entered chan<- struct{}, ended chan<- error) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		buf := make([]byte, 1)
		n, err := io.ReadFull(r.Body, buf)
		entered <- struct{}{}
		if err == nil {
			var rest int64
			rest, err = io.Copy(io.Discard, r.Body)
			n += int(rest)
		}
		ended <- errors.Join(err, r.Context().Err())
		if err != nil {
			return
		}
		w.WriteHeader(http.StatusCreated)
		_, _ = fmt.Fprint(w, n)
	}
}

// TestDrainCompletesUpload starts an upload, stops the server in the middle of it and checks
// that /-/readyz turns 503, that new connections are refused, that the upload completes and
// that Run returns nil.
func TestDrainCompletesUpload(t *testing.T) {
	entered, ended := make(chan struct{}, 1), make(chan error, 1)
	s, err := New(Options{Listen: "127.0.0.1:0", Log: slog.New(slog.DiscardHandler), ShutdownTimeout: time.Minute,
		Facade: uploadHandler(entered, ended)})
	if err != nil {
		t.Fatal(err)
	}
	addr, cancel, done := start(t, s)

	pr, pw := io.Pipe()
	type result struct {
		status int
		body   string
		err    error
	}
	got := make(chan result, 1)
	go func() {
		resp, err := http.Post("http://"+addr+"/v2/raw/x/blobs/uploads/1", "application/octet-stream", pr)
		if err != nil {
			got <- result{err: err}
			return
		}
		defer func() { _ = resp.Body.Close() }()
		b, err := io.ReadAll(resp.Body)
		got <- result{resp.StatusCode, string(b), err}
	}()
	if _, err := io.WriteString(pw, "first half,"); err != nil {
		t.Fatal(err)
	}
	<-entered
	cancel() // SIGTERM in the middle of the upload

	waitRefused(t, addr)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/-/readyz", nil))
	if rec.Code != http.StatusServiceUnavailable || rec.Body.String() != "shutting down\n" {
		t.Errorf("/-/readyz while draining = %d %q, want 503 shutting down", rec.Code, rec.Body)
	}
	select {
	case err := <-done:
		t.Fatalf("Run returned %v before the upload ended", err)
	default:
	}

	_, _ = io.WriteString(pw, " second half")
	_ = pw.Close()
	if err := <-ended; err != nil {
		t.Errorf("the handler saw %v, want the whole body and a live request", err)
	}
	if r := <-got; r.err != nil || r.status != http.StatusCreated || r.body != "23" {
		t.Errorf("the upload = %d %q %v, want 201 with 23 bytes", r.status, r.body, r.err)
	}
	if err := <-done; err != nil {
		t.Errorf("Run = %v, want nil after the drain", err)
	}
}

// waitRefused waits until a connection to addr is refused: the listener is closed.
func waitRefused(t *testing.T, addr string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		c, err := net.DialTimeout("tcp", addr, time.Second)
		if err != nil {
			return
		}
		_ = c.Close()
		if time.Now().After(deadline) {
			t.Fatalf("connections to %s are still accepted while draining", addr)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestDrainTimeoutAborts stops the server while an upload stalls: at the shutdown timeout the
// request's context is cancelled and its connection closed, its handler returns before Run
// does, and Run reports the timeout.
func TestDrainTimeoutAborts(t *testing.T) {
	entered, ended := make(chan struct{}, 1), make(chan error, 1)
	s, err := New(Options{Listen: "127.0.0.1:0", Log: slog.New(slog.DiscardHandler), ShutdownTimeout: 100 * time.Millisecond,
		Facade: uploadHandler(entered, ended)})
	if err != nil {
		t.Fatal(err)
	}
	addr, cancel, done := start(t, s)
	c, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	_, _ = io.WriteString(c, "PUT /v2/raw/x/blobs/uploads/1 HTTP/1.1\r\nHost: x\r\nContent-Length: 100\r\n\r\nstarted") // and stalls
	<-entered
	cancel()

	err = <-done
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("Run = %v, want an error wrapping context.DeadlineExceeded", err)
	}
	select {
	case herr := <-ended:
		if herr == nil || !errors.Is(herr, context.Canceled) {
			t.Errorf("the aborted handler saw %v, want its context cancelled", herr)
		}
	default:
		t.Error("Run returned before the aborted handler did")
	}
	if n := s.inflight.Load(); n != 0 {
		t.Errorf("%d handlers still running after Run", n)
	}
}

// TestDrainReadyz checks /-/readyz before and during a drain, with a Ready that is fine.
func TestDrainReadyz(t *testing.T) {
	s, _ := newTestServer(t, Options{Ready: func(context.Context) error { return nil }})
	for _, draining := range []bool{false, true} {
		s.draining.Store(draining)
		rec := httptest.NewRecorder()
		s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/-/readyz", nil))
		want := http.StatusOK
		if draining {
			want = http.StatusServiceUnavailable
		}
		if rec.Code != want || (draining && !strings.Contains(rec.Body.String(), "shutting down")) {
			t.Errorf("draining %v: /-/readyz = %d %q, want %d", draining, rec.Code, rec.Body, want)
		}
	}
}

// TestDrainDelay stops a server with a ShutdownDelay: for the delay /-/readyz answers 503 and
// requests are still served, each answered with Connection: close, the one in flight on a
// keep-alive connection at the signal too; the listener closes and OnShutdown runs only after
// the delay, and Run returns nil.
func TestDrainDelay(t *testing.T) {
	const delay = 2 * time.Second
	entered, release := make(chan struct{}), make(chan struct{})
	var shut atomic.Bool
	s, err := New(Options{Listen: "127.0.0.1:0", Log: slog.New(slog.DiscardHandler), ShutdownDelay: delay,
		// The hook takes a while, as cancelling the proxy fetches may: Run returns only after it.
		OnShutdown: func() { time.Sleep(50 * time.Millisecond); shut.Store(true) },
		Facade: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/v2/slow" {
				close(entered)
				<-release
			}
			_, _ = io.WriteString(w, "served\n")
		})})
	if err != nil {
		t.Fatal(err)
	}
	addr, cancel, done := start(t, s)
	served, closing := answer{http.StatusOK, false, "served\n"}, answer{http.StatusOK, true, "served\n"}

	// A keep-alive connection: the first answer leaves it open, the second request on it is in
	// flight when the signal comes.
	kept, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = kept.Close() }()
	br := bufio.NewReader(kept)
	send(t, kept, "/v2/fast")
	if got := receive(t, br, "/v2/fast"); got != served {
		t.Fatalf("before the signal: GET /v2/fast = %+v, want %+v", got, served)
	}
	send(t, kept, "/v2/slow")
	<-entered
	signalled := time.Now()
	cancel()

	waitNotReady(t, addr)
	if got := ask(t, addr, "/v2/fast"); got != closing {
		t.Errorf("a new request in the delay = %+v, want %+v", got, closing)
	}
	close(release)
	if got := receive(t, br, "/v2/slow"); got != closing {
		t.Errorf("the request in flight on the kept connection = %+v, want %+v", got, closing)
	}
	if _, err := br.ReadByte(); !errors.Is(err, io.EOF) {
		t.Errorf("the kept connection after its answer: read = %v, want EOF", err)
	}
	if shut.Load() {
		t.Error("OnShutdown ran in the delay")
	}
	if took := time.Since(signalled); took >= delay {
		t.Fatalf("the checks in the delay took %v, longer than the delay %v", took, delay)
	}

	if err := <-done; err != nil {
		t.Errorf("Run = %v, want nil after the delay and the drain", err)
	}
	if took := time.Since(signalled); took < delay || !shut.Load() {
		t.Errorf("Run returned %v after the signal, OnShutdown run %v; want after the delay %v, and run", took, shut.Load(), delay)
	}
	if c, err := net.DialTimeout("tcp", addr, time.Second); err == nil {
		_ = c.Close()
		t.Errorf("connection to %s accepted after the drain", addr)
	}
}

// answer is what a test reads of a response: its status, whether it closes the connection
// (Connection: close), and its body.
type answer struct {
	status int
	close  bool
	body   string
}

// waitNotReady waits up to a second for /-/readyz on addr to answer 503, and checks that the
// answer says shutting down and closes the connection.
func waitNotReady(t *testing.T, addr string) {
	t.Helper()
	want := answer{http.StatusServiceUnavailable, true, "shutting down\n"}
	for deadline := time.Now().Add(time.Second); ; time.Sleep(5 * time.Millisecond) {
		got := ask(t, addr, "/-/readyz")
		if got.status == want.status {
			if got != want {
				t.Errorf("/-/readyz in the delay = %+v, want %+v", got, want)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("/-/readyz = %+v a second after the signal, want %+v", got, want)
		}
	}
}

// ask sends a GET of path to addr on a connection of its own and returns the answer.
func ask(t *testing.T, addr, path string) answer {
	t.Helper()
	c, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	defer func() { _ = c.Close() }()
	send(t, c, path)
	return receive(t, bufio.NewReader(c), path)
}

// send writes a GET of path on c.
func send(t *testing.T, c net.Conn, path string) {
	t.Helper()
	if _, err := io.WriteString(c, "GET "+path+" HTTP/1.1\r\nHost: x\r\n\r\n"); err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
}

// receive reads the answer to the GET of path from br.
func receive(t *testing.T, br *bufio.Reader, path string) answer {
	t.Helper()
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("GET %s: body: %v", path, err)
	}
	return answer{resp.StatusCode, resp.Close, string(b)}
}
