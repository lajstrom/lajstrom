package maven

import (
	"context"
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

// Validate checks a maven manifest (an oras push through the facade, a reindex, or the output of
// Build) beyond the profile, so that the annotations, the file names, the metadata layer and the
// POM agree:
//
//   - no lajstrom.variant; lajstrom.maven.groupId, .artifactId and .packaging, and no other
//     lajstrom.maven.* annotation; lajstrom.name is groupId:artifactId; the coordinates are well
//     formed and the version a release or a SNAPSHOT build (whichever the repo's policy takes:
//     Admit asks that, on a publish only);
//   - every file layer is titled <artifactId>-<version>[-<classifier>].<extension>
//     (ParseFilename), each title once, digested with sha256; the POM, <artifactId>-<version>.pom,
//     is the one layer typed pom and has no lajstrom.* annotations; every other layer is typed
//     artifact with the lajstrom.maven.extension and .classifier of its title;
//   - the metadata layer (at most 4 MiB) has the coordinates and the packaging of the
//     annotations, and one file per file layer with its title, extension, classifier, size and
//     sha256, and md5, sha1 and sha512 in lowercase hex;
//   - the POM, read through fetch, is the one of the metadata: the coordinates and the packaging
//     of the annotations (ParsePOM; the POM of a build has its SNAPSHOT version), its parent and
//     dependencies, and its md5, sha1 and sha512.
//
// Validate reads the metadata layer and the POM and no other blob: the checksums of an artifact
// are those Build computed when the file was received, or the pusher's claim, of which only the
// form is checked, since reading every jar of a reindex is what the metadata layer is there to
// avoid. Errors wrap format.ErrInvalid, except a failure to fetch or read a blob.
func (Plugin) Validate(ctx context.Context, _ index.Repo, m ocispec.Manifest, fetch format.BlobFetcher) (*format.Entry, error) {
	parsed, err := profile.Validate(m)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", format.ErrInvalid, err)
	}
	a := parsed.Annotations
	c, packaging, err := checkAnnotations(a)
	if err != nil {
		return nil, err
	}
	files, pom, err := checkLayers(c, parsed.Files)
	if err != nil {
		return nil, err
	}
	if parsed.Metadata == nil { // profile.Validate requires it for maven; Validate relies on it
		return nil, fmt.Errorf("%w: a maven manifest has a metadata layer", format.ErrInvalid)
	}
	if fetch == nil {
		return nil, fmt.Errorf("maven: no blob fetcher for the metadata layer %s", parsed.Metadata.Digest)
	}
	md, raw, err := readMetadata(ctx, fetch, *parsed.Metadata)
	if err != nil {
		return nil, err
	}
	if err := md.check(c, packaging, parsed.Files, files); err != nil {
		return nil, err
	}
	if err := md.checkPOM(ctx, fetch, parsed.Files[pom], packaging); err != nil {
		return nil, err
	}
	entryFiles := make([]format.FileEntry, len(parsed.Files))
	for i, l := range parsed.Files {
		entryFiles[i] = format.FileEntry{Name: l.Title, MediaType: l.MediaType, Digest: l.Digest, Size: l.Size, Attrs: l.Annotations}
	}
	return &format.Entry{Name: a.Name, Version: a.Version, Attrs: a.Attrs, Files: entryFiles, Metadata: raw}, nil
}

// checkAnnotations checks the manifest annotations of a maven manifest and returns the
// coordinates and the packaging they name.
func checkAnnotations(a profile.Annotations) (Coordinates, string, error) {
	if a.Variant != "" {
		return Coordinates{}, "", fmt.Errorf("%w: a maven manifest has no %s, this one %q", format.ErrInvalid, profile.AnnotationVariant, a.Variant)
	}
	keys := []string{profile.AnnotationMavenGroupID, profile.AnnotationMavenArtifactID, profile.AnnotationMavenPackaging}
	for k := range a.Attrs {
		if !slices.Contains(keys, k) {
			return Coordinates{}, "", fmt.Errorf("%w: %s is not an annotation of a maven manifest", format.ErrInvalid, k)
		}
	}
	for _, k := range keys {
		if a.Attrs[k] == "" {
			return Coordinates{}, "", fmt.Errorf("%w: a maven manifest has %s", format.ErrInvalid, k)
		}
	}
	c := Coordinates{GroupID: a.Attrs[profile.AnnotationMavenGroupID], ArtifactID: a.Attrs[profile.AnnotationMavenArtifactID], Version: a.Version}
	if err := c.check(); err != nil {
		return Coordinates{}, "", err
	}
	if a.Name != c.Name() {
		return Coordinates{}, "", fmt.Errorf("%w: %s is %q, not %s:%s, %q", format.ErrInvalid, profile.AnnotationName, a.Name,
			profile.AnnotationMavenGroupID, profile.AnnotationMavenArtifactID, c.Name())
	}
	packaging := a.Attrs[profile.AnnotationMavenPackaging]
	if !artifactIDPattern.MatchString(packaging) {
		return Coordinates{}, "", fmt.Errorf("%w: %s %q is not letters, digits, \"_\", \"-\" and \".\"", format.ErrInvalid, profile.AnnotationMavenPackaging, packaging)
	}
	return c, packaging, nil
}

