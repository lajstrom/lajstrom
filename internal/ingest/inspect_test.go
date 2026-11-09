package ingest_test

import (
	"bytes"
	"errors"
	"testing"
	"time"

	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/lajstrom/lajstrom/internal/format"
	"github.com/lajstrom/lajstrom/internal/index"
	"github.com/lajstrom/lajstrom/internal/ingest"
	"github.com/lajstrom/lajstrom/internal/oci/profile"
)

// TestInspectIsIngest checks the one mapping from a manifest to its rows: what Inspect returns
// is what Ingest writes for the same manifest, and Inspect itself pushes, claims and writes
// nothing.
func TestInspectIsIngest(t *testing.T) {
	ctx := t.Context()
	e := newEnv(t)
	built, raw, pkgPath := e.prepare(t, upload(readme, "1.0.0", file{readme, "hello"}))
	pushes := len(e.prov.Pushes())

	rows, err := e.pipe.Inspect(ctx, e.repo, pkgPath, built.Tag, built.Manifest, raw, ingest.InspectOptions{})
	must(t, err)
	if n := len(e.prov.Pushes()); n != pushes || len(e.prov.PushReferences()) != 0 {
		t.Errorf("Inspect pushed: %d blob pushes after %d, %d manifests", n, pushes, len(e.prov.PushReferences()))
	}
	e.noClaim(t, readme, built.Tag)
	if got := e.rows(t); len(got.Packages) != 0 || len(got.OCIRepos) != 0 || len(got.Audit) != 0 {
		t.Fatalf("Inspect wrote rows: %+v", got)
	}

	_, err = e.ingest(t, built, raw, pkgPath)
	must(t, err)
	got := e.rows(t)
	stored := ingest.Rows{Package: got.Packages[0], Version: got.Versions[0], Files: got.Files[0], OCIRepo: got.OCIRepos[0].Path}
	rows.Package.ID, rows.Package.CreatedAt = stored.Package.ID, stored.Package.CreatedAt
	rows.Version.ID, rows.Version.PackageID = stored.Version.ID, stored.Package.ID
	for i := range rows.Files {
		rows.Files[i].ID, rows.Files[i].VersionID = stored.Files[i].ID, stored.Version.ID
	}
	same(t, "the rows of Inspect and the rows Ingest wrote", stored, *rows)
}

// TestInspectInvalid checks that Inspect refuses what Ingest refuses, and that a missing blob,
// the empty config included, is ErrInvalid rather than a push.
func TestInspectInvalid(t *testing.T) {
	ctx := t.Context()
	for _, tt := range []struct {
		name    string
		tag     string
		pkgPath string
		drop    func(m ocispec.Manifest) ocispec.Descriptor // a blob to delete from the backend
		opts    ingest.InspectOptions
	}{
		{name: "another tag", tag: "2.0.0"},
		{name: "another package path", pkgPath: "docs/other.md"},
		{name: "a file layer missing", drop: func(m ocispec.Manifest) ocispec.Descriptor { return m.Layers[0] },
			opts: ingest.InspectOptions{AllowDegraded: true}},
		{name: "the empty config missing", drop: func(ocispec.Manifest) ocispec.Descriptor { return profile.EmptyConfig }},
		{name: "the metadata layer missing, no degraded rows", drop: metadataLayer},
	} {
		t.Run(tt.name, func(t *testing.T) {
			e := newEnv(t)
			built, raw, pkgPath := e.prepare(t, upload(readme, "1.0.0", file{readme, "hello"}))
			tag := built.Tag
			if tt.tag != "" {
				tag = tt.tag
			}
			if tt.pkgPath != "" {
				pkgPath = tt.pkgPath
			}
			if tt.drop != nil {
				e.deleteBlob(t, built.RepoPath, tt.drop(built.Manifest))
			}
			pushes := len(e.prov.Pushes())
			_, err := e.pipe.Inspect(ctx, e.repo, pkgPath, tag, built.Manifest, raw, tt.opts)
			if !errors.Is(err, ingest.ErrInvalid) {
				t.Errorf("Inspect: %v, want ErrInvalid", err)
			}
			if n := len(e.prov.Pushes()); n != pushes {
				t.Errorf("Inspect pushed %d blobs", n-pushes)
			}
		})
	}
}

