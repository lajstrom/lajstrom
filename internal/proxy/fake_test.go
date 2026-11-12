package proxy_test

import (
	"bytes"
	"context"
	"crypto/sha1" //nolint:gosec // an upstream's sha1, the weakest checksum it may give
	"crypto/sha256"
	"crypto/sha512"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/lajstrom/lajstrom/internal/config"
	"github.com/lajstrom/lajstrom/internal/format"
	"github.com/lajstrom/lajstrom/internal/format/formattest"
	"github.com/lajstrom/lajstrom/internal/index"
	"github.com/lajstrom/lajstrom/internal/index/sqlite"
	"github.com/lajstrom/lajstrom/internal/ingest"
	"github.com/lajstrom/lajstrom/internal/oci/profile"
	"github.com/lajstrom/lajstrom/internal/provider"
	"github.com/lajstrom/lajstrom/internal/provider/providertest"
	"github.com/lajstrom/lajstrom/internal/proxy"
)

// pkgPlugin is the format of these tests, a plugin with the hooks of a proxy (format.Proxier).
// Its manifests are of the profile's raw type, named by the parameters name and version as an
// upload carries them, one file layer per file; Validate refuses a file whose bytes start with
// "evil". Its upstream's document of a name is {"name", "versions": [{"version", "file", "url",
// "sha512", "sha256", "sha1"}]}, and a file is <name>/-/<name>-<version>.tgz below the repo.
type pkgPlugin struct {
	formattest.Fake
	broken bool // ParseMetadata refuses every document
}

func newPlugin() *pkgPlugin {
	p := &pkgPlugin{}
	p.Fake = formattest.Fake{FormatName: "raw", BuildFunc: p.build, ValidateFunc: p.validate}
	return p
}

// created is org.opencontainers.image.created of every manifest the plugin builds.
var created = time.Date(2026, 11, 10, 9, 0, 0, 0, time.UTC)

// docKind is the one document kind of the plugin.
const docKind = "index"

// EscapeName escapes a name of one component.
func (*pkgPlugin) EscapeName(name string) (string, error) {
	if name == "" || strings.Contains(name, "/") {
		return "", fmt.Errorf("%w: name %q", format.ErrInvalid, name)
	}
	return profile.EscapeComponent(name), nil
}

func (p *pkgPlugin) build(_ context.Context, repo index.Repo, up format.Upload) (*format.Built, error) {
	name, version := up.Params["name"], up.Params["version"]
	path, err := p.EscapeName(name)
	if err != nil {
		return nil, err
	}
	blobs := []format.BlobSource{bytesBlob(profile.EmptyConfig, ocispec.DescriptorEmptyJSON.Data)}
	var layers []profile.Layer
	for _, f := range up.Files {
		rc, err := f.Open()
		if err != nil {
			return nil, err
		}
		b, err := io.ReadAll(rc)
		_ = rc.Close()
		if err != nil {
			return nil, err
		}
		l := profile.Layer{MediaType: profile.MediaTypeRawFile, Digest: digest.FromBytes(b), Size: int64(len(b)), Title: f.Name}
		layers = append(layers, l)
		blobs = append(blobs, bytesBlob(ocispec.Descriptor{MediaType: l.MediaType, Digest: l.Digest, Size: l.Size}, b))
	}
	m, err := profile.Build(profile.Annotations{Format: repo.Format, Repo: repo.Name, Name: name, Version: version, Created: created,
		UploadedBy: up.Params["uploaded-by"], Attrs: map[string]string{profile.AnnotationRawPath: name}}, layers, nil)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", format.ErrInvalid, err)
	}
	entry, err := entryOf(m)
	if err != nil {
		return nil, err
	}
	return &format.Built{Manifest: m, Blobs: blobs, Entry: *entry, Tag: profile.Tag(version, ""),
		RepoPath: profile.RepositoryPath("", repo.Format, repo.Name, path)}, nil
}

