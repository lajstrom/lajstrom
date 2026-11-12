package pypi

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/lajstrom/lajstrom/internal/format"
	"github.com/lajstrom/lajstrom/internal/format/formattest"
	"github.com/lajstrom/lajstrom/internal/index"
	"github.com/lajstrom/lajstrom/internal/oci/profile"
	"github.com/lajstrom/lajstrom/internal/testutil"
)

// sdistOf returns a .tar.gz sdist of the project tool at version, the members, PKG-INFO among
// them, in the order given: name and content.
func sdistOf(t *testing.T, version string, members ...string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for i := 0; i+1 < len(members); i += 2 {
		must(t, tw.WriteHeader(&tar.Header{Name: "tool-" + version + "/" + members[i], Mode: 0o644, Size: int64(len(members[i+1]))}))
		_, err := io.WriteString(tw, members[i+1])
		must(t, err)
	}
	must(t, tw.Close())
	must(t, gz.Close())
	return buf.Bytes()
}

// pkgInfo is the PKG-INFO of tool at version.
func pkgInfo(version string) string {
	return "Metadata-Version: 2.1\nName: tool\nVersion: " + version + "\n"
}

// TestUploadSdistWritesNoTempFile: with a TMPDIR that takes no file, an sdist whose PKG-INFO
// comes first, as setuptools writes it, and one whose PKG-INFO comes last, as flit and hatchling
// do, are uploaded and served: the sdist streams into the registry, its PKG-INFO read on the
// way, and nothing is spooled (ADR 0014). The fields may also come after the file. No upload
// session is left open.
func TestUploadSdistWritesNoTempFile(t *testing.T) {
	dir := formattest.NoTempDir(t)
	e := newEnv(t)
	e.fake.ChunkSize = 64
	rest := strings.Repeat("print('streamed')\n", 64)
	first := sdistOf(t, "1.0", "PKG-INFO", pkgInfo("1.0"), "tool/__init__.py", rest)
	last := sdistOf(t, "2.0", "tool/__init__.py", rest, "PKG-INFO", pkgInfo("2.0"))
	for _, f := range []form{twineForm("tool-1.0.tar.gz", "tool", "1.0", PackagetypeSdist, first),
		twineForm("tool-2.0.tar.gz", "tool", "2.0", PackagetypeSdist, last)} {
		if rr := e.post(t, f); rr.Code != http.StatusOK {
			t.Fatalf("upload %s: %d %s", f.filename, rr.Code, rr.Body)
		}
	}
	fieldsAfter := twineForm("tool-3.0.tar.gz", "tool", "3.0", PackagetypeSdist, sdistOf(t, "3.0", "PKG-INFO", pkgInfo("3.0")))
	if rr := e.postContentFirst(t, fieldsAfter); rr.Code != http.StatusOK {
		t.Fatalf("upload with the fields after the file: %d %s", rr.Code, rr.Body)
	}
	for _, v := range []struct{ file, body string }{{"1.0/tool-1.0.tar.gz", string(first)}, {"2.0/tool-2.0.tar.gz", string(last)}} {
		if got := e.do(http.MethodGet, "files/tool/"+v.file); got.Code != http.StatusOK || got.Body.String() != v.body {
			t.Errorf("GET %s = %d with %d bytes, want 200 with the %d uploaded", v.file, got.Code, got.Body.Len(), len(v.body))
		}
	}
	if n := e.fake.OpenUploads(); n != 0 {
		t.Errorf("%d upload sessions open", n)
	}
	if entries, err := os.ReadDir(dir); err != nil || len(entries) != 0 {
		t.Errorf("TMPDIR holds %v, %v", entries, err)
	}
}

