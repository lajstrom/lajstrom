package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
)

var hex32 = regexp.MustCompile(`^[0-9a-f]{32}$`)

func TestRequestID(t *testing.T) {
	long64 := strings.Repeat("a", 64)
	tests := []struct {
		name   string
		client string // X-Request-Id sent by the client; "" sends none
		want   string // the id expected; "" means a generated one
	}{
		{name: "client id is kept", client: "build-42_step.3", want: "build-42_step.3"},
		{name: "64 characters are kept", client: long64, want: long64},
		{name: "missing id is generated"},
		{name: "overlong id is replaced", client: long64 + "a"},
		{name: "space is replaced", client: "a b"},
		{name: "markup is replaced", client: "<script>"},
		{name: "non-ASCII is replaced", client: "ид"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s, _ := newTestServer(t, Options{})
			var seen string
			s.mux.HandleFunc("GET /test/id", func(_ http.ResponseWriter, r *http.Request) {
				seen = RequestID(r.Context())
			})
			req := httptest.NewRequest(http.MethodGet, "/test/id", nil)
			if tt.client != "" {
				req.Header.Set("X-Request-Id", tt.client)
			}
			rr := httptest.NewRecorder()
			s.Handler().ServeHTTP(rr, req)

			got := rr.Header().Get("X-Request-Id")
			if seen != got {
				t.Errorf("handler saw %q, response header is %q", seen, got)
			}
			switch {
			case tt.want != "" && got != tt.want:
				t.Errorf("request id = %q, want %q", got, tt.want)
			case tt.want == "" && !hex32.MatchString(got):
				t.Errorf("request id = %q, want 32 hex characters", got)
			}
		})
	}
}

func TestRecovery(t *testing.T) {
	tests := []struct {
		name       string
		path       string
		clientID   string
		wantStatus int   // 0: the handler's panic must reach net/http
		wantPanic  error // the value net/http must see when wantStatus is 0
		wantLog    bool  // an Error line "panic" is logged
	}{
		{name: "panic before writing", path: "/test/panic", clientID: "req-1", wantStatus: 500, wantLog: true},
		{name: "panic with a generated id", path: "/test/panic", wantStatus: 500, wantLog: true},
		{name: "panic under /api/ answers the envelope", path: "/api/v1/test/panic", clientID: "req-api", wantStatus: 500, wantLog: true},
		{name: "panic after writing aborts", path: "/test/panic-after-write", clientID: "req-2", wantPanic: http.ErrAbortHandler, wantLog: true},
		{name: "ErrAbortHandler is not swallowed", path: "/test/abort", clientID: "req-3", wantPanic: http.ErrAbortHandler},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s, buf := newTestServer(t, Options{})
			boom := func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Length", "9999")
				panic("boom")
			}
			s.mux.HandleFunc("GET /test/panic", boom)
			s.mux.HandleFunc("GET /api/v1/test/panic", boom)
			s.mux.HandleFunc("GET /test/panic-after-write", func(w http.ResponseWriter, _ *http.Request) {
				_, _ = io.WriteString(w, "partial")
				panic("boom")
			})
			s.mux.HandleFunc("GET /test/abort", func(http.ResponseWriter, *http.Request) {
				panic(http.ErrAbortHandler)
			})
			req := httptest.NewRequest(http.MethodGet, tt.path, nil)
			if tt.clientID != "" {
				req.Header.Set("X-Request-Id", tt.clientID)
			}
			rr := httptest.NewRecorder()
			panicked := serve(s.Handler(), rr, req)

			if tt.wantStatus == 0 {
				err, _ := panicked.(error)
				if !errors.Is(err, tt.wantPanic) {
					t.Fatalf("panic = %v, want %v", panicked, tt.wantPanic)
				}
			} else {
				if panicked != nil {
					t.Fatalf("handler panic escaped: %v", panicked)
				}
				checkInternalError(t, rr, tt.wantStatus, tt.path)
			}

			id := rr.Header().Get("X-Request-Id")
			if tt.clientID != "" && id != tt.clientID {
				t.Errorf("request id = %q, want %q", id, tt.clientID)
			}
			hasPanicLog := strings.Contains(buf.String(), `"msg":"panic"`)
			if hasPanicLog != tt.wantLog {
				t.Fatalf("panic logged = %v, want %v; log:\n%s", hasPanicLog, tt.wantLog, buf)
			}
			if tt.wantLog {
				l := findLog(t, buf, "panic")
				if l["level"] != "ERROR" || l["requestId"] != id || l["panic"] != "boom" || l["path"] != tt.path {
					t.Errorf("panic log line = %v, want level ERROR, requestId %q, panic boom, path %s", l, id, tt.path)
				}
				if stack, _ := l["stack"].(string); !strings.Contains(stack, "middleware_test.go") {
					t.Errorf("panic log has no stack of the handler: %q", stack)
				}
			}
		})
	}
}

