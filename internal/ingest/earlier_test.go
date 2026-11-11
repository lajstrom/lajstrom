package ingest_test

import (
	"bytes"
	"errors"
	"net/http"
	"strings"
	"testing"

	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/lajstrom/lajstrom/internal/format"
	"github.com/lajstrom/lajstrom/internal/ingest"
	"github.com/lajstrom/lajstrom/internal/oci/profile"
)

// pushAt pushes the blobs and the manifest of up to the package path pkgPath of the repo, past
// Lajstrom, as an earlier Lajstrom wrote them; it returns the manifest and its bytes.
func (e *env) pushAt(t *testing.T, up format.Upload, pkgPath string) (*format.Built, []byte) {
	t.Helper()
	ctx := t.Context()
	built, err := e.plugin.Build(ctx, e.repo, up)
	must(t, err)
	raw, d, err := profile.Encode(built.Manifest)
	must(t, err)
	rr, err := e.prov.Repository(ctx, "raw/scratch/"+pkgPath)
	must(t, err)
	for _, b := range built.Blobs {
		rc, err := b.Open(ctx)
		must(t, err)
		must(t, rr.Push(ctx, b.Descriptor, rc))
		_ = rc.Close()
	}
	must(t, rr.PushReference(ctx, ocispec.Descriptor{MediaType: ocispec.MediaTypeImageManifest, Digest: d, Size: int64(len(raw))},
		bytes.NewReader(raw), built.Tag))
	return built, raw
}

// TestEarlierPackagePath: a package stored at an earlier form of its package path, of before ADR
// 0017 or of before its amendment, each form of one of each kind of name that they change, keeps
// that OCI repository (manifest-profile.md §4.3). Inspect reads the earlier form; a new version
// through Upload and through Ingest goes there; a push of it to the newest form is the 409 naming
// the earlier, before the registry sees a manifest; and a new package at an earlier form is a 400.
func TestEarlierPackagePath(t *testing.T) {
	for _, name := range []string{"site/.gitignore", "netlify/blobs", "a--b.txt", "x/index.json", "k8s/manifests/app.yaml",
		"x/referrers/y", "x/tags/list", "k8s/manifests/.gitignore"} {
		newPath, err := escapeRaw(name)
		must(t, err)
		forms := profile.EarlierPackagePaths(newPath)
		if len(forms) == 0 {
			t.Fatalf("%s: the package path %s has no earlier form", name, newPath)
		}
		for _, earlier := range forms {
			t.Run(name+" at "+earlier, func(t *testing.T) {
				e := newEnv(t)
				e.earlierNewPackage(t, name, earlier)
				e.earlierInspect(t, name, earlier)
				e.earlierUpload(t, name, earlier)
				e.earlierIngest(t, name, newPath, earlier)
				r := e.rows(t)
				if len(r.Packages) != 1 || r.Packages[0].NameOCI != earlier || len(r.Versions) != 3 || len(r.OCIRepos) != 1 {
					t.Errorf("rows: %d packages %+v, %d versions, OCI repositories %+v", len(r.Packages), r.Packages, len(r.Versions), r.OCIRepos)
				}
			})
		}
	}
}

// earlierNewPackage: a new package is not written at the earlier form, a 400.
func (e *env) earlierNewPackage(t *testing.T, name, earlier string) {
	t.Helper()
	built, raw := e.pushAt(t, upload(name, "0.1.0", file{"f", "new"}), earlier)
	_, err := e.ingest(t, built, raw, earlier)
	if !errors.Is(err, ingest.ErrInvalid) || !strings.Contains(err.Error(), "an earlier form") {
		t.Errorf("Ingest of a new package at %s: %v, want ErrInvalid", earlier, err)
	}
}

// earlierInspect: an earlier Lajstrom wrote 1.0.0 at the earlier form; reindex reads it there.
func (e *env) earlierInspect(t *testing.T, name, earlier string) {
	t.Helper()
	ctx := t.Context()
	built, raw := e.pushAt(t, upload(name, "1.0.0", file{"f", "one"}), earlier)
	rows, err := e.pipe.Inspect(ctx, e.repo, earlier, built.Tag, built.Manifest, raw, ingest.InspectOptions{})
	must(t, err)
	if rows.Package.NameOCI != earlier || rows.OCIRepo != "raw/scratch/"+earlier {
		t.Fatalf("Inspect at %s: %+v", earlier, rows.Package)
	}
	must(t, ingest.WriteRows(ctx, e.st, rows, now))
}

// earlierUpload: 2.0.0 through Upload goes to the earlier form, and so does its repeat.
func (e *env) earlierUpload(t *testing.T, name, earlier string) {
	t.Helper()
	pub, err := e.pipe.Upload(t.Context(), e.repo, upload(name, "2.0.0", file{"f", "two"}), alice)
	must(t, err)
	if pub.RepoPath != "raw/scratch/"+earlier || e.tagged(t, pub.RepoPath, "2.0.0") != pub.Digest {
		t.Errorf("Upload of 2.0.0 went to %s, want raw/scratch/%s", pub.RepoPath, earlier)
	}
	again, err := e.pipe.Upload(t.Context(), e.repo, upload(name, "2.0.0", file{"f", "two"}), alice)
	if err != nil || !again.Existed || again.RepoPath != pub.RepoPath {
		t.Errorf("Upload of 2.0.0 again = %+v, %v, want the repeat at %s", again, err, pub.RepoPath)
	}
}

// earlierIngest: 3.0.0 through Ingest at the new form is the 409 naming the earlier, which
// leaves nothing; at the earlier form it is stored.
func (e *env) earlierIngest(t *testing.T, name, newPath, earlier string) {
	t.Helper()
	three, threeRaw, threePath := e.prepare(t, upload(name, "3.0.0", file{"f", "three"}))
	_, err := e.ingest(t, three, threeRaw, threePath)
	if !errors.Is(err, ingest.ErrConflict) || format.StatusOf(err) != http.StatusConflict || !strings.Contains(err.Error(), earlier) {
		t.Errorf("Ingest of 3.0.0 at %s: %v, want the 409 naming %s", threePath, err, earlier)
	}
	if d := e.tagged(t, "raw/scratch/"+newPath, "3.0.0"); d != "" {
		t.Errorf("the refused push left %s at %s", d, newPath)
	}
	e.noClaim(t, name, "3.0.0")
	three, threeRaw = e.pushAt(t, upload(name, "3.0.0", file{"f", "three"}), earlier)
	if _, err := e.ingest(t, three, threeRaw, earlier); err != nil {
		t.Errorf("Ingest of 3.0.0 at %s: %v", earlier, err)
	}
}
