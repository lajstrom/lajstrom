package generic

import (
	"errors"
	"net/http"
	"testing"

	dto "github.com/prometheus/client_model/go"

	"github.com/lajstrom/lajstrom/internal/config"
	"github.com/lajstrom/lajstrom/internal/metrics"
	"github.com/lajstrom/lajstrom/internal/provider"
)

// TestOperation checks the operation label of the provider metrics, also for repository names
// that contain the segments of the API.
func TestOperation(t *testing.T) {
	for _, tt := range []struct{ path, want string }{
		{"/v2/", "ping"},
		{"/v2/_catalog", "catalog"},
		{"/v2/raw/assets/tool/manifests/1.0", "manifest"},
		{"/v2/raw/assets/tool/manifests/sha256:abc", "manifest"},
		{"/v2/raw/assets/tool/blobs/sha256:abc", "blob"},
		{"/v2/raw/assets/tool/blobs/uploads/", "upload"},
		{"/v2/raw/assets/tool/blobs/uploads/0b5c-11", "upload"},
		{"/v2/raw/assets/tool/tags/list", "tags"},
		{"/v2/raw/assets/tool/referrers/sha256:abc", "referrers"},
		{"/v2/raw/assets/a/manifests/b/blobs/sha256:abc", "blob"},
		{"/v2/raw/assets/x/blobs/y/manifests/2.0", "manifest"},
		{"/v2/raw/assets/tags/list/tags/list", "tags"},
		{"/token", "other"},
		{"/bucket/docker/registry/v2/blobs/sha256/ab/abc/data", "other"},
	} {
		if got := operation(tt.path); got != tt.want {
			t.Errorf("operation(%q) = %q, want %q", tt.path, got, tt.want)
		}
	}
}

// TestMetricsTransport checks that every request is a sample of its registry, operation, method
// and status, and that a request without an answer is status "error".
func TestMetricsTransport(t *testing.T) {
	failing := errors.New("connection refused")
	rt := &metricsTransport{registry: "metrics-test", next: roundTripper(func(r *http.Request) (*http.Response, error) {
		if r.URL.Host == "down" {
			return nil, failing
		}
		return &http.Response{StatusCode: http.StatusNotFound, Body: http.NoBody, Request: r}, nil
	})}
	// The histogram is the process's: count from where it stands, so that -count=N runs pass.
	before := sampleCount(t, "metrics-test", "manifest", http.MethodHead, "404")
	beforeErr := sampleCount(t, "metrics-test", "tags", http.MethodGet, "error")
	for range 2 {
		req, _ := http.NewRequest(http.MethodHead, "http://up/v2/raw/a/tool/manifests/1.0", nil)
		resp, err := rt.RoundTrip(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
	}
	if got := sampleCount(t, "metrics-test", "manifest", http.MethodHead, "404") - before; got != 2 {
		t.Errorf("samples of the HEADs: %d, want 2", got)
	}
	req, _ := http.NewRequest(http.MethodGet, "http://down/v2/raw/a/tool/tags/list", nil)
	resp, err := rt.RoundTrip(req)
	if resp != nil {
		_ = resp.Body.Close()
	}
	if !errors.Is(err, failing) {
		t.Fatalf("RoundTrip: %v", err)
	}
	if got := sampleCount(t, "metrics-test", "tags", http.MethodGet, "error") - beforeErr; got != 1 {
		t.Errorf("samples of the failed GET: %d, want 1", got)
	}
}

type roundTripper func(*http.Request) (*http.Response, error)

func (f roundTripper) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// sampleCount returns the samples of the provider histogram with the labels.
func sampleCount(t *testing.T, labels ...string) uint64 {
	t.Helper()
	var m dto.Metric
	h, err := metrics.ProviderDuration.GetMetricWithLabelValues(labels...)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.(interface{ Write(*dto.Metric) error }).Write(&m); err != nil {
		t.Fatal(err)
	}
	return m.GetHistogram().GetSampleCount()
}

// TestPing checks provider.Pinger on the fake registry: nil with the right credentials,
// ErrUnauthorized without them, and an error when the registry is gone.
func TestPing(t *testing.T) {
	ctx := t.Context()
	f := newFakeRegistry(t)
	f.authMode = "basic"
	log, _ := testLogger()
	p := f.provider(t, log, func(c *config.Registry) {
		c.Auth = config.RegistryAuth{Username: f.user, Password: f.pass, PasswordEnv: "FAKE_PASSWORD"}
	})
	if err := p.Ping(ctx); err != nil {
		t.Errorf("Ping with the credentials: %v", err)
	}
	if err := f.provider(t, log, nil).Ping(ctx); !errors.Is(err, provider.ErrUnauthorized) {
		t.Errorf("Ping without credentials: %v, want ErrUnauthorized", err)
	}
	f.srv.Close()
	if err := p.Ping(ctx); err == nil {
		t.Error("Ping of a registry that is gone: no error")
	}
}
