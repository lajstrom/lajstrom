package maven

import (
	"bytes"
	"context"
	"crypto/md5"  //nolint:gosec // maven names md5 checksums; not a security use
	"crypto/sha1" //nolint:gosec // maven names sha1 checksums; not a security use
	"crypto/sha256"
	"crypto/sha512"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"hash"
	"io"
	"slices"
	"strings"
	"time"

	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/lajstrom/lajstrom/internal/format"
	"github.com/lajstrom/lajstrom/internal/index"
	"github.com/lajstrom/lajstrom/internal/oci/profile"
)

// Build makes the manifest of one version from the files of its deploy. in.Files holds every
// file of one groupId:artifactId:version, each named by its path below /maven/<repo>/
// (ParsePath): the artifacts, the POM and the signatures, no checksum file and no
// maven-metadata.xml, each file name once, and the POM among them. Build always reads the POM
// and parses it (ParsePOM): its coordinates must be those of the paths. A file that comes with
// all four format.UploadedFile.Checksums and a size is in the backend already, a publication
// session pushed it: its layer is described from them and left out of Built.Blobs, and Build
// does not open it unless it is the POM. Build reads any other file to digest it and puts it in
// Built.Blobs. The checksums that come with a file Build reads must be those of its bytes. The
// manifest has a file layer per file, sorted by file name, the POM typed
// pom and the others artifact with lajstrom.maven.extension and .classifier, and the metadata
// layer; the annotations are name groupId:artifactId, version, no variant, and
// lajstrom.maven.groupId, .artifactId and .packaging; the tag is profile.Tag(version, "").
// created is now, in whole seconds. Errors wrap format.ErrInvalid, except a failure to read a
// file.
func (Plugin) Build(_ context.Context, repo index.Repo, in format.Upload) (*format.Built, error) {
	files, err := parseUpload(in.Files)
	if err != nil {
		return nil, err
	}
	c := files[0].file.Coordinates
	var pom *POM
	for i := range files {
		r := &files[i]
		b, err := r.receive()
		if err != nil {
			return nil, err
		}
		if r.file.IsPOM() {
			if pom, err = ParsePOM(b); err != nil {
				return nil, fmt.Errorf("%s: %w", r.up.Name, err)
			}
		}
	}
	if !pom.of(c) {
		return nil, fmt.Errorf("%w: the POM is of %s, its path of %s", format.ErrInvalid, pom.Coordinates, c)
	}
	pkgPath := packagePath(c.GroupID, c.ArtifactID)
	repoPath := profile.RepositoryPath("", Name, repo.Name, pkgPath)
	md, err := metadataOf(c, pom, files)
	if err != nil {
		return nil, err
	}
	layers := make([]profile.Layer, len(files))
	entryFiles := make([]format.FileEntry, len(files))
	for i, r := range files {
		layers[i] = r.layer()
		entryFiles[i] = format.FileEntry{Name: layers[i].Title, MediaType: layers[i].MediaType, Digest: layers[i].Digest,
			Size: layers[i].Size, Attrs: r.file.annotations()}
	}
	attrs := map[string]string{
		profile.AnnotationMavenGroupID: c.GroupID, profile.AnnotationMavenArtifactID: c.ArtifactID, profile.AnnotationMavenPackaging: pom.Packaging,
	}
	m, err := profile.Build(profile.Annotations{
		Format: Name, Repo: repo.Name, Name: c.Name(), Version: c.Version,
		Created: time.Now().UTC().Truncate(time.Second), UploadedBy: in.Params[ParamUploadedBy], Attrs: attrs,
	}, layers, &profile.Layer{Digest: digest.FromBytes(md), Size: int64(len(md))})
	if err != nil {
		return nil, fmt.Errorf("%w: %w", format.ErrInvalid, err)
	}
	blobs := []format.BlobSource{bytesBlob(profile.EmptyConfig, ocispec.DescriptorEmptyJSON.Data)}
	for i, r := range files {
		if r.read {
			blobs = append(blobs, format.BlobSource{Descriptor: m.Layers[i], Open: func(context.Context) (io.ReadCloser, error) { return r.up.Open() }})
		}
	}
	return &format.Built{
		Manifest: m,
		Blobs:    append(blobs, bytesBlob(m.Layers[len(files)], md)),
		Entry:    format.Entry{Name: c.Name(), Version: c.Version, Attrs: attrs, Files: entryFiles, Metadata: md},
		Tag:      profile.Tag(c.Version, ""),
		RepoPath: repoPath,
	}, nil
}

