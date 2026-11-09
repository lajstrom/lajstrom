package sqlite

import (
	"errors"
	"fmt"
	"testing"

	dto "github.com/prometheus/client_model/go"

	"github.com/lajstrom/lajstrom/internal/index"
	"github.com/lajstrom/lajstrom/internal/metrics"
)

// TestDBMetrics checks lajstrom_db_query_duration_seconds: a statement is a sample of the store
// method that ran it; a row that is not there is "ok", a refused statement "error"; a method
// called inside Tx is still named.
func TestDBMetrics(t *testing.T) {
	ctx := t.Context()
	s := openMigrated(t, fmt.Sprintf("file:metrics-%d?mode=memory&cache=shared", memoryDBs.Add(1)))
	count := func(method, result string) uint64 {
		t.Helper()
		var m dto.Metric
		h, err := metrics.DBDuration.GetMetricWithLabelValues(method, result)
		if err != nil {
			t.Fatal(err)
		}
		if err := h.(interface{ Write(*dto.Metric) error }).Write(&m); err != nil {
			t.Fatal(err)
		}
		return m.GetHistogram().GetSampleCount()
	}
	gets, creates, refused := count("GetRepo", "ok"), count("CreateRepo", "ok"), count("CreateRepo", "error")
	if _, err := s.GetRepo(ctx, "raw", "missing"); !errors.Is(err, index.ErrNotFound) {
		t.Fatalf("GetRepo: %v", err)
	}
	repo := index.Repo{Format: "raw", Name: "metrics", Registry: "local", Kind: "hosted"}
	if err := s.CreateRepo(ctx, &repo); err != nil {
		t.Fatal(err)
	}
	again := repo
	if err := s.CreateRepo(ctx, &again); !errors.Is(err, index.ErrConflict) {
		t.Fatalf("CreateRepo of a name taken: %v", err)
	}
	if err := s.Tx(ctx, func(w index.Writer) error {
		_, err := w.GetRepo(ctx, "raw", "metrics")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if got := count("GetRepo", "ok") - gets; got != 2 {
		t.Errorf("GetRepo samples: %d, want 2 (one not found, one inside Tx)", got)
	}
	if got := count("CreateRepo", "ok") - creates; got < 1 {
		t.Errorf("CreateRepo ok samples: %d, want at least 1", got)
	}
	if got := count("CreateRepo", "error") - refused; got != 1 {
		t.Errorf("CreateRepo error samples: %d, want 1", got)
	}
}
