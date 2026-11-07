package deb

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"slices"
	"strings"

	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/lajstrom/lajstrom/internal/format"
	"github.com/lajstrom/lajstrom/internal/index"
	"github.com/lajstrom/lajstrom/internal/oci/profile"
)

// Validate checks a deb manifest (an oras push through the facade, or the output of Build)
// beyond the profile, so that annotations, metadata layer and the package agree:
//
//   - exactly one file layer, titled Filename(lajstrom.name, lajstrom.version,
//     lajstrom.variant);
//   - lajstrom.deb.arch is lajstrom.variant, lajstrom.deb.suite and .component are present
//     and well formed;
//   - the metadata layer (at most 4 MiB) has the control object, whose Package, Version and
//     Architecture are the annotations, the md5sums object, the size and the sha256 of the file
//     layer, and its title as filename;
//   - the control file and md5sums of the package itself, read through fetch with
//     ParseControl, which reads the first KiB of the blob and not the rest, are the metadata's,
//     field by field.
//
// The md5 and sha1 of the metadata are the uploader's claim when present: checking them means
// reading the whole blob, and a wrong one breaks apt-get for this one package only, which then
// fails its hash check. Only their form is checked. Validate returns the entry with the
// metadata bytes as they are in the registry. Errors wrap format.ErrInvalid, except a failure
// to fetch or read a blob.
func (Plugin) Validate(ctx context.Context, _ index.Repo, m ocispec.Manifest, fetch format.BlobFetcher) (*format.Entry, error) {
	parsed, err := profile.Validate(m)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", format.ErrInvalid, err)
	}
	a := parsed.Annotations
	if len(parsed.Files) != 1 {
		return nil, fmt.Errorf("%w: a deb manifest has one file layer, got %d", format.ErrInvalid, len(parsed.Files))
	}
	f := parsed.Files[0]
	if err := checkAnnotations(a, f); err != nil {
		return nil, err
	}
	if parsed.Metadata == nil { // profile.Validate requires it for deb; Validate relies on it
		return nil, fmt.Errorf("%w: a deb manifest has a metadata layer", format.ErrInvalid)
	}
	if fetch == nil {
		return nil, fmt.Errorf("deb: no blob fetcher for the metadata layer %s", parsed.Metadata.Digest)
	}
	md, raw, err := readMetadata(ctx, fetch, *parsed.Metadata)
	if err != nil {
		return nil, err
	}
	if err := md.check(a, f); err != nil {
		return nil, err
	}
	if err := checkPackage(ctx, fetch, f, md); err != nil {
		return nil, err
	}
	return &format.Entry{Name: a.Name, Version: a.Version, Variant: a.Variant, Attrs: a.Attrs, Metadata: raw,
		Files: []format.FileEntry{{Name: f.Title, MediaType: f.MediaType, Digest: f.Digest, Size: f.Size, Attrs: f.Annotations}}}, nil
}

// checkAnnotations compares the annotations of a deb manifest with each other and with the
// title of its file layer f.
func checkAnnotations(a profile.Annotations, f profile.Layer) error {
	if a.Variant == "" {
		return fmt.Errorf("%w: a deb manifest has %s, the architecture", format.ErrInvalid, profile.AnnotationVariant)
	}
	if arch := a.Attrs[profile.AnnotationDebArch]; arch != a.Variant {
		return fmt.Errorf("%w: %s %q is not %s %q", format.ErrInvalid, profile.AnnotationDebArch, arch, profile.AnnotationVariant, a.Variant)
	}
	for _, key := range []string{profile.AnnotationDebSuite, profile.AnnotationDebComponent} {
		if err := checkDistName(key, a.Attrs[key]); err != nil {
			return err
		}
	}
	if want := Filename(a.Name, a.Version, a.Variant); f.Title != want {
		return fmt.Errorf("%w: the file layer is titled %q, not %q, the file name of %s, %s and %s", format.ErrInvalid,
			f.Title, want, profile.AnnotationName, profile.AnnotationVersion, profile.AnnotationVariant)
	}
	return nil
}

// metadataIn is the deb metadata layer as Validate reads it: a missing key is a nil field.
type metadataIn struct {
	Control  map[string]string  `json:"control"`
	MD5Sums  *map[string]string `json:"md5sums"`
	Size     *int64             `json:"size"`
	SHA256   *string            `json:"sha256"`
	MD5      *string            `json:"md5"`
	SHA1     *string            `json:"sha1"`
	Filename *string            `json:"filename"`
}

// readMetadata fetches the metadata layer l, at most maxMetadata bytes, and decodes it. It
// returns the bytes as they are in the registry too.
func readMetadata(ctx context.Context, fetch format.BlobFetcher, l profile.Layer) (*metadataIn, json.RawMessage, error) {
	if l.Size > maxMetadata {
		return nil, nil, fmt.Errorf("%w: the metadata layer has %d bytes, at most %d", format.ErrInvalid, l.Size, maxMetadata)
	}
	rc, err := fetch(ctx, l.Digest)
	if err != nil {
		return nil, nil, fmt.Errorf("deb: fetch the metadata layer %s: %w", l.Digest, err)
	}
	defer func() { _ = rc.Close() }()
	b, err := io.ReadAll(io.LimitReader(rc, maxMetadata+1))
	if err != nil {
		return nil, nil, fmt.Errorf("deb: read the metadata layer %s: %w", l.Digest, err)
	}
	var md metadataIn
	if err := json.Unmarshal(b, &md); err != nil {
		return nil, nil, fmt.Errorf("%w: the metadata layer is not the deb metadata object: %w", format.ErrInvalid, err)
	}
	return &md, b, nil
}

