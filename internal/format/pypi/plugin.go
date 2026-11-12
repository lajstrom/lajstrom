package pypi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/lajstrom/lajstrom/internal/format"
	"github.com/lajstrom/lajstrom/internal/index"
	"github.com/lajstrom/lajstrom/internal/oci/profile"
)

// Name is the plugin name: the repo format, the first URL segment, lajstrom.format and the type
// of the artifactType.
const Name = "pypi"

// ParamUploadedBy is the upload parameter with the principal of the upload, for
// lajstrom.uploaded-by.
const ParamUploadedBy = "uploaded-by"

// ParamVariant is the upload parameter with the earlier form of the variant of a wheel with a
// build tag, its tag set without the build tag, which the variant was before #726: the upload
// handler sets it when the index has the file under that form, so that the same file again
// goes to its tag (the idempotent repeat, or 409 for other bytes) rather than to a second one.
// Build takes no other value from it.
const ParamVariant = "variant"

// The keys of the metadata layer besides the core metadata (docs/spec/manifest-profile.md §2).
const (
	FieldFilename     = "filename"
	FieldSHA256       = "sha256"
	FieldPackagetype  = "packagetype"
	FieldCoreMetadata = "core_metadata"
)

// The packagetype of the metadata layer, as the upload API of PyPI names them.
const (
	PackagetypeWheel = "bdist_wheel"
	PackagetypeSdist = "sdist"
)

// VariantSdist is the lajstrom.variant of a source distribution; a wheel's is its tag set.
const VariantSdist = "sdist"

// maxMetadata is the largest metadata layer Validate reads: the core metadata with its
// description, twice (the fields and core_metadata).
const maxMetadata = 2*maxCoreMetadata + 64<<10

// Plugin is the pypi format plugin: Build and Validate of wheels and source distributions (#31),
// the simple index (#32); the upload API is #33.
type Plugin struct{}

var _ format.Plugin = Plugin{}

func init() { format.Register(Plugin{}) }

// Name returns "pypi".
func (Plugin) Name() string { return Name }

// ArtifactType returns application/vnd.lajstrom.pypi.v1.
func (Plugin) ArtifactType() string { return profile.ArtifactType(Name) }

// EscapeName returns the package path of a project name: profile.EscapeComponent of its PEP 503
// normalized form (docs/spec/manifest-profile.md §4.1), My_Pkg → my-pkg. A name that is not a
// project name wraps format.ErrInvalid.
func (Plugin) EscapeName(name string) (string, error) {
	if !ValidName(name) {
		return "", fmt.Errorf("%w: %q is not a project name", format.ErrInvalid, name)
	}
	return profile.EscapeComponent(Normalize(name)), nil
}

// dist is a distribution file as its name and media type say: wheel or sdist. earlier is the
// variant of a wheel with a build tag as it was written before #726, its tag set; "" for the
// others, whose variant has one form.
type dist struct {
	name, version, variant, earlier, packagetype, mediaType string
}

// describe reads the file name of a distribution: a wheel (.whl) or a source distribution
// (.tar.gz, .zip). mediaType, when not empty, must be the one the name has.
func describe(filename, mediaType string) (dist, error) {
	var d dist
	if isWheel(filename) {
		w, err := ParseWheelFilename(filename)
		if err != nil {
			return dist{}, err
		}
		d = dist{name: w.Name, version: w.Version, variant: w.Variant(), packagetype: PackagetypeWheel, mediaType: profile.MediaTypePypiWheel}
		if w.Build != "" {
			d.earlier = w.TagSet()
		}
	} else {
		s, err := ParseSdistFilename(filename)
		if err != nil {
			return dist{}, err
		}
		d = dist{name: s.Name, version: s.Version, variant: VariantSdist, packagetype: PackagetypeSdist, mediaType: profile.MediaTypePypiSdistTarGz}
		if s.Kind == SdistZip {
			d.mediaType = profile.MediaTypePypiSdistZip
		}
	}
	if mediaType != "" && mediaType != d.mediaType {
		return dist{}, fmt.Errorf("%w: %q is a %s, not %s", format.ErrInvalid, filename, d.mediaType, mediaType)
	}
	return d, nil
}

