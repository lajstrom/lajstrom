package ingest_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/lajstrom/lajstrom/internal/format"
	"github.com/lajstrom/lajstrom/internal/ingest"
)

// TestBuildRoutes: on a registry that creates no repository whose name ends in a build route
// (capabilities.buildRoutes, Quay), a new package so named is refused by Upload, UploadStream, a
// session's Add and Ingest with ErrInvalid that names the rule, before anything is pushed; a
// package the index has keeps taking versions on every path, and a name that is no route is
// published (#662).
func TestBuildRoutes(t *testing.T) {
	ctx := t.Context()
	e, _ := newStreamEnv(t)
	built, raw, pkgPath := e.prepare(t, upload("build/app.tar", "1.0.0", file{"app.tar", "new"}))
	pushed := len(e.prov.Pushes())
	e.prov.BuildRoutes = true
	const want = "the OCI repository raw/scratch/build/app.tar ends in a build route of the registry"

	refused := func(what string, err error) {
		t.Helper()
		if !errors.Is(err, ingest.ErrInvalid) || !strings.Contains(err.Error(), want) || !strings.Contains(err.Error(), "capabilities.buildRoutes") {
			t.Errorf("%s = %v; want ErrInvalid containing %q and the key", what, err, want)
		}
	}
	_, err := e.pipe.Upload(ctx, e.repo, upload("build/app.tar", "1.0.0", file{"app.tar", "new"}), alice)
	refused("Upload", err)
	_, err = e.ingest(t, built, raw, pkgPath)
	refused("Ingest", err)
	s, err := e.pipe.Open(ctx, e.repo, "build/app.tar", "1.0.0", alice)
	must(t, err)
	_, err = e.pipe.Add(ctx, s.ID, "app.tar", strings.NewReader("new"), 3)
	refused("Add", err)
	_, err = e.pipe.UploadStream(ctx, e.repo, namedStream("build/app.tar", "1.0.0", "new"), alice)
	refused("UploadStream", err)
	if n := len(e.prov.Pushes()) - pushed; n != 0 || len(e.prov.PushReferences()) != 0 || e.prov.OpenUploads() != 0 {
		t.Errorf("%d blobs, %d manifests and %d sessions pushed for a name the registry does not create", n,
			len(e.prov.PushReferences()), e.prov.OpenUploads())
	}
	if r := e.rows(t); len(r.Packages) != 0 {
		t.Errorf("packages indexed: %+v", r.Packages)
	}

	// The package created where the registry took the name, as on Quay 3.18.2, keeps taking
	// versions once the rule holds: the registry exempts a repository that exists.
	e.prov.BuildRoutes = false
	if _, err := e.pipe.Upload(ctx, e.repo, upload("build/app.tar", "1.0.0", file{"app.tar", "first"}), alice); err != nil {
		t.Fatalf("Upload of 1.0.0 without the rule: %v", err)
	}
	e.prov.BuildRoutes = true
	if _, err := e.pipe.Upload(ctx, e.repo, upload("build/app.tar", "2.0.0", file{"app.tar", "second"}), alice); err != nil {
		t.Errorf("Upload of 2.0.0 of a package the index has: %v", err)
	}
	if _, err := e.pipe.UploadStream(ctx, e.repo, namedStream("build/app.tar", "3.0.0", "third"), alice); err != nil {
		t.Errorf("UploadStream of 3.0.0 of a package the index has: %v", err)
	}
	b4, raw4, path4 := e.prepare(t, upload("build/app.tar", "4.0.0", file{"app.tar", "fourth"}))
	if _, err := e.ingest(t, b4, raw4, path4); err != nil {
		t.Errorf("Ingest of 4.0.0 of a package the index has: %v", err)
	}
	s, err = e.pipe.Open(ctx, e.repo, "build/app.tar", "5.0.0", alice)
	must(t, err)
	if _, err := e.pipe.Add(ctx, s.ID, "app.tar", strings.NewReader("fifth"), 5); err != nil {
		t.Errorf("Add to 5.0.0 of a package the index has: %v", err)
	}
	if _, err := e.pipe.Upload(ctx, e.repo, upload("build/linux/app.tar", "1.0.0", file{"app.tar", "deeper"}), alice); err != nil {
		t.Errorf("Upload of raw/scratch/build/linux/app.tar, no build route: %v", err)
	}
}

// namedStream is the StreamUpload of content as the file name of the package name at version.
func namedStream(name, version, content string) format.StreamUpload {
	up := streamUpload(version, content)
	up.Name, up.File.Name, up.Params["name"] = name, name, name
	return up
}
