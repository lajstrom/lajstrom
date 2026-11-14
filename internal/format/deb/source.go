package deb

import (
	"context"
	"crypto/md5"  //nolint:gosec // apt's Sources names an md5 of the .dsc; not a security use
	"crypto/sha1" //nolint:gosec // apt's Sources names a sha1 of the .dsc; not a security use
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"strings"
	"time"

	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/lajstrom/lajstrom/internal/format"
	"github.com/lajstrom/lajstrom/internal/index"
	"github.com/lajstrom/lajstrom/internal/oci/profile"
)

// sourceMetadata is the metadata layer of a source version (docs/spec/manifest-profile.md §2):
// the fields of the .dsc as written, and the name, size and checksums of the .dsc; the other
// files are described by the .dsc's own lists. The fields are in the order of their keys, so
// that the JSON has its keys sorted.
type sourceMetadata struct {
	Control  map[string]string `json:"control"`
	Filename string            `json:"filename"`
	MD5      string            `json:"md5"`
	SHA1     string            `json:"sha1"`
	SHA256   string            `json:"sha256"`
	Size     int64             `json:"size"`
}

// isSourceUpload reports whether in is a source package: its first file is a .dsc that did not
// stream through the deb inspector (an upload of one .deb does).
func isSourceUpload(in format.Upload) bool {
	return len(in.Files) > 0 && strings.HasSuffix(in.Files[0].Name, ".dsc") && in.Files[0].Inspector == nil
}

// buildSource makes the manifest of a source package to suite/component: in.Files holds the
// .dsc first, read whole (at most MaxDsc), then files it names. Each of those is checked
// against the size and the checksums the .dsc gives it: by the checksums the receiver computed
// as it arrived when it has all four (a publication session pushed it, and it is left out of
// Built.Blobs), else by a read of the file. A file the .dsc names and in lacks is described by
// the .dsc's own lists and left to ingest to find in the backend: the frontend took it from the
// pool (an .orig.tar.* of an earlier revision). The manifest has the .dsc layer, a layer per file
// in the order of Checksums-Sha256, and the metadata layer; name = Source, version = Version,
// variant = <suite>/<component>/source; the tag profile.Tag(Version, variant).
func buildSource(repo index.Repo, in format.Upload, suite, component string) (*format.Built, error) {
	dscFile := in.Files[0]
	raw, err := readDsc(dscFile)
	if err != nil {
		return nil, err
	}
	dsc, err := ParseDsc(raw)
	if err != nil {
		return nil, err
	}
	if dscFile.Name != dsc.Filename {
		return nil, fmt.Errorf("%w: the .dsc is called %s; its Source and Version make it %s", format.ErrInvalid, dscFile.Name, dsc.Filename)
	}
	uploaded := map[string]format.UploadedFile{}
	for _, f := range in.Files[1:] {
		if _, dup := uploaded[f.Name]; dup {
			return nil, fmt.Errorf("%w: %s is uploaded twice", format.ErrInvalid, f.Name)
		}
		uploaded[f.Name] = f
	}
	sum := checksumsOfBytes(raw)
	md, err := json.Marshal(sourceMetadata{Control: dsc.Control.Map(), Filename: dsc.Filename, MD5: sum["md5"],
		SHA1: sum["sha1"], SHA256: sum["sha256"], Size: int64(len(raw))})
	if err != nil {
		return nil, fmt.Errorf("deb: metadata of %s: %w", dsc.Filename, err)
	}
	dscDigest := digest.NewDigestFromEncoded(digest.SHA256, sum["sha256"])
	layers := []profile.Layer{{MediaType: profile.MediaTypeDebDsc, Digest: dscDigest, Size: int64(len(raw)), Title: dsc.Filename}}
	blobs := []format.BlobSource{bytesBlob(profile.EmptyConfig, ocispec.DescriptorEmptyJSON.Data),
		bytesBlob(ocispec.Descriptor{MediaType: profile.MediaTypeDebDsc, Digest: dscDigest, Size: int64(len(raw))}, raw)}
	for _, want := range dsc.Files {
		d := digest.NewDigestFromEncoded(digest.SHA256, want.SHA256)
		layers = append(layers, profile.Layer{MediaType: profile.MediaTypeDebSource, Digest: d, Size: want.Size, Title: want.Name})
		f, ok := uploaded[want.Name]
		if !ok {
			continue
		}
		delete(uploaded, want.Name)
		pushed, err := checkSourceFile(f, want)
		if err != nil {
			return nil, err
		}
		if !pushed {
			open := f.Open
			blobs = append(blobs, format.BlobSource{Descriptor: ocispec.Descriptor{MediaType: profile.MediaTypeDebSource, Digest: d, Size: want.Size},
				Open: func(context.Context) (io.ReadCloser, error) { return open() }})
		}
	}
	for name := range uploaded {
		return nil, fmt.Errorf("%w: %s is not a file %s names", format.ErrInvalid, name, dsc.Filename)
	}
	variant := variantOf(suite, component, ArchSource)
	attrs := map[string]string{
		profile.AnnotationDebArch: ArchSource, profile.AnnotationDebSuite: suite, profile.AnnotationDebComponent: component,
	}
	m, err := profile.Build(profile.Annotations{
		Format: Name, Repo: repo.Name, Name: dsc.Source, Version: dsc.Version, Variant: variant,
		Created: time.Now().UTC().Truncate(time.Second), UploadedBy: in.Params[ParamUploadedBy], Attrs: attrs,
	}, layers, &profile.Layer{Digest: digest.FromBytes(md), Size: int64(len(md))})
	if err != nil {
		return nil, fmt.Errorf("%w: %w", format.ErrInvalid, err)
	}
	files := make([]format.FileEntry, len(layers))
	for i, l := range layers {
		files[i] = format.FileEntry{Name: l.Title, MediaType: l.MediaType, Digest: l.Digest, Size: l.Size}
	}
	return &format.Built{
		Manifest: m,
		Blobs:    append(blobs, bytesBlob(m.Layers[len(m.Layers)-1], md)),
		Entry:    format.Entry{Name: dsc.Source, Version: dsc.Version, Variant: variant, Attrs: attrs, Metadata: md, Files: files},
		Tag:      profile.Tag(dsc.Version, variant),
		RepoPath: profile.RepositoryPath("", Name, repo.Name, profile.EscapeComponent(dsc.Source)),
	}, nil
}

