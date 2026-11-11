package ingest_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/lajstrom/lajstrom/internal/ingest"
	"github.com/lajstrom/lajstrom/internal/provider"
	"github.com/lajstrom/lajstrom/internal/provider/providertest"
)

// depthRegistry is the fake with a capabilities.maxNameDepth.
type depthRegistry struct {
	*providertest.Fake
	depth int
}

func (r depthRegistry) Capabilities(ctx context.Context) (provider.Capabilities, error) {
	c, err := r.Fake.Capabilities(ctx)
	c.MaxNameDepth = r.depth
	return c, err
}

// TestNameDepth: a package whose OCI repository is deeper than the registry's maxNameDepth is
// refused by Upload and by Ingest with ErrInvalid that names the limit, before anything is
// pushed to it; a package within the limit is published (#599).
func TestNameDepth(t *testing.T) {
	ctx := t.Context()
	e := newEnv(t)
	built, raw, pkgPath := e.prepare(t, upload("ci/build/app.tar", "1.0.0", file{"app.tar", "deep"}))
	pushed := len(e.prov.Pushes())
	e.pipe = ingest.New(e.options(depthRegistry{Fake: e.prov, depth: 4}))
	const want = "the OCI repository raw/scratch/ci/build/app.tar has 5 components, and the registry takes at most 4 below its prefix"

	_, err := e.pipe.Upload(ctx, e.repo, upload("ci/build/app.tar", "1.0.0", file{"app.tar", "deep"}), alice)
	if !errors.Is(err, ingest.ErrInvalid) || !strings.Contains(err.Error(), want) {
		t.Errorf("Upload = %v; want ErrInvalid containing %q", err, want)
	}
	_, err = e.ingest(t, built, raw, pkgPath)
	if !errors.Is(err, ingest.ErrInvalid) || !strings.Contains(err.Error(), want) {
		t.Errorf("Ingest = %v; want ErrInvalid containing %q", err, want)
	}
	if n := len(e.prov.Pushes()) - pushed; n != 0 || len(e.prov.PushReferences()) != 0 {
		t.Errorf("%d blobs and %d manifests pushed for a name the registry does not take", n, len(e.prov.PushReferences()))
	}
	if r := e.rows(t); len(r.Packages) != 0 {
		t.Errorf("packages indexed: %+v", r.Packages)
	}

	if _, err := e.pipe.Upload(ctx, e.repo, upload("ci/app.tar", "1.0.0", file{"app.tar", "within"}), alice); err != nil {
		t.Errorf("Upload of raw/scratch/ci/app.tar, 4 components: %v", err)
	}
}
