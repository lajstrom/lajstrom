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
