package npm

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha1" //nolint:gosec // the dist.shasum of the fixture
	"crypto/sha512"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/lajstrom/lajstrom/internal/auth"
	"github.com/lajstrom/lajstrom/internal/format"
	"github.com/lajstrom/lajstrom/internal/format/formattest"
	"github.com/lajstrom/lajstrom/internal/index"
	"github.com/lajstrom/lajstrom/internal/ingest"
	"github.com/lajstrom/lajstrom/internal/oci/profile"
	"github.com/lajstrom/lajstrom/internal/provider"
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
		says    string // the answer is checkPublish's own, which says this, and nothing of ingest's
	}{
		{name: "not a tarball", tarball: []byte(strings.Repeat("not gzip ", 200)), status: http.StatusBadRequest},
		{name: "data that stops being base64 half way", edit: func(body, _ map[string]any) {
			a := body["_attachments"].(map[string]any)["widgets-1.0.0.tgz"].(map[string]any)
			data := a["data"].(string)
			a["data"] = data[:len(data)/8*4] + "!!!!" + data[len(data)/8*4:]
		}, status: http.StatusBadRequest, says: "base64"},
		{name: "a package.json of another version", tarball: largeTarball(t, "2.0.0", 1000), status: http.StatusBadRequest},
		{name: "a document after the attachment of another dependency", edit: func(_, doc map[string]any) {
			doc["dependencies"] = map[string]any{"lodash": "^4.0.0", "evil": "*"}
		}, status: http.StatusBadRequest},
		{name: "another integrity", edit: func(_, doc map[string]any) {
			doc["dist"].(map[string]any)["integrity"] = "sha512-" + base64.StdEncoding.EncodeToString(make([]byte, 64))
		}, status: http.StatusBadRequest, says: "dist.integrity"},
		{name: "a dist-tag of another version", edit: func(body, _ map[string]any) { body["dist-tags"] = map[string]any{"beta": "0.9.0"} },
			status: http.StatusBadRequest, says: "0.9.0"},
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
			answer := do(p.routes(nil), http.MethodPut, "/npm/internal/widgets", body, false)
			if answer.Code != tt.status {
				t.Fatalf("publish = %d %s, want %d", answer.Code, answer.Body, tt.status)
			}
			if msg := answer.Body.String(); tt.says != "" && (!strings.Contains(msg, tt.says) || strings.Contains(msg, "ingest")) {
				t.Errorf("publish answered %s, want checkPublish's reason, %q", msg, tt.says)
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

// panickingPublisher is the pipeline whose UploadStream reads a little of the tarball and then
// panics, as a bug on the goroutine of the upload would.
type panickingPublisher struct{ *ingest.Pipeline }

func (panickingPublisher) UploadStream(_ context.Context, _ index.Repo, up format.StreamUpload, _ auth.Principal) (*format.Published, error) {
	_, _ = io.ReadFull(up.Body, make([]byte, 16))
	panic("the upload's bug")
}

// TestPublishUploadPanics: a panic on the goroutine of the tarball's upload does not end the
// process: it is raised again on the handler's, where the server's recovery answers 500.
func TestPublishUploadPanics(t *testing.T) {
	p := newPipeline(t)
	deps := format.Deps{Index: p.st, Ingest: panickingPublisher{p.pipe}, Providers: provider.Map{"local": p.fake}, Log: slog.New(slog.DiscardHandler)}
	mux := http.NewServeMux()
	Plugin{}.Routes(format.NewRouter(mux, Name, deps), deps)
	b, info := testutil.NpmTarball(testutil.NpmOptions{})
	defer func() {
		if v := recover(); v == nil || !strings.Contains(fmt.Sprint(v), "the upload's bug") {
			t.Errorf("the handler panicked with %v, want the upload's panic", v)
		}
	}()
	rr := do(mux, http.MethodPut, "/npm/internal/widgets", npmBody(t, b, info, nil), false)
	t.Errorf("publish = %d %s, want a panic", rr.Code, rr.Body)
}

// TestStreamedTarballAbort: a publish that returns without ending its tarball, as after a panic
// of its own, ends the upload with it (abort): its goroutine returns, the session is cancelled.
func TestStreamedTarballAbort(t *testing.T) {
	p := newPipeline(t)
	p.fake.ChunkSize = 64
	h := &handlers{deps: format.Deps{Index: p.st, Ingest: p.pipe, Providers: provider.Map{"local": p.fake}, Log: slog.New(slog.DiscardHandler)}}
	b, _ := testutil.NpmTarball(testutil.NpmOptions{})
	tb := h.streamTarball(t.Context(), p.repo, "widgets", "widgets-1.0.0.tgz", alice)
	must(t, tb.send(bytes.NewReader(b[:len(b)/2])))
	tb.abort()
	tb.abort() // once ended, nothing
	<-tb.done
	if !errors.Is(tb.err, errPublishEnded) || p.fake.OpenUploads() != 0 {
		t.Errorf("the upload returned %v, %d sessions open; want errPublishEnded, none", tb.err, p.fake.OpenUploads())
	}
}

// countingWriter counts the bytes written to it.
type countingWriter struct{ n int64 }

func (c *countingWriter) Write(p []byte) (int, error) {
	c.n += int64(len(p))
	return len(p), nil
}
