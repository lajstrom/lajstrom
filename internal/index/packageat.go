package index

import (
	"context"
	"errors"

	"github.com/lajstrom/lajstrom/internal/oci/profile"
)

// PackageAt returns the package of a repo at the package path pkgPath, as Plugin.EscapeName
// computes it, or at one of the forms that path had before ADR 0017 and its amendment
// (profile.EarlierPackagePaths), newest first: a package keeps the OCI repository it was first
// written to, and its NameOCI says which. A writer writes to that NameOCI, and to pkgPath only
// for a package that is not found; a reader that computes the path from a name finds the package
// in any form. ErrNotFound when the index has none; one lookup for every name that ADR 0017 left
// alone.
func PackageAt(ctx context.Context, r Reader, repoID int64, pkgPath string) (*Package, error) {
	pkg, err := r.GetPackageByNameOCI(ctx, repoID, pkgPath)
	if !errors.Is(err, ErrNotFound) {
		return pkg, err
	}
	for _, earlier := range profile.EarlierPackagePaths(pkgPath) {
		if pkg, err = r.GetPackageByNameOCI(ctx, repoID, earlier); !errors.Is(err, ErrNotFound) {
			return pkg, err
		}
	}
	return nil, err
}

// PackageOf returns the package of a repo whose versions may be in the OCI repository of the
// package path path: the package whose NameOCI is path, or else one whose NameOCI is another
// form of it (profile.PackagePathForms), newest first, whose versions reindex may have found at
// path (a split, Version.OCIRepo, #642). Readers that start from the path of an OCI repository
// use it, the /v2 tag list and delete and reindex, and keep the versions whose ManifestPath is
// path. ErrNotFound when the index has none; one lookup when the package is at path.
func PackageOf(ctx context.Context, r Reader, repoID int64, path string) (*Package, error) {
	pkg, err := r.GetPackageByNameOCI(ctx, repoID, path)
	if !errors.Is(err, ErrNotFound) {
		return pkg, err
	}
	for _, form := range profile.PackagePathForms(path) {
		if form == path {
			continue
		}
		if pkg, err = r.GetPackageByNameOCI(ctx, repoID, form); !errors.Is(err, ErrNotFound) {
			return pkg, err
		}
	}
	return nil, err
}

// ManifestPath returns the package path of the OCI repository that holds the manifest of v, a
// version of p: v.OCIRepo when reindex found it at another form of p's OCI name (a split, #642),
// else p.NameOCI. Downloads, deletes, the /v2 tag list and reindex's recheck go there.
func ManifestPath(p Package, v Version) string {
	if v.OCIRepo != "" {
		return v.OCIRepo
	}
	return p.NameOCI
}
