package sqlstore

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"
	"unicode/utf8"

	"github.com/opencontainers/go-digest"

	"github.com/lajstrom/lajstrom/internal/index"
)

// Derived tables: packages, versions, files, index_cache, oci_repos.

const packageCols = "id, repo_id, name, name_oci, created_at"

func packageDests(p *index.Package) []any {
	return []any{&p.ID, &p.RepoID, &p.Name, &p.NameOCI, timeDest{&p.CreatedAt}}
}

// GetPackage returns the package with the given name in a repo.
func (c *conn) GetPackage(ctx context.Context, repoID int64, name string) (*index.Package, error) {
	return getOne(ctx, c, fmt.Sprintf("packages %q in repo %d", name, repoID), packageDests,
		"SELECT "+packageCols+" FROM packages WHERE repo_id = ? AND name = ?", repoID, name)
}

// ListPackages returns the packages of a repo whose name starts with prefix, ordered by name.
// substr, not LIKE: LIKE ignores case on SQLite and treats % and _ as wildcards.
func (c *conn) ListPackages(ctx context.Context, repoID int64, prefix string) ([]index.Package, error) {
	return getAll(ctx, c, fmt.Sprintf("packages in repo %d", repoID), packageDests,
		"SELECT "+packageCols+" FROM packages WHERE repo_id = ? AND substr(name, 1, ?) = ? ORDER BY name",
		repoID, utf8.RuneCountInString(prefix), prefix)
}

// GetPackageByNameOCI returns the package of a repo whose escaped package path is nameOCI; the
// unique index packages_repo_name_oci allows one.
func (c *conn) GetPackageByNameOCI(ctx context.Context, repoID int64, nameOCI string) (*index.Package, error) {
	return getOne(ctx, c, fmt.Sprintf("packages with name_oci %q in repo %d", nameOCI, repoID), packageDests,
		"SELECT "+packageCols+" FROM packages WHERE repo_id = ? AND name_oci = ?", repoID, nameOCI)
}

// UpsertPackage inserts p or, when (RepoID, Name) exists, fills p from the existing row. Of two
// writers that insert the same package at once, the second finds the first's row. A NameOCI
// that another name holds is index.ErrConflict naming both (nameOCIConflict), also when that
// name was inserted at the same time.
//
// ON CONFLICT has no target, so both unique indexes are arbiters: with the target (repo_id,
// name), Postgres raises the violation of (repo_id, name_oci) when two writers insert one new
// package at once, and the second never gets to find the first's row.
func (c *conn) UpsertPackage(ctx context.Context, p *index.Package) error {
	return c.withTx(ctx, func(c *conn) error {
		existing, err := c.GetPackage(ctx, p.RepoID, p.Name)
		if errors.Is(err, index.ErrNotFound) {
			inserted, err := c.insertID(ctx, fmt.Sprintf("packages %q in repo %d", p.Name, p.RepoID),
				"INSERT INTO packages (repo_id, name, name_oci, created_at) VALUES (?, ?, ?, ?)"+
					" ON CONFLICT DO NOTHING RETURNING id",
				[]any{p.RepoID, p.Name, p.NameOCI, c.d.EncodeTime(stamp(&p.CreatedAt, now()))},
				&p.ID)
			if inserted || err != nil {
				return err
			}
			existing, err = c.GetPackage(ctx, p.RepoID, p.Name) // inserted concurrently
			if errors.Is(err, index.ErrNotFound) {
				return c.nameOCIConflict(ctx, p)
			}
			if err != nil {
				return err
			}
		} else if err != nil {
			return err
		}
		*p = *existing
		return nil
	})
}

// nameOCIConflict is the error of p, whose insert found p.NameOCI held by another name: OCI
// repository names are lowercase, so two spellings of one raw path are one repository.
func (c *conn) nameOCIConflict(ctx context.Context, p *index.Package) error {
	holder := "another package" // deleted again since the insert
	if h, err := c.GetPackageByNameOCI(ctx, p.RepoID, p.NameOCI); err == nil {
		holder = strconv.Quote(h.Name)
	} else if !errors.Is(err, index.ErrNotFound) {
		return err
	}
	return fmt.Errorf("%w: packages %q in repo %d: the package path %q belongs to %s",
		index.ErrConflict, p.Name, p.RepoID, p.NameOCI, holder)
}

// DeletePackage removes the package; the foreign keys cascade to its versions and files.
func (c *conn) DeletePackage(ctx context.Context, id int64) error {
	return c.execOne(ctx, fmt.Sprintf("packages id %d", id), "DELETE FROM packages WHERE id = ?", id)
}

