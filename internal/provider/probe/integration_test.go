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

// TestIntegrationRun probes the registry at LAJSTROM_TEST_REGISTRY (host:port; https with
// LAJSTROM_TEST_REGISTRY_TLS=1), with the credentials of LAJSTROM_TEST_REGISTRY_USERNAME and
// _PASSWORD and the instance prefix of LAJSTROM_TEST_REGISTRY_PREFIX, with small bounds: every
// cell the probe measures has a value, the cells every registry of the matrix agrees on have
// theirs, and nothing is left behind. The run's scratch prefix is new each time.
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
	} {
		if v := r.Cell(key).Value; !strings.HasPrefix(v, want) {
			t.Errorf("%s = %q, want %q…", key, v, want)
		}
	}
	if len(r.LeftBehind) > 0 {
		t.Errorf("left behind: %v", r.LeftBehind)
	}
}
