package main

import (
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/lajstrom/lajstrom/internal/api"
	"github.com/lajstrom/lajstrom/internal/config"
	"github.com/lajstrom/lajstrom/internal/provider"
	"github.com/lajstrom/lajstrom/internal/provider/providertest"
)

// reporting is a pinging fake registry that reports report.
type reporting struct {
	pinging
	report provider.Report
}

func (r *reporting) Report() provider.Report { return r.report }

// TestRegistryStatus: down when no ping was answered 200 for a minute, as /-/readyz counts it,
// or the breaker is open; degraded when the last ping failed or the breaker is half-open; ok
// otherwise, and for a registry that is not pinged.
func TestRegistryStatus(t *testing.T) {
	now := time.Date(2026, 11, 15, 10, 0, 0, 0, time.UTC)
	ok := pingStatus{pinged: true, checkedAt: now, answeredAt: now}
	failed := pingStatus{pinged: true, checkedAt: now, answeredAt: now.Add(-30 * time.Second), err: "connection refused"}
	stale := pingStatus{pinged: true, checkedAt: now, answeredAt: now.Add(-61 * time.Second), err: "connection refused"}
	for _, tt := range []struct {
		name string
		s    pingStatus
		b    provider.BreakerState
		want string
	}{
		{"answered", ok, provider.BreakerClosed, api.RegistryOK},
		{"answered, the breaker half-open", ok, provider.BreakerHalfOpen, api.RegistryDegraded},
		{"answered, the breaker open", ok, provider.BreakerOpen, api.RegistryDown},
		{"the last ping failed", failed, provider.BreakerClosed, api.RegistryDegraded},
		{"no answer for a minute", stale, provider.BreakerClosed, api.RegistryDown},
		{"never pinged yet", pingStatus{pinged: true}, provider.BreakerClosed, api.RegistryDown},
		{"not pinged", pingStatus{}, provider.BreakerClosed, api.RegistryOK},
	} {
		if got := registryStatus(tt.s, tt.b, now); got.State != tt.want || got.Breaker != tt.b {
			t.Errorf("%s: %+v, want %s", tt.name, got, tt.want)
		}
	}
}

// TestRegistryReport: serve reports each registry of the configuration with its provider's
// report, the vendor's support tier, and its last ping, the URL and prefix but never its
// credentials; a provider that does not report has no vendor, no capabilities and a closed
// breaker.
func TestRegistryReport(t *testing.T) {
	caps := provider.Capabilities{Referrers: true, Catalog: true, MaxNameLength: 246}
	zot := &reporting{pinging: pinging{Fake: &providertest.Fake{RegistryName: "zot"}},
		report: provider.Report{Vendor: "zot", Capabilities: &caps, Breaker: provider.BreakerHalfOpen}}
	down := &reporting{pinging: pinging{Fake: &providertest.Fake{RegistryName: "down"}, err: errors.New("dial tcp: connection refused")},
		report: provider.Report{Breaker: provider.BreakerClosed}}
	providers := provider.Map{"zot": zot, "down": down, "plain": &providertest.Fake{RegistryName: "plain"}}
	health := newRegistryHealth(providers, slog.New(slog.DiscardHandler))
	now := time.Date(2026, 11, 15, 10, 0, 0, 0, time.UTC)
	health.now = func() time.Time { return now }
	health.Ping(t.Context())
	cfgs := []config.Registry{
		{Name: "zot", Type: "generic", URL: "http://zot:5000", Prefix: "lajstrom/",
			Auth: config.RegistryAuth{Username: "lajstrom-robot", Password: "s3cr3t-pa55", PasswordEnv: "ZOT_PASSWORD"}},
		{Name: "down", Type: "generic", URL: "https://down.example"},
		{Name: "plain", Type: "generic", URL: "http://plain:5000"},
	}
	replica, regs := registryReport{cfgs: cfgs, providers: providers, health: health, replica: "lajstrom-a 7"}.Registries(t.Context())
	if replica != "lajstrom-a 7" || len(regs) != 3 {
		t.Fatalf("Registries = %q, %d registries", replica, len(regs))
	}
	for i, latency := range []bool{true, true, false} {
		if (regs[i].Status.LatencyMs != nil) != latency {
			t.Errorf("%s: latencyMs %v", regs[i].Name, regs[i].Status.LatencyMs)
		}
		regs[i].Status.LatencyMs = nil // how long a fake's ping took
	}
	b, err := json.Marshal(regs)
	if err != nil {
		t.Fatal(err)
	}
	const at = `"2026-11-15T10:00:00Z"`
	want := `[{"name":"zot","type":"generic","url":"http://zot:5000","prefix":"lajstrom/","vendor":"zot","support":"ci",` +
		`"capabilities":{"createOnPush":false,"referrers":true,"catalog":true,"deleteManifest":false,"immutableTags":false,` +
		`"blobRedirect":false,"referrersAlwaysTag":false,"maxBlobSize":0,"consistency":"","consistencyBound":"0s","maxNameDepth":0,` +
		`"maxNameLength":246,"tagsFoldCase":false,"buildRoutes":false},"repos":null,` +
		`"status":{"state":"degraded","checkedAt":` + at + `,"answeredAt":` + at + `,"latencyMs":null,"message":null,"breaker":"half-open"}},` +
		`{"name":"down","type":"generic","url":"https://down.example","prefix":"","vendor":null,"support":null,"capabilities":null,"repos":null,` +
		`"status":{"state":"down","checkedAt":` + at + `,"answeredAt":null,"latencyMs":null,"message":"dial tcp: connection refused","breaker":"closed"}},` +
		`{"name":"plain","type":"generic","url":"http://plain:5000","prefix":"","vendor":null,"support":null,"capabilities":null,"repos":null,` +
		`"status":{"state":"ok","checkedAt":null,"answeredAt":null,"latencyMs":null,"message":null,"breaker":"closed"}}]`
	if string(b) != want {
		t.Errorf("Registries =\n%s\nwant\n%s", b, want)
	}
	for _, secret := range []string{"lajstrom-robot", "s3cr3t-pa55", "ZOT_PASSWORD"} {
		if strings.Contains(string(b), secret) {
			t.Errorf("the report holds %q", secret)
		}
	}
}
