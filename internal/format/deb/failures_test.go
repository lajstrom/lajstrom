package deb

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/lajstrom/lajstrom/internal/format"
	"github.com/lajstrom/lajstrom/internal/index"
	"github.com/lajstrom/lajstrom/internal/oci/profile"
	"github.com/lajstrom/lajstrom/internal/provider"
	"github.com/lajstrom/lajstrom/internal/testutil"
)

// TestRenderRefuses checks the versions a render refuses, each in a suite of its own: no
// component, metadata that is not the deb object or lacks a field, a Source that is not one, a
// field that would end the paragraph or start another, a field name that is not one, and no
// metadata in the index for a Plugin without providers. Each is an error of the server (500),
// never a 4xx of the request.
func TestRenderRefuses(t *testing.T) {
	p := newPipeline(t)
	cases := []struct {
		suite string
		v     version
		want  string
	}{
		{"no-component", version{component: "", arch: "amd64", metadata: metadataJSON("x1", "1.0", "amd64", nil, true)}, "has no lajstrom.deb.component"},
		{"not-json", version{component: "main", arch: "amd64", metadata: "[1]"}, "cannot unmarshal"},
		{"no-filename", version{component: "main", arch: "amd64", metadata: `{"control":{"Package":"x1"},"sha256":"aa"}`}, "lacks control, filename or sha256"},
		{"bad-source", version{component: "main", arch: "amd64", metadata: metadataJSON("x1", "1.0", "amd64",
			map[string]string{"Source": "Not A Name"}, true)}, "Source"},
		{"injected", version{component: "main", arch: "amd64", metadata: metadataJSON("x1", "1.0", "amd64",
			map[string]string{"Depends": "a\nFilename: pool/evil.deb"}, true)}, `control field "Depends"`},
		{"blank-line", version{component: "main", arch: "amd64", metadata: metadataJSON("x1", "1.0", "amd64",
			map[string]string{"Description": "a\n \n b"}, true)}, `control field "Description"`},
		{"bad-name", version{component: "main", arch: "amd64", metadata: metadataJSON("x1", "1.0", "amd64",
			map[string]string{"Bad Name": "x"}, true)}, `control field "Bad Name"`},
		{"no-metadata", version{component: "main", arch: "amd64"}, "has no metadata in the index"},
	}
	for i, c := range cases {
		c.v.name, c.v.version, c.v.suite = fmt.Sprintf("x%d", i+1), "1.0", c.suite
		p.addVersion(t, c.v)
	}
	for _, c := range cases {
		_, err := Plugin{}.RenderIndex(t.Context(), p.repo, format.IndexScope(c.suite), p.st)
		if err == nil || !strings.Contains(err.Error(), c.want) || format.StatusOf(err) != http.StatusInternalServerError {
			t.Errorf("RenderIndex(%s) = %v, want a 500 error containing %q", c.suite, err, c.want)
		}
	}
}

// faultyReader is a store whose ListPackages or ListVersions fails with errBoom.
type faultyReader struct {
	index.Reader
	fail string
}

func (r faultyReader) ListPackages(ctx context.Context, repoID int64, prefix string) ([]index.Package, error) {
	if r.fail == "ListPackages" {
		return nil, errBoom
	}
	return r.Reader.ListPackages(ctx, repoID, prefix)
}

func (r faultyReader) ListVersions(ctx context.Context, packageID int64) ([]index.Version, error) {
	if r.fail == "ListVersions" {
		return nil, errBoom
	}
	return r.Reader.ListVersions(ctx, packageID)
}

// TestRenderStoreFails checks that a failing store fails the render with its error.
func TestRenderStoreFails(t *testing.T) {
	p := newPipeline(t)
	hello, _ := testutil.Deb(testutil.DebOptions{})
	p.upload(t, hello, "jammy")
	for _, fail := range []string{"ListPackages", "ListVersions"} {
		if _, err := (Plugin{}).RenderIndex(t.Context(), p.repo, "jammy", faultyReader{Reader: p.st, fail: fail}); !errors.Is(err, errBoom) {
			t.Errorf("RenderIndex with a failing %s = %v, want errBoom", fail, err)
		}
	}
}

// stubRepository is a registry repository of blobs and manifests by digest; Resolve answers a
// digest it has with the manifest media type and the size of its bytes, or oversize.
type stubRepository struct {
	provider.Repository
	blobs    map[digest.Digest][]byte
	oversize bool
	read     error // the error of every read after the bytes
}

