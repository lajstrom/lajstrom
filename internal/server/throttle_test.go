package server

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/lajstrom/lajstrom/internal/auth"
)

// serveAs answers a GET of path with the Authorization header authz.
func serveAs(s *Server, path, authz string) *httptest.ResponseRecorder {
	rr := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, path, nil)
	r.Header.Set("Authorization", authz)
	s.Handler().ServeHTTP(rr, r)
	return rr
}

// TestThrottled checks the answer of an account under throttle (auth.ErrThrottled): 429 with
// Retry-After, no challenge, in the error format of the route's family, the handler not called,
// a log line at info with the account and the address and without the password; a personal
// access token of the account still works.
func TestThrottled(t *testing.T) {
	const wrongPassword = "not-alice-7"
	s, sw, logs := authServer(t, usersReader{})
	for range auth.LoginFailures {
		if rr := serveAs(s, "/test/x", basic("alice", wrongPassword)); rr.Code != http.StatusUnauthorized {
			t.Fatalf("a wrong password: %d, want 401", rr.Code)
		}
	}
	const msg = "too many failed logins: try again after Retry-After seconds"
	tests := []struct{ path, body string }{
		{"/test/x", msg + "\n"},
		{"/v2/raw/scratch/x/tags/list", `{"errors":[{"code":"TOOMANYREQUESTS","message":"` + msg + `"}]}` + "\n"},
		{"/v2/token", `{"errors":[{"code":"TOOMANYREQUESTS","message":"` + msg + `"}]}` + "\n"},
		{"/api/v1/repos", `{"error":{"code":"UNAVAILABLE","message":"` + msg + `"}}` + "\n"},
	}
	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			*sw = seen{}
			wantThrottled(t, serveAs(s, tt.path, basic("alice", alicePassword)), tt.body, 60)
			if sw.called {
				t.Error("the handler was called")
			}
		})
	}
	l := findLog(t, logs, "throttled")
	if l["level"] != "INFO" || l["throttle"] != "account" || l["account"] != "user:"+aliceUID || l["address"] != "192.0.2.1" {
		t.Errorf("log line = %v", l)
	}
	if strings.Contains(logs.String(), alicePassword) || strings.Contains(logs.String(), wrongPassword) {
		t.Errorf("the log contains a password:\n%s", logs)
	}

	if rr := serveAs(s, "/test/x", "Bearer "+alicePAT); rr.Code != http.StatusNoContent || sw.principal.String() != "user:"+aliceUID {
		t.Errorf("a personal access token under throttle: %d as %v, want 204 as user:%s", rr.Code, sw.principal, aliceUID)
	}
}

// TestAddressThrottleTLS: with server.tls and no server.trustedProxies the peer is the client,
// and AddressFailures failed logins from it, on as many accounts, throttle the address: the
// next request from it is 429 whatever its password, another address goes on (#248). Without
// server.tls the per-address throttle is off, as behind an edge without the list.
func TestAddressThrottleTLS(t *testing.T) {
	for _, withTLS := range []bool{true, false} {
		t.Run(fmt.Sprintf("tls=%t", withTLS), func(t *testing.T) {
			o := Options{Authenticator: auth.NewAuthenticator(auth.Options{Index: usersReader{}})}
			if withTLS {
				o.TLSCertFile, o.TLSKeyFile, _ = selfSigned(t)
			}
			s, logs := newTestServer(t, o)
			s.mux.HandleFunc("/test/", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
			from := func(addr, authz string) *httptest.ResponseRecorder {
				rr := httptest.NewRecorder()
				r := httptest.NewRequest(http.MethodGet, "/test/x", nil)
				r.RemoteAddr = addr + ":443"
				r.Header.Set("Authorization", authz)
				s.Handler().ServeHTTP(rr, r)
				return rr
			}
			for i := range auth.AddressFailures {
				if rr := from("192.0.2.77", basic(fmt.Sprintf("nobody-%d", i), "wrong")); rr.Code != http.StatusUnauthorized {
					t.Fatalf("failed login %d: %d, want 401", i, rr.Code)
				}
			}
			rr := from("192.0.2.77", basic("alice", alicePassword))
			if !withTLS {
				if rr.Code != http.StatusNoContent {
					t.Errorf("without server.tls, alice from the address: %d, want 204", rr.Code)
				}
				return
			}
			wantThrottled(t, rr, "too many failed logins: try again after Retry-After seconds\n", 60)
			if l := findLog(t, logs, "throttled"); l["throttle"] != "address" || l["address"] != "192.0.2.77" {
				t.Errorf("log line = %v", l)
			}
			if rr := from("192.0.2.78", basic("alice", alicePassword)); rr.Code != http.StatusNoContent {
				t.Errorf("alice from another address: %d, want 204", rr.Code)
			}
		})
	}
}

// wantThrottled checks a 429 with body, a Retry-After of 1 to most seconds and no challenge.
func wantThrottled(t *testing.T, rr *httptest.ResponseRecorder, body string, most int) {
	t.Helper()
	if rr.Code != http.StatusTooManyRequests || rr.Body.String() != body {
		t.Errorf("answer %d %q, want 429 %q", rr.Code, rr.Body, body)
	}
	if n, err := strconv.Atoi(rr.Header().Get("Retry-After")); err != nil || n < 1 || n > most {
		t.Errorf("Retry-After %q, want 1 to %d seconds", rr.Header().Get("Retry-After"), most)
	}
	if c := rr.Header().Get("WWW-Authenticate"); c != "" {
		t.Errorf("a 429 has the challenge %q", c)
	}
}

func TestRetryAfter(t *testing.T) {
	for d, want := range map[time.Duration]string{0: "1", time.Nanosecond: "1", time.Second: "1", 1200 * time.Millisecond: "2", time.Minute: "60"} {
		if got := retryAfter(d); got != want {
			t.Errorf("retryAfter(%v) = %q, want %q", d, got, want)
		}
	}
}
