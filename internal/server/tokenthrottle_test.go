package server

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"testing"
	"time"

	"github.com/lajstrom/lajstrom/internal/auth"
)

// TestTokenThrottle is the third criterion of #130: 200 requests to /v2/token from one address
// in one instant, the later ones 429 with Retry-After in the distribution's error format. From
// behind a trusted proxy the address is the forwarded one, and another client behind the same
// proxy goes on; from behind an untrusted one it is the proxy's, whatever the header says;
// without server.trustedProxies the per-address throttle is off.
func TestTokenThrottle(t *testing.T) {
	const body = `{"errors":[{"code":"TOOMANYREQUESTS","message":"too many token requests: try again after Retry-After seconds"}]}` + "\n"
	edge := []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8")}
	tests := []struct {
		name    string
		trusted []netip.Prefix
		remote  string
		xff     func(i int) string
		refused int
		address string // of the log line
		other   string // the X-Forwarded-For of a request that is not refused after the 200
	}{
		{name: "behind a trusted proxy", trusted: edge, remote: "10.1.2.3:443", xff: func(int) string { return "198.51.100.7" },
			refused: 200 - auth.TokenRequests, address: "198.51.100.7", other: "198.51.100.8"},
		{name: "behind an untrusted proxy", trusted: edge, remote: "192.0.2.50:443", xff: func(i int) string { return fmt.Sprintf("198.51.100.%d", i%250) },
			refused: 200 - auth.TokenRequests, address: "192.0.2.50"},
		{name: "without trusted proxies", remote: "192.0.2.50:443", xff: func(int) string { return "198.51.100.7" }},
	}
	now := time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			facade := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
			s, logs := newTestServer(t, Options{Facade: facade, TrustedProxies: tt.trusted,
				Authenticator: auth.NewAuthenticator(auth.Options{Index: usersReader{}, Now: func() time.Time { return now }})})
			get := func(xff string) *httptest.ResponseRecorder {
				rr := httptest.NewRecorder()
				r := httptest.NewRequest(http.MethodGet, "/v2/token?scope=repository:raw/scratch:pull", nil)
				r.RemoteAddr = tt.remote
				r.Header.Set("X-Forwarded-For", xff)
				s.Handler().ServeHTTP(rr, r)
				return rr
			}
			refused := 0
			for i := range 200 {
				rr := get(tt.xff(i))
				switch {
				case rr.Code == http.StatusNoContent && refused == 0:
				case rr.Code == http.StatusTooManyRequests:
					refused++
					wantThrottled(t, rr, body, 1)
				default:
					t.Fatalf("request %d: %d after %d refused", i, rr.Code, refused)
				}
			}
			if refused != tt.refused {
				t.Errorf("%d of 200 refused, want %d", refused, tt.refused)
			}
			if tt.other != "" {
				if rr := get(tt.other); rr.Code != http.StatusNoContent {
					t.Errorf("another client behind the proxy: %d", rr.Code)
				}
			}
			if tt.refused > 0 {
				if l := findLog(t, logs, "throttled"); l["throttle"] != "token" || l["address"] != tt.address || l["account"] != nil {
					t.Errorf("log line = %v", l)
				}
			}
		})
	}
}