// received is one file of an upload: the file, what its path says, and what Build learnt of
// its bytes.
type received struct {
	up   format.UploadedFile
	file File
	// md describes the file in the metadata layer.
	md fileMetadata
	// read is true when Build read the bytes to digest them: the file goes in Built.Blobs.
	read bool
}

// parseUpload reads the paths of the files of an upload: one version, each file name once, the
// POM among them. The result is sorted by file name.
func parseUpload(ups []format.UploadedFile) ([]received, error) {
	if len(ups) == 0 {
		return nil, fmt.Errorf("%w: a maven upload has the files of one version, this one none", format.ErrInvalid)
	}
	files := make([]received, 0, len(ups))
	seen := map[string]bool{}
	pom := false
	for _, up := range ups {
		f, err := ParsePath(up.Name)
		if err != nil {
			return nil, err
		}
		if len(files) > 0 && f.Coordinates != files[0].file.Coordinates {
			return nil, fmt.Errorf("%w: %q is a file of %s, the upload of %s; one upload is one version",
				format.ErrInvalid, up.Name, f.Coordinates, files[0].file.Coordinates)
		}
		name := f.Filename()
		if seen[name] {
			return nil, fmt.Errorf("%w: the upload has %s twice", format.ErrInvalid, name)
		}
		seen[name] = true
		pom = pom || f.IsPOM()
		files = append(files, received{up: up, file: f})
	}
	if !pom {
		return nil, fmt.Errorf("%w: the upload of %s has no POM, %s", format.ErrInvalid,
			files[0].file.Coordinates, File{Coordinates: files[0].file.Coordinates, Extension: "pom"}.Filename())
	}
	slices.SortFunc(files, func(a, b received) int { return strings.Compare(a.file.Filename(), b.file.Filename()) })
	return files, nil
}

// receive describes the bytes of the file in r.md. A file that comes with all four checksums and
// its size is described from them and not opened, unless it is the POM; any other file is read
// and digested, and the checksums it comes with must be those of its bytes. receive returns the
// bytes of the POM, nil for another file.
func (r *received) receive() ([]byte, error) {
	r.md = fileMetadata{Name: r.file.Filename(), Extension: r.file.Extension, Classifier: r.file.Classifier, Size: r.up.Size}
	given := r.up.Checksums
	complete := r.up.Size >= 0
	for _, a := range algorithms {
		v, ok := given[a.name]
		complete = complete && ok
		*r.md.checksum(a.name) = v
	}
	if complete {
		if err := r.md.checkChecksums(); err != nil {
			return nil, err
		}
		if !r.file.IsPOM() {
			return nil, nil
		}
	}
	b, sums, size, err := r.digest()
	if err != nil {
		return nil, err
	}
	if r.up.Size >= 0 && size != r.up.Size {
		return nil, fmt.Errorf("%w: %s has %d bytes, the upload says %d", format.ErrInvalid, r.up.Name, size, r.up.Size)
	}
	for _, a := range algorithms {
		if v, ok := given[a.name]; ok && v != sums[a.name] {
			return nil, fmt.Errorf("%w: the %s of %s is %s, the upload says %s", format.ErrInvalid, a.name, r.up.Name, sums[a.name], v)
		}
		*r.md.checksum(a.name) = sums[a.name]
	}
	r.md.Size, r.read = size, !complete
	return b, nil
}

