package npm

import (
	"context"
	"crypto/sha1" //nolint:gosec // npm's dist.shasum is a sha1, checked, not trusted
	"crypto/sha512"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/lajstrom/lajstrom/internal/format"
	"github.com/lajstrom/lajstrom/internal/index"
	"github.com/lajstrom/lajstrom/internal/oci/profile"
)

// metadata is the content of the npm metadata layer (docs/spec/manifest-profile.md §2): the
// version document as manifest, its readme, and the facts of the tarball.
type metadata struct {
	Name             string          `json:"name"`
	Version          string          `json:"version"`
	Manifest         json.RawMessage `json:"manifest"`
	Readme           string          `json:"readme,omitempty"`
	Filename         string          `json:"filename"`
	Size             int64           `json:"size"`
	Integrity        string          `json:"integrity"`
	Shasum           string          `json:"shasum"`
	SHA256           string          `json:"sha256"`
	HasShrinkwrap    bool            `json:"hasShrinkwrap"`
	HasInstallScript bool            `json:"hasInstallScript"`
}

// facts are what one read of a tarball finds: its contents and its digests.
type facts struct {
	tarball   *Tarball
	digest    digest.Digest // sha256, the layer's
	integrity string        // sha512-<base64>
	shasum    string        // sha1, lowercase hex
	size      int64
}

// readFacts reads a tarball from r once, whole, with ReadTarball, and digests every byte of it.
func readFacts(r io.Reader) (*facts, error) {
	s256, s512, s1 := digest.SHA256.Digester(), sha512.New(), sha1.New() //nolint:gosec // see the import
	counted := &counter{r: io.TeeReader(r, io.MultiWriter(s256.Hash(), s512, s1))}
	tb, err := ReadTarball(counted)
	if err != nil {
		return nil, err
	}
	if _, err := io.Copy(io.Discard, counted); err != nil {
		return nil, fmt.Errorf("npm: read the tarball: %w", err)
	}
	return &facts{tarball: tb, digest: s256.Digest(), size: counted.n,
		integrity: "sha512-" + base64.StdEncoding.EncodeToString(s512.Sum(nil)), shasum: hex.EncodeToString(s1.Sum(nil))}, nil
}

// counter counts the bytes read through it.
type counter struct {
	r io.Reader
	n int64
}

func (c *counter) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}

// layer returns the tarball layer of the facts for the package name at version.
func (fc *facts) layer(name, version string) profile.Layer {
	return profile.Layer{MediaType: profile.MediaTypeNpmTarball, Digest: fc.digest, Size: fc.size, Title: Filename(name, version),
		Annotations: map[string]string{profile.AnnotationNpmIntegrity: fc.integrity, profile.AnnotationNpmShasum: fc.shasum}}
}

// check compares the document of a version with the tarball's package.json: the name and the
// version npm takes, the same in both, and what an install runs (checkInstall), both normalised
// as npm's CLI normalises the document it sends (Tarball.normalise).
func (fc *facts) check(doc *PackageJSON) error {
	pkg := fc.tarball.PackageJSON
	if err := CheckName(pkg.Name); err != nil {
		return fmt.Errorf("package.json: %w", err)
	}
	if err := CheckVersion(pkg.Version); err != nil {
		return fmt.Errorf("package.json: %w", err)
	}
	if doc.Name != pkg.Name || doc.Version != pkg.Version {
		return fmt.Errorf("%w: the version document is of %s@%s, the tarball's package.json of %s@%s",
			format.ErrInvalid, doc.Name, doc.Version, pkg.Name, pkg.Version)
	}
	doc, err := fc.tarball.normalise(doc)
	if err != nil {
		return err
	}
	if pkg, err = fc.tarball.normalise(pkg); err != nil {
		return err
	}
	return checkInstall(doc, pkg)
}