const versionCols = "id, package_id, version, variant, tag, digest, artifact_type, attrs, metadata, created_at, indexed_at"

func versionDests(v *index.Version) []any {
	return []any{&v.ID, &v.PackageID, &v.Version, &v.Variant, &v.Tag, (*string)(&v.Digest), &v.ArtifactType,
		jsonDest[map[string]string]{&v.Attrs}, rawDest{&v.Metadata}, timeDest{&v.CreatedAt}, timeDest{&v.IndexedAt}}
}

// GetVersion returns the version of a package by version and variant.
func (c *conn) GetVersion(ctx context.Context, packageID int64, version, variant string) (*index.Version, error) {
	return getOne(ctx, c, fmt.Sprintf("versions %q variant %q of package %d", version, variant, packageID), versionDests,
		"SELECT "+versionCols+" FROM versions WHERE package_id = ? AND version = ? AND variant = ?",
		packageID, version, variant)
}

// GetVersionByTag returns the version of a package that has tag.
func (c *conn) GetVersionByTag(ctx context.Context, packageID int64, tag string) (*index.Version, error) {
	return getOne(ctx, c, fmt.Sprintf("versions tag %q of package %d", tag, packageID), versionDests,
		"SELECT "+versionCols+" FROM versions WHERE package_id = ? AND tag = ?", packageID, tag)
}

// The column lists of the joins, qualified with the table aliases.
var (
	versionColsV = qualify("v", versionCols)
	packageColsP = qualify("p", packageCols)
	fileColsF    = qualify("f", fileCols)
)

// GetVersionByDigest returns the version in a repo whose manifest digest is d.
func (c *conn) GetVersionByDigest(ctx context.Context, repoID int64, d digest.Digest) (*index.Version, error) {
	return getOne(ctx, c, fmt.Sprintf("versions %s in repo %d", d, repoID), versionDests,
		"SELECT "+versionColsV+" FROM versions v JOIN packages p ON p.id = v.package_id"+
			" WHERE p.repo_id = ? AND v.digest = ? ORDER BY v.id LIMIT 1",
		repoID, string(d))
}

// ListVersions returns the versions of a package ordered by CreatedAt, then ID.
func (c *conn) ListVersions(ctx context.Context, packageID int64) ([]index.Version, error) {
	return getAll(ctx, c, fmt.Sprintf("versions of package %d", packageID), versionDests,
		"SELECT "+versionCols+" FROM versions WHERE package_id = ? ORDER BY created_at, id", packageID)
}

// UpsertVersion applies the tag rule (docs/spec/index-schema.md, Invariants and rules) in one
// transaction. The row with v's tag decides: with v's digest it is refreshed in place and v.ID
// filled (an idempotent re-ingest); with another digest nothing changes and the error is a
// *index.TagConflictError; without one, v is inserted. When the insert finds the tag taken
// after all, by a writer that inserted it concurrently, the same rule applies, so the loser of
// the race gets the TagConflictError too, never a driver error. (Version, Variant) taken under
// another tag is a plain index.ErrConflict. CreatedAt and IndexedAt are set to now when zero.
func (c *conn) UpsertVersion(ctx context.Context, v *index.Version) error {
	ts := now()
	stamp(&v.CreatedAt, ts)
	stamp(&v.IndexedAt, ts)
	return c.withTx(ctx, func(c *conn) error {
		if found, err := c.refreshVersion(ctx, v); found || err != nil {
			return err
		}
		inserted, err := c.insertID(ctx, fmt.Sprintf("versions %q of package %d", v.Tag, v.PackageID),
			"INSERT INTO versions (package_id, version, variant, tag, digest, artifact_type, attrs, metadata, created_at, indexed_at)"+
				" VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?) ON CONFLICT DO NOTHING RETURNING id",
			[]any{v.PackageID, v.Version, v.Variant, v.Tag, string(v.Digest), v.ArtifactType, attrsArg(v.Attrs),
				rawArg(v.Metadata), c.d.EncodeTime(v.CreatedAt), c.d.EncodeTime(v.IndexedAt)},
			&v.ID)
		if inserted || err != nil {
			return err
		}
		if found, err := c.refreshVersion(ctx, v); found || err != nil {
			return err
		}
		return fmt.Errorf("%w: versions %q variant %q of package %d exists with another tag than %q",
			index.ErrConflict, v.Version, v.Variant, v.PackageID, v.Tag)
	})
}

