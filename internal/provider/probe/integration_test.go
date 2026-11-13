//go:build integration

package probe

import (
	"context"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/lajstrom/lajstrom/internal/config"
)

// deviations are the values that a vendor of the matrix answers in place of what the others
// agree on, by vendor (Report.Vendor) and cell, as a prefix of the cell's value: measured on the
// registry and documented on its page, so the probe is right to report them (#800).
var deviations = map[string]map[string]string{
	// Forgejo 17.0.1, which the vendor table takes for Gitea: the session's GET answers one byte
	// past the bytes it holds (#816; Gitea's is right). The provider reads it one byte back
	// (#814).
	"gitea": {"sessionStatus": "204 with `Range`: **one byte too many**"},
}

// TestIntegrationRun probes the registry at LAJSTROM_TEST_REGISTRY (host:port; https with
// LAJSTROM_TEST_REGISTRY_TLS=1), with the credentials of LAJSTROM_TEST_REGISTRY_USERNAME and
// _PASSWORD and the instance prefix of LAJSTROM_TEST_REGISTRY_PREFIX, with small bounds: every
// cell the probe measures has a value, the cells every registry of the matrix agrees on have
// theirs, but for the deviations of the registry's vendor, and nothing is left behind. The run's
// scratch prefix is new each time.
func TestIntegrationRun(t *testing.T) {
	addr := os.Getenv("LAJSTROM_TEST_REGISTRY")
	if addr == "" {
		t.Skip("set LAJSTROM_TEST_REGISTRY=host:port to run against a registry")
	}
	scheme := "http"
	if os.Getenv("LAJSTROM_TEST_REGISTRY_TLS") == "1" {
		scheme = "https"
	}
	cfg := config.Registry{
		Name: "integration", Type: "generic", URL: scheme + "://" + addr,
		Prefix: os.Getenv("LAJSTROM_TEST_REGISTRY_PREFIX"),
		Auth: config.RegistryAuth{
			Username: os.Getenv("LAJSTROM_TEST_REGISTRY_USERNAME"),
			Password: os.Getenv("LAJSTROM_TEST_REGISTRY_PASSWORD"),
		},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	r, err := Run(ctx, cfg, Options{MaxBlobSize: 2 << 20, Log: slog.New(slog.DiscardHandler)})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	notMeasured := map[string]bool{"listingAPI": true, "auth": true, "rateLimits": true, "gc": true}
	for _, c := range r.Cells {
		t.Logf("%-16s %s", c.Key, c.Value)
		if c.Measured == notMeasured[c.Key] {
			t.Errorf("%s: measured %v: %s", c.Key, c.Measured, c.Value)
		}
	}
	for key, want := range map[string]string{
		"createOnPush":  "yes",
		"profile":       "accepted, stored as pushed",
		"largestBlob":   "2 MiB in one `PUT` and in one `PATCH`; no limit met",
		"sessionStatus": "works, 204 with `Range`",
		"replica":       "works",
		"monotonic":     "none: ",
	} {
		v := r.Cell(key).Value
		if dev, ok := deviations[r.Vendor][key]; ok && strings.HasPrefix(v, dev) {
			t.Logf("%s: %q, a deviation of vendor %s that the probe measures and its page documents", key, v, r.Vendor)
			continue
		}
		if !strings.HasPrefix(v, want) {
			t.Errorf("%s = %q, want %q…", key, v, want)
		}
	}
	if len(r.LeftBehind) > 0 {
		t.Errorf("left behind: %v", r.LeftBehind)
	}
}
