package ingest

import (
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/lajstrom/lajstrom/internal/index"
)

// InspectOptions configures Inspect.
type InspectOptions struct {
	// AllowDegraded gives rows from the annotations alone when the manifest's metadata layer is
	// missing from the backend or unreadable (its bytes are not JSON): Version.Degraded, no
	// metadata, no plugin Validate (docs/spec/manifest-profile.md §2). Without it, that is
	// ErrInvalid, as for Ingest.
	AllowDegraded bool
}

// Rows are the derived rows of one stored manifest (docs/spec/index-schema.md): what Ingest
// writes after the registry accepted the manifest, and what reindex writes for a manifest it
// finds. WriteRows fills their IDs.
type Rows struct {
	// Package has RepoID, Name and NameOCI; the store keeps its CreatedAt the earliest
	// CreatedAt of its versions.
	Package index.Package
	// Version is the versions row; WriteRows sets PackageID and IndexedAt.
	Version index.Version
	// Files are the file layers in manifest order.
	Files []index.File
	// OCIRepo is the OCI repository of the manifest without the instance prefix.
	OCIRepo string
}

// Inspect runs the steps of Ingest that only read (1 to 3) on a manifest found in the registry
// at <repo base>/<pkgPath>:<tag> and returns the rows that step 6 writes. Nothing is pushed or
// claimed: a layer the backend does not have is ErrInvalid, the empty config included. raw is
// the manifest as the registry serves it and defines the digest. Reindex calls it for every
// manifest whose digest the index does not have; the errors are those of Ingest's steps 1 to 3.
func (p *Pipeline) Inspect(ctx context.Context, repo index.Repo, pkgPath, tag string, m ocispec.Manifest, raw []byte, o InspectOptions) (*Rows, error) {
	plugin, prov, err := p.lookup(repo)
	if err != nil {
		return nil, err
	}
	t := &target{repo: repo, plugin: plugin, pkgPath: pkgPath, name: ociName(repo, pkgPath)}
	if t.rr, err = prov.Repository(ctx, t.name); err != nil {
		return nil, fmt.Errorf("ingest: repository %s: %w", t.name, err)
	}
	pub := &publish{target: t, tag: tag, manifest: m, raw: raw, digest: digest.FromBytes(raw),
		readOnly: true, allowDegraded: o.AllowDegraded}
	if err := p.validate(ctx, pub); err != nil {
		return nil, err
	}
	rows := pub.rows(p.now())
	return &rows, nil
}

// WriteRows writes rows on w with IndexedAt at: the package, the version, its files and the
// OCI repository, and fills the IDs of rows. It does not touch index_cache, tag claims or the
// audit log; a tag that holds another digest is the *index.TagConflictError of UpsertVersion.
func WriteRows(ctx context.Context, w index.Writer, rows *Rows, at time.Time) error {
	pkg := rows.Package
	if pkg.CreatedAt.IsZero() {
		pkg.CreatedAt = at
	}
	if err := w.UpsertPackage(ctx, &pkg); err != nil {
		return err
	}
	v := rows.Version
	v.PackageID, v.IndexedAt = pkg.ID, at
	if err := w.UpsertVersion(ctx, &v); err != nil {
		return err
	}
	files := slices.Clone(rows.Files)
	if err := w.ReplaceFiles(ctx, v.ID, files); err != nil {
		return err
	}
	if err := w.UpsertOCIRepo(ctx, &index.OCIRepo{RepoID: pkg.RepoID, Path: rows.OCIRepo, SeenAt: at}); err != nil {
		return err
	}
	rows.Package, rows.Version, rows.Files = pkg, v, files
	return nil
}

// rows are the derived rows of pub, validated, at now.
func (pub *publish) rows(now time.Time) Rows {
	a := pub.parsed.Annotations
	return Rows{
		Package: index.Package{RepoID: pub.repo.ID, Name: a.Name, NameOCI: pub.pkgPath, CreatedAt: now},
		Version: pub.version(0, now),
		Files:   pub.files(),
		OCIRepo: pub.name,
	}
}
