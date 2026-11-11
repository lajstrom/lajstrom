package pypi

import (
	"archive/zip"
	"bytes"
	"context"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/lajstrom/lajstrom/internal/format/formattest"
	"github.com/lajstrom/lajstrom/internal/oci/profile"
	"github.com/lajstrom/lajstrom/internal/provider"
	"github.com/lajstrom/lajstrom/internal/provider/providertest"
	"github.com/lajstrom/lajstrom/internal/testutil"
)

// keeping is the fake registry that keeps the bytes of the blobs streamed into it, which the fake
// drops above providertest.MaxKept, and serves their ranges, with the byte at flip changed when it
// is not negative; it counts the range reads.
type keeping struct {
	*providertest.Fake
	mu     sync.Mutex
	blobs  map[digest.Digest][]byte
	ranges int
	flip   int64
}

func keepAll(f *providertest.Fake) provider.Provider {
	return &keeping{Fake: f, blobs: map[digest.Digest][]byte{}, flip: -1}
}

func (k *keeping) Repository(ctx context.Context, name string) (provider.Repository, error) {
	rr, err := k.Fake.Repository(ctx, name)
	return keepingRepo{Repository: rr, k: k}, err
}

func (k *keeping) OpenBlob(ctx context.Context, name string, d digest.Digest, rangeHeader string, redirect bool) (provider.Blob, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	data, ok := k.blobs[d]
	if !ok || rangeHeader == "" {
		return k.Fake.OpenBlob(ctx, name, d, rangeHeader, redirect)
	}
	k.ranges++
	var first, last int64
	if _, err := fmt.Sscanf(rangeHeader, "bytes=%d-%d", &first, &last); err != nil {
		return provider.Blob{}, err
	}
	b := bytes.Clone(data[first : last+1])
	if first <= k.flip && k.flip <= last {
		b[k.flip-first] ^= 0xff
	}
	return provider.Blob{Status: http.StatusPartialContent, Size: last - first + 1, Digest: d, Body: io.NopCloser(bytes.NewReader(b))}, nil
}

// rangeReads returns the range reads so far.
func (k *keeping) rangeReads() int {
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.ranges
}

type keepingRepo struct {
	provider.Repository
	k *keeping
}

func (r keepingRepo) OpenBlobWriter(ctx context.Context) (provider.BlobWriter, error) {
	w, err := r.Repository.OpenBlobWriter(ctx)
	return &keepingWriter{BlobWriter: w, k: r.k}, err
}

type keepingWriter struct {
	provider.BlobWriter
	k   *keeping
	buf bytes.Buffer
}

func (w *keepingWriter) Write(p []byte) (int, error) {
	w.buf.Write(p)
	return w.BlobWriter.Write(p)
}

func (w *keepingWriter) Commit(ctx context.Context, d digest.Digest) (ocispec.Descriptor, error) {
	desc, err := w.BlobWriter.Commit(ctx, d)
	if err == nil {
		w.k.mu.Lock()
		w.k.blobs[d] = w.buf.Bytes()
		w.k.mu.Unlock()
	}
	return desc, err
}

// filetype is the filetype twine sends for the file name.
func filetype(filename string) string {
	if strings.HasSuffix(filename, ".whl") {
		return PackagetypeWheel
	}
	return PackagetypeSdist
}