func (p *pkgPlugin) validate(ctx context.Context, _ index.Repo, m ocispec.Manifest, fetch format.BlobFetcher) (*format.Entry, error) {
	entry, err := entryOf(m)
	if err != nil {
		return nil, err
	}
	for _, f := range entry.Files {
		rc, err := fetch(ctx, f.Digest)
		if err != nil {
			return nil, err
		}
		head := make([]byte, 4)
		n, _ := io.ReadFull(rc, head)
		_ = rc.Close()
		if string(head[:n]) == "evil" {
			return nil, fmt.Errorf("%w: %s is not a package", format.ErrInvalid, f.Name)
		}
	}
	return entry, nil
}

func entryOf(m ocispec.Manifest) (*format.Entry, error) {
	parsed, err := profile.Validate(m)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", format.ErrInvalid, err)
	}
	a := parsed.Annotations
	e := &format.Entry{Name: a.Name, Version: a.Version, Attrs: a.Attrs}
	for _, l := range parsed.Files {
		e.Files = append(e.Files, format.FileEntry{Name: l.Title, MediaType: l.MediaType, Digest: l.Digest, Size: l.Size})
	}
	return e, nil
}

func bytesBlob(d ocispec.Descriptor, b []byte) format.BlobSource {
	return format.BlobSource{Descriptor: ocispec.Descriptor{MediaType: d.MediaType, Digest: d.Digest, Size: d.Size},
		Open: func(context.Context) (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(b)), nil }}
}

// upstreamDoc is the document of a name on the fake upstream.
type upstreamDoc struct {
	Name     string            `json:"name"`
	Versions []upstreamVersion `json:"versions"`
	Tags     map[string]string `json:"tags,omitempty"` // tag → version: the pointers (Pointed)
}

type upstreamVersion struct {
	Version string `json:"version"`
	File    string `json:"file"`
	URL     string `json:"url"`
	SHA512  string `json:"sha512,omitempty"`
	SHA256  string `json:"sha256,omitempty"`
	SHA1    string `json:"sha1,omitempty"`
	Time    string `json:"time,omitempty"` // RFC 3339: the entry's Published
}

// Key is the name in lower case.
func (*pkgPlugin) Key(name string) (string, error) {
	if name == "" {
		return "", fmt.Errorf("%w: an empty name", format.ErrInvalid)
	}
	return strings.ToLower(name), nil
}

// MetadataRequest asks meta/<name in lower case>, with ?kind=other for otherKind.
func (*pkgPlugin) MetadataRequest(name, kind string) (format.UpstreamRequest, error) {
	path := "meta/" + url.PathEscape(strings.ToLower(name))
	if kind == otherKind {
		path += "?kind=" + otherKind
	}
	return format.UpstreamRequest{Path: path, Accept: "application/json"}, nil
}

// ParseMetadata reads the document as it is and keeps it whole.
func (p *pkgPlugin) ParseMetadata(name, _ string, body []byte) (*format.UpstreamIndex, error) {
	var d upstreamDoc
	if err := json.Unmarshal(body, &d); err != nil || p.broken {
		return nil, fmt.Errorf("%w: %w", format.ErrInvalid, err)
	}
	out := &format.UpstreamIndex{Keep: body, ContentType: "application/json"}
	for _, v := range d.Versions {
		sums := map[string]string{}
		for alg, s := range map[string]string{"sha512": v.SHA512, "sha256": v.SHA256, "sha1": v.SHA1} {
			if s != "" {
				sums[alg] = s
			}
		}
		published, _ := time.Parse(time.RFC3339, v.Time)
		out.Entries = append(out.Entries, format.UpstreamEntry{Version: v.Version, Published: published,
			Files:  []format.UpstreamFile{{Name: v.File, URL: v.URL, Checksums: sums}},
			Params: map[string]string{"name": name, "version": v.Version}})
	}
	return out, nil
}

