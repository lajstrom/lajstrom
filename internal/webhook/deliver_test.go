package webhook_test

import (
	"crypto/hmac"
	"encoding/base64"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/lajstrom/lajstrom/internal/webhook"
)

// TestSign: the signature of the example of the Standard Webhooks specification, which openssl
// computes alike (HMAC-SHA256 over "<id>.<timestamp>.<body>", the key the base64 after whsec_).
func TestSign(t *testing.T) {
	key, err := webhook.ParseSecret("whsec_MfKQ9r8GKYqrTwjUPD8ILPZIo2LaLaSw")
	must(t, err)
	got := webhook.Sign(key, "msg_p5jXN8AQM9LWM0D4loKWxJek", 1614265330, []byte(`{"test": 2432232314}`))
	if want := "v1,g0hM9SsE+OTPJTGt/tmIKtSyZlE3uFJELVlNIOLJ1OE="; got != want {
		t.Errorf("Sign = %s, want %s", got, want)
	}
	for _, bad := range []string{"", "whsec_", "ljs_abc", "whsec_!!"} {
		if _, err := webhook.ParseSecret(bad); err == nil {
			t.Errorf("ParseSecret(%q) took it", bad)
		}
	}
	text, key := webhook.NewSecret()
	parsed, err := webhook.ParseSecret(text)
	must(t, err)
	if !strings.HasPrefix(text, "whsec_") || len(key) != 32 || !hmac.Equal(parsed, key) {
		t.Errorf("NewSecret = %d bytes, %q", len(key), text[:6])
	}
}

// receiver is a test server that records the requests it gets and answers status.
type receiver struct {
	*httptest.Server
	reqs   chan *http.Request
	bodies chan []byte
}

func newReceiver(t *testing.T, h http.HandlerFunc) *receiver {
	t.Helper()
	r := &receiver{reqs: make(chan *http.Request, 256), bodies: make(chan []byte, 256)}
	r.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		b, _ := io.ReadAll(req.Body)
		select { // a test that does not read them keeps the server going
		case r.reqs <- req:
			r.bodies <- b
		default:
		}
		h(w, req)
	}))
	t.Cleanup(r.Close)
	return r
}

func status(code int) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(code) }
}

// allowed is a Deliverer that dials the test servers on loopback, as webhooks.allowedHosts lists
// 127.0.0.1.
func allowed(t *testing.T, timeout time.Duration) *webhook.Deliverer {
	t.Helper()
	return webhook.NewDeliverer(webhook.Options{AllowedHosts: []string{"127.0.0.1"}, Timeout: timeout,
		Proxy: func(*http.Request) (*url.URL, error) { return nil, nil }, Now: func() time.Time { return t0 }})
}

var t0 = time.Date(2026, 11, 14, 10, 0, 0, 0, time.UTC)

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// TestAttemptSigned: a delivery is a POST of the body with the CloudEvents content type, the
// Standard Webhooks headers whose signature verifies with the key, and Lajstrom's user agent;
// a 2xx is delivered.
func TestAttemptSigned(t *testing.T) {
	rec := newReceiver(t, status(http.StatusNoContent))
	_, key := webhook.NewSecret()
	body := []byte(`{"specversion":"1.0","id":"e1"}`)
	res, err := allowed(t, 0).Attempt(t.Context(), rec.URL+"/hook", key, "e1", body)
	must(t, err)
	if !res.Delivered || res.Status != http.StatusNoContent || res.Error != "" {
		t.Fatalf("Attempt = %+v, want delivered", res)
	}
	req, got := <-rec.reqs, <-rec.bodies
	ts := strconv.FormatInt(t0.Unix(), 10)
	switch {
	case req.Method != http.MethodPost || req.URL.Path != "/hook" || string(got) != string(body):
		t.Errorf("request %s %s %q", req.Method, req.URL.Path, got)
	case req.Header.Get("Content-Type") != "application/cloudevents+json; charset=utf-8":
		t.Errorf("Content-Type %q", req.Header.Get("Content-Type"))
	case req.Header.Get("webhook-id") != "e1" || req.Header.Get("webhook-timestamp") != ts:
		t.Errorf("webhook-id %q, webhook-timestamp %q", req.Header.Get("webhook-id"), req.Header.Get("webhook-timestamp"))
	case req.Header.Get("webhook-signature") != webhook.Sign(key, "e1", t0.Unix(), body):
		t.Errorf("webhook-signature %q", req.Header.Get("webhook-signature"))
	case !strings.HasPrefix(req.Header.Get("User-Agent"), "lajstrom/"), req.Header.Get("Authorization") != "":
		t.Errorf("headers %v", req.Header)
	}
	mac, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(req.Header.Get("webhook-signature"), "v1,"))
	if err != nil || len(mac) != 32 {
		t.Errorf("the signature is not the base64 of an HMAC-SHA256: %v", err)
	}
}