// TestUploadWheelWritesNoTempFile: with a TMPDIR that takes no file, wheels and a .zip sdist are
// uploaded and served: a small wheel and the sdist, held whole; a wheel of 6 MiB with its
// METADATA first, held from its local entry as it passed, as the wheels of PyPI allow; and the
// same with data descriptors, which the inspector cannot follow, read back from the registry by
// range requests (ADR 0014 step 5). Nothing is spooled, no session is left open, and only the
// last reads ranges.
func TestUploadWheelWritesNoTempFile(t *testing.T) {
	dir := formattest.NoTempDir(t)
	e := newEnvWith(t, keepAll)
	reg := e.prov.(*keeping)
	small, si := testutil.Wheel(testutil.WheelOptions{Name: "tool", Version: "1.0"})
	held, heldWheel := largeWheel(t, "2.0", "2.0", false)
	back, backWheel := largeWheel(t, "3.0", "3.0", true)
	for _, tt := range []struct {
		filename, version string
		content           []byte
		ranges            bool
	}{
		{si.Filename, "1.0", small, false},
		{held, "2.0", heldWheel, false},
		{back, "3.0", backWheel, true},
		{"tool-4.0.zip", "4.0", storedZip(t, false, "tool-4.0/PKG-INFO", pkgInfo("4.0"), "tool-4.0/tool/__init__.py", "x = 1\n"), false},
	} {
		before := reg.rangeReads()
		if rr := e.post(t, twineForm(tt.filename, "tool", tt.version, filetype(tt.filename), tt.content)); rr.Code != http.StatusOK {
			t.Fatalf("upload %s: %d %s", tt.filename, rr.Code, rr.Body)
		}
		if read := reg.rangeReads() > before; read != tt.ranges {
			t.Errorf("%s: ranges read %v, want %v", tt.filename, read, tt.ranges)
		}
		if tt.content == nil || len(tt.content) > providertest.MaxKept {
			continue // the fake serves no file this large
		}
		if got := e.do(http.MethodGet, "files/tool/"+tt.version+"/"+tt.filename); got.Code != http.StatusOK || !bytes.Equal(got.Body.Bytes(), tt.content) {
			t.Errorf("GET %s = %d with %d bytes, want 200 with the %d uploaded", tt.filename, got.Code, got.Body.Len(), len(tt.content))
		}
	}
	if page := e.do(http.MethodGet, "simple/tool/").Body.String(); strings.Count(page, "<a ") != 4 {
		t.Errorf("the page after the uploads:\n%s", page)
	}
	if n := e.fake.OpenUploads(); n != 0 {
		t.Errorf("%d upload sessions open", n)
	}
	if entries, err := os.ReadDir(dir); err != nil || len(entries) != 0 {
		t.Errorf("TMPDIR holds %v, %v", entries, err)
	}
}

// TestUploadWheelRefused: a wheel refused while it streams, or after it from what its inspector
// held, stores nothing: no blob, no session, no manifest, no version. One refused after a
// read-back, its METADATA read back of another version, or the registry serving other bytes of
// it than streamed (the CRC-32 of the directory held from the end), stores no manifest and no
// version; the file stays in the registry, committed, for its garbage collection (ADR 0014).
func TestUploadWheelRefused(t *testing.T) {
	whl, wi := testutil.Wheel(testutil.WheelOptions{Name: "tool", Version: "1.0"})
	other, _ := testutil.Wheel(testutil.WheelOptions{Name: "tool", Version: "1.1"})
	back, backWheel := largeWheel(t, "1.0", "1.0", true)
	_, lying := largeWheel(t, "1.0", "1.1", true)
	for _, tt := range []struct {
		name     string
		filename string
		content  []byte
		edit     func(f *form)
		policy   string
		setup    func(t *testing.T, e *env)
		flip     bool // the registry serves the METADATA read back changed
		readBack bool
		status   int
	}{
		{name: "the sha256 of other bytes", edit: func(f *form) { f.fields["sha256_digest"] = strings.Repeat("ab", 32) }, status: http.StatusBadRequest},
		{name: "not a zip", content: []byte(strings.Repeat("not a wheel ", 100)), status: http.StatusBadRequest},
		{name: "no METADATA", content: storedZip(t, false, "tool/__init__.py", "x = 1\n"), status: http.StatusBadRequest},
		{name: "METADATA of another version", content: other, status: http.StatusBadRequest},
		{name: "maxFileSize", policy: `{"maxFileSize":500}`, status: http.StatusRequestEntityTooLarge},
		{name: "the tag is taken in the registry", setup: func(t *testing.T, e *env) {
			e.squat(t, "tool", profile.Tag("1.0", "py3-none-any"))
		}, status: http.StatusConflict},
		{name: "METADATA read back of another version", filename: back, content: lying, readBack: true, status: http.StatusBadRequest},
		{name: "the registry serves other bytes of the METADATA read back", filename: back, content: backWheel, flip: true, readBack: true,
			status: http.StatusBadRequest},
	} {
		t.Run(tt.name, func(t *testing.T) {
			e := newEnvWith(t, keepAll)
			if tt.policy != "" {
				e.repo.Policy = []byte(tt.policy)
				must(t, e.st.UpdateRepo(t.Context(), &e.repo))
			}
			if tt.setup != nil {
				tt.setup(t, e)
			}
			pushed := len(e.fake.PushReferences())
			filename, content := wi.Filename, whl
			if tt.filename != "" {
				filename = tt.filename
			}
			if tt.content != nil {
				content = tt.content
			}
			if tt.flip {
				zr, err := zip.NewReader(bytes.NewReader(content), int64(len(content)))
				must(t, err)
				off, err := zr.File[0].DataOffset()
				must(t, err)
				e.prov.(*keeping).flip = off + 20 // in the METADATA
			}
			f := twineForm(filename, "tool", "1.0", PackagetypeWheel, content)
			if tt.edit != nil {
				tt.edit(&f)
			}
			if rr := e.post(t, f); rr.Code != tt.status {
				t.Fatalf("upload = %d %s, want %d", rr.Code, rr.Body, tt.status)
			}
			if !tt.readBack {
				e.nothingStored(t, "tool", content, pushed)
				return
			}
			e.noVersion(t, "tool", content)
		})
	}
}

