package terraform

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"

	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/lajstrom/lajstrom/internal/format"
	"github.com/lajstrom/lajstrom/internal/index"
	"github.com/lajstrom/lajstrom/internal/oci/profile"
)

// Validate checks a terraform manifest (an oras push through the facade, or the output of Build)
// beyond the profile (docs/spec/manifest-profile.md, terraform):
//
//   - lajstrom.name is <name>/<system> and lajstrom.version a SemVer version without build
//     metadata, in the grammars of ParseModule and CheckVersion; no variant and no other
//     lajstrom.* annotation;
//   - exactly one file layer, of sha256, titled <name>-<system>-<version>.tar.gz, and no
//     metadata layer;
//   - the archive itself, read whole through fetch, passes ReadArchive; but for the file of a
//     publish that streams in (format.ErrStreamed), which the inspector read on its way.
//
// Errors wrap format.ErrInvalid, except a failure to fetch or read the archive.
func (Plugin) Validate(ctx context.Context, _ index.Repo, m ocispec.Manifest, fetch format.BlobFetcher) (*format.Entry, error) {
	parsed, err := profile.Validate(m)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", format.ErrInvalid, err)
	}
	switch {
	case len(parsed.Files) != 1:
		return nil, fmt.Errorf("%w: a terraform manifest has one file layer, the archive, got %d", format.ErrInvalid, len(parsed.Files))
	case parsed.Metadata != nil:
		return nil, fmt.Errorf("%w: a terraform manifest has no metadata layer", format.ErrInvalid)
	}
	a, f := parsed.Annotations, parsed.Files[0]
	if err := checkAnnotations(a, f); err != nil {
		return nil, err
	}
	if fetch == nil {
		return nil, fmt.Errorf("terraform: no blob fetcher for the archive %s", f.Digest)
	}
	rc, err := fetch(ctx, f.Digest)
	switch {
	case errors.Is(err, format.ErrStreamed):
	case err != nil:
		return nil, fmt.Errorf("terraform: fetch the archive %s: %w", f.Digest, err)
	default:
		err := ReadArchive(rc)
		_ = rc.Close()
		if err != nil {
			return nil, fmt.Errorf("%s: %w", f.Title, err)
		}
	}
	return &format.Entry{Name: a.Name, Version: a.Version,
		Files: []format.FileEntry{{Name: f.Title, MediaType: f.MediaType, Digest: f.Digest, Size: f.Size, Attrs: f.Annotations}}}, nil
}

// checkAnnotations checks what the manifest says without its blobs: the name and the version,
// no variant and no lajstrom.* annotation beyond the profile's, and the digest and the title of
// the archive layer f.
func checkAnnotations(a profile.Annotations, f profile.Layer) error {
	mod, err := ParsePackageName(a.Name)
	if err != nil {
		return fmt.Errorf("%s: %w", profile.AnnotationName, err)
	}
	if err := CheckVersion(a.Version); err != nil {
		return fmt.Errorf("%s: %w", profile.AnnotationVersion, err)
	}
	if a.Variant != "" {
		return fmt.Errorf("%w: %s is %q; a terraform module has no variant", format.ErrInvalid, profile.AnnotationVariant, a.Variant)
	}
	if len(a.Attrs) > 0 {
		return fmt.Errorf("%w: %s is not an annotation of a terraform version", format.ErrInvalid, slices.Sorted(maps.Keys(a.Attrs))[0])
	}
	if f.Digest.Algorithm() != digest.SHA256 {
		return fmt.Errorf("%w: the archive layer digest %s is not sha256", format.ErrInvalid, f.Digest)
	}
	if want := mod.Filename(a.Version); f.Title != want {
		return fmt.Errorf("%w: the archive layer is titled %q, want %q, the file name of %s and %s", format.ErrInvalid,
			f.Title, want, profile.AnnotationName, profile.AnnotationVersion)
	}
	return nil
}