// metadataIn is what Validate reads of the metadata layer.
type metadataIn struct {
	Name         string `json:"name"`
	Version      string `json:"version"`
	Filename     string `json:"filename"`
	SHA256       string `json:"sha256"`
	Packagetype  string `json:"packagetype"`
	CoreMetadata string `json:"core_metadata"`
}

// Validate checks a pypi manifest (docs/spec/manifest-profile.md): one file layer, a wheel or
// a source distribution, and the metadata layer; the annotations, the file name and the
// metadata layer agree: the name (lajstrom.name is the normalized name of both), the version
// (exactly), the variant (the wheel's build tag and tag set, or sdist; a wheel with a build tag
// written before #726 has its tag set alone, which is read too), the file name, its sha256 and
// packagetype, and lajstrom.pypi.nonstandard is true exactly when the version is not PEP 440.
// The core_metadata of the layer must say the same name and version, and be, byte for byte, the
// METADATA or PKG-INFO inside the distribution (checkArchive), which pip is told it is (PEP 658);
// but for the file of an upload that streams in (format.ErrStreamed), whose core_metadata Build
// took from what the inspector read of the same bytes.
func (Plugin) Validate(ctx context.Context, _ index.Repo, m ocispec.Manifest, fetch format.BlobFetcher) (*format.Entry, error) {
	parsed, err := profile.Validate(m)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", format.ErrInvalid, err)
	}
	if len(parsed.Files) != 1 {
		return nil, fmt.Errorf("%w: a pypi manifest has one file layer, this one %d", format.ErrInvalid, len(parsed.Files))
	}
	f := parsed.Files[0]
	d, err := describe(f.Title, f.MediaType)
	if err != nil {
		return nil, err
	}
	if fetch == nil {
		return nil, fmt.Errorf("pypi: no blob fetcher for the metadata layer %s", parsed.Metadata.Digest)
	}
	md, raw, err := readMetadata(ctx, fetch, *parsed.Metadata)
	if err != nil {
		return nil, err
	}
	a := parsed.Annotations
	if err := check(a, f, d, md); err != nil {
		return nil, err
	}
	if err := checkArchive(ctx, fetch, f, md.CoreMetadata); err != nil {
		return nil, err
	}
	return &format.Entry{Name: a.Name, Version: a.Version, Variant: a.Variant, Attrs: a.Attrs, Metadata: raw,
		Files: []format.FileEntry{{Name: f.Title, MediaType: f.MediaType, Digest: f.Digest, Size: f.Size, Attrs: f.Annotations}}}, nil
}

// check compares the annotations, the file layer, its file name and the metadata layer.
func check(a profile.Annotations, f profile.Layer, d dist, md *metadataIn) error {
	core, err := ParseCoreMetadata([]byte(md.CoreMetadata))
	if err != nil {
		return fmt.Errorf("%w: the core_metadata of the metadata layer: %w", format.ErrInvalid, err)
	}
	nonstandard := ""
	if !IsPEP440(a.Version) {
		nonstandard = "true"
	}
	variant := d.variant
	if d.earlier != "" && a.Variant == d.earlier { // a wheel with a build tag written before #726
		variant = d.earlier
	}
	for _, c := range []struct{ what, got, want string }{
		{profile.AnnotationName + " is not the normalized name", a.Name, Normalize(a.Name)},
		{"the name of the file name", Normalize(d.name), a.Name},
		{"the name of the metadata layer", Normalize(md.Name), a.Name},
		{"the name of core_metadata", core.Name, md.Name},
		{"the version of the file name", d.version, a.Version},
		{"the version of the metadata layer", md.Version, a.Version},
		{"the version of core_metadata", core.Version, md.Version},
		{profile.AnnotationVariant, a.Variant, variant},
		{"the filename of the metadata layer", md.Filename, f.Title},
		{"the sha256 of the metadata layer", md.SHA256, f.Digest.Encoded()},
		{"the packagetype of the metadata layer", md.Packagetype, d.packagetype},
		{profile.AnnotationPypiNonstandard, a.Attrs[profile.AnnotationPypiNonstandard], nonstandard},
	} {
		if c.got != c.want {
			return fmt.Errorf("%w: %s: %q, want %q", format.ErrInvalid, c.what, c.got, c.want)
		}
	}
	for k := range a.Attrs {
		if k != profile.AnnotationPypiNonstandard {
			return fmt.Errorf("%w: %s is not a pypi annotation", format.ErrInvalid, k)
		}
	}
	if f.Digest.Algorithm() != digest.SHA256 {
		return fmt.Errorf("%w: the file layer has a %s digest; pypi clients check sha256", format.ErrInvalid, f.Digest.Algorithm())
	}
	return nil
}