// TestInspectDegraded checks AllowDegraded: a metadata layer missing from the backend, or not
// JSON, gives rows from the annotations alone, flagged; a backend that does not answer is an
// error, not a degraded row.
func TestInspectDegraded(t *testing.T) {
	ctx := t.Context()
	degraded := ingest.InspectOptions{AllowDegraded: true}

	e := newEnv(t)
	built, raw, pkgPath := e.prepare(t, upload(readme, "1.0.0", file{readme, "hello"}))
	whole, err := e.pipe.Inspect(ctx, e.repo, pkgPath, built.Tag, built.Manifest, raw, degraded)
	must(t, err)
	if whole.Version.Degraded || whole.Version.Metadata == nil {
		t.Fatalf("a readable metadata layer: degraded %v, metadata %s", whole.Version.Degraded, whole.Version.Metadata)
	}
	e.deleteBlob(t, built.RepoPath, metadataLayer(built.Manifest))
	rows, err := e.pipe.Inspect(ctx, e.repo, pkgPath, built.Tag, built.Manifest, raw, degraded)
	must(t, err)
	want := *whole
	want.Version.Degraded, want.Version.Metadata = true, nil
	same(t, "the rows without the metadata layer", want, *rows)

	// A metadata layer whose bytes are not JSON.
	e = newEnv(t)
	built, _, pkgPath = e.prepare(t, upload(readme, "1.0.0", file{readme, "hello"}))
	m, raw := e.withMetadata(t, built, []byte("not json"))
	rows, err = e.pipe.Inspect(ctx, e.repo, pkgPath, built.Tag, m, raw, degraded)
	must(t, err)
	if !rows.Version.Degraded || rows.Version.Metadata != nil {
		t.Errorf("a metadata layer that is not JSON: degraded %v, metadata %s", rows.Version.Degraded, rows.Version.Metadata)
	}
	if _, err := e.pipe.Inspect(ctx, e.repo, pkgPath, built.Tag, m, raw, ingest.InspectOptions{}); !errors.Is(err, ingest.ErrInvalid) {
		t.Errorf("the same without AllowDegraded: %v, want ErrInvalid", err)
	}

	e.prov.FailFetch = errors.New("connection refused")
	if _, err := e.pipe.Inspect(ctx, e.repo, pkgPath, built.Tag, m, raw, degraded); err == nil || errors.Is(err, ingest.ErrInvalid) {
		t.Errorf("a backend that does not answer: %v, want an error that is not ErrInvalid", err)
	}
}

// TestWriteRows checks WriteRows: the rows with IndexedAt, the IDs filled, the same rows again
// in place, and the tag rule.
func TestWriteRows(t *testing.T) {
	ctx := t.Context()
	e := newEnv(t)
	built, raw, pkgPath := e.prepare(t, upload(readme, "1.0.0", file{readme, "hello"}))
	rows, err := e.pipe.Inspect(ctx, e.repo, pkgPath, built.Tag, built.Manifest, raw, ingest.InspectOptions{AllowDegraded: true})
	must(t, err)
	first := now.Add(time.Hour)
	again := *rows
	must(t, ingest.WriteRows(ctx, e.st, rows, first))
	if rows.Package.ID == 0 || rows.Version.ID == 0 || rows.Version.PackageID != rows.Package.ID || rows.Files[0].VersionID != rows.Version.ID {
		t.Fatalf("WriteRows did not fill the IDs: %+v", rows)
	}
	got := e.rows(t)
	same(t, "the version", rows.Version, got.Versions[0])
	same(t, "the files", rows.Files, got.Files[0])
	same(t, "the OCI repository", []index.OCIRepo{{ID: got.OCIRepos[0].ID, RepoID: e.repo.ID, Path: rows.OCIRepo, SeenAt: first}}, got.OCIRepos)
	same(t, "the package, created with its version", created, got.Packages[0].CreatedAt)

	second := first.Add(time.Hour)
	must(t, ingest.WriteRows(ctx, e.st, &again, second))
	if again.Version.ID != rows.Version.ID || again.Version.IndexedAt != second {
		t.Errorf("the same rows again: version %d at %v, want %d at %v", again.Version.ID, again.Version.IndexedAt, rows.Version.ID, second)
	}

	other := again
	other.Version.Digest = digest.FromString("another manifest")
	var conflict *index.TagConflictError
	if err := ingest.WriteRows(ctx, e.st, &other, second); !errors.As(err, &conflict) || conflict.Existing != rows.Version.Digest {
		t.Errorf("another digest under the tag: %v, want a TagConflictError naming %s", err, rows.Version.Digest)
	}
}

// metadataLayer returns the descriptor of the metadata layer of a manifest built by buildRaw:
// its last layer.
func metadataLayer(m ocispec.Manifest) ocispec.Descriptor { return m.Layers[len(m.Layers)-1] }

// deleteBlob removes a blob from the repository name of the fake.
func (e *env) deleteBlob(t *testing.T, name string, desc ocispec.Descriptor) {
	t.Helper()
	rr, err := e.prov.Repository(t.Context(), name)
	must(t, err)
	must(t, rr.Delete(t.Context(), desc))
}

// withMetadata returns built's manifest with md as the bytes of its metadata layer, pushed to
// the fake, and the manifest encoded.
func (e *env) withMetadata(t *testing.T, built *format.Built, md []byte) (ocispec.Manifest, []byte) {
	t.Helper()
	m := built.Manifest
	m.Layers = append([]ocispec.Descriptor(nil), m.Layers...)
	l := &m.Layers[len(m.Layers)-1]
	l.Digest, l.Size = digest.FromBytes(md), int64(len(md))
	rr, err := e.prov.Repository(t.Context(), built.RepoPath)
	must(t, err)
	must(t, rr.Push(t.Context(), *l, bytes.NewReader(md)))
	raw, _, err := profile.Encode(m)
	must(t, err)
	return m, raw
}