// Build makes the manifest of one version from its tarball, the one file of the upload, and the
// version document of ParamDocument (the tarball's package.json without it): the tarball layer,
// titled <name without the scope>-<version>.tgz with lajstrom.npm.integrity and .shasum, and the
// metadata layer with the document as manifest, without dist, readme and the keys that begin
// with _. The document must agree with package.json (facts.check). lajstrom.name and .version
// are package.json's, created is now in whole seconds. A tarball that streamed into the
// registry, with an inspector (Inspect), is described from what the inspector found and the
// sha256 of ingest, and its layer is not among the blobs. Errors wrap format.ErrInvalid, except
// a failure to read the file.
func (Plugin) Build(_ context.Context, repo index.Repo, in format.Upload) (*format.Built, error) {
	if len(in.Files) != 1 {
		return nil, fmt.Errorf("%w: an npm upload is one tarball, got %d files", format.ErrInvalid, len(in.Files))
	}
	f := in.Files[0]
	fc, err := uploadedFacts(f)
	switch {
	case err != nil:
		return nil, err
	case f.Size >= 0 && fc.size != f.Size:
		return nil, fmt.Errorf("%w: the tarball has %d bytes, the upload says %d", format.ErrInvalid, fc.size, f.Size)
	}
	doc := fc.tarball.PackageJSON
	if raw := in.Params[ParamDocument]; raw != "" {
		if doc, err = ParsePackageJSON([]byte(raw)); err != nil {
			return nil, fmt.Errorf("the version document: %w", err)
		}
	}
	if err := fc.check(doc); err != nil {
		return nil, err
	}
	md, err := fc.metadata(doc, in.Params[ParamReadme])
	if err != nil {
		return nil, err
	}
	name, version := doc.Name, doc.Version
	tarball := fc.layer(name, version)
	m, err := profile.Build(profile.Annotations{Format: Name, Repo: repo.Name, Name: name, Version: version,
		Created: time.Now().UTC().Truncate(time.Second), UploadedBy: in.Params[ParamUploadedBy]},
		[]profile.Layer{tarball}, &profile.Layer{Digest: digest.FromBytes(md), Size: int64(len(md))})
	if err != nil {
		return nil, fmt.Errorf("%w: %w", format.ErrInvalid, err)
	}
	blobs := []format.BlobSource{bytesBlob(profile.EmptyConfig, ocispec.DescriptorEmptyJSON.Data)}
	if f.Inspector == nil { // a streamed tarball is in its upload session, which ingest commits
		blobs = append(blobs, format.BlobSource{Descriptor: m.Layers[0], Open: func(context.Context) (io.ReadCloser, error) { return f.Open() }})
	}
	return &format.Built{
		Manifest: m,
		Blobs:    append(blobs, bytesBlob(m.Layers[1], md)),
		Entry: format.Entry{Name: name, Version: version, Metadata: md, Files: []format.FileEntry{{Name: tarball.Title,
			MediaType: tarball.MediaType, Digest: tarball.Digest, Size: tarball.Size, Attrs: tarball.Annotations}}},
		Tag:      profile.Tag(version, ""),
		RepoPath: profile.RepositoryPath("", Name, repo.Name, packagePath(name)),
	}, nil
}

// uploadedFacts returns the facts of the tarball f: those its inspector found, for a tarball
// that streamed into the registry, else those of a read of it.
func uploadedFacts(f format.UploadedFile) (*facts, error) {
	if f.Inspector != nil {
		return streamedFacts(f)
	}
	rc, err := f.Open()
	if err != nil {
		return nil, fmt.Errorf("npm: open %s: %w", f.Name, err)
	}
	defer func() { _ = rc.Close() }()
	return readFacts(rc)
}

// metadata returns the metadata layer of the version doc of the tarball: the document without
// dist, readme and the keys that begin with _, its readme or else readme, the facts.
func (fc *facts) metadata(doc *PackageJSON, readme string) ([]byte, error) {
	manifest := make(map[string]json.RawMessage, len(doc.Fields))
	for k, v := range doc.Fields {
		if k != "dist" && k != "readme" && !strings.HasPrefix(k, "_") {
			manifest[k] = v
		}
	}
	var own string
	if json.Unmarshal(doc.Fields["readme"], &own) == nil && own != "" {
		readme = own
	}
	if readme == readmePlaceholder {
		readme = ""
	}
	m, err := json.Marshal(manifest)
	if err != nil {
		return nil, fmt.Errorf("%w: the version document: %w", format.ErrInvalid, err)
	}
	b, err := json.Marshal(metadata{Name: doc.Name, Version: doc.Version, Manifest: m, Readme: readme,
		Filename: Filename(doc.Name, doc.Version), Size: fc.size, Integrity: fc.integrity, Shasum: fc.shasum,
		SHA256: fc.digest.Encoded(), HasShrinkwrap: fc.tarball.HasShrinkwrap, HasInstallScript: fc.tarball.HasInstallScript()})
	if err != nil {
		return nil, fmt.Errorf("npm: the metadata of %s@%s: %w", doc.Name, doc.Version, err)
	}
	if len(b) > maxMetadata {
		return nil, fmt.Errorf("%w: the metadata of %s@%s, the version document and its readme, is %d bytes, at most %d",
			format.ErrInvalid, doc.Name, doc.Version, len(b), maxMetadata)
	}
	return b, nil
}

// bytesBlob is a blob source over bytes held in memory.
func bytesBlob(desc ocispec.Descriptor, b []byte) format.BlobSource {
	return format.BlobSource{Descriptor: desc, Open: func(context.Context) (io.ReadCloser, error) {
		return io.NopCloser(strings.NewReader(string(b))), nil
	}}
}
