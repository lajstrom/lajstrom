package ingest_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/lajstrom/lajstrom/internal/auth"
	"github.com/lajstrom/lajstrom/internal/format"
	"github.com/lajstrom/lajstrom/internal/format/formattest"
	"github.com/lajstrom/lajstrom/internal/index"
	"github.com/lajstrom/lajstrom/internal/index/sqlite"
	"github.com/lajstrom/lajstrom/internal/ingest"
	"github.com/lajstrom/lajstrom/internal/oci/profile"
	"github.com/lajstrom/lajstrom/internal/provider"
	"github.com/lajstrom/lajstrom/internal/provider/providertest"
)

// rawPlugin is a raw-like plugin for the tests, looser than internal/format/raw (several files,
// a version from the parameters), so that the pipeline is tested on its own. Build makes
// one raw.file layer per uploaded file, digested while it streams through once, and a metadata
// layer {"files": [{"name", "size"}]}, with the annotations from the parameters name and
// version. Validate checks the titles and returns the entry. EscapeName escapes each path
// component (docs/spec/manifest-profile.md §4.1).
type rawPlugin struct {
	formattest.Fake
}

func newRawPlugin() *rawPlugin {
	return &rawPlugin{Fake: formattest.Fake{FormatName: "raw", BuildFunc: buildRaw, ValidateFunc: validateRaw}}
}

// EscapeName escapes each component of a /-separated path with profile.EscapeComponent.
func (*rawPlugin) EscapeName(name string) (string, error) { return escapeRaw(name) }

func escapeRaw(name string) (string, error) {
	parts := strings.Split(name, "/")
	for i, c := range parts {
		if c == "" {
			return "", fmt.Errorf("%w: path %q has an empty component", format.ErrInvalid, name)
		}
		parts[i] = profile.EscapeComponent(c)
	}
	return strings.Join(parts, "/"), nil
}

// created is org.opencontainers.image.created of every manifest the plugin builds: fixed, so
// that the same upload gives the same manifest bytes.
var created = time.Date(2026, 11, 7, 9, 0, 0, 0, time.UTC)

// rawMetadata is the content of the metadata layer the plugin builds.
type rawMetadata struct {
	Files []rawMetadataFile `json:"files"`
}

type rawMetadataFile struct {
	Name string `json:"name"`
	Size int64  `json:"size"`
}

func buildRaw(ctx context.Context, repo index.Repo, up format.Upload) (*format.Built, error) {
	name, version := up.Params["name"], up.Params["version"]
	path, err := escapeRaw(name)
	if err != nil {
		return nil, err
	}
	var (
		layers []profile.Layer
		blobs  = []format.BlobSource{blobOf(profile.EmptyConfig, ocispec.DescriptorEmptyJSON.Data)}
		meta   rawMetadata
	)
	for _, f := range up.Files {
		d, size, err := digestFile(f)
		if err != nil {
			return nil, err
		}
		layers = append(layers, profile.Layer{MediaType: profile.MediaTypeRawFile, Digest: d, Size: size, Title: f.Name})
		blobs = append(blobs, format.BlobSource{
			Descriptor: ocispec.Descriptor{MediaType: profile.MediaTypeRawFile, Digest: d, Size: size},
			Open:       func(context.Context) (io.ReadCloser, error) { return f.Open() },
		})
		meta.Files = append(meta.Files, rawMetadataFile{Name: f.Name, Size: size})
	}
	md, err := json.Marshal(meta)
	if err != nil {
		return nil, err
	}
	ann := profile.Annotations{Format: repo.Format, Repo: repo.Name, Name: name, Version: version, Created: created,
		Attrs: map[string]string{profile.AnnotationRawPath: name}}
	m, err := profile.Build(ann, layers, &profile.Layer{Digest: digest.FromBytes(md), Size: int64(len(md))})
	if err != nil {
		return nil, fmt.Errorf("%w: %w", format.ErrInvalid, err)
	}
	blobs = append(blobs, blobOf(m.Layers[len(m.Layers)-1], md))
	entry, err := validateRaw(ctx, repo, m, nil)
	if err != nil {
		return nil, err
	}
	entry.Metadata = md
	return &format.Built{Manifest: m, Blobs: blobs, Entry: *entry, Tag: profile.Tag(version, ""),
		RepoPath: profile.RepositoryPath("", repo.Format, repo.Name, path)}, nil
}