// TestAttemptAnswers: an answer that is not 2xx, a redirect other than 307 and 308, and a
// timeout are failed attempts that are tried again, never refused; 307 and 308 are followed.
func TestAttemptAnswers(t *testing.T) {
	ok := newReceiver(t, status(http.StatusOK))
	for _, tt := range []struct {
		name    string
		h       http.HandlerFunc
		status  int
		errText string
		done    bool
	}{
		{"503", status(http.StatusServiceUnavailable), 503, "answered 503", false},
		{"404", status(http.StatusNotFound), 404, "answered 404", false},
		{"302 is not followed", func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, ok.URL, http.StatusFound) },
			302, "answered 302", false},
		{"307 is followed", func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, ok.URL, http.StatusTemporaryRedirect)
		}, 200, "", true},
		{"308 is followed", func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, ok.URL, http.StatusPermanentRedirect)
		}, 200, "", true},
		{"timeout", func(_ http.ResponseWriter, r *http.Request) {
			select {
			case <-r.Context().Done():
			case <-time.After(5 * time.Second):
			}
		}, 0, "timeout", false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			rec := newReceiver(t, tt.h)
			res, err := allowed(t, 200*time.Millisecond).Attempt(t.Context(), rec.URL, []byte("k"), "e", []byte("{}"))
			must(t, err)
			if res.Delivered != tt.done || res.Status != tt.status || res.Error != tt.errText || res.Refused {
				t.Errorf("Attempt = %+v, want delivered %v, status %d, error %q, not refused", res, tt.done, tt.status, tt.errText)
			}
		})
	}
	if got := len(ok.reqs); got != 2 {
		t.Errorf("the target of the redirects got %d requests, want 2 (307, 308)", got)
	}
}

// TestEgressRefused: without webhooks.allowedHosts, loopback, the metadata service, a name that
// resolves to loopback, plain http to a public host, and a redirect to any of them, or from
// https to http, are refused at once, and the error never names the address a name resolved to.
func TestEgressRefused(t *testing.T) {
	rec := newReceiver(t, status(http.StatusOK))
	_, port, _ := strings.Cut(strings.TrimPrefix(rec.URL, "http://"), ":")
	strict := webhook.NewDeliverer(webhook.Options{Timeout: time.Second, Proxy: func(*http.Request) (*url.URL, error) { return nil, nil }})
	for _, tt := range []struct{ target, errText string }{
		{"https://127.0.0.1:" + port + "/", "refused: the host has no public address"},
		{"https://localhost:" + port + "/", "refused: the host has no public address"},
		{"https://169.254.169.254/latest/meta-data/", "refused: the host has no public address"},
		{"https://10.0.0.1/", "refused: the host has no public address"},
		{"https://[::1]:" + port + "/", "refused: the host has no public address"},
		{"http://example.com/", "refused: http to a host that webhooks.allowedHosts does not list"},
	} {
		res, err := strict.Attempt(t.Context(), tt.target, []byte("k"), "e", []byte("{}"))
		must(t, err)
		if res.Delivered || !res.Refused || res.Error != tt.errText || strings.Contains(res.Error, "127.0.0.1") {
			t.Errorf("Attempt(%s) = %+v, want refused: %q", tt.target, res, tt.errText)
		}
	}
	if n := len(rec.reqs); n != 0 {
		t.Errorf("the loopback receiver got %d requests", n)
	}

	// From an allowed host, a redirect to a host that is not allowed meets the same rules.
	for _, to := range []string{"https://169.254.169.254/latest/meta-data/", "https://localhost:" + port + "/",
		"http://169.254.169.254/"} {
		redir := newReceiver(t, func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, to, http.StatusTemporaryRedirect) })
		res, err := allowed(t, time.Second).Attempt(t.Context(), redir.URL, []byte("k"), "e", []byte("{}"))
		must(t, err)
		if res.Delivered || !res.Refused || !strings.HasPrefix(res.Error, "refused: ") {
			t.Errorf("a redirect to %s: %+v, want refused", to, res)
		}
	}

	// From https to http, even to an allowed host.
	tlsRec := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, rec.URL, http.StatusTemporaryRedirect)
	}))
	t.Cleanup(tlsRec.Close)
	d := webhook.NewDeliverer(webhook.Options{AllowedHosts: []string{"127.0.0.1"}, Timeout: time.Second,
		TLS: tlsRec.Client().Transport.(*http.Transport).TLSClientConfig.Clone(), Proxy: func(*http.Request) (*url.URL, error) { return nil, nil }})
	res, err := d.Attempt(t.Context(), tlsRec.URL, []byte("k"), "e", []byte("{}"))
	must(t, err)
	if !res.Refused || res.Error != "refused: a redirect from https to http" {
		t.Errorf("a redirect from https to http: %+v", res)
	}
	if n := len(rec.reqs); n != 0 {
		t.Errorf("the target of the downgrade got %d requests", n)
	}
}

// TestCheckURL: the URLs a webhook may have (webhooks.md §5).
func TestCheckURL(t *testing.T) {
	strict := webhook.NewDeliverer(webhook.Options{AllowedHosts: []string{"ci.internal"}})
	for _, tt := range []struct {
		url string
		ok  bool
	}{
		{"https://hooks.example.com/lajstrom?token=x", true},
		{"http://ci.internal:8080/hook", true},
		{"https://10.1.2.3/hook", false},
		{"https://169.254.169.254/", false},
		{"http://hooks.example.com/", false},
		{"https://user:pass@hooks.example.com/", false},
		{"https://hooks.example.com/#frag", false},
		{"ftp://hooks.example.com/", false},
		{"/relative", false},
		{"https:///nohost", false},
		{"https://hooks.example.com/" + strings.Repeat("a", 2048), false},
		{"::", false},
	} {
		if err := strict.CheckURL(tt.url); (err == nil) != tt.ok {
			t.Errorf("CheckURL(%.60s) = %v, want ok %v", tt.url, err, tt.ok)
		}
	}
}

// TestBackoff: 30 s doubled at each attempt, at most an hour, plus up to 10 % of jitter.
func TestBackoff(t *testing.T) {
	for attempt, want := range map[int]time.Duration{1: 30 * time.Second, 2: time.Minute, 3: 2 * time.Minute, 7: 32 * time.Minute,
		8: time.Hour, 20: time.Hour} {
		if got := webhook.Backoff(attempt); got < want || got > want+want/10 {
			t.Errorf("Backoff(%d) = %s, want %s to %s", attempt, got, want, want+want/10)
		}
	}
}
