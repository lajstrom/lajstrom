package generic

import (
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/lajstrom/lajstrom/internal/provider"
)

// TestReport: Report sends no request; before a probe it has no vendor and no capabilities,
// after one the vendor the probe detected and the capabilities Capabilities answers, the
// configuration's overrides included; its breaker is closed, open during the cooldown and
// half-open after it until an answer closes it.
func TestReport(t *testing.T) {
	f := newFakeRegistry(t)
	f.hook = func(w http.ResponseWriter, r *http.Request) bool {
		if r.URL.Path == "/v2/" {
			w.Header().Set(headerGitLabVersion, "v4.43.0-gitlab")
		}
		return false
	}
	log, _ := testLogger()
	p := f.provider(t, log, nil)
	before := len(f.log())
	if got := p.Report(); got != (provider.Report{Breaker: provider.BreakerClosed}) {
		t.Errorf("Report before a probe = %+v", got)
	}
	if n := len(f.log()); n != before {
		t.Errorf("Report sent %d requests", n-before)
	}
	want, err := p.Capabilities(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	got := p.Report()
	if got.Vendor != string(vendorGitLab) || got.Capabilities == nil || *got.Capabilities != want || got.Breaker != provider.BreakerClosed {
		t.Errorf("Report after the probe = %+v, capabilities %+v; want gitlab, %+v, closed", got, got.Capabilities, want)
	}

	clk := &clock{t: time.Unix(1_700_000_000, 0)}
	b := p.clients.breaker
	b.now = clk.now
	b.mu.Lock()
	b.state, b.opened = breakerOpen, clk.now()
	b.mu.Unlock()
	if s := p.Report().Breaker; s != provider.BreakerOpen {
		t.Errorf("breaker during the cooldown: %s, want open", s)
	}
	clk.add(breakerCooldown)
	if s := p.Report().Breaker; s != provider.BreakerHalfOpen {
		t.Errorf("breaker after the cooldown: %s, want half-open", s)
	}
}

// TestVendorsHaveTiers: every vendor the probe can detect has a row in the table Support tiers
// of docs/registries/README.md, which GET /api/v1/registries answers its tier from (#953).
func TestVendorsHaveTiers(t *testing.T) {
	md, err := os.ReadFile("../../../docs/registries/README.md")
	if err != nil {
		t.Fatal(err)
	}
	_, section, ok := strings.Cut(string(md), "\n## Support tiers\n")
	if !ok {
		t.Fatal("docs/registries/README.md has no section Support tiers")
	}
	section, _, _ = strings.Cut(section, "\n## ")
	for _, v := range []vendor{vendorUnknown, vendorGitLab, vendorGHCR, vendorHarbor, vendorNexus, vendorGitea, vendorForgejo,
		vendorJFrog, vendorZot, vendorDocker, vendorQuay, vendorECR, vendorDistribution} {
		if !strings.Contains(section, "\n| `"+string(v)+"` |") {
			t.Errorf("the vendor %s has no row in Support tiers", v)
		}
	}
}