// noVersion fails the test when a manifest was pushed, a session is open or the project has a
// version, or the file content is not in the registry: the outcome of a refusal after a
// read-back.
func (e *env) noVersion(t *testing.T, project string, content []byte) {
	t.Helper()
	rr, err := e.prov.Repository(t.Context(), "pypi/internal/"+project)
	must(t, err)
	stored, err := rr.Exists(t.Context(), ocispec.Descriptor{Digest: digest.FromBytes(content), Size: int64(len(content))})
	must(t, err)
	_, err = e.st.GetPackageByNameOCI(t.Context(), e.repo.ID, project)
	if !stored || e.fake.OpenUploads() != 0 || len(e.fake.PushReferences()) != 0 || err == nil {
		t.Errorf("file stored %v, %d sessions open, %d manifests pushed, package %v; want the file only", stored, e.fake.OpenUploads(),
			len(e.fake.PushReferences()), err)
	}
}

// TestUploadWheelStreams sends a wheel of 256 MiB over a real connection, its METADATA last as
// most wheels have it, with a TMPDIR that takes no file and chunks of 8 MiB, and checks that it
// is 200, read in one pass without a range request, and that the heap in use stayed under
// formattest.HeapLimit: the wheel is neither spooled nor held.
func TestUploadWheelStreams(t *testing.T) {
	if testing.Short() {
		t.Skip("moves 256 MiB; skipped under -short")
	}
	const size = 256 << 20
	formattest.NoTempDir(t)
	e := newEnv(t)
	e.fake.ChunkSize = 8 << 20
	srv := httptest.NewServer(e.h)
	defer srv.Close()
	peak := formattest.SampleHeap(t)

	pr, pw := io.Pipe()
	mw := multipart.NewWriter(pw)
	go func() {
		pw.CloseWithError(func() error {
			for k, v := range map[string]string{":action": "file_upload", "protocol_version": "1", "name": "big", "version": "1.0"} {
				if err := mw.WriteField(k, v); err != nil {
					return err
				}
			}
			w, err := mw.CreateFormFile("content", "big-1.0-py3-none-any.whl")
			if err != nil {
				return err
			}
			zw := zip.NewWriter(w)
			blob, err := zw.CreateHeader(&zip.FileHeader{Name: "big/data.bin", Method: zip.Store})
			if err != nil {
				return err
			}
			if _, err := io.Copy(blob, io.LimitReader(&formattest.Pattern{}, size)); err != nil {
				return err
			}
			md, err := zw.Create("big-1.0.dist-info/METADATA")
			if err != nil {
				return err
			}
			if _, err := io.WriteString(md, "Metadata-Version: 2.1\nName: big\nVersion: 1.0\n"); err != nil {
				return err
			}
			if err := zw.Close(); err != nil {
				return err
			}
			return mw.Close()
		}())
	}()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, srv.URL+"/pypi/internal/", pr)
	must(t, err)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	resp, err := http.DefaultClient.Do(req)
	must(t, err)
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("upload of %d MiB: %d %s", size>>20, resp.StatusCode, body)
	}
	for _, r := range e.fake.OpenBlobs() {
		if r.Range != "" {
			t.Errorf("a range read: %+v", r)
		}
	}
	top := peak()
	t.Logf("peak heap in use: %d MiB", top>>20)
	if top > formattest.HeapLimit {
		t.Errorf("peak heap in use %d MiB, want under %d MiB", top>>20, formattest.HeapLimit>>20)
	}
}
