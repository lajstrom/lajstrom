package deb

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/lajstrom/lajstrom/internal/format"
	"github.com/lajstrom/lajstrom/internal/format/formattest"
	"github.com/lajstrom/lajstrom/internal/ingest"
	"github.com/lajstrom/lajstrom/internal/oci/profile"
	"github.com/lajstrom/lajstrom/internal/provider"
	"github.com/lajstrom/lajstrom/internal/provider/providertest"
	"github.com/lajstrom/lajstrom/internal/testutil"
)

// bigDeb is the hello package with a data file of n bytes that do not compress.
func bigDeb(t *testing.T, n int64) ([]byte, testutil.DebInfo) {
	t.Helper()
	data, err := io.ReadAll(io.LimitReader(&formattest.Pattern{}, n))
	must(t, err)
	b, info := testutil.Deb(testutil.DebOptions{Files: map[string]string{"usr/share/hello/big": string(data)}})
	return b, info
}

// TestUploadWritesNoTempFile: with a TMPDIR that takes no file, an upload of a .deb of several
// chunks, with its length and without, is 201 and the pool serves the bytes back: the body
// streams into the registry and nothing is spooled (ADR 0014). The second upload, to another
// suite, passes the pool file name check of the first. No upload session is left open.
func TestUploadWritesNoTempFile(t *testing.T) {
	dir := formattest.NoTempDir(t)
	e := newAptEnv(t, envOptions{})
	e.prov.ChunkSize = 16 << 10
	big, info := bigDeb(t, 300<<10)
	uploaded(t, e.put("suite=jammy&component=main", big))
	rr := e.send(http.MethodPut, "upload?suite=noble&component=main", io.MultiReader(bytes.NewReader(big)), "X-Test-User", "alice")
	uploaded(t, rr)
	if got := e.get("pool/main/h/hello/" + info.Filename); got.Code != http.StatusOK || !bytes.Equal(got.Body.Bytes(), big) {
		t.Errorf("GET of the pool file = %d with %d bytes, want 200 with the %d uploaded", got.Code, got.Body.Len(), len(big))
	}
	if n := e.prov.OpenUploads(); n != 0 {
		t.Errorf("%d upload sessions open", n)
	}
	if entries, err := os.ReadDir(dir); err != nil || len(entries) != 0 {
		t.Errorf("TMPDIR holds %v, %v", entries, err)
	}
}

// TestUploadRefusedStoresNothing: an upload refused before its body streams, while it streams
// or after it, stores nothing: no blob in the registry, no session left open, no manifest, no
// version. Before: maxFileSize against Content-Length, arch; while: maxFileSize cut, the third
// member; after: a short body, a cut data member, the pool file name held by another content,
// the registry getting other bytes than were hashed, the tag taken in the registry.
func TestUploadRefusedStoresNothing(t *testing.T) {
	hello, _ := bigDeb(t, 2<<10)
	other, _ := testutil.Deb(testutil.DebOptions{Description: "another greeting"})
	end, _ := headLen(hello)
	notData := append(hello[:end:end], arOf(arMember{"foo.tar", []byte("not the data")})[len(arMagic):]...)
	for _, tt := range []struct {
		name     string
		query    string
		body     []byte
		policy   string
		registry func(*providertest.Fake) provider.Provider // over the env's fake; nil: the fake
		setup    func(t *testing.T, e *aptEnv)              // before the PUT
		req      func(r *http.Request)
		status   int
		want     string
	}{
		{name: "maxFileSize, cut while the body streams", policy: `{"maxFileSize":2000}`,
			req: func(r *http.Request) { r.ContentLength = -1 }, status: http.StatusRequestEntityTooLarge, want: "maxFileSize"},
		{name: "maxFileSize, a Content-Length above it", policy: `{"maxFileSize":2000}`, status: http.StatusRequestEntityTooLarge,
			want: "maxFileSize"},
		{name: "arch, from the head", query: "&arch=arm64", status: http.StatusBadRequest, want: `the upload is for architecture "arm64"`},
		{name: "a body shorter than its Content-Length", req: func(r *http.Request) { r.ContentLength += 10 },
			status: http.StatusBadRequest, want: "Content-Length says"},
		{name: "the data member cut short", body: hello[:len(hello)-5], status: http.StatusBadRequest,
			want: "truncated archive: member 3 lacks"},
		{name: "the third member not data.tar", body: notData, status: http.StatusBadRequest, want: `the third member is "foo.tar"`},
		{name: "the pool file name held by another content", setup: func(t *testing.T, e *aptEnv) {
			e.upload(t, other, "jammy")
		}, status: http.StatusConflict, want: "is held by version 1.0.0-1 in jammy/main with another content"},
		{name: "the registry gets other bytes than were hashed", registry: providertest.Corrupting, status: http.StatusInternalServerError},
		{name: "the tag taken in the registry", setup: func(t *testing.T, e *aptEnv) {
			e.squat(t, "1.0.0-1__noble_2fmain_2famd64")
		}, status: http.StatusConflict, want: "1.0.0-1__noble_2fmain_2famd64"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			p := newPipeline(t)
			p.prov.ChunkSize = 512
			if tt.registry != nil {
				p.pipe = ingest.New(ingest.Options{Providers: provider.Map{"local": tt.registry(p.prov)}, Index: p.st,
					Log: slog.New(slog.DiscardHandler), Plugins: func(name string) (format.Plugin, bool) { return Plugin{}, name == Name }})
			}
			e := envOn(t, p, envOptions{})
			if tt.policy != "" {
				e.repo.Policy = []byte(tt.policy)
				must(t, e.st.UpdateRepo(t.Context(), &e.repo))
			}
			if tt.setup != nil {
				tt.setup(t, e)
			}
			body := hello
			if tt.body != nil {
				body = tt.body
			}
			before := e.snapshot(t)
			req := httptest.NewRequest(http.MethodPut, "/apt/ubuntu/upload?suite=noble&component=main"+tt.query, bytes.NewReader(body))
			req.Header.Set("X-Test-User", "alice")
			if tt.req != nil {
				tt.req(req)
			}
			rr := httptest.NewRecorder()
			e.h.ServeHTTP(rr, req)
			if rr.Code != tt.status || !strings.Contains(rr.Body.String(), tt.want) {
				t.Fatalf("PUT = %d %s, want %d with %q", rr.Code, rr.Body, tt.status, tt.want)
			}
			e.nothingStored(t, body, before)
		})
	}
}