// refreshVersion applies the tag rule to the row that has v's tag and reports whether there is
// one: with v's digest it refreshes the row and fills v.ID, with another digest it returns a
// *index.TagConflictError.
func (c *conn) refreshVersion(ctx context.Context, v *index.Version) (bool, error) {
	existing, err := c.GetVersionByTag(ctx, v.PackageID, v.Tag)
	switch {
	case errors.Is(err, index.ErrNotFound):
		return false, nil
	case err != nil:
		return false, err
	case existing.Digest != v.Digest:
		return true, &index.TagConflictError{Tag: v.Tag, Existing: existing.Digest}
	}
	v.ID = existing.ID
	return true, c.execOne(ctx, fmt.Sprintf("versions id %d", v.ID),
		"UPDATE versions SET artifact_type = ?, attrs = ?, metadata = ?, created_at = ?, indexed_at = ? WHERE id = ?",
		v.ArtifactType, attrsArg(v.Attrs), rawArg(v.Metadata), c.d.EncodeTime(v.CreatedAt), c.d.EncodeTime(v.IndexedAt), v.ID)
}

// DeleteVersion removes the version; the foreign key cascades to its files.
func (c *conn) DeleteVersion(ctx context.Context, id int64) error {
	return c.execOne(ctx, fmt.Sprintf("versions id %d", id), "DELETE FROM versions WHERE id = ?", id)
}

const fileCols = "id, version_id, name, media_type, digest, size, attrs"

func fileDests(f *index.File) []any {
	return []any{&f.ID, &f.VersionID, &f.Name, &f.MediaType, (*string)(&f.Digest), &f.Size,
		jsonDest[map[string]string]{&f.Attrs}}
}

func fileMatchDests(m *index.FileMatch) []any {
	dests := fileDests(&m.File)
	dests = append(dests, versionDests(&m.Version)...)
	return append(dests, packageDests(&m.Package)...)
}

// ListFiles returns the files of a version in manifest order, the order ReplaceFiles got them.
func (c *conn) ListFiles(ctx context.Context, versionID int64) ([]index.File, error) {
	return getAll(ctx, c, fmt.Sprintf("files of version %d", versionID), fileDests,
		"SELECT "+fileCols+" FROM files WHERE version_id = ? ORDER BY id", versionID)
}

// FindFiles returns the files called name in a repo with their versions and packages, in the
// order they were stored.
func (c *conn) FindFiles(ctx context.Context, repoID int64, name string) ([]index.FileMatch, error) {
	return getAll(ctx, c, fmt.Sprintf("files %q in repo %d", name, repoID), fileMatchDests,
		"SELECT "+fileColsF+", "+versionColsV+", "+packageColsP+
			" FROM files f JOIN versions v ON v.id = f.version_id JOIN packages p ON p.id = v.package_id"+
			" WHERE p.repo_id = ? AND f.name = ? ORDER BY f.id",
		repoID, name)
}

// ReplaceFiles replaces the files of a version with files, in order, and fills their ID and
// VersionID.
func (c *conn) ReplaceFiles(ctx context.Context, versionID int64, files []index.File) error {
	return c.withTx(ctx, func(c *conn) error {
		what := fmt.Sprintf("versions id %d", versionID)
		var one int
		if err := c.scanRow(ctx, what, "SELECT 1 FROM versions WHERE id = ?", []any{versionID}, &one); err != nil {
			return err
		}
		if _, err := c.exec(ctx, "files of "+what, "DELETE FROM files WHERE version_id = ?", versionID); err != nil {
			return err
		}
		for i := range files {
			f := &files[i]
			f.VersionID = versionID
			err := c.scanRow(ctx, fmt.Sprintf("files %q of version %d", f.Name, versionID),
				"INSERT INTO files (version_id, name, media_type, digest, size, attrs) VALUES (?, ?, ?, ?, ?, ?) RETURNING id",
				[]any{versionID, f.Name, f.MediaType, string(f.Digest), f.Size, attrsArg(f.Attrs)},
				&f.ID)
			if err != nil {
				return err
			}
		}
		return nil
	})
}

const cacheCols = "id, repo_id, scope, kind, content, sha256, rendered_at, signed_at"

func cacheDests(ca *index.Cache) []any {
	return []any{&ca.ID, &ca.RepoID, &ca.Scope, &ca.Kind, blobDest{&ca.Content}, &ca.SHA256,
		timeDest{&ca.RenderedAt}, nullTimeDest{&ca.SignedAt}}
}

