package server

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/lajstrom/lajstrom/internal/format"
	"github.com/lajstrom/lajstrom/internal/format/formattest"
)

// stallBound is the StallTimeout of the stall tests.
const stallBound = 300 * time.Millisecond

// stallServer runs a server with StallTimeout stallBound whose /v2/ is h, and returns its
// address.
func stallServer(t *testing.T, h http.HandlerFunc) string {
	t.Helper()
	s, err := New(Options{Listen: "127.0.0.1:0", Log: slog.New(slog.DiscardHandler), StallTimeout: stallBound, Facade: h})
	if err != nil {
		t.Fatal(err)
	}
	addr, _, _ := start(t, s)
	return addr
}

// rawRequest dials addr and sends head, a request without its body.
func rawRequest(t *testing.T, addr, head string) net.Conn {
	t.Helper()
	c, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	if _, err := io.WriteString(c, head); err != nil {
		t.Fatal(err)
	}
	return c
}

// readBody is a handler that reads the body, and once more after its end, as a decoder that
// looks for trailing data does; it reports how that went, waits after, and answers 201.
func readBody(got chan<- error, after time.Duration) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		_, err := io.ReadAll(r.Body)
		if err == nil {
			if _, again := r.Body.Read(make([]byte, 1)); again != io.EOF {
				err = again
			}
		}
		if err == nil {
			time.Sleep(after)
			err = r.Context().Err() // the request must still be alive
		}
		got <- err
		if err == nil {
			w.WriteHeader(http.StatusCreated)
		}
	}
}

func TestStallRequestBody(t *testing.T) {
	head := "PUT /v2/x HTTP/1.1\r\nHost: x\r\nContent-Length: 10\r\n\r\n"
	t.Run("a client that stops sending is cut off", func(t *testing.T) {
		got := make(chan error, 1)
		c := rawRequest(t, stallServer(t, readBody(got, 0)), head+"abc")
		select {
		case err := <-got:
			if !errors.Is(err, os.ErrDeadlineExceeded) {
				t.Errorf("body read = %v, want a deadline error", err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("the handler still waits for a body that stopped")
		}
		// net/http reads the rest of a small body before it answers; that must not wait for
		// the client either.
		wantClosed(t, c)
	})
	t.Run("a body the handler leaves unread does not hold the connection", func(t *testing.T) {
		c := rawRequest(t, stallServer(t, func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusAccepted)
		}), head+"abc")
		wantClosed(t, c)
	})
	t.Run("a slow client that keeps sending passes", func(t *testing.T) {
		got := make(chan error, 1)
		c := rawRequest(t, stallServer(t, readBody(got, 0)), head)
		for range 10 { // 10 bytes over 1 s, more than three times the bound
			time.Sleep(stallBound / 3)
			_, _ = io.WriteString(c, "x")
		}
		if err := <-got; err != nil {
			t.Errorf("body read = %v, want the whole body", err)
		}
		wantStatus(t, c, http.StatusCreated)
	})
	t.Run("the handler may work after the body for longer than the bound", func(t *testing.T) {
		got := make(chan error, 1)
		c := rawRequest(t, stallServer(t, readBody(got, 2*stallBound)), head+"0123456789")
		if err := <-got; err != nil {
			t.Errorf("after the body: %v, want the request alive", err)
		}
		wantStatus(t, c, http.StatusCreated)
	})
}

// TestStallAnswer: a body cut by the stall bound is the client's doing: the handler that
// answers its read error with format.WriteError answers 408, and the server logs the stall at
// info, with the bytes read, and nothing at error (#576).
func TestStallAnswer(t *testing.T) {
	s, logs := newTestServer(t, Options{StallTimeout: stallBound, Facade: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, err := io.ReadAll(r.Body)
		format.WriteError(w, fmt.Errorf("upload: %w", err))
	})})
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodPut, "/v2/x", formattest.Stalled(strings.NewReader("abc"))))
	if rr.Code != http.StatusRequestTimeout {
		t.Errorf("status %d %q, want 408", rr.Code, rr.Body)
	}
	l := findLog(t, logs, "request body stalled: nothing came within server.stallTimeout, the connection is closed")
	if l["level"] != "INFO" || l["read"] != 3.0 || l["path"] != "/v2/x" || l["stallTimeout"] != stallBound.String() {
		t.Errorf("stall log %v", l)
	}
	for _, l := range logLines(t, logs) {
		if l["level"] == "ERROR" {
			t.Errorf("logged at error: %v", l)
		}
	}
}

// wantStatus reads a response from c and checks its status.
func wantStatus(t *testing.T, c net.Conn, want int) {
	t.Helper()
	_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
	resp, err := http.ReadResponse(bufio.NewReader(c), nil)
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != want {
		t.Errorf("status %d, want %d", resp.StatusCode, want)
	}
}