// TestUploadLargeControl: a .deb whose uncompressed control member of 3 MiB, an md5sums of
// 60 000 files, is held as its head while the route reads the package name, streams and is
// published with every md5sums line.
func TestUploadLargeControl(t *testing.T) {
	e := newAptEnv(t, envOptions{})
	var sums strings.Builder
	for i := range 60_000 {
		fmt.Fprintf(&sums, "d41d8cd98f00b204e9800998ecf8427e  usr/share/hello/%07d\n", i)
	}
	b := debOf(t, "Package: hello\nVersion: 1.0\nArchitecture: all\nMaintainer: m\nDescription: d\n", ptr(sums.String()), "")
	if n, _ := headLen(b); n < 3<<20 || n > maxHead {
		t.Fatalf("the head has %d bytes, want 3 MiB to maxHead", n)
	}
	uploaded(t, e.put("suite=jammy&component=main", b))
	v := e.versionOf(t, "hello", "jammy/main/all")
	var md struct {
		MD5Sums map[string]string `json:"md5sums"`
	}
	must(t, json.Unmarshal(v.Metadata, &md))
	if len(md.MD5Sums) != 60_000 {
		t.Errorf("the metadata has %d md5sums, want 60000", len(md.MD5Sums))
	}
}

// TestUploadControlOverMaxControl: a .deb whose control member is larger than MaxControl, and
// so would not fit the head held in memory, is 400 naming the bound once its header is read:
// no more of the body is read, and nothing is stored.
func TestUploadControlOverMaxControl(t *testing.T) {
	e := newAptEnv(t, envOptions{})
	head := fmt.Appendf(arOf(arMember{"debian-binary", []byte("2.0\n")}), "%-16s%-12d%-6d%-6d%-8o%-10d`\n",
		"control.tar.xz", 0, 0, 0, 0o100644, MaxControl+2)
	body := &countingReader{r: io.MultiReader(bytes.NewReader(head), io.LimitReader(&formattest.Pattern{}, MaxControl+2))}
	before := e.snapshot(t)
	rr := e.send(http.MethodPut, "upload?suite=jammy&component=main", body, "X-Test-User", "alice")
	want := fmt.Sprintf("the control member control.tar.xz has %d bytes, at most %d", MaxControl+2, MaxControl)
	if rr.Code != http.StatusBadRequest || !strings.Contains(rr.Body.String(), want) {
		t.Errorf("PUT = %d %s, want 400 with %q", rr.Code, rr.Body, want)
	}
	if body.n > int64(len(head)) {
		t.Errorf("the upload route read %d bytes of the body, want at most the %d of the headers", body.n, len(head))
	}
	e.nothingStored(t, head, before)
}

// stored is what the registry and the index hold of the hello package, for nothingStored.
type stored struct {
	manifests, pushes, versions int
}