// check compares the metadata with the file layer f and the annotations a.
func (md *metadataIn) check(a profile.Annotations, f profile.Layer) error {
	if err := md.checkFile(f); err != nil {
		return err
	}
	for _, c := range []struct{ field, key, want string }{
		{FieldPackage, profile.AnnotationName, a.Name},
		{FieldVersion, profile.AnnotationVersion, a.Version},
		{FieldArchitecture, profile.AnnotationVariant, a.Variant},
	} {
		switch v, ok := lookup(md.Control, c.field); {
		case !ok:
			return fmt.Errorf("%w: the metadata control has no field %s", format.ErrInvalid, c.field)
		case v != c.want:
			return fmt.Errorf("%w: the metadata control field %s is %q, %s is %q", format.ErrInvalid, c.field, v, c.key, c.want)
		}
	}
	return nil
}

// checkFile checks that the metadata has its objects and describes the file layer f.
func (md *metadataIn) checkFile(f profile.Layer) error {
	switch {
	case md.Control == nil:
		return fmt.Errorf("%w: the metadata has no control object", format.ErrInvalid)
	case md.MD5Sums == nil:
		return fmt.Errorf("%w: the metadata has no md5sums object", format.ErrInvalid)
	case md.Size == nil || *md.Size != f.Size:
		return fmt.Errorf("%w: the metadata size is not the file layer's %d", format.ErrInvalid, f.Size)
	case f.Digest.Algorithm() != digest.SHA256:
		return fmt.Errorf("%w: the file layer digest %s is not sha256, which the metadata and apt name", format.ErrInvalid, f.Digest)
	case md.SHA256 == nil || *md.SHA256 != f.Digest.Encoded():
		return fmt.Errorf("%w: the metadata sha256 is not the file layer's %s", format.ErrInvalid, f.Digest.Encoded())
	case md.Filename == nil || *md.Filename != f.Title:
		return fmt.Errorf("%w: the metadata filename is not the file layer's title %q", format.ErrInvalid, f.Title)
	case md.MD5 != nil && !isHex(*md.MD5, 32):
		return fmt.Errorf("%w: the metadata md5 %q is not 32 lowercase hex digits", format.ErrInvalid, *md.MD5)
	case md.SHA1 != nil && !isHex(*md.SHA1, 40):
		return fmt.Errorf("%w: the metadata sha1 %q is not 40 lowercase hex digits", format.ErrInvalid, *md.SHA1)
	}
	return nil
}

// checkPackage reads the control member of the package f through fetch and compares its
// control file and md5sums with the metadata, field by field.
func checkPackage(ctx context.Context, fetch format.BlobFetcher, f profile.Layer, md *metadataIn) error {
	rc, err := fetch(ctx, f.Digest)
	if err != nil {
		return fmt.Errorf("deb: fetch the package %s: %w", f.Digest, err)
	}
	defer func() { _ = rc.Close() }()
	pkg, err := ParseControl(rc, MaxControl)
	if err != nil {
		return fmt.Errorf("deb: the package %s: %w", f.Title, err)
	}
	if err := sameControl(pkg.Control.Map(), md.Control); err != nil {
		return err
	}
	if !maps.Equal(pkg.MD5Sums, *md.MD5Sums) {
		return fmt.Errorf("%w: the metadata md5sums is not the md5sums of the package (%d files, the metadata lists %d)",
			format.ErrInvalid, len(pkg.MD5Sums), len(*md.MD5Sums))
	}
	return nil
}

// sameControl reports the first field, in the order of names, where the control file of the
// package and the control object of the metadata differ.
func sameControl(pkg, md map[string]string) error {
	names := slices.Sorted(maps.Keys(pkg))
	for k := range md {
		if _, ok := pkg[k]; !ok {
			names = append(names, k)
		}
	}
	slices.Sort(names)
	for _, k := range names {
		p, inPkg := pkg[k]
		m, inMD := md[k]
		switch {
		case !inMD:
			return fmt.Errorf("%w: the metadata control lacks the field %s of the package's control file", format.ErrInvalid, k)
		case !inPkg:
			return fmt.Errorf("%w: the metadata control has a field %s that the package's control file lacks", format.ErrInvalid, k)
		case p != m:
			return fmt.Errorf("%w: the metadata control field %s is %q, the package's control file says %q", format.ErrInvalid, k, m, p)
		}
	}
	return nil
}

// lookup returns the value of field name in a control map, matched without regard to case.
func lookup(control map[string]string, name string) (string, bool) {
	if v, ok := control[name]; ok {
		return v, true
	}
	for k, v := range control {
		if strings.EqualFold(k, name) {
			return v, true
		}
	}
	return "", false
}

// isHex reports whether s is n lowercase hex digits.
func isHex(s string, n int) bool {
	if len(s) != n || strings.ToLower(s) != s {
		return false
	}
	_, err := hex.DecodeString(s)
	return err == nil
}