// wantClosed checks that the server answers on c and closes it within a few stall bounds.
func wantClosed(t *testing.T, c net.Conn) {
	t.Helper()
	_ = c.SetReadDeadline(time.Now().Add(10 * stallBound))
	b, err := io.ReadAll(c)
	if err != nil {
		t.Fatalf("connection still open after the stall: %v (read %q)", err, b)
	}
	if !strings.HasPrefix(string(b), "HTTP/1.1 ") {
		t.Errorf("read %q before the close, want a response", b)
	}
}

func TestStallResponse(t *testing.T) {
	t.Run("a client that stops reading is cut off", func(t *testing.T) {
		got := make(chan error, 1)
		addr := stallServer(t, func(w http.ResponseWriter, _ *http.Request) {
			chunk := make([]byte, 32<<10)
			for range 4096 { // 128 MiB, more than the socket buffers hold
				if _, err := w.Write(chunk); err != nil {
					got <- err
					return
				}
			}
			got <- nil
		})
		rawRequest(t, addr, "GET /v2/big HTTP/1.1\r\nHost: x\r\n\r\n") // and never read
		select {
		case err := <-got:
			if err == nil {
				t.Error("the whole response was written to a client that reads nothing")
			}
		case <-time.After(10 * time.Second):
			t.Fatal("the handler still writes to a client that stopped reading")
		}
	})
	t.Run("the handler may work after its last write for longer than the bound", func(t *testing.T) {
		// The write is buffered; net/http sends it when the handler returns, and a deadline set
		// by the write would have passed by then.
		addr := stallServer(t, func(w http.ResponseWriter, _ *http.Request) {
			_, _ = io.WriteString(w, "partial")
			time.Sleep(2 * stallBound)
		})
		resp, err := http.Get("http://" + addr + "/v2/slow")
		if err != nil {
			t.Fatalf("GET: %v", err)
		}
		defer func() { _ = resp.Body.Close() }()
		if b, err := io.ReadAll(resp.Body); err != nil || string(b) != "partial" {
			t.Errorf("body %q, %v; want partial", b, err)
		}
	})
}

func TestMaxUploads(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	block := func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/block") {
			entered <- struct{}{}
			<-release
		}
		w.WriteHeader(http.StatusCreated)
	}
	s, _ := newTestServer(t, Options{MaxUploads: 1, Facade: http.HandlerFunc(block), API: http.HandlerFunc(block)})
	serve := func(method, path string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		s.Handler().ServeHTTP(rec, httptest.NewRequest(method, path, strings.NewReader("body")))
		return rec
	}
	first := make(chan int, 1)
	go func() { first <- serve(http.MethodPut, "/v2/raw/x/blobs/uploads/1/block").Code }()
	<-entered

	tests := []struct {
		method, path string
		want         int
	}{
		{http.MethodPatch, "/v2/raw/x/blobs/uploads/2", http.StatusServiceUnavailable},
		{http.MethodPost, "/v2/raw/x/blobs/uploads/", http.StatusServiceUnavailable},
		{http.MethodGet, "/v2/raw/x/blobs/sha256:00", http.StatusCreated},
		{http.MethodPost, "/v2/token", http.StatusCreated},
		{http.MethodPost, "/api/v1/repos", http.StatusCreated},
	}
	for _, tt := range tests {
		rec := serve(tt.method, tt.path)
		if rec.Code != tt.want {
			t.Errorf("%s %s while an upload runs = %d, want %d", tt.method, tt.path, rec.Code, tt.want)
		}
		if rec.Code == http.StatusServiceUnavailable {
			if rec.Header().Get("Retry-After") == "" || !strings.Contains(rec.Body.String(), `"UNAVAILABLE"`) {
				t.Errorf("%s %s refused without Retry-After or the UNAVAILABLE error: %v %q", tt.method, tt.path, rec.Header(), rec.Body)
			}
		}
	}
	if rec := serve(http.MethodPut, "/raw/scratch/file"); rec.Code != http.StatusServiceUnavailable || rec.Header().Get("Retry-After") == "" {
		t.Errorf("a frontend upload while an upload runs = %d %v, want 503 with Retry-After", rec.Code, rec.Header())
	}

	close(release)
	if code := <-first; code != http.StatusCreated {
		t.Errorf("the first upload = %d, want 201", code)
	}
	if rec := serve(http.MethodPatch, "/v2/raw/x/blobs/uploads/2"); rec.Code != http.StatusCreated {
		t.Errorf("an upload after the first ended = %d, want 201", rec.Code)
	}
}
