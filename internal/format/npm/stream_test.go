package npm

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha1" //nolint:gosec // the dist.shasum of the fixture
	"crypto/sha512"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/lajstrom/lajstrom/internal/format/formattest"
	"github.com/lajstrom/lajstrom/internal/oci/profile"
	"github.com/lajstrom/lajstrom/internal/testutil"
)

// writeTarball writes to w the tarball of widgets at version: package/package.json, and
// package/data.bin of size bytes of formattest.Pattern, stored, not compressed, so that the
// tarball is a little larger than size.
func writeTarball(w io.Writer, version string, size int64) error {
	gz, _ := gzip.NewWriterLevel(w, gzip.NoCompression)
	tw := tar.NewWriter(gz)
	pkg := `{"name":"widgets","version":"` + version + `"}`
	for _, m := range []struct {
		name string
		size int64
		r    io.Reader
	}{{"package/package.json", int64(len(pkg)), strings.NewReader(pkg)}, {"package/data.bin", size, io.LimitReader(&formattest.Pattern{}, size)}} {
		if err := tw.WriteHeader(&tar.Header{Name: m.name, Mode: 0o644, Size: m.size}); err != nil {
			return err
		}
		if _, err := io.Copy(tw, m.r); err != nil {
			return err
		}
	}
	if err := tw.Close(); err != nil {
		return err
	}
	return gz.Close()
}

// largeTarball is writeTarball into memory.
func largeTarball(t *testing.T, version string, size int64) []byte {
	t.Helper()
	var buf bytes.Buffer
	must(t, writeTarball(&buf, version, size))
	return buf.Bytes()
}

// TestPublishWritesNoTempFile: with a TMPDIR that takes no file, the captured bodies of npm, pnpm
// and yarn, whose _attachments come before or after the versions, are published: the tarball
// streams into the registry as its data is decoded, and nothing is spooled (ADR 0014). No upload
// session is left open.
func TestPublishWritesNoTempFile(t *testing.T) {
	dir := formattest.NoTempDir(t)
	p := newPipeline(t)
	p.fake.ChunkSize = 64
	h := p.routes(nil)
	for _, f := range []string{"testdata/publish/npm11.json", "testdata/publish/yarn4-scoped.json"} {
		body, err := os.ReadFile(f) //nolint:gosec // a captured body of this package's testdata
		must(t, err)
		var b struct{ Name string }
		must(t, json.Unmarshal(body, &b))
		if rr := do(h, http.MethodPut, "/npm/internal/"+strings.Replace(b.Name, "/", "%2f", 1), body, false); rr.Code != http.StatusCreated {
			t.Errorf("%s: %d %s", f, rr.Code, rr.Body)
		}
	}
	if n := p.fake.OpenUploads(); n != 0 {
		t.Errorf("%d upload sessions open", n)
	}
	if entries, err := os.ReadDir(dir); err != nil || len(entries) != 0 {
		t.Errorf("TMPDIR holds %v, %v", entries, err)
	}
}

// TestPublishRefusedStoresNothing: a publish refused while its tarball streams, by its
// inspector, or after it, by the members of the body that come after the attachment, by Build or
// by the tag taken in the registry, stores nothing: no blob, no session left open, no manifest,
// no version.
func TestPublishRefusedStoresNothing(t *testing.T) {
	b, info := testutil.NpmTarball(testutil.NpmOptions{Dependencies: map[string]string{"lodash": "^4.0.0"}})
	for _, tt := range []struct {
		name    string
		tarball []byte
		edit    func(body, doc map[string]any)
		setup   func(t *testing.T, p *pipeline)
		status  int
	}{
		{name: "not a tarball", tarball: []byte(strings.Repeat("not gzip ", 200)), status: http.StatusBadRequest},
		{name: "a package.json of another version", tarball: largeTarball(t, "2.0.0", 1000), status: http.StatusBadRequest},
		{name: "a document after the attachment of another dependency", edit: func(_, doc map[string]any) {
			doc["dependencies"] = map[string]any{"lodash": "^4.0.0", "evil": "*"}
		}, status: http.StatusBadRequest},
		{name: "another integrity", edit: func(_, doc map[string]any) {
			doc["dist"].(map[string]any)["integrity"] = "sha512-" + base64.StdEncoding.EncodeToString(make([]byte, 64))
		}, status: http.StatusBadRequest},
		{name: "a dist-tag of another version", edit: func(body, _ map[string]any) { body["dist-tags"] = map[string]any{"beta": "0.9.0"} },
			status: http.StatusBadRequest},
		{name: "the tag is taken in the registry", setup: func(t *testing.T, p *pipeline) { p.squat(t, "widgets", profile.Tag("1.0.0", "")) },
			status: http.StatusConflict},
	} {
		t.Run(tt.name, func(t *testing.T) {
			p := newPipeline(t)
			p.fake.ChunkSize = 64
			if tt.setup != nil {
				tt.setup(t, p)
			}
			tarball := b
			if tt.tarball != nil {
				tarball = tt.tarball
			}
			pushed := len(p.fake.PushReferences())
			// encoding/json writes _attachments first: every other member comes after the tarball.
			body := npmBody(t, tarball, info, func(body, doc map[string]any) {
				d := doc["dist"].(map[string]any)
				s512, s1 := sha512.Sum512(tarball), sha1.Sum(tarball) //nolint:gosec // see the import
				d["integrity"], d["shasum"] = "sha512-"+base64.StdEncoding.EncodeToString(s512[:]), hex.EncodeToString(s1[:])
				body["_attachments"].(map[string]any)["widgets-1.0.0.tgz"].(map[string]any)["length"] = len(tarball)
				if tt.edit != nil {
					tt.edit(body, doc)
				}
			})
			if rr := do(p.routes(nil), http.MethodPut, "/npm/internal/widgets", body, false); rr.Code != tt.status {
				t.Fatalf("publish = %d %s, want %d", rr.Code, rr.Body, tt.status)
			}
			rr, err := p.fake.Repository(t.Context(), "npm/internal/widgets")
			must(t, err)
			stored, err := rr.Exists(t.Context(), ocispec.Descriptor{Digest: digest.FromBytes(tarball), Size: int64(len(tarball))})
			must(t, err)
			config, err := rr.Exists(t.Context(), profile.EmptyConfig)
			must(t, err)
			pkgs, err := p.st.ListPackages(t.Context(), p.repo.ID, "")
			must(t, err)
			if stored || config || p.fake.OpenUploads() != 0 || len(p.fake.PushReferences()) != pushed || len(pkgs) != 0 {
				t.Errorf("tarball stored %v, config %v, %d sessions open, %d manifests pushed (%d before), %d packages; want nothing",
					stored, config, p.fake.OpenUploads(), len(p.fake.PushReferences()), pushed, len(pkgs))
			}
		})
	}
}