// checkLayers checks the file layers of the version c and returns what each title says and the
// index of the POM.
func checkLayers(c Coordinates, layers []profile.Layer) ([]File, int, error) {
	files := make([]File, len(layers))
	seen := map[string]bool{}
	pom := -1
	for i, l := range layers {
		f, err := ParseFilename(c, l.Title)
		if err != nil {
			return nil, 0, err
		}
		if seen[l.Title] {
			return nil, 0, fmt.Errorf("%w: two file layers are titled %q", format.ErrInvalid, l.Title)
		}
		seen[l.Title] = true
		if l.Digest.Algorithm() != digest.SHA256 {
			return nil, 0, fmt.Errorf("%w: the layer %q has a %s digest; maven clients ask for its sha256", format.ErrInvalid, l.Title, l.Digest.Algorithm())
		}
		if f.IsPOM() {
			pom = i
		}
		if err := checkLayer(f, l); err != nil {
			return nil, 0, err
		}
		files[i] = f
	}
	if pom < 0 {
		return nil, 0, fmt.Errorf("%w: a maven manifest has the POM %s as a layer", format.ErrInvalid, File{Coordinates: c, Extension: "pom"}.Filename())
	}
	return files, pom, nil
}

// checkLayer checks the media type and the lajstrom.* annotations of the layer l of the file f.
func checkLayer(f File, l profile.Layer) error {
	want := profile.MediaTypeMavenArtifact
	if f.IsPOM() {
		want = profile.MediaTypeMavenPom
	}
	if l.MediaType != want {
		return fmt.Errorf("%w: the layer %q is a %s, not a %s", format.ErrInvalid, l.Title, l.MediaType, want)
	}
	var got map[string]string
	for k, v := range l.Annotations {
		if strings.HasPrefix(k, "lajstrom.") {
			if got == nil {
				got = map[string]string{}
			}
			got[k] = v
		}
	}
	if wantAnn := f.annotations(); !maps.Equal(got, wantAnn) {
		return fmt.Errorf("%w: the layer %q has the lajstrom annotations %v, its file name gives %v", format.ErrInvalid, l.Title, got, wantAnn)
	}
	return nil
}

// readMetadata fetches the metadata layer l, at most maxMetadata bytes, and decodes it. It
// returns the bytes as they are in the registry too.
func readMetadata(ctx context.Context, fetch format.BlobFetcher, l profile.Layer) (*metadata, json.RawMessage, error) {
	b, err := readBlob(ctx, fetch, l, maxMetadata)
	if err != nil {
		return nil, nil, err
	}
	var md metadata
	if err := json.Unmarshal(b, &md); err != nil {
		return nil, nil, fmt.Errorf("%w: the metadata layer is not the maven metadata object: %w", format.ErrInvalid, err)
	}
	return &md, b, nil
}

// readBlob fetches the blob of l, which has at most limit bytes.
func readBlob(ctx context.Context, fetch format.BlobFetcher, l profile.Layer, limit int64) ([]byte, error) {
	if l.Size > limit {
		return nil, fmt.Errorf("%w: the layer %q has %d bytes, at most %d", format.ErrInvalid, l.Title, l.Size, limit)
	}
	rc, err := fetch(ctx, l.Digest)
	if err != nil {
		return nil, fmt.Errorf("maven: fetch %s %s: %w", l.Title, l.Digest, err)
	}
	defer func() { _ = rc.Close() }()
	b, err := io.ReadAll(io.LimitReader(rc, limit+1))
	if err != nil {
		return nil, fmt.Errorf("maven: read %s %s: %w", l.Title, l.Digest, err)
	}
	return b, nil
}