// buildRawAt is buildRaw with another org.opencontainers.image.created: the manifest of the
// same files differs by that annotation alone.
func buildRawAt(at time.Time) func(context.Context, index.Repo, format.Upload) (*format.Built, error) {
	return func(ctx context.Context, repo index.Repo, up format.Upload) (*format.Built, error) {
		b, err := buildRaw(ctx, repo, up)
		if err != nil {
			return nil, err
		}
		b.Manifest.Annotations[ocispec.AnnotationCreated] = at.UTC().Format(time.RFC3339Nano)
		return b, nil
	}
}

// blobOf is a BlobSource of bytes in memory.
func blobOf(desc ocispec.Descriptor, b []byte) format.BlobSource {
	return format.BlobSource{
		Descriptor: ocispec.Descriptor{MediaType: desc.MediaType, Digest: desc.Digest, Size: desc.Size},
		Open:       func(context.Context) (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(b)), nil },
	}
}

// digestFile streams f once through sha256 and returns its digest and size.
func digestFile(f format.UploadedFile) (digest.Digest, int64, error) {
	rc, err := f.Open()
	if err != nil {
		return "", 0, err
	}
	defer func() { _ = rc.Close() }()
	d := digest.Canonical.Digester()
	n, err := io.Copy(d.Hash(), rc)
	if err != nil {
		return "", 0, err
	}
	return d.Digest(), n, nil
}

func validateRaw(_ context.Context, _ index.Repo, m ocispec.Manifest, _ format.BlobFetcher) (*format.Entry, error) {
	parsed, err := profile.Validate(m)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", format.ErrInvalid, err)
	}
	a := parsed.Annotations
	e := &format.Entry{Name: a.Name, Version: a.Version, Variant: a.Variant, Attrs: a.Attrs}
	for _, l := range parsed.Files {
		if strings.TrimSpace(l.Title) == "" {
			return nil, fmt.Errorf("%w: a file layer without a title", format.ErrInvalid)
		}
		e.Files = append(e.Files, format.FileEntry{Name: l.Title, MediaType: l.MediaType, Digest: l.Digest, Size: l.Size, Attrs: l.Annotations})
	}
	return e, nil
}

// now is the pipeline's clock in the tests.
var now = time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)

// alice publishes in the tests.
var alice = auth.Principal{Kind: auth.KindUser, Name: "alice"}

// env is a pipeline over a fake provider and the real SQLite store in memory, with the repo
// raw/scratch on the registry "local".
type env struct {
	st     *sqlite.Store
	prov   *providertest.Fake
	plugin *rawPlugin
	pipe   *ingest.Pipeline
	repo   index.Repo
}

// memoryDBs numbers the in-memory databases, so that every test gets its own.
var memoryDBs atomic.Int64

func newEnv(t *testing.T) *env {
	t.Helper()
	ctx := t.Context()
	st, err := sqlite.Open(ctx, fmt.Sprintf("file:ingest-%d?mode=memory&cache=shared", memoryDBs.Add(1)))
	must(t, err)
	t.Cleanup(func() { _ = st.Close() })
	must(t, st.Migrate(ctx))
	e := &env{st: st, prov: &providertest.Fake{RegistryName: "local"}, plugin: newRawPlugin()}
	e.repo = index.Repo{Format: "raw", Name: "scratch", Registry: "local", Prefix: "raw/scratch", Kind: "hosted"}
	must(t, st.CreateRepo(ctx, &e.repo))
	e.pipe = ingest.New(e.options(e.prov))
	return e
}

// options are the Options of the env's pipeline with prov as the registry "local".
func (e *env) options(prov provider.Provider) ingest.Options {
	return ingest.Options{
		Providers: provider.Map{"local": prov},
		Index:     e.st,
		Plugins: func(name string) (format.Plugin, bool) {
			return e.plugin, name == "raw"
		},
		Now: func() time.Time { return now },
		Log: slog.New(slog.DiscardHandler),
	}
}