// FileRequest maps <name>/-/<name>-<version>.tgz.
func (*pkgPlugin) FileRequest(path string) (format.ArtifactRef, error) {
	name, file, ok := strings.Cut(path, "/-/")
	version, vok := strings.CutPrefix(strings.TrimSuffix(file, ".tgz"), name+"-")
	if !ok || !vok || version == "" || !strings.HasSuffix(file, ".tgz") {
		return format.ArtifactRef{}, fmt.Errorf("%w: %s is no file", index.ErrNotFound, path)
	}
	return format.ArtifactRef{Name: name, Version: version, File: file, Kind: docKind}, nil
}

// RenderProxy lists version → "<link> <sha256>": the upstream's versions while there is a
// document, with the held bytes' sha256 where the repo holds the version; the held versions
// alone without one.
func (p *pkgPlugin) RenderProxy(ctx context.Context, repo index.Repo, name, _ string, kept []byte, st index.Reader, base string) (format.Document, error) {
	out := map[string]string{}
	if kept != nil {
		var d upstreamDoc
		if err := json.Unmarshal(kept, &d); err != nil {
			return format.Document{}, err
		}
		for _, v := range d.Versions {
			out[v.Version] = base + name + "/-/" + v.File + " " + v.SHA256
		}
	}
	path, _ := p.EscapeName(name)
	if pkg, err := st.GetPackageByNameOCI(ctx, repo.ID, path); err == nil {
		vs, err := st.ListVersions(ctx, pkg.ID)
		if err != nil {
			return format.Document{}, err
		}
		for _, v := range vs {
			if _, listed := out[v.Version]; kept != nil && !listed {
				continue // the upstream's listing is the listing
			}
			files, err := st.ListFiles(ctx, v.ID)
			if err != nil || len(files) == 0 {
				return format.Document{}, fmt.Errorf("files of %s: %d, %w", v.Version, len(files), err)
			}
			out[v.Version] = base + name + "/-/" + files[0].Name + " " + files[0].Digest.Encoded()
		}
	}
	b, err := json.Marshal(out)
	return format.Document{Kind: docKind, Path: name, ContentType: "application/json", Content: b}, err
}

// RenderIndex renders a hosted repo's document of a name, scope the name, as RenderProxy does
// without a kept document, with its links under https://art.test/raw/<repo>/; nothing for a
// name the repo does not hold.
func (p *pkgPlugin) RenderIndex(ctx context.Context, repo index.Repo, scope format.IndexScope, st index.Reader) ([]format.Document, error) {
	path, err := p.EscapeName(string(scope))
	if err != nil {
		return nil, nil
	}
	if _, err := st.GetPackageByNameOCI(ctx, repo.ID, path); errors.Is(err, index.ErrNotFound) {
		return nil, nil
	}
	doc, err := p.RenderProxy(ctx, repo, string(scope), docKind, nil, st, "https://art.test/raw/"+repo.Name+"/")
	return []format.Document{doc}, err
}

// MergeGroup merges the members' listings, version → "<link> <sha256>": the first member's
// entry of a version, each link moved below base.
func (p *pkgPlugin) MergeGroup(_ context.Context, _ index.Repo, name, _ string, docs []format.MemberDocument, base string) (format.Document, error) {
	out := map[string]string{}
	for _, d := range docs {
		var listing map[string]string
		if err := json.Unmarshal(d.Content, &listing); err != nil {
			return format.Document{}, err
		}
		for v, entry := range listing {
			if _, ok := out[v]; ok {
				continue
			}
			link, sum, _ := strings.Cut(entry, " ")
			out[v] = base + name + "/-/" + link[strings.LastIndex(link, "/")+1:] + " " + sum + " " + d.Repo.Name
		}
	}
	b, err := json.Marshal(out)
	return format.Document{Kind: docKind, Path: name, ContentType: "application/json", Content: b}, err
}

// otherKind is a second kind a prefetch reads: the upstream answers it as it answers docKind.
const otherKind = "other"

