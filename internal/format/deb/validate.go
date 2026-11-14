package deb

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
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
// beyond the profile, so that annotations, metadata layer and the package agree. A manifest whose
// lajstrom.deb.arch is ArchSource is a source package, which validateSource checks (#79); one of
// a binary package has:
//
//   - lajstrom.deb.suite, .component and .arch are present and well formed, and
//     lajstrom.variant is the three joined with "/", jammy/main/amd64; a variant that is the
//     architecture alone, the form before #110, is refused with the variant and the tag it
//     must be;
//   - exactly one file layer, a .deb (profile.MediaTypeDebPackage), titled
//     Filename(lajstrom.name, lajstrom.version, lajstrom.deb.arch);
//   - the metadata layer (at most 4 MiB) has the control object, whose Package, Version and
//     Architecture are lajstrom.name, .version and .deb.arch, the md5sums object, the size
//     and the sha256 of the file layer, and its title as filename;
//   - the control file and md5sums of the package itself, read through fetch with
//     ParseControl, which reads the first KiB of the blob and not the rest, are the metadata's,
//     field by field; but for the file of an upload that streams in (format.ErrStreamed), whose
//     metadata Build made from what the inspector read of the same bytes.
//
// The md5 and sha1 of the metadata are the uploader's claim when present: checking them means
// reading the whole blob, and a wrong one breaks apt-get for this one package only, which then
// fails its hash check. Only their form is checked. Validate returns the entry with the
// metadata bytes as they are in the registry. Errors wrap format.ErrInvalid, except a failure
// to fetch or read a blob.
//
// Validate has no index, so an oras push does not meet the pool file name rule of the upload
// route (poolHolders): a manifest whose file layer has a pool file name that another version
// of the repo holds with another blob is accepted. The pool route answers 500 for that name,
// logged, and reindex (#29) reports it.
func (Plugin) Validate(ctx context.Context, _ index.Repo, m ocispec.Manifest, fetch format.BlobFetcher) (*format.Entry, error) {
	parsed, err := profile.Validate(m)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", format.ErrInvalid, err)
	}
	a := parsed.Annotations
	if a.Attrs[profile.AnnotationDebArch] == ArchSource {
		return validateSource(ctx, parsed, fetch)
	}
	if len(parsed.Files) != 1 {
		return nil, fmt.Errorf("%w: a deb manifest has one file layer, got %d", format.ErrInvalid, len(parsed.Files))
	}
	f := parsed.Files[0]
	if f.MediaType != profile.MediaTypeDebPackage {
		return nil, fmt.Errorf("%w: the file layer of a binary package is %s, not %s", format.ErrInvalid, profile.MediaTypeDebPackage, f.MediaType)
	}
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
	suite, component, arch := a.Attrs[profile.AnnotationDebSuite], a.Attrs[profile.AnnotationDebComponent], a.Attrs[profile.AnnotationDebArch]
	for _, c := range []struct{ key, value string }{{profile.AnnotationDebSuite, suite}, {profile.AnnotationDebComponent, component}} {
		if err := checkDistName(c.key, c.value); err != nil {
			return err
		}
	}
	if err := checkArchitecture(profile.AnnotationDebArch, arch); err != nil {
		return err
	}
	if err := checkVariant(a, suite, component, arch); err != nil {
		return err
	}
	if want := Filename(a.Name, a.Version, arch); f.Title != want {
		return fmt.Errorf("%w: the file layer is titled %q, not %q, the file name of %s, %s and %s", format.ErrInvalid,
			f.Title, want, profile.AnnotationName, profile.AnnotationVersion, profile.AnnotationDebArch)
	}
	return nil
}

// checkVariant checks that lajstrom.variant is variantOf(suite, component, arch), which are
// well formed, and names the annotation it disagrees with. A manifest pushed in the shape of
// #39, the architecture alone as the variant, is told the variant and the tag it must have.
func checkVariant(a profile.Annotations, suite, component, arch string) error {
	want := variantOf(suite, component, arch)
	parts := strings.Split(a.Variant, "/")
	switch {
	case a.Variant == "":
		return fmt.Errorf("%w: a deb manifest has %s, <suite>/<component>/<arch>: %q", format.ErrInvalid, profile.AnnotationVariant, want)
	case a.Variant == arch:
		return fmt.Errorf("%w: %s %q is the architecture alone; the variant of a deb manifest is %s, %s and %s joined with \"/\": "+
			"%q, and its tag %s", format.ErrInvalid, profile.AnnotationVariant, a.Variant,
			profile.AnnotationDebSuite, profile.AnnotationDebComponent, profile.AnnotationDebArch, want, profile.Tag(a.Version, want))
	case len(parts) != 3:
		return fmt.Errorf("%w: %s %q is not <suite>/<component>/<arch>; %s, %s and %s make it %q", format.ErrInvalid,
			profile.AnnotationVariant, a.Variant, profile.AnnotationDebSuite, profile.AnnotationDebComponent, profile.AnnotationDebArch, want)
	}
	for i, c := range []struct{ what, key, value string }{
		{"suite", profile.AnnotationDebSuite, suite},
		{"component", profile.AnnotationDebComponent, component},
		{"architecture", profile.AnnotationDebArch, arch},
	} {
		if parts[i] != c.value {
			return fmt.Errorf("%w: %s %q names the %s %q, %s is %q", format.ErrInvalid,
				profile.AnnotationVariant, a.Variant, c.what, parts[i], c.key, c.value)
		}
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
	Files    *[]string          `json:"files"`
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
		{FieldArchitecture, profile.AnnotationDebArch, a.Attrs[profile.AnnotationDebArch]},
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
	return md.checkFiles()
}

// checkFiles checks the form of the file list of the metadata, when it has one: every path is
// one a file list holds (validListedPath). Its agreement with the data member is the uploader's
// claim, as md5 and sha1 are: checking it means reading the whole package.
func (md *metadataIn) checkFiles() error {
	if md.Files == nil {
		return nil
	}
	for i, p := range *md.Files {
		if !validListedPath(p) {
			return fmt.Errorf("%w: the metadata files lists %q at %d, which is not a relative path without empty, \".\" or \"..\" components "+
				"and control characters", format.ErrInvalid, p, i)
		}
	}
	return nil
}

// checkPackage reads the control member of the package f through fetch and compares its
// control file and md5sums with the metadata, field by field.
func checkPackage(ctx context.Context, fetch format.BlobFetcher, f profile.Layer, md *metadataIn) error {
	rc, err := fetch(ctx, f.Digest)
	switch {
	case errors.Is(err, format.ErrStreamed):
		return nil
	case err != nil:
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
