package ingest_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/lajstrom/lajstrom/internal/ingest"
)

// TestNameDepth: a package whose OCI repository is deeper than the registry's maxNameDepth is
// refused by Upload, UploadStream, a session's Add and Ingest with ErrInvalid that names the
// limit, before anything is pushed to it; a package within the limit is published (#599).
func TestNameDepth(t *testing.T) {
	ctx := t.Context()
	e, _ := newStreamEnv(t)
	built, raw, pkgPath := e.prepare(t, upload("ci/build/app.tar", "1.0.0", file{"app.tar", "deep"}))
	pushed := len(e.prov.Pushes())
	e.prov.MaxNameDepth = 4
	const want = "the OCI repository raw/scratch/ci/build/app.tar has 5 components, and the registry takes at most 4 below its prefix"

	_, err := e.pipe.Upload(ctx, e.repo, upload("ci/build/app.tar", "1.0.0", file{"app.tar", "deep"}), alice)
	if !errors.Is(err, ingest.ErrInvalid) || !strings.Contains(err.Error(), want) {
		t.Errorf("Upload = %v; want ErrInvalid containing %q", err, want)
	}
	_, err = e.ingest(t, built, raw, pkgPath)
	if !errors.Is(err, ingest.ErrInvalid) || !strings.Contains(err.Error(), want) {
		t.Errorf("Ingest = %v; want ErrInvalid containing %q", err, want)
	}
	s, err := e.pipe.Open(ctx, e.repo, "ci/build/app.tar", "1.0.0", alice)
	must(t, err)
	if _, err := e.pipe.Add(ctx, s.ID, "app.tar", strings.NewReader("deep"), 4); !errors.Is(err, ingest.ErrInvalid) || !strings.Contains(err.Error(), want) {
		t.Errorf("Add = %v; want ErrInvalid containing %q", err, want)
	}
	e.prov.MaxNameDepth = 3 // docs/notes.txt has 4 components
	if _, err := e.pipe.UploadStream(ctx, e.repo, streamUpload("1.0.0", "deep"), alice); !errors.Is(err, ingest.ErrInvalid) ||
		!strings.Contains(err.Error(), "raw/scratch/docs/notes.txt has 4 components, and the registry takes at most 3") {
		t.Errorf("UploadStream = %v; want ErrInvalid naming the limit", err)
	}
	if n := len(e.prov.Pushes()) - pushed; n != 0 || len(e.prov.PushReferences()) != 0 || e.prov.OpenUploads() != 0 {
		t.Errorf("%d blobs, %d manifests and %d sessions pushed for a name the registry does not take", n,
			len(e.prov.PushReferences()), e.prov.OpenUploads())
	}
	if r := e.rows(t); len(r.Packages) != 0 {
		t.Errorf("packages indexed: %+v", r.Packages)
	}

	e.prov.MaxNameDepth = 4
	if _, err := e.pipe.Upload(ctx, e.repo, upload("ci/app.tar", "1.0.0", file{"app.tar", "within"}), alice); err != nil {
		t.Errorf("Upload of raw/scratch/ci/app.tar, 4 components: %v", err)
	}
}