// PrefetchKinds are the kind of the plugin and otherKind.
func (*pkgPlugin) PrefetchKinds() []string { return []string{docKind, otherKind} }

// Pointed returns the versions of the document's tags.
func (*pkgPlugin) Pointed(_, _ string, kept []byte) ([]string, error) {
	var d upstreamDoc
	if err := json.Unmarshal(kept, &d); err != nil {
		return nil, fmt.Errorf("%w: %w", format.ErrInvalid, err)
	}
	var out []string
	for _, v := range d.Tags {
		out = append(out, v)
	}
	return out, nil
}

var (
	_ format.Grouper    = (*pkgPlugin)(nil)
	_ format.Prefetcher = (*pkgPlugin)(nil)
)

// fakeUpstream is the upstream of these tests: meta/<name> answers the document of the name
// with an ETag and 304 for it, files/<file> its bytes. Status, when set, answers every request.
type fakeUpstream struct {
	srv       *httptest.Server
	mu        sync.Mutex
	docs      map[string]*upstreamDoc
	files     map[string][]byte
	status    atomic.Int32
	fileDelay atomic.Int64    // nanoseconds before a file's body
	metaDelay atomic.Int64    // nanoseconds before a document's answer
	noOther   map[string]bool // names whose document of otherKind is 404
	metaCalls atomic.Int32
	fileCalls atomic.Int32
}

func newFakeUpstream(t *testing.T) *fakeUpstream {
	u := &fakeUpstream{docs: map[string]*upstreamDoc{}, files: map[string][]byte{}}
	u.srv = httptest.NewServer(http.HandlerFunc(u.serve))
	t.Cleanup(u.srv.Close)
	return u
}

