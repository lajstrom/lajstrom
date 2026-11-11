package ingest_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/lajstrom/lajstrom/internal/ingest"
	"github.com/lajstrom/lajstrom/internal/provider/providertest"
)

// TestValidateReadsBack: on a registry that misses the first reads of each blob it took, the
// layers just pushed are read again instead of refused, and the publish goes through; a layer
// the registry never serves is refused once the visibility timeout has passed.
func TestValidateReadsBack(t *testing.T) {
	ctx := t.Context()
	e := newEnv(t)
	lag := &providertest.Lag{Fake: e.prov, Blobs: 3}
	o := e.options(lag)
	o.VisibilityTimeout = 10 * time.Second
	e.pipe = ingest.New(o)
	pub, err := e.pipe.Upload(ctx, e.repo, upload("tool", "1.0.0", file{"tool", "linux binary"}), alice)
	must(t, err)
	if lag.Misses() == 0 || len(e.rows(t).Versions) != 1 || pub.Existed {
		t.Errorf("%d reads missed, %+v; want the version published after them", lag.Misses(), pub)
	}

	lag = &providertest.Lag{Fake: e.prov, Blobs: -1}
	o = e.options(lag)
	o.VisibilityTimeout = 100 * time.Millisecond
	e.pipe = ingest.New(o)
	start := time.Now()
	_, err = e.pipe.Upload(ctx, e.repo, upload("tool", "2.0.0", file{"tool", "never served"}), alice)
	if !errors.Is(err, ingest.ErrInvalid) || !strings.Contains(err.Error(), "was not uploaded") {
		t.Errorf("a layer never served: %v, want ErrInvalid naming it", err)
	}
	if d := time.Since(start); d < 100*time.Millisecond {
		t.Errorf("refused after %s, want the visibility timeout first", d)
	}
}
