package server

import (
	"bufio"
	"bytes"
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/lajstrom/lajstrom/internal/metrics"
)

// TestClientGone checks that only a plain cancel of the request's context is the client's
// leaving: not the abort at the shutdown deadline, which net/http's own cancel follows when
// Close closes the connection, not a deadline, not a live request.
func TestClientGone(t *testing.T) {
	tests := []struct {
		name string
		ctx  func(t *testing.T) context.Context
		gone bool
	}{
		{"live", func(*testing.T) context.Context { return context.Background() }, false},
		{"connection closed", func(*testing.T) context.Context {
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			return ctx
		}, true},
		{"aborted at the shutdown deadline", func(t *testing.T) context.Context {
			s, _ := newTestServer(t, Options{})
			ctx, cancel := context.WithCancel(s.baseCtx) // as net/http derives a request's
			s.abort(errAborted)
			cancel() // the connection closed after the abort
			return ctx
		}, false},
		{"deadline", func(t *testing.T) context.Context {
			ctx, cancel := context.WithTimeout(context.Background(), time.Nanosecond)
			t.Cleanup(cancel)
			<-ctx.Done()
			return ctx
		}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := httptest.NewRequestWithContext(tt.ctx(t), http.MethodGet, "/", nil)
			level := map[bool]slog.Level{false: slog.LevelError, true: slog.LevelDebug}[tt.gone]
			if got := ClientGone(r); got != tt.gone {
				t.Errorf("ClientGone = %v, want %v (cause %v)", got, tt.gone, context.Cause(r.Context()))
			}
			if got := FailLevel(r); got != level {
				t.Errorf("FailLevel = %v, want %v", got, level)
			}
		})
	}
}

// TestAccessLogClientGone checks the acceptance criteria of #877: a handler that waits for its
// client, which leaves, and then answers 500, has an access line with status 499 and no Error
// line, and is counted with status 499.
func TestAccessLogClientGone(t *testing.T) {
	s, buf := newTestServer(t, Options{})
	entered := make(chan struct{})
	s.mux.HandleFunc("GET /test/gone", func(w http.ResponseWriter, r *http.Request) {
		close(entered)
		<-r.Context().Done()
		s.log.Log(r.Context(), FailLevel(r), "test: request failed", "status", 500, "error", r.Context().Err().Error())
		http.Error(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
	})
	addr, cancel, done := start(t, s)
	c, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	send(t, c, "/test/gone")
	<-entered
	_ = c.Close()
	stop(t, s, cancel, done)

	l := findLog(t, buf, "request")
	if l["status"] != float64(StatusClientClosedRequest) || l["level"] != "INFO" || l["aborted"] != nil {
		t.Errorf("access line %v, want INFO with status 499 and no aborted", l)
	}
	for _, l := range logLines(t, buf) {
		if l["level"] == "ERROR" {
			t.Errorf("an Error line for a client that left: %v", l)
		}
	}
	if l := findLog(t, buf, "test: request failed"); l["level"] != "DEBUG" {
		t.Errorf("the cause is logged at %v, want DEBUG", l["level"])
	}
	m := scrape(t)
	for _, series := range []string{"lajstrom_http_request_duration_seconds_count", "lajstrom_http_response_size_bytes_count"} {
		if want := series + `{method="GET",route="GET /test/gone",status="499"}`; !strings.Contains(m, want) {
			t.Errorf("/metrics has no %s", want)
		}
	}
	if strings.Contains(m, `route="GET /test/gone",status="500"`) {
		t.Error("the request is counted as a 500")
	}
}

// TestAccessLogAborted checks aborted=true on the access line of a response whose client left
// in the middle of its body, which keeps its status, and not on one the client got whole before
// it closed the connection while the handler was still at work.
func TestAccessLogAborted(t *testing.T) {
	tests := []struct {
		name    string
		handler http.HandlerFunc
		read    int64 // how much of the body the client reads before it leaves
		aborted bool
	}{
		{"fewer bytes than declared", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Length", "1000")
			_, _ = io.WriteString(w, "first")
			_ = http.NewResponseController(w).Flush()
			<-r.Context().Done()
		}, 5, true},
		{"a write failed", func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.WriteString(w, "first") // chunked, no Content-Length
			_ = http.NewResponseController(w).Flush()
			<-r.Context().Done()
			chunk := bytes.Repeat([]byte("x"), 64<<10)
			for range 1024 {
				if _, err := w.Write(chunk); err != nil {
					return
				}
				_ = http.NewResponseController(w).Flush()
			}
			t.Error("64 MiB written to a closed connection without an error")
		}, 0, true},
		{"complete", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Length", "5")
			_, _ = io.WriteString(w, "whole")
			_ = http.NewResponseController(w).Flush()
			<-r.Context().Done() // still finishing when the client leaves
		}, 5, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s, buf := newTestServer(t, Options{})
			s.mux.Handle("GET /test/body", tt.handler)
			addr, cancel, done := start(t, s)
			c, err := net.Dial("tcp", addr)
			if err != nil {
				t.Fatal(err)
			}
			send(t, c, "/test/body")
			resp, err := http.ReadResponse(bufio.NewReader(c), nil)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = resp.Body.Close() }()
			if _, err := io.CopyN(io.Discard, resp.Body, tt.read); err != nil {
				t.Fatal(err)
			}
			_ = c.Close()
			stop(t, s, cancel, done)

			l := findLog(t, buf, "request")
			if aborted := l["aborted"] == true; l["status"] != float64(http.StatusOK) || aborted != tt.aborted {
				t.Errorf("access line %v, want status 200 and aborted %v", l, tt.aborted)
			}
		})
	}
}

// stop waits for the handlers of s to return and then stops it.
func stop(t *testing.T, s *Server, cancel context.CancelFunc, done <-chan error) {
	t.Helper()
	if !s.waitHandlers(5 * time.Second) {
		t.Fatal("the handler did not return after its client left")
	}
	cancel()
	if err := <-done; err != nil {
		t.Errorf("Run: %v", err)
	}
}

// scrape returns what the metrics listener answers.
func scrape(t *testing.T) string {
	t.Helper()
	rr := httptest.NewRecorder()
	metrics.Handler(false).ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("/metrics: %d", rr.Code)
	}
	return rr.Body.String()
}