// isSource reports whether v is a version of a source package.
func isSource(v index.Version) bool { return v.Attrs[profile.AnnotationDebArch] == ArchSource }

// readDsc reads the .dsc f whole, at most MaxDsc bytes.
func readDsc(f format.UploadedFile) ([]byte, error) {
	if f.Open == nil {
		return nil, fmt.Errorf("%w: the .dsc %s cannot be read", format.ErrInvalid, f.Name)
	}
	if f.Size > MaxDsc {
		return nil, fmt.Errorf("%w: the .dsc %s has %d bytes, at most %d", format.ErrInvalid, f.Name, f.Size, MaxDsc)
	}
	rc, err := f.Open()
	if err != nil {
		return nil, fmt.Errorf("deb: open %s: %w", f.Name, err)
	}
	defer func() { _ = rc.Close() }()
	b, err := io.ReadAll(io.LimitReader(rc, MaxDsc+1))
	switch {
	case err != nil:
		return nil, fmt.Errorf("deb: read %s: %w", f.Name, err)
	case len(b) > MaxDsc:
		return nil, fmt.Errorf("%w: the .dsc %s has more than %d bytes", format.ErrInvalid, f.Name, MaxDsc)
	case f.Size >= 0 && int64(len(b)) != f.Size:
		return nil, fmt.Errorf("%w: %s has %d bytes, the upload says %d", format.ErrInvalid, f.Name, len(b), f.Size)
	}
	return b, nil
}

