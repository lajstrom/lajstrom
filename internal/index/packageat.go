package index

import (
	"context"
	"errors"

	"github.com/lajstrom/lajstrom/internal/oci/profile"
)

// PackageAt returns the package of a repo at the package path pkgPath, as Plugin.EscapeName
// computes it, or at the form that path had before ADR 0017 (profile.LegacyPackagePath): a
// package keeps the OCI repository it was first written to, and its NameOCI says which. A writer
// writes to that NameOCI, and to pkgPath only for a package that is not found; a reader that
// computes the path from a name finds the package in either form. ErrNotFound when the index has
// neither; one lookup when the two are the same, as for every name ADR 0017 left alone.
func PackageAt(ctx context.Context, r Reader, repoID int64, pkgPath string) (*Package, error) {
	pkg, err := r.GetPackageByNameOCI(ctx, repoID, pkgPath)
	if legacy := profile.LegacyPackagePath(pkgPath); errors.Is(err, ErrNotFound) && legacy != pkgPath {
		return r.GetPackageByNameOCI(ctx, repoID, legacy)
	}
	return pkg, err
}
