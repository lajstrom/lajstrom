package rpm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/lajstrom/lajstrom/internal/format"
	"github.com/lajstrom/lajstrom/internal/index"
	"github.com/lajstrom/lajstrom/internal/oci/profile"
)

// Validate checks an rpm manifest (an oras push through the facade, or the output of Build)
// beyond the profile, so that annotations, metadata layer and the package agree:
//
//   - exactly one file layer, of sha256, titled Filename of lajstrom.name, the version and
//     release of lajstrom.version, and lajstrom.variant, the arch;
//   - the metadata layer (at most 16 MiB) is the rpm metadata object, without a key it does not
//     define, whose name, EVR and arch are the annotations', whose filename is the title and
//     whose size and sha256 are the file layer's;
//   - the head of the package itself, read through fetch with ParseHead, which reads the header
//     and not the payload, makes that metadata, key by key; but for the file of an upload that
//     streams in (format.ErrStreamed), whose metadata Build made from what the inspector read of
//     the same bytes.
//
// Validate returns the entry with the metadata bytes as they are in the registry, or none when
// they are more than maxIndexedMetadata, which the index leaves out. Errors wrap
// format.ErrInvalid, except a failure to fetch or read a blob.
//
// Validate has no index, so an oras push does not meet the file name rule of the upload route
// (holders): a manifest whose file name another version of the repo holds with another blob,
// one that differs in its epoch, is accepted, and the Packages route answers 500 for that name,
// logged.
func (Plugin) Validate(ctx context.Context, _ index.Repo, m ocispec.Manifest, fetch format.BlobFetcher) (*format.Entry, error) {
	parsed, err := profile.Validate(m)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", format.ErrInvalid, err)
	}
	a := parsed.Annotations
	if len(parsed.Files) != 1 {
		return nil, fmt.Errorf("%w: an rpm manifest has one file layer, got %d", format.ErrInvalid, len(parsed.Files))
	}
	f := parsed.Files[0]
	if err := checkAnnotations(a, f); err != nil {
		return nil, err
	}
	if parsed.Metadata == nil { // profile.Validate requires it for rpm; Validate relies on it
		return nil, fmt.Errorf("%w: an rpm manifest has a metadata layer", format.ErrInvalid)
	}
	if fetch == nil {
		return nil, fmt.Errorf("rpm: no blob fetcher for the metadata layer %s", parsed.Metadata.Digest)
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
	return &format.Entry{Name: a.Name, Version: a.Version, Variant: a.Variant, Attrs: a.Attrs, Metadata: indexed(raw),
		Files: []format.FileEntry{{Name: f.Title, MediaType: f.MediaType, Digest: f.Digest, Size: f.Size, Attrs: f.Annotations}}}, nil
}

// checkAnnotations checks the name, the version and the variant of an rpm manifest against
// their grammars and the title of its file layer f against them.
func checkAnnotations(a profile.Annotations, f profile.Layer) error {
	epoch, version, release := SplitEVR(a.Version)
	switch {
	case !nameGrammar.MatchString(a.Name):
		return fmt.Errorf("%w: %s %q is not an rpm NAME", format.ErrInvalid, profile.AnnotationName, a.Name)
	case !verGrammar.MatchString(version) || !verGrammar.MatchString(release):
		return fmt.Errorf("%w: %s %q is not [<epoch>:]<version>-<release>", format.ErrInvalid, profile.AnnotationVersion, a.Version)
	case epoch == "0" || (len(epoch) > 1 && epoch[0] == '0'):
		return fmt.Errorf("%w: %s %q writes the epoch %q; it is written without leading zeros, and not at all when it is 0",
			format.ErrInvalid, profile.AnnotationVersion, a.Version, epoch)
	case !archGrammar.MatchString(a.Variant):
		return fmt.Errorf("%w: %s %q is not an rpm arch", format.ErrInvalid, profile.AnnotationVariant, a.Variant)
	case f.Digest.Algorithm() != digest.SHA256:
		return fmt.Errorf("%w: the file layer digest %s is not sha256, which the metadata and the repodata name", format.ErrInvalid, f.Digest)
	}
	if want := Filename(a.Name, version, release, a.Variant); f.Title != want {
		return fmt.Errorf("%w: the file layer is titled %q, not %q, the file name of %s, %s and %s", format.ErrInvalid,
			f.Title, want, profile.AnnotationName, profile.AnnotationVersion, profile.AnnotationVariant)
	}
	return nil
}