// checkSourceFile checks the file f against what the .dsc says of it, want: its size and its
// md5, sha1 (when the .dsc gives one) and sha256. pushed is true when f is in the backend
// already: it has the four checksums of a publication session and a size, which are compared;
// else f is read and hashed.
func checkSourceFile(f format.UploadedFile, want SourceFile) (pushed bool, err error) {
	sums, size := f.Checksums, f.Size
	pushed = size >= 0 && sums["md5"] != "" && sums["sha1"] != "" && sums["sha256"] != "" && sums["sha512"] != ""
	if !pushed {
		if f.Open == nil {
			return false, fmt.Errorf("%w: %s cannot be read", format.ErrInvalid, f.Name)
		}
		if sums, size, err = hashFile(f); err != nil {
			return false, err
		}
	}
	if size != want.Size {
		return false, fmt.Errorf("%w: %s has %d bytes, the .dsc says %d", format.ErrInvalid, f.Name, size, want.Size)
	}
	for _, c := range []struct{ alg, want, field string }{
		{"md5", want.MD5, FieldFiles}, {"sha1", want.SHA1, FieldChecksumsSha1}, {"sha256", want.SHA256, FieldChecksumsSha256},
	} {
		if c.want != "" && sums[c.alg] != c.want {
			return false, fmt.Errorf("%w: the %s of %s is %s, the .dsc's %s says %s", format.ErrInvalid, c.alg, f.Name, sums[c.alg], c.field, c.want)
		}
	}
	return pushed, nil
}

// hashFile reads f through md5, sha1 and sha256 and returns them by name with its size.
func hashFile(f format.UploadedFile) (map[string]string, int64, error) {
	rc, err := f.Open()
	if err != nil {
		return nil, 0, fmt.Errorf("deb: open %s: %w", f.Name, err)
	}
	defer func() { _ = rc.Close() }()
	hs := map[string]hash.Hash{"md5": md5.New(), "sha1": sha1.New(), "sha256": sha256.New()} //nolint:gosec // apt's checksums
	w := io.MultiWriter(hs["md5"], hs["sha1"], hs["sha256"])
	n, err := io.Copy(w, rc)
	if err != nil {
		return nil, 0, fmt.Errorf("deb: read %s: %w", f.Name, err)
	}
	out := make(map[string]string, len(hs))
	for k, h := range hs {
		out[k] = hex.EncodeToString(h.Sum(nil))
	}
	return out, n, nil
}

// checksumsOfBytes returns the md5, sha1 and sha256 of b by name.
func checksumsOfBytes(b []byte) map[string]string {
	m := md5.Sum(b)   //nolint:gosec // apt's checksums
	s1 := sha1.Sum(b) //nolint:gosec // apt's checksums
	s2 := sha256.Sum256(b)
	return map[string]string{"md5": hex.EncodeToString(m[:]), "sha1": hex.EncodeToString(s1[:]), "sha256": hex.EncodeToString(s2[:])}
}

