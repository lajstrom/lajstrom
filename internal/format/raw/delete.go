package raw

import (
	"context"
	"errors"
	"fmt"

	"github.com/lajstrom/lajstrom/internal/auth"
	"github.com/lajstrom/lajstrom/internal/format"
	"github.com/lajstrom/lajstrom/internal/index"
)

// actor is who deletes: the principal and the client address, for the audit rows.
type actor struct {
	principal auth.Principal
	ip        string
}

// deletePath removes every version of the file at path p, each through the write path's
// delete (format.Publisher.Delete, #61): its referrers first, then its manifest in the
// registry, then its rows in the index, the package's with the last version, the repo's
// index_cache, and one audit row. A path without a version is index.ErrNotFound; a backend
// that cannot delete manifests is provider.ErrUnsupported, 501, and nothing changes. A version
// that another delete took meanwhile is skipped. A failure after the first version is a
// *format.PartialError naming the versions deleted before it. Blobs are left to the registry's
// garbage collection.
func deletePath(ctx context.Context, deps format.Deps, repo index.Repo, p string, who actor) error {
	pkg, err := deps.Index.GetPackage(ctx, repo.ID, p)
	if errors.Is(err, index.ErrNotFound) {
		return fmt.Errorf("%w: raw/%s has no file %q", index.ErrNotFound, repo.Name, p)
	}
	if err != nil {
		return fmt.Errorf("raw: package %q: %w", p, err)
	}
	versions, err := deps.Index.ListVersions(ctx, pkg.ID)
	switch {
	case err != nil:
		return fmt.Errorf("raw: versions of %q: %w", p, err)
	case len(versions) == 0:
		return fmt.Errorf("%w: raw/%s has no version of %q", index.ErrNotFound, repo.Name, p)
	case deps.Ingest == nil:
		return errors.New("raw: no write path to delete through: Deps.Ingest is nil")
	}
	var deleted []string
	for _, v := range versions {
		del := format.Deletion{Path: index.ManifestPath(*pkg, v), Reference: v.Digest.String(), ClientIP: who.ip}
		_, err := deps.Ingest.Delete(ctx, repo, del, who.principal)
		switch {
		case err == nil:
			deleted = append(deleted, v.Version+" ("+v.Digest.String()+")")
		case !errors.Is(err, index.ErrNotFound):
			return format.Partial(fmt.Errorf("raw: delete %q %s: %w", p, v.Version, err), deleted)
		}
	}
	return nil
}