// readMetadata fetches the metadata layer l, at most maxMetadata bytes, and decodes it, refusing
// a key the metadata does not define. It returns the bytes as they are in the registry too.
func readMetadata(ctx context.Context, fetch format.BlobFetcher, l profile.Layer) (*metadata, json.RawMessage, error) {
	if l.Size > maxMetadata {
		return nil, nil, fmt.Errorf("%w: the metadata layer has %d bytes, at most %d", format.ErrInvalid, l.Size, maxMetadata)
	}
	rc, err := fetch(ctx, l.Digest)
	if err != nil {
		return nil, nil, fmt.Errorf("rpm: fetch the metadata layer %s: %w", l.Digest, err)
	}
	defer func() { _ = rc.Close() }()
	b, err := io.ReadAll(io.LimitReader(rc, maxMetadata+1))
	if err != nil {
		return nil, nil, fmt.Errorf("rpm: read the metadata layer %s: %w", l.Digest, err)
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	var md metadata
	if err := dec.Decode(&md); err != nil {
		return nil, nil, fmt.Errorf("%w: the metadata layer is not the rpm metadata object: %w", format.ErrInvalid, err)
	}
	return &md, b, nil
}

// check compares the metadata with the file layer f and the annotations a.
func (md *metadata) check(a profile.Annotations, f profile.Layer) error {
	switch {
	case md.Name != a.Name:
		return fmt.Errorf("%w: the metadata name is %q, %s is %q", format.ErrInvalid, md.Name, profile.AnnotationName, a.Name)
	case EVR(md.Epoch, md.Version, md.Release) != a.Version:
		return fmt.Errorf("%w: the metadata epoch, version and release make %q, %s is %q", format.ErrInvalid,
			EVR(md.Epoch, md.Version, md.Release), profile.AnnotationVersion, a.Version)
	case md.Arch != a.Variant:
		return fmt.Errorf("%w: the metadata arch is %q, %s is %q", format.ErrInvalid, md.Arch, profile.AnnotationVariant, a.Variant)
	case md.Filename != f.Title:
		return fmt.Errorf("%w: the metadata filename is not the file layer's title %q", format.ErrInvalid, f.Title)
	case md.Size != f.Size:
		return fmt.Errorf("%w: the metadata size is not the file layer's %d", format.ErrInvalid, f.Size)
	case md.SHA256 != f.Digest.Encoded():
		return fmt.Errorf("%w: the metadata sha256 is not the file layer's %s", format.ErrInvalid, f.Digest.Encoded())
	}
	return nil
}

// checkPackage reads the head of the package f through fetch and compares the metadata it makes
// with md, key by key.
func checkPackage(ctx context.Context, fetch format.BlobFetcher, f profile.Layer, md *metadata) error {
	rc, err := fetch(ctx, f.Digest)
	switch {
	case errors.Is(err, format.ErrStreamed):
		return nil
	case err != nil:
		return fmt.Errorf("rpm: fetch the package %s: %w", f.Digest, err)
	}
	defer func() { _ = rc.Close() }()
	pkg, err := ParseHead(rc, MaxHeader)
	if err != nil {
		return fmt.Errorf("rpm: the package %s: %w", f.Title, err)
	}
	pkg.Size, pkg.SHA256 = f.Size, f.Digest.Encoded()
	return sameMetadata(metadataOf(pkg), md)
}

// sameMetadata reports the first key, in the order of the metadata object, where want, what the
// header makes, and got, the metadata layer, differ.
func sameMetadata(want, got *metadata) error {
	w, err := keyed(want)
	if err != nil {
		return err
	}
	g, err := keyed(got)
	if err != nil {
		return err
	}
	for _, k := range w.keys {
		if !bytes.Equal(w.values[k], g.values[k]) {
			return fmt.Errorf("%w: the metadata %s is not what the header of the package says", format.ErrInvalid, k)
		}
	}
	return nil
}

// keyedJSON is a JSON object as its keys in order and the encoding of each value.
type keyedJSON struct {
	keys   []string
	values map[string]json.RawMessage
}

// keyed returns md as keyedJSON, its keys in the order of the struct.
func keyed(md *metadata) (keyedJSON, error) {
	b, err := json.Marshal(md)
	if err != nil {
		return keyedJSON{}, fmt.Errorf("rpm: encode the metadata: %w", err)
	}
	k := keyedJSON{values: map[string]json.RawMessage{}}
	dec := json.NewDecoder(bytes.NewReader(b))
	if _, err := dec.Token(); err != nil { // {
		return keyedJSON{}, fmt.Errorf("rpm: decode the metadata: %w", err)
	}
	for dec.More() {
		t, err := dec.Token()
		if err != nil {
			return keyedJSON{}, fmt.Errorf("rpm: decode the metadata: %w", err)
		}
		var v json.RawMessage
		if err := dec.Decode(&v); err != nil {
			return keyedJSON{}, fmt.Errorf("rpm: decode the metadata: %w", err)
		}
		key, _ := t.(string)
		k.keys, k.values[key] = append(k.keys, key), v
	}
	return k, nil
}