// validateSource checks a manifest of a source version beyond the profile, its annotations a
// with lajstrom.deb.arch source: the suite, the component and the variant; the first file layer
// the .dsc, titled DscFilename(name, version), the others source files; the metadata layer with
// the .dsc's fields, name, size and checksums; and the .dsc itself, read through fetch: its
// checksums and fields are the metadata's, its Source and Version the annotations, and it names
// exactly the other file layers, each with its size and, as Checksums-Sha256, the layer's
// sha256 digest. The md5 and sha1 the .dsc gives a file are not checked here: that needs the
// whole file, and the upload route checks them as the files stream in.
func validateSource(ctx context.Context, parsed *profile.Parsed, fetch format.BlobFetcher) (*format.Entry, error) {
	a := parsed.Annotations
	if err := checkSourceAnnotations(a); err != nil {
		return nil, err
	}
	if err := checkSourceLayers(parsed, fetch); err != nil {
		return nil, err
	}
	files := parsed.Files
	b, err := fetchAll(ctx, fetch, parsed.Metadata.Digest, maxMetadata)
	if err != nil {
		return nil, err
	}
	var md sourceMetadataIn
	if err := json.Unmarshal(b, &md); err != nil {
		return nil, fmt.Errorf("%w: the metadata layer is not the deb source metadata object: %w", format.ErrInvalid, err)
	}
	raw, err := fetchAll(ctx, fetch, files[0].Digest, MaxDsc)
	if err != nil {
		return nil, err
	}
	if err := md.check(files[0], raw); err != nil {
		return nil, err
	}
	if err := checkDsc(raw, md.Control, a, files[1:]); err != nil {
		return nil, err
	}
	entries := make([]format.FileEntry, len(files))
	for i, f := range files {
		entries[i] = format.FileEntry{Name: f.Title, MediaType: f.MediaType, Digest: f.Digest, Size: f.Size, Attrs: f.Annotations}
	}
	return &format.Entry{Name: a.Name, Version: a.Version, Variant: a.Variant, Attrs: a.Attrs, Metadata: b, Files: entries}, nil
}

// checkSourceLayers checks the layers of a source manifest: two file layers at least, the first
// the .dsc titled DscFilename of the name and version, and a metadata layer.
func checkSourceLayers(parsed *profile.Parsed, fetch format.BlobFetcher) error {
	a, files := parsed.Annotations, parsed.Files
	want := DscFilename(a.Name, a.Version)
	switch {
	case len(files) < 2:
		return fmt.Errorf("%w: a deb source manifest has the .dsc and the files it names, got %d file layers", format.ErrInvalid, len(files))
	case files[0].MediaType != profile.MediaTypeDebDsc || files[0].Title != want:
		return fmt.Errorf("%w: the first file layer of a deb source manifest is the .dsc, %s, titled %q, the name of %s and %s",
			format.ErrInvalid, profile.MediaTypeDebDsc, want, profile.AnnotationName, profile.AnnotationVersion)
	case parsed.Metadata == nil || fetch == nil:
		return fmt.Errorf("%w: a deb manifest has a metadata layer", format.ErrInvalid)
	}
	return nil
}

// checkDsc parses the .dsc raw and checks it against the control object of the metadata, field
// by field, against the annotations a for its Source and Version, and against the file layers
// after it (sameSourceFiles).
func checkDsc(raw []byte, control map[string]string, a profile.Annotations, layers []profile.Layer) error {
	dsc, err := ParseDsc(raw)
	if err != nil {
		return err
	}
	if err := sameControl(dsc.Control.Map(), control); err != nil {
		return err
	}
	if dsc.Source != a.Name || dsc.Version != a.Version {
		return fmt.Errorf("%w: the .dsc is %s %s, the annotations say %s %s", format.ErrInvalid, dsc.Source, dsc.Version, a.Name, a.Version)
	}
	return sameSourceFiles(dsc, layers)
}

// checkSourceAnnotations checks the annotations of a source manifest: the suite, the component,
// the variant <suite>/<component>/source, the name and the version.
func checkSourceAnnotations(a profile.Annotations) error {
	suite, component := a.Attrs[profile.AnnotationDebSuite], a.Attrs[profile.AnnotationDebComponent]
	for _, c := range []struct{ key, value string }{{profile.AnnotationDebSuite, suite}, {profile.AnnotationDebComponent, component}} {
		if err := checkDistName(c.key, c.value); err != nil {
			return err
		}
	}
	if err := checkVariant(a, suite, component, ArchSource); err != nil {
		return err
	}
	if err := checkName(profile.AnnotationName, a.Name); err != nil {
		return err
	}
	return checkVersion(profile.AnnotationVersion, a.Version)
}

// sourceMetadataIn is the metadata layer of a source version as validateSource reads it: a
// missing key is a nil field.
type sourceMetadataIn struct {
	Control  map[string]string `json:"control"`
	Filename *string           `json:"filename"`
	Size     *int64            `json:"size"`
	MD5      *string           `json:"md5"`
	SHA1     *string           `json:"sha1"`
	SHA256   *string           `json:"sha256"`
}