func (r *stubRepository) Resolve(_ context.Context, ref string) (ocispec.Descriptor, error) {
	b, ok := r.blobs[digest.Digest(ref)]
	if !ok {
		return ocispec.Descriptor{}, fmt.Errorf("resolve %s: %w", ref, provider.ErrNotFound)
	}
	size := int64(len(b))
	if r.oversize {
		size = maxManifest + 1
	}
	return ocispec.Descriptor{MediaType: ocispec.MediaTypeImageManifest, Digest: digest.Digest(ref), Size: size}, nil
}

func (r *stubRepository) Fetch(_ context.Context, desc ocispec.Descriptor) (io.ReadCloser, error) {
	b, ok := r.blobs[desc.Digest]
	if !ok {
		return nil, fmt.Errorf("fetch %s: %w", desc.Digest, provider.ErrNotFound)
	}
	if r.read != nil {
		return io.NopCloser(io.MultiReader(bytes.NewReader(b), iotestErr{})), nil
	}
	return io.NopCloser(bytes.NewReader(b)), nil
}

// stubProvider gives one repository, or the error of Repository.
type stubProvider struct {
	provider.Provider
	rr  provider.Repository
	err error
}

func (p stubProvider) Repository(context.Context, string) (provider.Repository, error) {
	return p.rr, p.err
}

// TestFetchMetadata checks the read of a metadata layer from the registry and how it fails:
// no providers, a registry that is not configured, a repository, a manifest or a metadata layer
// the registry does not give, a manifest too large or not JSON or not of the profile or
// without a metadata layer, bytes that are not the digest's, and a read that fails.
func TestFetchMetadata(t *testing.T) {
	deb, info := testutil.Deb(testutil.DebOptions{})
	b, err := Plugin{}.Build(t.Context(), ubuntu, upload([]format.UploadedFile{debFile(deb)}))
	must(t, err)
	blobs := blobsOf(t, b)
	manifest, md, err := profile.Encode(b.Manifest)
	must(t, err)
	metadata := blobs[b.Manifest.Layers[1].Digest]
	raw, err := profile.Build(profile.Annotations{Format: "raw", Repo: "x", Name: "a", Version: "1", Created: time.Unix(0, 0).UTC(),
		Attrs: map[string]string{profile.AnnotationRawPath: "a"}},
		[]profile.Layer{{MediaType: profile.MediaTypeRawFile, Digest: digest.FromString("a"), Size: 1, Title: "a"}}, nil)
	must(t, err)
	rawManifest, rawDigest, err := profile.Encode(raw)
	must(t, err)

	pkg := index.Package{Name: info.Package, NameOCI: info.Package}
	with := func(rr provider.Repository) Plugin {
		return Plugin{svc: &services{providers: provider.Map{"local": stubProvider{rr: rr}}}}
	}
	all := func(extra map[digest.Digest][]byte) map[digest.Digest][]byte {
		m := map[digest.Digest][]byte{md: manifest, b.Manifest.Layers[1].Digest: metadata}
		for d, b := range extra {
			if b == nil {
				delete(m, d)
			} else {
				m[d] = b
			}
		}
		return m
	}
	notJSON := []byte("{")
	notProfile := []byte("{}")
	for _, c := range []struct {
		name   string
		plugin Plugin
		d      digest.Digest
		want   string
	}{
		{"no services", Plugin{}, md, `registry "local" of apt/ubuntu is not configured`},
		{"no registry", Plugin{svc: &services{providers: provider.Map{}}}, md, "is not configured"},
		{"repository", Plugin{svc: &services{providers: provider.Map{"local": stubProvider{err: errBoom}}}}, md, "repository apt/ubuntu/hello: boom"},
		{"resolve", with(&stubRepository{blobs: all(map[digest.Digest][]byte{md: nil})}), md, "resolve " + md.String()},
		{"oversize", with(&stubRepository{blobs: all(nil), oversize: true}), md, "at most 4194304"},
		{"read", with(&stubRepository{blobs: all(nil), read: errBoom}), md, "read " + md.String() + ": boom"},
		{"not the bytes", with(&stubRepository{blobs: all(map[digest.Digest][]byte{md: metadata})}), md, "are not " + md.String()},
		{"not JSON", with(&stubRepository{blobs: all(map[digest.Digest][]byte{digest.FromBytes(notJSON): notJSON})}), digest.FromBytes(notJSON), "unexpected end"},
		{"not the profile", with(&stubRepository{blobs: all(map[digest.Digest][]byte{digest.FromBytes(notProfile): notProfile})}), digest.FromBytes(notProfile), "profile"},
		{"no metadata layer", with(&stubRepository{blobs: all(map[digest.Digest][]byte{rawDigest: rawManifest})}), rawDigest, "has no metadata layer"},
		{"no metadata blob", with(&stubRepository{blobs: all(map[digest.Digest][]byte{b.Manifest.Layers[1].Digest: nil})}), md, "the metadata layer in apt/ubuntu/hello: fetch"},
	} {
		_, err := c.plugin.fetchMetadata(t.Context(), ubuntu, pkg, index.Version{Digest: c.d})
		if err == nil || !strings.Contains(err.Error(), c.want) || errors.Is(err, provider.ErrNotFound) {
			t.Errorf("%s: %v, want an error containing %q that does not wrap the provider's", c.name, err, c.want)
		}
	}
	got, err := with(&stubRepository{blobs: all(nil)}).fetchMetadata(t.Context(), index.Repo{Format: Name, Name: "ubuntu", Registry: "local"}, pkg, index.Version{Digest: md})
	if err != nil || !bytes.Equal(got, metadata) {
		t.Errorf("fetchMetadata = %q, %v; want the metadata layer", got, err)
	}
	if got := ociName(index.Repo{Format: Name, Name: "ubuntu"}, "hello"); got != "apt/ubuntu/hello" {
		t.Errorf("ociName without a prefix = %q", got)
	}
}

