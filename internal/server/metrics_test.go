package server

import (
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"
)

// TestMetricsListener checks the acceptance criterion of #23: /metrics answers on the metrics
// listener and not on the main one; a request through the main listener increments the series
// of its route pattern; the labels carry no path, repo or package name; pprof is there only with
// Pprof.
func TestMetricsListener(t *testing.T) {
	for _, pprof := range []bool{false, true} {
		s, err := New(Options{Listen: "127.0.0.1:0", MetricsListen: "127.0.0.1:0", Pprof: pprof, Log: slog.New(slog.DiscardHandler)})
		if err != nil {
			t.Fatal(err)
		}
		addr, cancel, done := start(t, s)
		metricsAddr := s.MetricsAddr()
		if metricsAddr == nil {
			t.Fatal("no metrics listener")
		}
		get := func(url string) (int, string) {
			t.Helper()
			resp, err := http.Get(url) //nolint:gosec // the test server's own addresses
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = resp.Body.Close() }()
			b, err := io.ReadAll(resp.Body)
			if err != nil {
				t.Fatal(err)
			}
			return resp.StatusCode, string(b)
		}

		if status, _ := get("http://" + addr + "/-/metrics"); status != http.StatusNotFound {
			t.Errorf("/-/metrics on the main listener: %d, want 404", status)
		}
		if status, _ := get("http://" + addr + "/metrics"); status != http.StatusNotFound {
			t.Errorf("/metrics on the main listener: %d, want 404", status)
		}
		get("http://" + addr + "/-/healthz")
		get("http://" + addr + "/raw/secret-repo/some/package.tar.gz")

		status, body := get("http://" + metricsAddr.String() + "/metrics")
		if status != http.StatusOK {
			t.Fatalf("/metrics: %d", status)
		}
		for _, want := range []string{
			`lajstrom_http_request_duration_seconds_count{method="GET",route="GET /-/healthz",status="200"}`,
			`lajstrom_http_response_size_bytes_count{method="GET",route="unmatched",status="404"}`,
			"go_goroutines",
		} {
			if !strings.Contains(body, want) {
				t.Errorf("/metrics has no %s", want)
			}
		}
		for _, leaked := range []string{"secret-repo", "package.tar.gz", "/-/metrics"} {
			if strings.Contains(body, leaked) {
				t.Errorf("/metrics names %q", leaked)
			}
		}
		status, _ = get("http://" + metricsAddr.String() + "/debug/pprof/")
		if want := map[bool]int{false: http.StatusNotFound, true: http.StatusOK}[pprof]; status != want {
			t.Errorf("pprof %v: /debug/pprof/ answered %d, want %d", pprof, status, want)
		}
		cancel()
		if err := <-done; err != nil {
			t.Errorf("Run: %v", err)
		}
	}
}

// TestNoMetricsListener checks that an empty MetricsListen starts no listener.
func TestNoMetricsListener(t *testing.T) {
	s, err := New(Options{Listen: "127.0.0.1:0", Log: slog.New(slog.DiscardHandler)})
	if err != nil {
		t.Fatal(err)
	}
	_, cancel, done := start(t, s)
	if s.MetricsAddr() != nil {
		t.Errorf("a metrics listener at %s", s.MetricsAddr())
	}
	cancel()
	if err := <-done; err != nil {
		t.Errorf("Run: %v", err)
	}
}
