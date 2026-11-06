package server

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"testing"
	"time"
)

// start runs s in a goroutine. It returns the address, the function that cancels Run's context
// and the channel Run's result arrives on.
func start(t *testing.T, s *Server) (string, context.CancelFunc, <-chan error) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx) }()
	<-s.bound
	addr := s.Addr()
	if addr == nil {
		t.Fatalf("Run did not bind: %v", <-done)
	}
	return addr.String(), cancel, done
}

func TestRunGracefulShutdown(t *testing.T) {
	entered := make(chan struct{})
	shutdown := make(chan struct{})
	s, err := New(Options{
		Listen: "127.0.0.1:0",
		Log:    slog.New(slog.DiscardHandler),
		// The in-flight request: /-/readyz blocks in Ready until the shutdown has begun.
		Ready: func(context.Context) error {
			close(entered)
			<-shutdown
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	s.srv.RegisterOnShutdown(func() { close(shutdown) }) // runs once the listener is closed

	addr, cancel, done := start(t, s)

	type result struct {
		status int
		body   string
		err    error
	}
	got := make(chan result, 1)
	go func() {
		resp, err := http.Get("http://" + addr + "/-/readyz")
		if err != nil {
			got <- result{err: err}
			return
		}
		defer func() { _ = resp.Body.Close() }()
		b, err := io.ReadAll(resp.Body)
		got <- result{resp.StatusCode, string(b), err}
	}()

	<-entered
	cancel()

	r := <-got
	if r.err != nil || r.status != http.StatusOK || r.body != "ok\n" {
		t.Errorf("in-flight request = %d %q %v, want 200 ok", r.status, r.body, r.err)
	}
	if err := <-done; err != nil {
		t.Errorf("Run = %v, want nil after a clean stop", err)
	}
	if c, err := net.DialTimeout("tcp", addr, time.Second); err == nil {
		_ = c.Close()
		t.Errorf("connection to %s accepted after shutdown", addr)
	}
}

func TestRunShutdownTimeout(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	s, err := New(Options{
		Listen:          "127.0.0.1:0",
		Log:             slog.New(slog.DiscardHandler),
		ShutdownTimeout: 20 * time.Millisecond,
		Ready: func(context.Context) error {
			close(entered)
			<-release // outlives the shutdown timeout
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	addr, cancel, done := start(t, s)

	go func() {
		if resp, err := http.Get("http://" + addr + "/-/readyz"); err == nil {
			_ = resp.Body.Close()
		}
	}()
	<-entered
	cancel()
	if err := <-done; !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("Run = %v, want an error wrapping context.DeadlineExceeded", err)
	}
}

func TestRunListenError(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()

	s, err := New(Options{Listen: ln.Addr().String(), Log: slog.New(slog.DiscardHandler)})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Run(context.Background()); err == nil {
		t.Fatal("Run on a used address succeeded")
	}
	if s.Addr() != nil {
		t.Errorf("Addr = %v after a failed listen, want nil", s.Addr())
	}
	if err := s.Run(context.Background()); err == nil {
		t.Error("second Run succeeded, want an error")
	}
}
