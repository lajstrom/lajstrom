package ingest_test

import (
	"errors"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/lajstrom/lajstrom/internal/ingest"
	"github.com/lajstrom/lajstrom/internal/metrics"
)

// TestIngestMetrics checks lajstrom_ingest_total: a publish, its idempotent repeat through
// Ingest and through Upload, and a refused manifest, each under its result.
func TestIngestMetrics(t *testing.T) {
	ctx := t.Context()
	value := func(result string) float64 { return testutil.ToFloat64(metrics.Ingest.WithLabelValues("raw", result)) }
	published, existed, invalid := value("published"), value("existed"), value("invalid")

	e := newEnv(t)
	up := upload(readme, "1.0.0", file{readme, "hello"})
	built, raw, pkgPath := e.prepare(t, up)
	_, err := e.ingest(t, built, raw, pkgPath)
	must(t, err)
	_, err = e.ingest(t, built, raw, pkgPath)
	must(t, err)
	_, err = e.pipe.Upload(ctx, e.repo, up, alice)
	must(t, err)
	if _, err := e.pipe.Ingest(ctx, e.repo, pkgPath, "2.0.0", built.Manifest, raw, alice); !errors.Is(err, ingest.ErrInvalid) {
		t.Fatalf("Ingest under another tag: %v", err)
	}
	for _, c := range []struct {
		result string
		before float64
		want   float64
	}{{"published", published, 1}, {"existed", existed, 2}, {"invalid", invalid, 1}} {
		if got := value(c.result) - c.before; got != c.want {
			t.Errorf("%s: %v more, want %v", c.result, got, c.want)
		}
	}
}