// maxSdistScan bounds the bytes of a .tar.gz sdist read to find its PKG-INFO, which a build
// writes near the start; a gzip stream cannot be read from the middle.
const maxSdistScan = 64 << 20

// checkArchive reads the core metadata inside the distribution f and compares it with core, the
// core_metadata of the metadata layer. A wheel and a zip sdist are read with ReadAt, through
// range requests, when the fetcher's reader has it (ingest's does: the zip's directory and the
// one member, a few small requests), and otherwise spooled to a temporary file for the call; a
// .tar.gz sdist is read as a stream until its PKG-INFO, at most maxSdistScan bytes of it.
func checkArchive(ctx context.Context, fetch format.BlobFetcher, f profile.Layer, core string) error {
	rc, err := fetch(ctx, f.Digest)
	switch {
	case errors.Is(err, format.ErrStreamed):
		return nil
	case err != nil:
		return fmt.Errorf("pypi: fetch %s: %w", f.Title, err)
	}
	defer func() { _ = rc.Close() }()
	var inside []byte
	switch f.MediaType {
	case profile.MediaTypePypiSdistTarGz:
		s, err := ParseSdistFilename(f.Title)
		if err != nil {
			return err
		}
		inside, err = ReadSdistPKGInfo(io.LimitReader(rc, maxSdistScan), f.Size, s)
		if err != nil {
			return fmt.Errorf("%s: %w", f.Title, err)
		}
	default:
		ra, cleanup, err := readerAt(rc)
		if err != nil {
			return err
		}
		defer cleanup()
		if f.MediaType == profile.MediaTypePypiWheel {
			inside, err = ReadWheelMetadata(ra, f.Size)
		} else {
			s, perr := ParseSdistFilename(f.Title)
			if perr != nil {
				return perr
			}
			inside, err = ReadSdistPKGInfo(io.NewSectionReader(ra, 0, f.Size), f.Size, s)
		}
		if err != nil {
			return fmt.Errorf("%s: %w", f.Title, err)
		}
	}
	if string(inside) != core {
		return fmt.Errorf("%w: the core_metadata of the metadata layer is not the core metadata inside %s", format.ErrInvalid, f.Title)
	}
	return nil
}

// readerAt returns rc as an io.ReaderAt: itself when it is one, else its bytes spooled to a
// temporary file, removed by cleanup.
func readerAt(rc io.Reader) (io.ReaderAt, func(), error) {
	if ra, ok := rc.(io.ReaderAt); ok {
		return ra, func() {}, nil
	}
	spool, err := os.CreateTemp("", "lajstrom-pypi-*")
	if err != nil {
		return nil, nil, fmt.Errorf("pypi: spool: %w", err)
	}
	cleanup := func() {
		_ = spool.Close()
		_ = os.Remove(spool.Name())
	}
	if _, err := io.Copy(spool, rc); err != nil {
		cleanup()
		return nil, nil, fmt.Errorf("pypi: spool: %w", err)
	}
	return spool, cleanup, nil
}