// digest reads the file once: its four checksums and its size, and for the POM its bytes, at
// most maxPOM of them.
func (r *received) digest() ([]byte, map[string]string, int64, error) {
	rc, err := r.up.Open()
	if err != nil {
		return nil, nil, 0, fmt.Errorf("maven: open %s: %w", r.up.Name, err)
	}
	defer func() { _ = rc.Close() }()
	h := newHashes()
	w, src := h.writer(), io.Reader(rc)
	var pom bytes.Buffer
	if r.file.IsPOM() {
		w, src = io.MultiWriter(w, &pom), io.LimitReader(rc, maxPOM+1)
	}
	size, err := io.Copy(w, src)
	switch {
	case err != nil:
		return nil, nil, 0, fmt.Errorf("maven: read %s: %w", r.up.Name, err)
	case r.file.IsPOM() && size > maxPOM:
		return nil, nil, 0, fmt.Errorf("%w: the POM %s has more than %d bytes", format.ErrInvalid, r.up.Name, maxPOM)
	}
	return pom.Bytes(), h.sums(), size, nil
}

// layer returns the file layer of r: the POM typed pom, any other file artifact with its
// extension and classifier as annotations.
func (r *received) layer() profile.Layer {
	l := profile.Layer{MediaType: profile.MediaTypeMavenPom, Digest: digest.NewDigestFromEncoded(digest.SHA256, r.md.SHA256),
		Size: r.md.Size, Title: r.md.Name}
	if !r.file.IsPOM() {
		l.MediaType, l.Annotations = profile.MediaTypeMavenArtifact, r.file.annotations()
	}
	return l
}

// annotations returns the layer annotations of the file: for an artifact lajstrom.maven.extension
// and, when it has one, lajstrom.maven.classifier; nil for the POM, whose media type says what it
// is.
func (f File) annotations() map[string]string {
	if f.IsPOM() {
		return nil
	}
	a := map[string]string{profile.AnnotationMavenExtension: f.Extension}
	if f.Classifier != "" {
		a[profile.AnnotationMavenClassifier] = f.Classifier
	}
	return a
}

// metadataOf returns the metadata layer of the version c (docs/spec/manifest-profile.md §2):
// compact JSON with its keys sorted, the version c's, a build's for a build whose POM has its
// SNAPSHOT version. One larger than maxMetadata wraps format.ErrInvalid: Validate would not read
// it.
func metadataOf(c Coordinates, pom *POM, files []received) ([]byte, error) {
	md := metadata{GroupID: c.GroupID, ArtifactID: c.ArtifactID, Version: c.Version, Packaging: pom.Packaging, POM: pom.essentials()}
	for _, r := range files {
		md.Files = append(md.Files, r.md)
	}
	b, err := json.Marshal(md)
	if err != nil {
		return nil, fmt.Errorf("maven: metadata of %s: %w", pom.Coordinates, err)
	}
	if len(b) > maxMetadata {
		return nil, fmt.Errorf("%w: the metadata of %s has %d bytes, at most %d", format.ErrInvalid, pom.Coordinates, len(b), maxMetadata)
	}
	return b, nil
}

// hashes are a hash of each checksum algorithm.
type hashes map[string]hash.Hash

// newHashes returns hashes of nothing yet.
func newHashes() hashes {
	return hashes{MD5: md5.New(), SHA1: sha1.New(), SHA256: sha256.New(), SHA512: sha512.New()} //nolint:gosec // maven checksums
}

// writer returns a writer to every hash.
func (h hashes) writer() io.Writer {
	return io.MultiWriter(h[MD5], h[SHA1], h[SHA256], h[SHA512])
}

// sums returns the lowercase hex digest of each algorithm.
func (h hashes) sums() map[string]string {
	out := make(map[string]string, len(h))
	for a, hh := range h {
		out[a] = hex.EncodeToString(hh.Sum(nil))
	}
	return out
}

// bytesBlob is a BlobSource of bytes in memory with the media type, digest and size of desc.
func bytesBlob(desc ocispec.Descriptor, b []byte) format.BlobSource {
	return format.BlobSource{
		Descriptor: ocispec.Descriptor{MediaType: desc.MediaType, Digest: desc.Digest, Size: desc.Size},
		Open:       func(context.Context) (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(b)), nil },
	}
}