func (u *fakeUpstream) serve(w http.ResponseWriter, r *http.Request) {
	if s := u.status.Load(); s != 0 {
		w.Header().Set("Retry-After", "7")
		w.WriteHeader(int(s))
		return
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	if name, ok := strings.CutPrefix(r.URL.Path, "/meta/"); ok {
		u.metaCalls.Add(1)
		if d := time.Duration(u.metaDelay.Load()); d > 0 {
			u.mu.Unlock()
			time.Sleep(d)
			u.mu.Lock()
		}
		d, ok := u.docs[name]
		if !ok || (r.URL.Query().Get("kind") == otherKind && u.noOther[name]) {
			http.NotFound(w, r)
			return
		}
		b, _ := json.Marshal(d)
		etag := fmt.Sprintf(`"%x"`, sha256.Sum256(b))
		w.Header().Set("ETag", etag)
		if r.Header.Get("If-None-Match") == etag {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		_, _ = w.Write(b)
		return
	}
	file, _ := strings.CutPrefix(r.URL.Path, "/files/")
	b, ok := u.files[file]
	u.fileCalls.Add(1)
	if !ok {
		http.NotFound(w, r)
		return
	}
	if d := time.Duration(u.fileDelay.Load()); d > 0 {
		u.mu.Unlock()
		time.Sleep(d)
		u.mu.Lock()
	}
	w.Header().Set("Content-Length", fmt.Sprint(len(b)))
	_, _ = w.Write(b)
}

// tag points tag of name at version on the upstream.
func (u *fakeUpstream) tag(name, tag, version string) {
	u.mu.Lock()
	defer u.mu.Unlock()
	d := u.docs[name]
	if d.Tags == nil {
		d.Tags = map[string]string{}
	}
	d.Tags[tag] = version
}

// publish puts version of name on the upstream with content and its checksums; sums changes
// what the document says of them.
func (u *fakeUpstream) publish(name, version, content string, sums func(*upstreamVersion)) {
	u.mu.Lock()
	defer u.mu.Unlock()
	file := name + "-" + version + ".tgz"
	s512, s256, s1 := sha512.Sum512([]byte(content)), sha256.Sum256([]byte(content)), sha1.Sum([]byte(content)) //nolint:gosec // the upstream's
	v := upstreamVersion{Version: version, File: file, URL: "files/" + file, SHA512: hex.EncodeToString(s512[:]),
		SHA256: hex.EncodeToString(s256[:]), SHA1: hex.EncodeToString(s1[:])}
	if sums != nil {
		sums(&v)
	}
	d, ok := u.docs[name]
	if !ok {
		d = &upstreamDoc{Name: name}
		u.docs[name] = d
	}
	d.Versions = append(d.Versions, v)
	u.files[file] = []byte(content)
}

// env is a proxy repo raw/up of the fake upstream "up", on a fake registry and the real SQLite
// store in memory, with the ingest pipeline over both.
type env struct {
	st     *sqlite.Store
	reg    *providertest.Fake
	pipe   *ingest.Pipeline
	plugin *pkgPlugin
	up     *fakeUpstream
	repo   index.Repo
	now    atomic.Int64 // the clock of the cores, in nanoseconds
}

var memoryDBs atomic.Int64

func newEnv(t *testing.T, settings string) *env {
	t.Helper()
	ctx := t.Context()
	st, err := sqlite.Open(ctx, fmt.Sprintf("file:proxy-%d?mode=memory&cache=shared", memoryDBs.Add(1)))
	must(t, err)
	t.Cleanup(func() { _ = st.Close() })
	must(t, st.Migrate(ctx))
	e := &env{st: st, reg: &providertest.Fake{RegistryName: "local"}, plugin: newPlugin(), up: newFakeUpstream(t)}
	e.now.Store(time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC).UnixNano())
	e.repo = index.Repo{Format: "raw", Name: "up", Registry: "local", Prefix: "raw/up", Kind: index.KindProxy,
		Upstream: json.RawMessage(settings)}
	must(t, st.CreateRepo(ctx, &e.repo))
	e.pipe = ingest.New(ingest.Options{Providers: provider.Map{"local": e.reg}, Index: st, Plugins: e.plugins,
		Log: slog.New(slog.DiscardHandler)})
	return e
}

func (e *env) plugins(name string) (format.Plugin, bool) { return e.plugin, name == "raw" }

// clock is the time of the env.
func (e *env) clock() time.Time { return time.Unix(0, e.now.Load()).UTC() }

// advance moves the clock of the env.
func (e *env) advance(d time.Duration) { e.now.Add(int64(d)) }

// noProxy is the egress proxy choice of the tests: none, whatever the environment says.
func noProxy(*http.Request) (*url.URL, error) { return nil, nil }

// upstream returns the client of the fake upstream, "up"; maxMetadataSize 0 is the default.
func (e *env) upstream(t *testing.T, maxMetadataSize int64) *proxy.Upstream {
	t.Helper()
	up, err := proxy.NewUpstream(config.Upstream{Name: "up", Format: "raw", URL: e.up.srv.URL, Timeout: 2 * time.Second,
		MaxMetadataSize: maxMetadataSize}, proxy.UpstreamOptions{Proxy: noProxy, Log: slog.New(slog.DiscardHandler)})
	must(t, err)
	return up
}

// core returns a Core over the env, a replica of its own name, with o's knobs; the upstream
// "up" unless o has upstreams, and the env's plugin unless o has plugins.
func (e *env) core(t *testing.T, replica string, o proxy.Options) *proxy.Core {
	t.Helper()
	if o.Upstreams == nil {
		o.Upstreams = map[string]*proxy.Upstream{"up": e.upstream(t, 0)}
	}
	if o.Plugins == nil {
		o.Plugins = e.plugins
	}
	o.Index, o.Ingest, o.Providers = e.st, e.pipe, provider.Map{"local": e.reg}
	o.ExternalURL, o.Replica = "https://art.test", replica
	if o.Now == nil {
		o.Now = e.clock
	}
	if o.Log == nil {
		o.Log = slog.New(slog.DiscardHandler)
	}
	return proxy.New(o)
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