// readMetadata reads the metadata layer l through fetch.
func readMetadata(ctx context.Context, fetch format.BlobFetcher, l profile.Layer) (*metadataIn, json.RawMessage, error) {
	if l.Size > maxMetadata {
		return nil, nil, fmt.Errorf("%w: the metadata layer has %d bytes, at most %d", format.ErrInvalid, l.Size, maxMetadata)
	}
	rc, err := fetch(ctx, l.Digest)
	if err != nil {
		return nil, nil, fmt.Errorf("pypi: fetch the metadata layer %s: %w", l.Digest, err)
	}
	defer func() { _ = rc.Close() }()
	b, err := io.ReadAll(io.LimitReader(rc, maxMetadata+1))
	if err != nil {
		return nil, nil, fmt.Errorf("pypi: read the metadata layer %s: %w", l.Digest, err)
	}
	var md metadataIn
	if err := json.Unmarshal(b, &md); err != nil {
		return nil, nil, fmt.Errorf("%w: the metadata layer is not the pypi metadata object: %w", format.ErrInvalid, err)
	}
	return &md, b, nil
}

// Build makes the manifest of one uploaded distribution, a wheel or a source distribution: one
// file layer titled with the file name and the metadata layer, the core metadata of its METADATA
// or PKG-INFO in the JSON form of PEP 566 with filename, sha256, packagetype and core_metadata.
// The name of the file name and of the core metadata must be one project (PEP 503), the versions
// the same string; lajstrom.name is the normalized name, lajstrom.version the version as the core
// metadata writes it, lajstrom.variant the wheel's build tag and tag set or sdist (the earlier
// form of ParamVariant when the upload has it), and a version that is not PEP 440 is accepted
// and flagged lajstrom.pypi.nonstandard. created is now, in whole seconds.
// A file that streamed into the registry, with an inspector (Inspect), has its sha256 from
// ingest and its core metadata from the inspector, which may need the file read back
// (format.ErrReadBack; Open then reads it), and its layer is not among the blobs. Errors wrap
// format.ErrInvalid, but format.ErrReadBack and a failure to read the file.
func (Plugin) Build(_ context.Context, repo index.Repo, in format.Upload) (*format.Built, error) {
	if len(in.Files) != 1 {
		return nil, fmt.Errorf("%w: a pypi upload is one file, got %d", format.ErrInvalid, len(in.Files))
	}
	f := in.Files[0]
	d, err := describe(f.Name, "")
	if err != nil {
		return nil, err
	}
	if d.earlier != "" && in.Params[ParamVariant] == d.earlier {
		d.variant = d.earlier
	}
	var sum digest.Digest
	var size int64
	var coreBytes []byte
	if f.Inspector != nil {
		sum, size, coreBytes, err = streamedUpload(f)
	} else {
		sum, size, coreBytes, err = readUpload(f, d)
	}
	if err != nil {
		return nil, err
	}
	core, err := ParseCoreMetadata(coreBytes)
	if err != nil {
		return nil, fmt.Errorf("%w: %s: %w", format.ErrInvalid, f.Name, err)
	}
	switch {
	case Normalize(core.Name) != Normalize(d.name):
		return nil, fmt.Errorf("%w: %s: the file name is of the project %q, its core metadata of %q", format.ErrInvalid, f.Name, d.name, core.Name)
	case core.Version != d.version:
		return nil, fmt.Errorf("%w: %s: the file name has the version %q, its core metadata %q", format.ErrInvalid, f.Name, d.version, core.Version)
	}
	fields := core.Fields
	fields[FieldFilename], fields[FieldSHA256], fields[FieldPackagetype], fields[FieldCoreMetadata] =
		f.Name, sum.Encoded(), d.packagetype, string(coreBytes)
	md, err := json.Marshal(fields)
	if err != nil {
		return nil, fmt.Errorf("pypi: the metadata of %s: %w", f.Name, err)
	}
	var attrs map[string]string
	if !IsPEP440(core.Version) {
		attrs = map[string]string{profile.AnnotationPypiNonstandard: "true"}
	}
	name := Normalize(core.Name)
	file := profile.Layer{MediaType: d.mediaType, Digest: sum, Size: size, Title: f.Name}
	m, err := profile.Build(profile.Annotations{
		Format: Name, Repo: repo.Name, Name: name, Version: core.Version, Variant: d.variant,
		Created: time.Now().UTC().Truncate(time.Second), UploadedBy: in.Params[ParamUploadedBy], Attrs: attrs,
	}, []profile.Layer{file}, &profile.Layer{Digest: digest.FromBytes(md), Size: int64(len(md))})
	if err != nil {
		return nil, fmt.Errorf("%w: %w", format.ErrInvalid, err)
	}
	pkgPath := profile.EscapeComponent(name)
	blobs := []format.BlobSource{bytesBlob(profile.EmptyConfig, ocispec.DescriptorEmptyJSON.Data)}
	if f.Inspector == nil { // a streamed file is in its upload session, which ingest commits
		blobs = append(blobs, format.BlobSource{Descriptor: m.Layers[0], Open: func(context.Context) (io.ReadCloser, error) { return f.Open() }})
	}
	return &format.Built{
		Manifest: m,
		Blobs:    append(blobs, bytesBlob(m.Layers[1], md)),
		Entry: format.Entry{Name: name, Version: core.Version, Variant: d.variant, Attrs: attrs, Metadata: md,
			Files: []format.FileEntry{{Name: f.Name, MediaType: d.mediaType, Digest: sum, Size: size}}},
		Tag:      profile.Tag(core.Version, d.variant),
		RepoPath: profile.RepositoryPath("", Name, repo.Name, pkgPath),
	}, nil
}

