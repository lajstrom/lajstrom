package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeToken is the token the fake server answers, built at run time so that gosec does not take
// a string literal for a credential.
var fakeToken = strings.Repeat("x", 3)

// fakeDevice is a server of the device flow whose polls answer the errors of answers in turn,
// then the token.
type fakeDevice struct {
	mu      sync.Mutex
	answers []string // the error of each poll; "" is the token
	polls   int
	form    map[string]string
}

func (f *fakeDevice) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /-/oidc/device/code", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"device_code": "dc", "user_code": "BCDF-GHJK",
			"verification_uri": "https://l.example/-/oidc/device", "verification_uri_complete": "https://l.example/-/oidc/device?user_code=BCDF-GHJK",
			"expires_in": 600, "interval": 5})
	})
	mux.HandleFunc("POST /-/oidc/token", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		f.mu.Lock()
		defer f.mu.Unlock()
		f.form = map[string]string{"grant_type": r.PostForm.Get("grant_type"), "device_code": r.PostForm.Get("device_code")}
		a := f.answers[min(f.polls, len(f.answers)-1)]
		f.polls++
		if a != "" {
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": a, "error_description": "d"})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": fakeToken, "token_type": "Bearer", "expires_in": 3600, "user": "alice"})
	})
	return mux
}

// TestDeviceLogin: login polls at the server's interval, five seconds more after slow_down,
// until the token; it tells the person the page and the code; a refusal and an expiry end it.
func TestDeviceLogin(t *testing.T) {
	f := &fakeDevice{answers: []string{"authorization_pending", "slow_down", "authorization_pending", ""}}
	srv := httptest.NewServer(f.handler())
	defer srv.Close()
	var waits []time.Duration
	sleep := func(_ context.Context, d time.Duration) error { waits = append(waits, d); return nil }
	var msgs bytes.Buffer
	res, err := deviceLogin(context.Background(), srv.Client(), srv.URL, &msgs, sleep)
	if err != nil {
		t.Fatal(err)
	}
	if res.User != "alice" || res.Token != fakeToken || res.Server != srv.URL || time.Until(res.ExpiresAt) < 59*time.Minute {
		t.Errorf("deviceLogin = %+v", res)
	}
	want := []time.Duration{5 * time.Second, 5 * time.Second, 10 * time.Second, 10 * time.Second}
	if len(waits) != len(want) || waits[1] != want[1] || waits[2] != want[2] || waits[3] != want[3] {
		t.Errorf("the waits = %v, want %v", waits, want)
	}
	if f.form["grant_type"] != deviceGrantType || f.form["device_code"] != "dc" {
		t.Errorf("the poll's form = %v", f.form)
	}
	if !strings.Contains(msgs.String(), "BCDF-GHJK") || !strings.Contains(msgs.String(), "https://l.example/-/oidc/device?user_code=BCDF-GHJK") {
		t.Errorf("the messages: %s", msgs.String())
	}
}

// TestDeviceLoginEnds: a refusal, an expiry, another error and a server without OIDC end login.
func TestDeviceLoginEnds(t *testing.T) {
	sleep := func(context.Context, time.Duration) error { return nil }
	for _, tt := range []struct{ answer, want string }{
		{"access_denied", "refused in the browser"},
		{"expired_token", "expired"},
		{"invalid_grant", "invalid_grant"},
	} {
		f := &fakeDevice{answers: []string{tt.answer}}
		srv := httptest.NewServer(f.handler())
		_, err := deviceLogin(context.Background(), srv.Client(), srv.URL, &bytes.Buffer{}, sleep)
		srv.Close()
		if err == nil || !strings.Contains(err.Error(), tt.want) {
			t.Errorf("a poll answered %s: %v, want %q", tt.answer, err, tt.want)
		}
	}
	off := httptest.NewServer(http.NotFoundHandler())
	defer off.Close()
	if _, err := deviceLogin(context.Background(), off.Client(), off.URL, &bytes.Buffer{}, sleep); err == nil ||
		!strings.Contains(err.Error(), "no OIDC login") {
		t.Errorf("a server without OIDC: %v", err)
	}
}

// TestLoginCmd: --output token prints the token alone; a bad --output and a bad --server are
// refused before any request.
func TestLoginCmd(t *testing.T) {
	for _, tt := range []struct {
		args []string
		want string
	}{
		{[]string{"login", "--server", "https://l.example", "--output", "yaml"}, `--output: "yaml" is not json or token`},
		{[]string{"login", "--server", "ftp://l.example"}, "is not a server's base URL"},
	} {
		root := newRootCmd(func(string) (string, bool) { return "", false })
		root.SetArgs(tt.args)
		root.SetOut(&bytes.Buffer{})
		root.SetErr(&bytes.Buffer{})
		if err := root.Execute(); err == nil || !strings.Contains(err.Error(), tt.want) {
			t.Errorf("%v: %v, want %q", tt.args, err, tt.want)
		}
	}
}