// squat pushes a manifest of nothing under tag to the OCI repository of the package pkg, straight
// to the registry: a write that bypassed Lajstrom.
func (p *pipeline) squat(t *testing.T, pkg, tag string) {
	t.Helper()
	m := []byte(`{"schemaVersion":2,"mediaType":"` + ocispec.MediaTypeImageManifest + `",` +
		`"config":{"mediaType":"` + ocispec.MediaTypeEmptyJSON + `","digest":"` + ocispec.DescriptorEmptyJSON.Digest.String() + `","size":2},"layers":[]}`)
	rr, err := p.fake.Repository(t.Context(), "npm/internal/"+pkg)
	must(t, err)
	d := ocispec.Descriptor{MediaType: ocispec.MediaTypeImageManifest, Digest: digest.FromBytes(m), Size: int64(len(m))}
	must(t, rr.PushReference(t.Context(), d, bytes.NewReader(m), tag))
}

// TestPublishStreams publishes a tarball of 160 MiB, more than npm's largest (152 MiB), over a
// real connection, its body encoded as it is sent with the attachment first, as yarn sends it,
// with a TMPDIR that takes no file and chunks of 8 MiB: 201, and the heap in use stayed under
// formattest.HeapLimit, so that the tarball was neither spooled nor held.
func TestPublishStreams(t *testing.T) {
	if testing.Short() {
		t.Skip("moves 160 MiB; skipped under -short")
	}
	const size = 160 << 20
	// The digests and the length of the tarball, which the body says before its data comes.
	s512, s1, n := sha512.New(), sha1.New(), &countingWriter{} //nolint:gosec // see the import
	must(t, writeTarball(io.MultiWriter(s512, s1, n), "1.0.0", size))
	formattest.NoTempDir(t)
	p := newPipeline(t)
	p.fake.ChunkSize = 8 << 20
	srv := httptest.NewServer(p.routes(nil))
	defer srv.Close()
	peak := formattest.SampleHeap(t)

	pr, pw := io.Pipe()
	go func() {
		pw.CloseWithError(func() error {
			if _, err := io.WriteString(pw, `{"_attachments":{"widgets-1.0.0.tgz":{"length":`+strconv.FormatInt(n.n, 10)+`,"data":"`); err != nil {
				return err
			}
			enc := base64.NewEncoder(base64.StdEncoding, pw)
			if err := writeTarball(enc, "1.0.0", size); err != nil {
				return err
			}
			if err := enc.Close(); err != nil {
				return err
			}
			doc := `{"name":"widgets","version":"1.0.0","dist":{"integrity":"sha512-` + base64.StdEncoding.EncodeToString(s512.Sum(nil)) +
				`","shasum":"` + hex.EncodeToString(s1.Sum(nil)) + `"}}`
			_, err := io.WriteString(pw, `"}},"name":"widgets","dist-tags":{"latest":"1.0.0"},"versions":{"1.0.0":`+doc+`}}`)
			return err
		}())
	}()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPut, srv.URL+"/npm/internal/widgets", pr)
	must(t, err)
	resp, err := http.DefaultClient.Do(req)
	must(t, err)
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("publish of %d MiB: %d %s", size>>20, resp.StatusCode, body)
	}
	top := peak()
	t.Logf("peak heap in use: %d MiB", top>>20)
	if top > formattest.HeapLimit {
		t.Errorf("peak heap in use %d MiB, want under %d MiB", top>>20, formattest.HeapLimit>>20)
	}
}

// countingWriter counts the bytes written to it.
type countingWriter struct{ n int64 }

func (c *countingWriter) Write(p []byte) (int, error) {
	c.n += int64(len(p))
	return len(p), nil
}