// check compares the metadata with the .dsc layer l and its bytes raw: the control object is
// there, and the filename, size and checksums are those of the .dsc, whose sha256 is the layer
// digest.
func (md *sourceMetadataIn) check(l profile.Layer, raw []byte) error {
	sum := checksumsOfBytes(raw)
	switch {
	case md.Control == nil:
		return fmt.Errorf("%w: the metadata has no control object", format.ErrInvalid)
	case md.Filename == nil || *md.Filename != l.Title:
		return fmt.Errorf("%w: the metadata filename is not the .dsc layer's title %q", format.ErrInvalid, l.Title)
	case md.Size == nil || *md.Size != l.Size || int64(len(raw)) != l.Size:
		return fmt.Errorf("%w: the metadata size is not the .dsc layer's %d", format.ErrInvalid, l.Size)
	case l.Digest != digest.NewDigestFromEncoded(digest.SHA256, sum["sha256"]):
		return fmt.Errorf("%w: the .dsc layer digest %s is not the sha256 of its bytes", format.ErrInvalid, l.Digest)
	}
	for _, c := range []struct {
		alg string
		got *string
	}{{"md5", md.MD5}, {"sha1", md.SHA1}, {"sha256", md.SHA256}} {
		if c.got == nil || *c.got != sum[c.alg] {
			return fmt.Errorf("%w: the metadata %s is not the .dsc's %s", format.ErrInvalid, c.alg, sum[c.alg])
		}
	}
	return nil
}

// sameSourceFiles checks that the .dsc names exactly the file layers, each a source file layer
// once, with the size the .dsc gives it and its sha256 as the layer digest.
func sameSourceFiles(dsc *Dsc, layers []profile.Layer) error {
	named := make(map[string]SourceFile, len(dsc.Files))
	for _, f := range dsc.Files {
		named[f.Name] = f
	}
	for _, l := range layers {
		f, ok := named[l.Title]
		switch {
		case l.MediaType != profile.MediaTypeDebSource:
			return fmt.Errorf("%w: the file layer %s is %s; the files of a .dsc are %s", format.ErrInvalid, l.Title, l.MediaType, profile.MediaTypeDebSource)
		case !ok:
			return fmt.Errorf("%w: the file layer %s is not a file %s names, or comes twice", format.ErrInvalid, l.Title, dsc.Filename)
		case l.Size != f.Size || l.Digest != digest.NewDigestFromEncoded(digest.SHA256, f.SHA256):
			return fmt.Errorf("%w: the file layer %s (%s, %d bytes) is not what %s says of it (sha256:%s, %d bytes)",
				format.ErrInvalid, l.Title, l.Digest, l.Size, dsc.Filename, f.SHA256, f.Size)
		}
		delete(named, l.Title)
	}
	for name := range named {
		return fmt.Errorf("%w: %s names %s, which the manifest has no layer for", format.ErrInvalid, dsc.Filename, name)
	}
	return nil
}

// fetchAll reads the blob d through fetch, at most limit bytes.
func fetchAll(ctx context.Context, fetch format.BlobFetcher, d digest.Digest, limit int64) ([]byte, error) {
	rc, err := fetch(ctx, d)
	if errors.Is(err, format.ErrStreamed) {
		return nil, fmt.Errorf("%w: %s streamed in; a source package does not", format.ErrInvalid, d)
	}
	if err != nil {
		return nil, fmt.Errorf("deb: fetch %s: %w", d, err)
	}
	defer func() { _ = rc.Close() }()
	b, err := io.ReadAll(io.LimitReader(rc, limit+1))
	switch {
	case err != nil:
		return nil, fmt.Errorf("deb: read %s: %w", d, err)
	case int64(len(b)) > limit:
		return nil, fmt.Errorf("%w: %s has more than %d bytes", format.ErrInvalid, d, limit)
	}
	return b, nil
}