// failingWriter fails the Write or the Close named in fail.
type failingWriter struct{ fail string }

func (w failingWriter) Write(p []byte) (int, error) {
	if w.fail == "write" {
		return 0, errBoom
	}
	return len(p), nil
}

func (w failingWriter) Close() error {
	if w.fail == "close" {
		return errBoom
	}
	return nil
}

// TestCompressedFails checks that compressed returns the error of the compressor it opens,
// writes and closes.
func TestCompressedFails(t *testing.T) {
	for _, open := range []func(io.Writer) (io.WriteCloser, error){
		func(io.Writer) (io.WriteCloser, error) { return nil, errBoom },
		func(io.Writer) (io.WriteCloser, error) { return failingWriter{"write"}, nil },
		func(io.Writer) (io.WriteCloser, error) { return failingWriter{"close"}, nil },
	} {
		if _, err := compressed([]byte("x"), open); !errors.Is(err, errBoom) {
			t.Errorf("compressed = %v, want errBoom", err)
		}
	}
}

// TestListed checks that listed reads the SHA256 section alone and stops at its end.
func TestListed(t *testing.T) {
	release := []byte("Origin: Lajstrom\nMD5Sum:\n aa 1 main/binary-amd64/Packages\nSHA256:\n bb 1 main/binary-amd64/Packages\n" +
		" cc 2 main/binary-amd64/Packages.gz\nX-After: not a line of the section\n dd 3 other\n")
	got := listed(release)
	if len(got) != 2 || got["main/binary-amd64/Packages"] != "bb" || got["main/binary-amd64/Packages.gz"] != "cc" {
		t.Errorf("listed = %v", got)
	}
	if got := listed([]byte("Origin: Lajstrom\n")); len(got) != 0 {
		t.Errorf("listed of a Release without SHA256 = %v", got)
	}
}

// TestRoutesClock checks that Routes keeps the clock of the plugin it is called on and works
// without a logger in format.Deps.
func TestRoutesClock(t *testing.T) {
	p := newPipeline(t)
	hello, _ := testutil.Deb(testutil.DebOptions{})
	p.upload(t, hello, "jammy")
	deps := format.Deps{Index: p.st, Providers: provider.Map{"local": p.prov}}
	mux := http.NewServeMux()
	fixedClock(p).Routes(format.NewRouter(mux, Name, format.Deps{Index: p.st, Log: slog.New(slog.DiscardHandler)}), deps)
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/apt/ubuntu/dists/jammy/Release", nil))
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), "\nDate: Sun, 08 Nov 2026 09:30:00 UTC\n") {
		t.Errorf("GET Release = %d:\n%s", rr.Code, rr.Body)
	}
}