// readUpload reads an uploaded distribution once: its sha256 and size, and its METADATA or
// PKG-INFO. A wheel or a zip sdist needs random access: when the upload's reader has none, the
// bytes are spooled to a temporary file for the duration of the call. A size other than the
// one the upload declares wraps format.ErrInvalid.
func readUpload(f format.UploadedFile, d dist) (digest.Digest, int64, []byte, error) {
	rc, err := f.Open()
	if err != nil {
		return "", 0, nil, fmt.Errorf("pypi: open %s: %w", f.Name, err)
	}
	defer func() { _ = rc.Close() }()
	spool, err := os.CreateTemp("", "lajstrom-pypi-*")
	if err != nil {
		return "", 0, nil, fmt.Errorf("pypi: spool %s: %w", f.Name, err)
	}
	defer func() {
		_ = spool.Close()
		_ = os.Remove(spool.Name())
	}()
	h := digest.SHA256.Digester()
	size, err := io.Copy(io.MultiWriter(spool, h.Hash()), rc)
	switch {
	case err != nil:
		return "", 0, nil, fmt.Errorf("pypi: read %s: %w", f.Name, err)
	case f.Size >= 0 && size != f.Size:
		return "", 0, nil, fmt.Errorf("%w: %s has %d bytes, the upload says %d", format.ErrInvalid, f.Name, size, f.Size)
	}
	var core []byte
	if d.packagetype == PackagetypeWheel {
		core, err = ReadWheelMetadata(spool, size)
	} else {
		s, perr := ParseSdistFilename(f.Name)
		if perr != nil {
			return "", 0, nil, perr
		}
		if _, err := spool.Seek(0, io.SeekStart); err != nil {
			return "", 0, nil, fmt.Errorf("pypi: spool %s: %w", f.Name, err)
		}
		core, err = ReadSdistPKGInfo(spool, size, s)
	}
	if err != nil {
		return "", 0, nil, fmt.Errorf("%s: %w", f.Name, err)
	}
	return h.Digest(), size, core, nil
}

// bytesBlob is a blob source over bytes held in memory.
func bytesBlob(desc ocispec.Descriptor, b []byte) format.BlobSource {
	return format.BlobSource{Descriptor: desc, Open: func(context.Context) (io.ReadCloser, error) {
		return io.NopCloser(strings.NewReader(string(b))), nil
	}}
}