// GetIndexCache returns the cached document of kind for a scope of a repo.
func (c *conn) GetIndexCache(ctx context.Context, repoID int64, scope, kind string) (*index.Cache, error) {
	return getOne(ctx, c, fmt.Sprintf("index_cache %q %q in repo %d", scope, kind, repoID), cacheDests,
		"SELECT "+cacheCols+" FROM index_cache WHERE repo_id = ? AND scope = ? AND kind = ?", repoID, scope, kind)
}

// PutIndexCache inserts ca or replaces the row with the same (RepoID, Scope, Kind), keeping its
// id. RenderedAt is set to now when zero.
func (c *conn) PutIndexCache(ctx context.Context, ca *index.Cache) error {
	ca.SignedAt = stampNull(ca.SignedAt)
	return c.scanRow(ctx, fmt.Sprintf("index_cache %q %q in repo %d", ca.Scope, ca.Kind, ca.RepoID),
		"INSERT INTO index_cache (repo_id, scope, kind, content, sha256, rendered_at, signed_at) VALUES (?, ?, ?, ?, ?, ?, ?)"+
			" ON CONFLICT (repo_id, scope, kind) DO UPDATE SET content = excluded.content, sha256 = excluded.sha256,"+
			" rendered_at = excluded.rendered_at, signed_at = excluded.signed_at RETURNING id",
		[]any{ca.RepoID, ca.Scope, ca.Kind, blobArg(ca.Content), ca.SHA256,
			c.d.EncodeTime(stamp(&ca.RenderedAt, now())), c.d.encodeNullTime(ca.SignedAt)},
		&ca.ID)
}

// InvalidateIndexCache removes the cached documents of a scope of a repo, or of every scope
// when scope is "".
func (c *conn) InvalidateIndexCache(ctx context.Context, repoID int64, scope string) error {
	var w where
	w.add("repo_id = ?", repoID)
	if scope != "" {
		w.add("scope = ?", scope)
	}
	_, err := c.exec(ctx, fmt.Sprintf("index_cache of repo %d", repoID), "DELETE FROM index_cache"+w.clause(), w.args...)
	return err
}

const ociRepoCols = "id, repo_id, path, seen_at"

func ociRepoDests(r *index.OCIRepo) []any {
	return []any{&r.ID, &r.RepoID, &r.Path, timeDest{&r.SeenAt}}
}

// ListOCIRepos returns the OCI repositories recorded for a repo, ordered by path.
func (c *conn) ListOCIRepos(ctx context.Context, repoID int64) ([]index.OCIRepo, error) {
	return getAll(ctx, c, fmt.Sprintf("oci_repos of repo %d", repoID), ociRepoDests,
		"SELECT "+ociRepoCols+" FROM oci_repos WHERE repo_id = ? ORDER BY path", repoID)
}

// UpsertOCIRepo inserts r or sets SeenAt of the row with the same (RepoID, Path), keeping its
// id. SeenAt is set to now when zero.
func (c *conn) UpsertOCIRepo(ctx context.Context, r *index.OCIRepo) error {
	return c.scanRow(ctx, fmt.Sprintf("oci_repos %q in repo %d", r.Path, r.RepoID),
		"INSERT INTO oci_repos (repo_id, path, seen_at) VALUES (?, ?, ?)"+
			" ON CONFLICT (repo_id, path) DO UPDATE SET seen_at = excluded.seen_at RETURNING id",
		[]any{r.RepoID, r.Path, c.d.EncodeTime(stamp(&r.SeenAt, now()))},
		&r.ID)
}

// PruneOCIRepos removes the OCI repositories of a repo not seen since seenBefore.
func (c *conn) PruneOCIRepos(ctx context.Context, repoID int64, seenBefore time.Time) (int, error) {
	return c.exec(ctx, fmt.Sprintf("oci_repos of repo %d", repoID),
		"DELETE FROM oci_repos WHERE repo_id = ? AND seen_at < ?", repoID, c.d.EncodeTime(seenBefore))
}

// derivedTables are the tables ResetDerived empties; the foreign keys of packages cascade to
// versions and files.
var derivedTables = []string{"packages", "index_cache", "oci_repos"}

// ResetDerived removes the derived rows of a repo, or of every repo when repoID is 0.
// Control-plane tables are not touched.
func (c *conn) ResetDerived(ctx context.Context, repoID int64) error {
	return c.withTx(ctx, func(c *conn) error {
		var w where
		if repoID != 0 {
			w.add("repo_id = ?", repoID)
		}
		for _, table := range derivedTables {
			if _, err := c.exec(ctx, table, "DELETE FROM "+table+w.clause(), w.args...); err != nil {
				return err
			}
		}
		return nil
	})
}