// file is an uploaded file in memory.
type file struct{ name, content string }

// upload is the Upload of files as package name at version.
func upload(name, version string, files ...file) format.Upload {
	up := format.Upload{Params: map[string]string{"name": name, "version": version}}
	for _, f := range files {
		up.Files = append(up.Files, format.UploadedFile{Name: f.name, Size: int64(len(f.content)),
			Open: func() (io.ReadCloser, error) { return io.NopCloser(strings.NewReader(f.content)), nil }})
	}
	return up
}

// prepare builds the manifest of up with the plugin and pushes its blobs to the fake, as a
// client does before PUT manifest. It returns the manifest, its bytes and the package path.
func (e *env) prepare(t *testing.T, up format.Upload) (built *format.Built, raw []byte, pkgPath string) {
	t.Helper()
	ctx := t.Context()
	built, err := e.plugin.Build(ctx, e.repo, up)
	must(t, err)
	raw, _, err = profile.Encode(built.Manifest)
	must(t, err)
	rr, err := e.prov.Repository(ctx, built.RepoPath)
	must(t, err)
	for _, b := range built.Blobs {
		rc, err := b.Open(ctx)
		must(t, err)
		must(t, rr.Push(ctx, b.Descriptor, rc))
		_ = rc.Close()
	}
	return built, raw, strings.TrimPrefix(built.RepoPath, "raw/scratch/")
}

// ingest stores what prepare returned.
func (e *env) ingest(t *testing.T, built *format.Built, raw []byte, pkgPath string) (*ingest.Result, error) {
	t.Helper()
	return e.pipe.Ingest(t.Context(), e.repo, pkgPath, built.Tag, built.Manifest, raw, alice)
}

// rows are the index rows of a repo.
type rows struct {
	Packages []index.Package
	Versions []index.Version
	Files    [][]index.File
	OCIRepos []index.OCIRepo
	Audit    []index.AuditEvent
}

func (e *env) rows(t *testing.T) rows {
	t.Helper()
	ctx := t.Context()
	var r rows
	var err error
	r.Packages, err = e.st.ListPackages(ctx, e.repo.ID, "")
	must(t, err)
	r.Versions, r.Files = []index.Version{}, [][]index.File{}
	for _, p := range r.Packages {
		vs, err := e.st.ListVersions(ctx, p.ID)
		must(t, err)
		for _, v := range vs {
			fs, err := e.st.ListFiles(ctx, v.ID)
			must(t, err)
			r.Versions, r.Files = append(r.Versions, v), append(r.Files, fs)
		}
	}
	r.OCIRepos, err = e.st.ListOCIRepos(ctx, e.repo.ID)
	must(t, err)
	r.Audit, err = e.st.ListAudit(ctx, index.AuditFilter{})
	must(t, err)
	return r
}

// noClaim fails the test when a claim on tag of package name is left.
func (e *env) noClaim(t *testing.T, name, tag string) {
	t.Helper()
	if c, err := e.st.GetTagClaim(t.Context(), e.repo.ID, name, tag); err == nil {
		t.Errorf("claim left: %+v", c)
	}
}

// tagged returns the digest the fake has under tag in the repository name, or "".
func (e *env) tagged(t *testing.T, name, tag string) digest.Digest {
	t.Helper()
	rr, err := e.prov.Repository(t.Context(), name)
	must(t, err)
	desc, err := rr.Resolve(t.Context(), tag)
	if err != nil {
		return ""
	}
	return desc.Digest
}

// pushRefs returns the PushReference calls of the fake for tag.
func (e *env) pushRefs(tag string) []providertest.PushRecord {
	var out []providertest.PushRecord
	for _, r := range e.prov.PushReferences() {
		if r.Tag == tag {
			out = append(out, r)
		}
	}
	return out
}

// must stops the test on an error.
func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// same fails the test when got differs from want.
func same(t *testing.T, what string, want, got any) {
	t.Helper()
	if d := cmp.Diff(want, got); d != "" {
		t.Errorf("%s: (-want +got)\n%s", what, d)
	}
}