// check compares the metadata with the coordinates and packaging of the annotations and with
// the file layers, whose titles say files.
func (md *metadata) check(c Coordinates, packaging string, layers []profile.Layer, files []File) error {
	for _, f := range []struct{ field, got, want string }{
		{"groupId", md.GroupID, c.GroupID}, {"artifactId", md.ArtifactID, c.ArtifactID},
		{"version", md.Version, c.Version}, {"packaging", md.Packaging, packaging},
	} {
		if f.got != f.want {
			return fmt.Errorf("%w: the metadata %s is %q, the annotations say %q", format.ErrInvalid, f.field, f.got, f.want)
		}
	}
	if len(md.Files) != len(layers) {
		return fmt.Errorf("%w: the metadata lists %d files, the manifest has %d file layers", format.ErrInvalid, len(md.Files), len(layers))
	}
	for i, l := range layers {
		e := md.file(l.Title)
		switch {
		case e == nil:
			return fmt.Errorf("%w: the metadata lists no file %q", format.ErrInvalid, l.Title)
		case e.Extension != files[i].Extension || e.Classifier != files[i].Classifier:
			return fmt.Errorf("%w: the metadata gives %q the extension %q and classifier %q, its name %q and %q",
				format.ErrInvalid, l.Title, e.Extension, e.Classifier, files[i].Extension, files[i].Classifier)
		case e.Size != l.Size:
			return fmt.Errorf("%w: the metadata gives %q %d bytes, its layer %d", format.ErrInvalid, l.Title, e.Size, l.Size)
		case e.SHA256 != l.Digest.Encoded():
			return fmt.Errorf("%w: the metadata sha256 of %q is not its layer's %s", format.ErrInvalid, l.Title, l.Digest.Encoded())
		}
		if err := e.checkChecksums(); err != nil {
			return err
		}
	}
	return nil
}

// file returns the metadata of the file name, nil when it lists none.
func (md *metadata) file(name string) *fileMetadata {
	i := slices.IndexFunc(md.Files, func(f fileMetadata) bool { return f.Name == name })
	if i < 0 {
		return nil
	}
	return &md.Files[i]
}

// checkPOM reads the POM of the layer l through fetch and compares it with the metadata, which
// check found to agree with the annotations: its checksums, its coordinates and packaging, its
// parent and dependencies.
func (md *metadata) checkPOM(ctx context.Context, fetch format.BlobFetcher, l profile.Layer, packaging string) error {
	b, err := readBlob(ctx, fetch, l, maxPOM)
	if err != nil {
		return err
	}
	if digest.SHA256.FromBytes(b) != l.Digest {
		return fmt.Errorf("%w: the blob of the POM %q is not its digest %s", format.ErrInvalid, l.Title, l.Digest)
	}
	h := newHashes()
	_, _ = h.writer().Write(b)
	sums, e := h.sums(), md.file(l.Title)
	for _, a := range algorithms {
		if got := *e.checksum(a.name); got != sums[a.name] {
			return fmt.Errorf("%w: the metadata %s of the POM is %s, the POM's %s", format.ErrInvalid, a.name, got, sums[a.name])
		}
	}
	pom, err := ParsePOM(b)
	if err != nil {
		return fmt.Errorf("%s: %w", l.Title, err)
	}
	c := Coordinates{GroupID: md.GroupID, ArtifactID: md.ArtifactID, Version: md.Version}
	switch {
	case !pom.of(c):
		return fmt.Errorf("%w: the POM is of %s, the annotations of %s", format.ErrInvalid, pom.Coordinates, c)
	case pom.Packaging != packaging:
		return fmt.Errorf("%w: the POM has packaging %q, %s is %q", format.ErrInvalid, pom.Packaging, profile.AnnotationMavenPackaging, packaging)
	case !sameParent(pom.Parent, md.POM.Parent):
		return fmt.Errorf("%w: the metadata parent of the POM is not the POM's", format.ErrInvalid)
	case !slices.Equal(pom.Dependencies, md.POM.Dependencies):
		return fmt.Errorf("%w: the metadata dependencies of the POM are not the POM's", format.ErrInvalid)
	}
	return nil
}

// sameParent reports whether two parents are both absent or the same coordinates.
func sameParent(a, b *Coordinates) bool {
	return (a == nil && b == nil) || (a != nil && b != nil && *a == *b)
}
