package sqlstore

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
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
//
// YugabyteDB takes the entries of the two indexes in no fixed order, so two writers of one new
// package, which share both entries, can each hold one and wait for the other's: a deadlock,
// which it breaks by aborting a transaction (40P01, #629). There the insert runs under
// Dialect.PackageLock on the package path.
func (c *conn) UpsertPackage(ctx context.Context, p *index.Package) error {
	return c.withTx(ctx, func(c *conn) error {
		existing, err := c.GetPackage(ctx, p.RepoID, p.Name)
		if errors.Is(err, index.ErrNotFound) {
			if c.d.PackageLock != "" {
				if _, err := c.exec(ctx, fmt.Sprintf("the lock of the package path %q in repo %d", p.NameOCI, p.RepoID),
					c.d.PackageLock, PackagePathLockKey(p.RepoID, p.NameOCI)); err != nil {
					return err
				}
			}
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

// PackagePathLockKey is the key of Dialect.PackageLock on the package path nameOCI of a repo:
// the first eight bytes of a sha256 of both, as referrersLockKey.
func PackagePathLockKey(repoID int64, nameOCI string) int64 {
	sum := sha256.Sum256([]byte("packages\x00" + strconv.FormatInt(repoID, 10) + "\x00" + nameOCI))
	return int64(binary.BigEndian.Uint64(sum[:8])) //nolint:gosec // a hash, any 64 bits do
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

// DeleteEmptyPackages removes the packages of a repo that have no version.
func (c *conn) DeleteEmptyPackages(ctx context.Context, repoID int64) (int, error) {
	return c.exec(ctx, fmt.Sprintf("packages without versions in repo %d", repoID),
		"DELETE FROM packages WHERE repo_id = ? AND NOT EXISTS (SELECT 1 FROM versions v WHERE v.package_id = packages.id)",
		repoID)
}

const versionCols = "id, package_id, version, variant, tag, digest, artifact_type, attrs, metadata, created_at, indexed_at, degraded"

func versionDests(v *index.Version) []any {
	return []any{&v.ID, &v.PackageID, &v.Version, &v.Variant, &v.Tag, (*string)(&v.Digest), &v.ArtifactType,
		jsonDest[map[string]string]{&v.Attrs}, rawDest{&v.Metadata}, timeDest{&v.CreatedAt}, timeDest{&v.IndexedAt}, &v.Degraded}
}

func versionMatchDests(m *index.VersionMatch) []any {
	return append(versionDests(&m.Version), packageDests(&m.Package)...)
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

// ListUnseenVersions returns the versions of a repo whose IndexedAt is before seenBefore, with
// their packages, ordered by package name and version id.
func (c *conn) ListUnseenVersions(ctx context.Context, repoID int64, seenBefore time.Time) ([]index.VersionMatch, error) {
	return getAll(ctx, c, fmt.Sprintf("versions of repo %d not seen since %s", repoID, Normalize(seenBefore).Format(time.RFC3339Nano)),
		versionMatchDests,
		"SELECT "+versionColsV+", "+packageColsP+" FROM versions v JOIN packages p ON p.id = v.package_id"+
			" WHERE p.repo_id = ? AND v.indexed_at < ? ORDER BY p.name, v.id",
		repoID, c.d.EncodeTime(seenBefore))
}

// UpsertVersion applies the tag rule (docs/spec/index-schema.md, Invariants and rules) in one
// transaction. The row with v's tag decides: with v's digest it is refreshed in place and v.ID
// filled (an idempotent re-ingest); with another digest nothing changes and the error is a
// *index.TagConflictError; without one, v is inserted. When the insert finds the tag taken
// after all, by a writer that inserted it concurrently, the same rule applies, so the loser of
// the race gets the TagConflictError too, never a driver error. (Version, Variant) taken under
// another tag is a plain index.ErrConflict. CreatedAt and IndexedAt are set to now when zero.
// The package's CreatedAt becomes the earliest CreatedAt of its versions.
func (c *conn) UpsertVersion(ctx context.Context, v *index.Version) error {
	ts := now()
	stamp(&v.CreatedAt, ts)
	stamp(&v.IndexedAt, ts)
	return c.withTx(ctx, func(c *conn) error {
		if err := c.lockPackage(ctx, fmt.Sprintf("packages id %d", v.PackageID), "id = ?", v.PackageID); err != nil {
			return err
		}
		if found, err := c.refreshVersion(ctx, v); found || err != nil {
			return c.packageCreated(ctx, v.PackageID, err)
		}
		inserted, err := c.insertID(ctx, fmt.Sprintf("versions %q of package %d", v.Tag, v.PackageID),
			"INSERT INTO versions (package_id, version, variant, tag, digest, artifact_type, attrs, metadata, created_at, indexed_at, degraded)"+
				" VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?) ON CONFLICT DO NOTHING RETURNING id",
			[]any{v.PackageID, v.Version, v.Variant, v.Tag, string(v.Digest), v.ArtifactType, attrsArg(v.Attrs),
				rawArg(v.Metadata), c.d.EncodeTime(v.CreatedAt), c.d.EncodeTime(v.IndexedAt), v.Degraded},
			&v.ID)
		if inserted || err != nil {
			return c.packageCreated(ctx, v.PackageID, err)
		}
		if found, err := c.refreshVersion(ctx, v); found || err != nil {
			return c.packageCreated(ctx, v.PackageID, err)
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
		"UPDATE versions SET artifact_type = ?, attrs = ?, metadata = ?, created_at = ?, indexed_at = ?, degraded = ? WHERE id = ?",
		v.ArtifactType, attrsArg(v.Attrs), rawArg(v.Metadata), c.d.EncodeTime(v.CreatedAt), c.d.EncodeTime(v.IndexedAt),
		v.Degraded, v.ID)
}

// lockPackage locks the package row that where selects, by its id or by the id of a version of
// it, until the transaction ends. Every version write and delete takes it first, so that the
// writers of one package's versions follow each other and packageCreated sees the versions of
// the writers before it. On Postgres (read committed) a statement sees the rows committed when
// it began: without the lock, a writer whose UPDATE of the package waited for another writer's
// commit still wrote the earliest time of the versions it saw before, and reindex, which writes
// the tags of a package at once, left a later time on the package than its earliest version's
// (#570). On SQLite, whose writers hold the database from BEGIN IMMEDIATE, it changes nothing.
// A missing row is no error: the version's own statement reports it.
func (c *conn) lockPackage(ctx context.Context, what, where string, id int64) error {
	_, err := c.exec(ctx, what, "UPDATE packages SET created_at = created_at WHERE "+where, id)
	return err
}

// packageCreated sets CreatedAt of the package to the earliest CreatedAt of its versions after
// a version write or delete whose error is err; a package without versions keeps its own. With
// err set it only returns err: the transaction rolls back. The caller holds lockPackage.
func (c *conn) packageCreated(ctx context.Context, packageID int64, err error) error {
	if err != nil {
		return err
	}
	_, err = c.exec(ctx, fmt.Sprintf("packages id %d", packageID),
		"UPDATE packages SET created_at = (SELECT MIN(created_at) FROM versions WHERE package_id = ?)"+
			" WHERE id = ? AND EXISTS (SELECT 1 FROM versions WHERE package_id = ?)",
		packageID, packageID, packageID)
	return err
}

// TouchVersions sets IndexedAt of the versions with the given ids, in statements of at most
// touchBatch ids; ids that do not exist are skipped.
func (c *conn) TouchVersions(ctx context.Context, ids []int64, at time.Time) error {
	return c.withTx(ctx, func(c *conn) error {
		for chunk := range slices.Chunk(ids, touchBatch) {
			args := make([]any, 0, len(chunk)+1)
			args = append(args, c.d.EncodeTime(at))
			for _, id := range chunk {
				args = append(args, id)
			}
			marks := strings.Repeat(", ?", len(chunk))[2:]
			if _, err := c.exec(ctx, fmt.Sprintf("versions %d to touch", len(chunk)),
				"UPDATE versions SET indexed_at = ? WHERE id IN ("+marks+")", args...); err != nil {
				return err
			}
		}
		return nil
	})
}

// touchBatch bounds the ids of one statement of TouchVersions, well below the parameter limits
// of SQLite (32 766) and Postgres (65 535).
const touchBatch = 500

// DeleteVersion removes the version; the foreign key cascades to its files. The package keeps
// the earliest CreatedAt of the versions left.
func (c *conn) DeleteVersion(ctx context.Context, id int64) error {
	return c.withTx(ctx, func(c *conn) error {
		what := fmt.Sprintf("versions id %d", id)
		if err := c.lockPackage(ctx, "the package of "+what, "id = (SELECT package_id FROM versions WHERE id = ?)", id); err != nil {
			return err
		}
		var packageID int64
		err := c.scanRow(ctx, what, "DELETE FROM versions WHERE id = ? RETURNING package_id", []any{id}, &packageID)
		return c.packageCreated(ctx, packageID, err)
	})
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

// IndexCacheGeneration returns the generation of a repo's rendered documents, 0 when the repo
// has no row in index_cache_generations.
func (c *conn) IndexCacheGeneration(ctx context.Context, repoID int64) (int64, error) {
	var gen int64
	err := c.scanRow(ctx, fmt.Sprintf("index_cache_generations of repo %d", repoID),
		"SELECT generation FROM index_cache_generations WHERE repo_id = ?", []any{repoID}, &gen)
	if errors.Is(err, index.ErrNotFound) {
		return 0, nil
	}
	return gen, err
}

// PutIndexCache inserts ca or replaces the row with the same (RepoID, Scope, Kind), keeping its
// id, when generation is the repo's; RenderedAt is set to now when zero. It first writes the
// repo's generation row as it is, which takes the row's lock: on Postgres the write waits for
// an invalidation that holds it and then returns the generation that one committed, and an
// invalidation that comes later waits for this transaction, whose rows its DELETE then sees.
func (c *conn) PutIndexCache(ctx context.Context, ca *index.Cache, generation int64) (stored bool, err error) {
	err = c.withTx(ctx, func(c *conn) error {
		var cur int64
		err := c.scanRow(ctx, fmt.Sprintf("index_cache_generations of repo %d", ca.RepoID),
			"INSERT INTO index_cache_generations (repo_id, generation) VALUES (?, 0)"+
				" ON CONFLICT (repo_id) DO UPDATE SET generation = index_cache_generations.generation RETURNING generation",
			[]any{ca.RepoID}, &cur)
		if err != nil || cur != generation {
			return err
		}
		ca.SignedAt = stampNull(ca.SignedAt)
		stored = true
		return c.scanRow(ctx, fmt.Sprintf("index_cache %q %q in repo %d", ca.Scope, ca.Kind, ca.RepoID),
			"INSERT INTO index_cache (repo_id, scope, kind, content, sha256, rendered_at, signed_at) VALUES (?, ?, ?, ?, ?, ?, ?)"+
				" ON CONFLICT (repo_id, scope, kind) DO UPDATE SET content = excluded.content, sha256 = excluded.sha256,"+
				" rendered_at = excluded.rendered_at, signed_at = excluded.signed_at RETURNING id",
			[]any{ca.RepoID, ca.Scope, ca.Kind, blobArg(ca.Content), ca.SHA256,
				c.d.EncodeTime(stamp(&ca.RenderedAt, now())), c.d.encodeNullTime(ca.SignedAt)},
			&ca.ID)
	})
	return stored && err == nil, err
}

// InvalidateIndexCache increments the repo's generation, then removes the cached documents of
// a scope of the repo, or of every scope when scope is "". In that order: the increment takes
// the lock of the generation row, so on Postgres the DELETE runs after the puts that held it
// have committed, and sees their rows.
func (c *conn) InvalidateIndexCache(ctx context.Context, repoID int64, scope string) error {
	return c.withTx(ctx, func(c *conn) error {
		if err := c.nextGeneration(ctx, repoID); err != nil {
			return err
		}
		var w where
		w.add("repo_id = ?", repoID)
		if scope != "" {
			w.add("scope = ?", scope)
		}
		_, err := c.exec(ctx, fmt.Sprintf("index_cache of repo %d", repoID), "DELETE FROM index_cache"+w.clause(), w.args...)
		return err
	})
}

// nextGeneration increments the generation of a repo's rendered documents, or of every repo's
// when repoID is 0; a repo without a row gets one at 1.
func (c *conn) nextGeneration(ctx context.Context, repoID int64) error {
	var w where
	if repoID != 0 {
		w.add("id = ?", repoID)
	} else {
		w.add("true") // SQLite parses INSERT … SELECT … ON CONFLICT only after a WHERE
	}
	_, err := c.exec(ctx, fmt.Sprintf("index_cache_generations of repo %d", repoID),
		"INSERT INTO index_cache_generations (repo_id, generation) SELECT id, 1 FROM repos"+w.clause()+
			" ON CONFLICT (repo_id) DO UPDATE SET generation = index_cache_generations.generation + 1", w.args...)
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

// ResetDerived removes the derived rows of a repo, or of every repo when repoID is 0, after it
// incremented their generations as InvalidateIndexCache does. Control-plane tables are not
// touched.
func (c *conn) ResetDerived(ctx context.Context, repoID int64) error {
	return c.withTx(ctx, func(c *conn) error {
		if err := c.nextGeneration(ctx, repoID); err != nil {
			return err
		}
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