// postContentFirst sends f with its file before its fields.
func (e *env) postContentFirst(t *testing.T, f form) *httptest.ResponseRecorder {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	w, err := mw.CreateFormFile("content", f.filename)
	must(t, err)
	_, err = w.Write(f.content)
	must(t, err)
	for k, v := range f.fields {
		must(t, mw.WriteField(k, v))
	}
	must(t, mw.Close())
	req := httptest.NewRequest(http.MethodPost, "/pypi/internal/", &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	rr := httptest.NewRecorder()
	e.h.ServeHTTP(rr, req)
	return rr
}

// streamed returns the file b of the name filename as ingest hands a streamed file to Build:
// written to the inspector of Inspect a few bytes at a time and closed, without Open, with its
// size and sha256.
func streamed(t *testing.T, project, filename string, b []byte) format.UploadedFile {
	t.Helper()
	in, err := Plugin{}.Inspect(index.Repo{Format: Name, Name: "internal"}, format.StreamUpload{Name: project,
		File: format.UploadedFile{Name: filename, Size: -1}})
	must(t, err)
	for rest := b; len(rest) > 0; rest = rest[min(len(rest), 100):] {
		_, err := in.Inspector.Write(rest[:min(len(rest), 100)])
		must(t, err)
	}
	must(t, in.Inspector.Close())
	return format.UploadedFile{Name: filename, Size: int64(len(b)), Inspector: in.Inspector,
		Checksums: map[string]string{"sha256": digest.FromBytes(b).Encoded()}}
}

// TestBuildStreamed: Build takes an sdist that streamed through the inspector of Inspect,
// without Open, from its sha256 and the PKG-INFO the inspector found, and leaves its layer out of
// the blobs; Validate takes the manifest when the fetch of the file is format.ErrStreamed, as
// ingest's is for the file of the upload, and not when the fetch fails otherwise. Inspect
// refuses a file of another project than the upload's.
func TestBuildStreamed(t *testing.T) {
	ctx := t.Context()
	tgz := sdistOf(t, "1.0", "tool/__init__.py", "x = 1\n", "PKG-INFO", pkgInfo("1.0")+"Requires-Dist: requests\n")
	f := streamed(t, "tool", "tool-1.0.tar.gz", tgz)
	built, err := Plugin{}.Build(ctx, repo, format.Upload{Files: []format.UploadedFile{f}})
	must(t, err)
	if l := built.Manifest.Layers[0]; l.Digest != digest.FromBytes(tgz) || l.Size != int64(len(tgz)) || len(built.Blobs) != 2 {
		t.Fatalf("the file layer %+v and %d blobs; want the sdist's sha256 and size, the config and the metadata", l, len(built.Blobs))
	}
	var md map[string]any
	must(t, json.Unmarshal(built.Entry.Metadata, &md))
	if md[FieldCoreMetadata] != pkgInfo("1.0")+"Requires-Dist: requests\n" {
		t.Errorf("core_metadata %q", md[FieldCoreMetadata])
	}
	served := fetcher(t, built)
	for _, tt := range []struct {
		err error
		ok  bool
	}{{fmt.Errorf("ingest: committed after Validate: %w", format.ErrStreamed), true}, {errors.New("connection reset"), false}} {
		fetch := func(ctx context.Context, d digest.Digest) (io.ReadCloser, error) {
			if d == built.Manifest.Layers[0].Digest {
				return nil, tt.err
			}
			return served(ctx, d)
		}
		if _, err := (Plugin{}).Validate(ctx, repo, built.Manifest, fetch); (err == nil) != tt.ok {
			t.Errorf("Validate with the fetch of the file %v: %v", tt.err, err)
		}
	}
	if _, err := (Plugin{}).Inspect(repo, format.StreamUpload{Name: "other", File: format.UploadedFile{Name: "tool-1.0.tar.gz"}}); !errors.Is(err, format.ErrInvalid) {
		t.Errorf("Inspect of tool-1.0.tar.gz for the project other = %v, want ErrInvalid", err)
	}
}

// TestUploadSdistRefusedStoresNothing: an sdist upload refused while it streams or after it, by
// a field before or after the file, the inspector, Build, the policy's maxFileSize, or the tag
// being taken in the registry, stores nothing: no blob, no session left open, no manifest, no
// version.
func TestUploadSdistRefusedStoresNothing(t *testing.T) {
	good := sdistOf(t, "1.0", "PKG-INFO", pkgInfo("1.0"), "tool/__init__.py", strings.Repeat("x = 1\n", 200))
	for _, tt := range []struct {
		name         string
		content      []byte
		edit         func(f *form)
		contentFirst bool
		policy       string
		setup        func(t *testing.T, e *env)
		status       int
	}{
		{name: "the sha256 of other bytes", edit: func(f *form) { f.fields["sha256_digest"] = strings.Repeat("ab", 32) }, status: http.StatusBadRequest},
		{name: "the sha256 of other bytes, after the file", edit: func(f *form) { f.fields["sha256_digest"] = strings.Repeat("ab", 32) },
			contentFirst: true, status: http.StatusBadRequest},
		{name: "another version, after the file", edit: func(f *form) { f.fields["version"] = "2.0" }, contentFirst: true, status: http.StatusBadRequest},
		{name: "another action", edit: func(f *form) { f.fields[":action"] = "submit" }, status: http.StatusBadRequest},
		{name: "not gzip", content: []byte(strings.Repeat("not a tarball ", 100)), status: http.StatusBadRequest},
		{name: "no PKG-INFO", content: sdistOf(t, "1.0", "tool/__init__.py", "x = 1\n"), status: http.StatusBadRequest},
		{name: "PKG-INFO of another version", content: sdistOf(t, "1.0", "PKG-INFO", pkgInfo("1.1")), status: http.StatusBadRequest},
		{name: "maxFileSize", policy: `{"maxFileSize":1000}`, status: http.StatusRequestEntityTooLarge},
		{name: "the tag is taken in the registry", setup: func(t *testing.T, e *env) {
			e.squat(t, "tool", profile.Tag("1.0", VariantSdist))
		}, status: http.StatusConflict},
	} {
		t.Run(tt.name, func(t *testing.T) {
			e := newEnv(t)
			e.fake.ChunkSize = 64
			if tt.policy != "" {
				e.repo.Policy = []byte(tt.policy)
				must(t, e.st.UpdateRepo(t.Context(), &e.repo))
			}
			if tt.setup != nil {
				tt.setup(t, e)
			}
			content := good
			if tt.content != nil {
				content = tt.content
			}
			f := twineForm("tool-1.0.tar.gz", "tool", "1.0", PackagetypeSdist, content)
			if tt.edit != nil {
				tt.edit(&f)
			}
			pushed := len(e.fake.PushReferences())
			var rr *httptest.ResponseRecorder
			if tt.contentFirst {
				rr = e.postContentFirst(t, f)
			} else {
				rr = e.post(t, f)
			}
			if rr.Code != tt.status {
				t.Fatalf("upload = %d %s, want %d", rr.Code, rr.Body, tt.status)
			}
			e.nothingStored(t, "tool", content, pushed)
		})
	}
}

// TestUploadZipDirectoryBombStoresNothing (#683): a wheel whose central directory has more than
// maxEntries headers, 12 MiB of them while the end record says 1, is refused with a 400 naming
// the bound when it has streamed, before the commit: nothing is stored, nothing read back.
func TestUploadZipDirectoryBombStoresNothing(t *testing.T) {
	e := newEnv(t)
	bomb := testutil.ZipDirectoryBomb(4<<16+1, 1)
	pushed := len(e.fake.PushReferences())
	rr := e.post(t, twineForm("tool-1.0-py3-none-any.whl", "tool", "1.0", PackagetypeWheel, bomb))
	if rr.Code != http.StatusBadRequest || !strings.Contains(rr.Body.String(), "more than 262144 entries") {
		t.Fatalf("upload = %d %s, want 400 naming the bound", rr.Code, rr.Body)
	}
	e.nothingStored(t, "tool", bomb, pushed)
}

// nothingStored fails the test when the registry holds content, the empty config or a blob
// pushed to the OCI repository of the project, a session is open, more manifests than pushed
// were pushed, or the project has a version.
func (e *env) nothingStored(t *testing.T, project string, content []byte, pushed int) {
	t.Helper()
	name := "pypi/internal/" + project
	rr, err := e.fake.Repository(t.Context(), name)
	must(t, err)
	stored, err := rr.Exists(t.Context(), ocispec.Descriptor{Digest: digest.FromBytes(content), Size: int64(len(content))})
	must(t, err)
	config, err := rr.Exists(t.Context(), profile.EmptyConfig)
	must(t, err)
	var blobs []digest.Digest
	for _, p := range e.fake.Pushes() {
		if p.Name == name {
			blobs = append(blobs, p.Digest)
		}
	}
	versions := 0
	if pkg, err := e.st.GetPackageByNameOCI(t.Context(), e.repo.ID, project); err == nil {
		vs, err := e.st.ListVersions(t.Context(), pkg.ID)
		must(t, err)
		versions = len(vs)
	}
	if stored || config || len(blobs) != 0 || e.fake.OpenUploads() != 0 || len(e.fake.PushReferences()) != pushed || versions != 0 {
		t.Errorf("file stored %v, empty config stored %v, blobs pushed %v, %d sessions open, %d manifests pushed (%d before), "+
			"%d versions; want nothing", stored, config, blobs, e.fake.OpenUploads(), len(e.fake.PushReferences()), pushed, versions)
	}
}

// squat pushes a manifest of nothing under tag to the OCI repository of project, straight to the
// registry: a write that bypassed Lajstrom.
func (e *env) squat(t *testing.T, project, tag string) {
	t.Helper()
	m := []byte(`{"schemaVersion":2,"mediaType":"` + ocispec.MediaTypeImageManifest + `",` +
		`"config":{"mediaType":"` + ocispec.MediaTypeEmptyJSON + `","digest":"` + ocispec.DescriptorEmptyJSON.Digest.String() + `","size":2},"layers":[]}`)
	rr, err := e.fake.Repository(t.Context(), "pypi/internal/"+project)
	must(t, err)
	d := ocispec.Descriptor{MediaType: ocispec.MediaTypeImageManifest, Digest: digest.FromBytes(m), Size: int64(len(m))}
	must(t, rr.PushReference(t.Context(), d, bytes.NewReader(m), tag))
}

// TestUploadSdistStreams uploads an sdist of 256 MiB over a real connection, its PKG-INFO
// first, with a TMPDIR that takes no file and the generic provider's chunks of 8 MiB, and checks
// that it is 200 and that the heap in use stayed under formattest.HeapLimit: the sdist is neither spooled
// nor held. The fake hashes the chunks and keeps none of a blob above providertest.MaxKept.
func TestUploadSdistStreams(t *testing.T) {
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
			for k, v := range map[string]string{":action": "file_upload", "protocol_version": "1", "name": "tool", "version": "1.0"} {
				if err := mw.WriteField(k, v); err != nil {
					return err
				}
			}
			w, err := mw.CreateFormFile("content", "tool-1.0.tar.gz")
			if err != nil {
				return err
			}
			gz, _ := gzip.NewWriterLevel(w, gzip.NoCompression)
			tw := tar.NewWriter(gz)
			for _, m := range []struct {
				name string
				size int64
				r    io.Reader
			}{{"PKG-INFO", int64(len(pkgInfo("1.0"))), strings.NewReader(pkgInfo("1.0"))}, {"data.bin", size, io.LimitReader(&formattest.Pattern{}, size)}} {
				if err := tw.WriteHeader(&tar.Header{Name: "tool-1.0/" + m.name, Mode: 0o644, Size: m.size}); err != nil {
					return err
				}
				if _, err := io.Copy(tw, m.r); err != nil {
					return err
				}
			}
			if err := tw.Close(); err != nil {
				return err
			}
			if err := gz.Close(); err != nil {
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
	top := peak()
	t.Logf("peak heap in use: %d MiB", top>>20)
	if top > formattest.HeapLimit {
		t.Errorf("peak heap in use %d MiB, want under %d MiB", top>>20, formattest.HeapLimit>>20)
	}
}