// snapshot returns what the registry and the index hold of the hello package now.
func (e *aptEnv) snapshot(t *testing.T) stored {
	t.Helper()
	s := stored{manifests: len(e.prov.PushReferences())}
	for _, p := range e.prov.Pushes() {
		if p.Name == "apt/ubuntu/hello" {
			s.pushes++
		}
	}
	if pkg, err := e.st.GetPackage(t.Context(), e.repo.ID, "hello"); err == nil {
		vs, err := e.st.ListVersions(t.Context(), pkg.ID)
		must(t, err)
		s.versions = len(vs)
	}
	return s
}

// nothingStored fails the test when the registry holds content as a blob of the hello package,
// a session is open, or a blob, a manifest or a version was added since before.
func (e *aptEnv) nothingStored(t *testing.T, content []byte, before stored) {
	t.Helper()
	rr, err := e.prov.Repository(t.Context(), "apt/ubuntu/hello")
	must(t, err)
	kept, err := rr.Exists(t.Context(), ocispec.Descriptor{Digest: digest.FromBytes(content), Size: int64(len(content))})
	must(t, err)
	if after := e.snapshot(t); kept || e.prov.OpenUploads() != 0 || after != before {
		t.Errorf("file stored %v, %d sessions open, %+v stored, %+v before; want nothing new", kept, e.prov.OpenUploads(), after, before)
	}
}

// squat pushes a manifest of nothing under tag to the OCI repository of the hello package,
// straight to the registry: a write that bypassed Lajstrom.
func (e *aptEnv) squat(t *testing.T, tag string) {
	t.Helper()
	m := []byte(`{"schemaVersion":2,"mediaType":"` + ocispec.MediaTypeImageManifest + `",` +
		`"config":{"mediaType":"` + ocispec.MediaTypeEmptyJSON + `","digest":"` + ocispec.DescriptorEmptyJSON.Digest.String() + `","size":2},"layers":[]}`)
	rr, err := e.prov.Repository(t.Context(), "apt/ubuntu/hello")
	must(t, err)
	d := ocispec.Descriptor{MediaType: ocispec.MediaTypeImageManifest, Digest: digest.FromBytes(m), Size: int64(len(m))}
	must(t, rr.PushReference(t.Context(), d, bytes.NewReader(m), tag))
}

// TestUploadStreams1GiB uploads a .deb of 1 GiB over a real connection, with a TMPDIR that
// takes no file and the generic provider's chunks of 8 MiB, and checks that it is 201, that the
// index has the file with the size and the sha256 of the bytes, and that the heap in use stayed
// under formattest.HeapLimit: the body is neither spooled nor held, only its head. The fake
// hashes the chunks and keeps none of a blob above providertest.MaxKept.
func TestUploadStreams1GiB(t *testing.T) {
	if testing.Short() {
		t.Skip("moves 1 GiB; skipped under -short")
	}
	const size = 1 << 30
	formattest.NoTempDir(t)
	e := newAptEnv(t, envOptions{})
	e.prov.ChunkSize = 8 << 20
	srv := httptest.NewServer(e.h)
	defer srv.Close()
	hello, _ := testutil.Deb(testutil.DebOptions{})
	n, _ := headLen(hello)
	head := fmt.Appendf(hello[:n:n], "%-16s%-12d%-6d%-6d%-8o%-10d`\n", "data.tar.xz", 0, 0, 0, 0o100644, size)
	peak := formattest.SampleHeap(t)

	h := sha256.New()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPut, srv.URL+"/apt/ubuntu/upload?suite=jammy&component=main",
		io.TeeReader(io.MultiReader(bytes.NewReader(head), io.LimitReader(&formattest.Pattern{}, size)), h))
	must(t, err)
	req.ContentLength = int64(len(head)) + size
	req.Header.Set("X-Test-User", "alice")
	resp, err := http.DefaultClient.Do(req)
	must(t, err)
	defer func() { _ = resp.Body.Close() }()
	var got uploadResponse
	if resp.StatusCode != http.StatusCreated || json.NewDecoder(resp.Body).Decode(&got) != nil {
		t.Fatalf("PUT = %d", resp.StatusCode)
	}
	top := peak()
	v := e.versionOf(t, "hello", "jammy/main/amd64")
	files, err := e.st.ListFiles(t.Context(), v.ID)
	must(t, err)
	if d := digest.NewDigest(digest.SHA256, h); len(files) != 1 || files[0].Digest != d || files[0].Size != req.ContentLength {
		t.Errorf("files %+v, want one of %d bytes with %s", files, req.ContentLength, d)
	}
	t.Logf("peak heap in use: %d MiB", top>>20)
	if top > formattest.HeapLimit {
		t.Errorf("peak heap in use %d MiB, want under %d MiB", top>>20, formattest.HeapLimit>>20)
	}
	if v.Attrs[profile.AnnotationDebArch] != "amd64" {
		t.Errorf("version %+v", v)
	}
}
