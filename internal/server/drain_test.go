package server

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
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