// TestRecoveryAbortCause checks an abort that says why: net/http gets http.ErrAbortHandler
// itself, which it does not log, and the cause is logged at Error with the request id, the
// status and the bytes written. A bare http.ErrAbortHandler logs nothing.
func TestRecoveryAbortCause(t *testing.T) {
	s, buf := newTestServer(t, Options{})
	s.mux.HandleFunc("GET /test/abort-cause", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "partial")
		panic(fmt.Errorf("the bytes are not the digest: %w", http.ErrAbortHandler))
	})
	s.mux.HandleFunc("GET /test/abort", func(http.ResponseWriter, *http.Request) { panic(http.ErrAbortHandler) })
	for _, path := range []string{"/test/abort-cause", "/test/abort"} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Header.Set("X-Request-Id", "req-4")
		if panicked := serve(s.Handler(), httptest.NewRecorder(), req); panicked != any(http.ErrAbortHandler) {
			t.Fatalf("%s: panic = %v, want http.ErrAbortHandler itself", path, panicked)
		}
	}
	l := findLog(t, buf, "response aborted")
	if l["level"] != "ERROR" || l["requestId"] != "req-4" || l["path"] != "/test/abort-cause" || l["status"] != float64(200) ||
		l["bytes"] != float64(len("partial")) || l["error"] != "the bytes are not the digest: "+http.ErrAbortHandler.Error() {
		t.Errorf("abort log line = %v", l)
	}
	if n := strings.Count(buf.String(), `"msg":"response aborted"`); n != 1 {
		t.Errorf("%d lines response aborted, want 1 (none for the bare abort):\n%s", n, buf)
	}
}

// serve calls h and returns the value of a panic that escapes it, as net/http would see it.
func serve(h http.Handler, w http.ResponseWriter, r *http.Request) (panicked any) {
	defer func() { panicked = recover() }()
	h.ServeHTTP(w, r)
	return nil
}

// checkInternalError checks the 500 of a panic on path: the envelope of the management API under
// /api/ (docs/api.md, Errors), the error and the request id elsewhere.
func checkInternalError(t *testing.T, rr *httptest.ResponseRecorder, status int, path string) {
	t.Helper()
	if rr.Code != status {
		t.Errorf("status = %d, want %d", rr.Code, status)
	}
	if ct := rr.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
	if cl := rr.Header().Get("Content-Length"); cl != "" {
		t.Errorf("Content-Length %q set by the handler survived", cl)
	}
	if strings.HasPrefix(path, "/api/") {
		var env struct {
			Error struct {
				Code, Message string
			} `json:"error"`
		}
		if err := json.Unmarshal(rr.Body.Bytes(), &env); err != nil || env.Error.Code != "INTERNAL" ||
			env.Error.Message != "Internal Server Error" {
			t.Errorf("body %q (%v), want the envelope with INTERNAL", rr.Body, err)
		}
		return
	}
	var body struct {
		Error     string `json:"error"`
		RequestID string `json:"requestId"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("body %q: %v", rr.Body, err)
	}
	if body.Error != "internal server error" || body.RequestID != rr.Header().Get("X-Request-Id") || body.RequestID == "" {
		t.Errorf("body = %+v, want the error and the request id %q", body, rr.Header().Get("X-Request-Id"))
	}
}

func TestAccessLog(t *testing.T) {
	s, buf := newTestServer(t, Options{})
	req := httptest.NewRequest(http.MethodGet, "/-/healthz?token=sekrit", nil)
	req.Header.Set("Authorization", "Basic dXNlcjpodW50ZXIy")
	req.Header.Set("X-Request-Id", "req-log")
	req.RemoteAddr = "192.0.2.7:51234"
	s.Handler().ServeHTTP(httptest.NewRecorder(), req)

	l := findLog(t, buf, "request")
	want := map[string]any{
		"level":      "INFO",
		"method":     "GET",
		"path":       "/-/healthz",
		"status":     float64(200),
		"bytes":      float64(len("ok\n")),
		"requestId":  "req-log",
		"remoteAddr": "192.0.2.7:51234",
		"principal":  "anonymous",
	}
	for k, v := range want {
		if l[k] != v {
			t.Errorf("access log %s = %v, want %v", k, l[k], v)
		}
	}
	if _, ok := l["duration"]; !ok {
		t.Errorf("access log has no duration: %v", l)
	}
	for _, secret := range []string{"sekrit", "dXNlcjpodW50ZXIy", "Authorization"} {
		if strings.Contains(buf.String(), secret) {
			t.Errorf("log contains %q:\n%s", secret, buf)
		}
	}
}

func TestRecorderKeepsFlusher(t *testing.T) {
	s, buf := newTestServer(t, Options{})
	s.mux.HandleFunc("GET /test/stream", func(w http.ResponseWriter, _ *http.Request) {
		if _, ok := w.(http.Flusher); !ok {
			t.Error("ResponseWriter does not implement http.Flusher")
		}
		w.WriteHeader(http.StatusAccepted)
		_, _ = io.WriteString(w, "chunk")
		if err := http.NewResponseController(w).Flush(); err != nil {
			t.Errorf("ResponseController.Flush: %v", err)
		}
	})
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/test/stream", nil))
	if !rr.Flushed {
		t.Error("response was not flushed")
	}
	if l := findLog(t, buf, "request"); l["status"] != float64(202) || l["bytes"] != float64(5) {
		t.Errorf("access log status %v bytes %v, want 202 and 5", l["status"], l["bytes"])
	}
}
