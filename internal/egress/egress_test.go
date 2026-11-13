package egress

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"
)

func TestPublic(t *testing.T) {
	for _, tc := range []struct {
		ip   string
		want bool
	}{
		{"8.8.8.8", true}, {"2606:4700:4700::1111", true}, {"100.128.0.1", true},
		{"127.0.0.1", false}, {"::1", false}, {"10.1.2.3", false}, {"172.16.0.1", false}, {"192.168.1.1", false},
		{"169.254.169.254", false}, {"fe80::1", false}, {"fc00::1", false}, {"100.64.0.1", false},
		{"0.0.0.0", false}, {"::", false}, {"224.0.0.1", false}, {"ff02::1", false}, {"255.255.255.255", false},
		{"::ffff:127.0.0.1", false}, {"93.184.215.14", true}, {"2606:2800:21f:cb07:6820:80da:af6b:8b2c", true},
		{"fd00::1", false}, {"::ffff:10.0.0.1", false},
	} {
		if got := Public(net.ParseIP(tc.ip)); got != tc.want {
			t.Errorf("Public(%s) = %v, want %v", tc.ip, got, tc.want)
		}
	}
}

func TestCheckURL(t *testing.T) {
	g := NewGuard("registry.internal", "Auth.Example.")
	for _, tc := range []struct {
		url string
		ok  bool
	}{
		{"http://registry.internal/v2/", true}, // the operator's host, whatever its scheme
		{"http://auth.example/token", true},
		{"https://cdn.example.com/blob", true},
		{"https://8.8.8.8/blob", true},
		{"http://cdn.example.com/blob", false},
		{"https://127.0.0.1/blob", false},
		{"https://[::1]/blob", false},
		{"https://169.254.169.254/latest/meta-data", false},
	} {
		u, _ := url.Parse(tc.url)
		err := g.CheckURL(u)
		if (err == nil) != tc.ok || err != nil && !errors.Is(err, ErrRefused) {
			t.Errorf("CheckURL(%s) = %v, want ok %v", tc.url, err, tc.ok)
		}
	}
}

func TestDialContext(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer srv.Close()
	_, port, _ := net.SplitHostPort(srv.Listener.Addr().String())
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	dial := NewGuard("127.0.0.1").DialContext(&net.Dialer{})
	c, err := dial(ctx, "tcp", net.JoinHostPort("127.0.0.1", port))
	if err != nil {
		t.Fatalf("a trusted host: %v", err)
	}
	_ = c.Close()
	// localhost is another name, which resolves to loopback.
	if _, err := dial(ctx, "tcp", net.JoinHostPort("localhost", port)); !errors.Is(err, ErrRefused) {
		t.Fatalf("a name that resolves to loopback: %v, want ErrRefused", err)
	}
}

func TestProxyTrusts(t *testing.T) {
	g := NewGuard()
	proxyURL, _ := url.Parse("http://egress.internal:3128")
	choose := g.Proxy(func(*http.Request) (*url.URL, error) { return proxyURL, nil })
	if g.Trusted("egress.internal") {
		t.Fatal("trusted before it was chosen")
	}
	if _, err := choose(httptest.NewRequest(http.MethodGet, "https://cdn.example.com/", nil)); err != nil {
		t.Fatal(err)
	}
	if !g.Trusted("egress.internal") {
		t.Fatal("the chosen egress proxy is not dialed as it is")
	}
}
