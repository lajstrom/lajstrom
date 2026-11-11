package npm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/lajstrom/lajstrom/internal/format"
	"github.com/lajstrom/lajstrom/internal/index"
	"github.com/lajstrom/lajstrom/internal/oci/profile"
)

// Validate checks an npm manifest (docs/spec/manifest-profile.md, npm), as it comes through /v2
// or reindex: one tarball layer and the metadata layer; lajstrom.name and .version that npm takes,
// no variant and no lajstrom.npm.* annotation of the manifest; the layer titled
// <name without the scope>-<version>.tgz with lajstrom.npm.integrity and .shasum. The metadata
// layer must say the name, the version, the file name, the size and the checksums of the layer,
// and its manifest the name and the version. The tarball is read whole: its digests, its
// package.json (the name, the version, what an install runs: checkInstall), and its
// hasShrinkwrap and hasInstallScript must be those of the metadata layer; but for the tarball of
// a publish that streams in (format.ErrStreamed), which Build checked and described from what
// the inspector read of the same bytes. Errors wrap format.ErrInvalid, except a failure to
// fetch.
func (Plugin) Validate(ctx context.Context, _ index.Repo, m ocispec.Manifest, fetch format.BlobFetcher) (*format.Entry, error) {
	parsed, err := profile.Validate(m)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", format.ErrInvalid, err)
	}
	if len(parsed.Files) != 1 {
		return nil, fmt.Errorf("%w: an npm manifest has one tarball layer, this one %d", format.ErrInvalid, len(parsed.Files))
	}
	a, f := parsed.Annotations, parsed.Files[0]
	if err := checkAnnotations(a, f); err != nil {
		return nil, err
	}
	if fetch == nil {
		return nil, fmt.Errorf("npm: no blob fetcher for the metadata layer %s", parsed.Metadata.Digest)
	}
	md, raw, err := readMetadata(ctx, fetch, *parsed.Metadata)
	if err != nil {
		return nil, err
	}
	doc, err := ParsePackageJSON(md.Manifest)
	if err != nil {
		return nil, fmt.Errorf("the manifest of the metadata layer: %w", err)
	}
	type check struct {
		what      string
		got, want any
	}
	checks := []check{
		{"the name of the metadata layer", md.Name, a.Name},
		{"the version of the metadata layer", md.Version, a.Version},
		{"the name of its manifest", doc.Name, a.Name},
		{"the version of its manifest", doc.Version, a.Version},
		{"the filename of the metadata layer", md.Filename, f.Title},
		{"the size of the metadata layer", md.Size, f.Size},
		{"the sha256 of the metadata layer", md.SHA256, f.Digest.Encoded()},
		{profile.AnnotationNpmIntegrity, f.Annotations[profile.AnnotationNpmIntegrity], md.Integrity},
		{profile.AnnotationNpmShasum, f.Annotations[profile.AnnotationNpmShasum], md.Shasum},
	}
	rc, err := fetch(ctx, f.Digest)
	switch {
	case errors.Is(err, format.ErrStreamed):
	case err != nil:
		return nil, fmt.Errorf("npm: fetch %s: %w", f.Title, err)
	default:
		fc, err := readFacts(rc)
		_ = rc.Close()
		if err != nil {
			return nil, fmt.Errorf("%s: %w", f.Title, err)
		}
		if err := fc.check(doc); err != nil {
			return nil, err
		}
		checks = append(checks, []check{
			{"the size of the tarball", fc.size, f.Size},
			{"the sha256 of the tarball", fc.digest, f.Digest},
			{"the integrity of the metadata layer", md.Integrity, fc.integrity},
			{"the shasum of the metadata layer", md.Shasum, fc.shasum},
			{"hasShrinkwrap of the metadata layer", md.HasShrinkwrap, fc.tarball.HasShrinkwrap},
			{"hasInstallScript of the metadata layer", md.HasInstallScript, fc.tarball.HasInstallScript()},
		}...)
	}
	for _, c := range checks {
		if c.got != c.want {
			return nil, fmt.Errorf("%w: %s: %v, want %v", format.ErrInvalid, c.what, c.got, c.want)
		}
	}
	return &format.Entry{Name: a.Name, Version: a.Version, Attrs: a.Attrs, Metadata: raw,
		Files: []format.FileEntry{{Name: f.Title, MediaType: f.MediaType, Digest: f.Digest, Size: f.Size, Attrs: f.Annotations}}}, nil
}

// checkAnnotations checks what the manifest says without its blobs: the name and the version, no
// variant, no lajstrom.npm.* annotation of the manifest (lajstrom.origin.* of a proxied version
// is allowed, manifest-profile.md §3.1), the title of the tarball layer and its annotations.
func checkAnnotations(a profile.Annotations, f profile.Layer) error {
	if err := CheckName(a.Name); err != nil {
		return fmt.Errorf("%s: %w", profile.AnnotationName, err)
	}
	if err := CheckVersion(a.Version); err != nil {
		return fmt.Errorf("%s: %w", profile.AnnotationVersion, err)
	}
	if a.Variant != "" {
		return fmt.Errorf("%w: %s is %q; npm has no variant", format.ErrInvalid, profile.AnnotationVariant, a.Variant)
	}
	for k := range a.Attrs {
		if !strings.HasPrefix(k, "lajstrom.origin.") {
			return fmt.Errorf("%w: %s is not an annotation of an npm version", format.ErrInvalid, k)
		}
	}
	if want := Filename(a.Name, a.Version); f.Title != want {
		return fmt.Errorf("%w: the tarball layer is titled %q, want %q", format.ErrInvalid, f.Title, want)
	}
	if f.Annotations[profile.AnnotationNpmIntegrity] == "" || f.Annotations[profile.AnnotationNpmShasum] == "" {
		return fmt.Errorf("%w: the tarball layer needs %s and %s", format.ErrInvalid, profile.AnnotationNpmIntegrity, profile.AnnotationNpmShasum)
	}
	return nil
}

// readMetadata reads the metadata layer l through fetch, at most maxMetadata bytes.
func readMetadata(ctx context.Context, fetch format.BlobFetcher, l profile.Layer) (*metadata, json.RawMessage, error) {
	if l.Size > maxMetadata {
		return nil, nil, fmt.Errorf("%w: the metadata layer has %d bytes, at most %d", format.ErrInvalid, l.Size, maxMetadata)
	}
	rc, err := fetch(ctx, l.Digest)
	if err != nil {
		return nil, nil, fmt.Errorf("npm: fetch the metadata layer %s: %w", l.Digest, err)
	}
	defer func() { _ = rc.Close() }()
	b, err := io.ReadAll(io.LimitReader(rc, maxMetadata+1))
	if err != nil {
		return nil, nil, fmt.Errorf("npm: read the metadata layer %s: %w", l.Digest, err)
	}
	var md metadata
	if err := json.Unmarshal(b, &md); err != nil {
		return nil, nil, fmt.Errorf("%w: the metadata layer is not the npm metadata object: %w", format.ErrInvalid, err)
	}
	return &md, b, nil
}
