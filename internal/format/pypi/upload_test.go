package pypi

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/md5" //nolint:gosec // the md5_digest twine sends
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"runtime"
	"strings"
	"testing"

	"github.com/opencontainers/go-digest"

	"github.com/lajstrom/lajstrom/internal/provider"
	"github.com/lajstrom/lajstrom/internal/provider/providertest"
	"github.com/lajstrom/lajstrom/internal/testutil"
)

// form is a twine upload: its fields and its file.
type form struct {
	fields   map[string]string
	filename string
	content  []byte
}

// twineForm is the form twine sends for a distribution: the action, the protocol, the name,
// version and filetype, and the sha256 and md5 of the file.
func twineForm(filename, name, version, filetype string, content []byte) form {
	s, m := sha256.Sum256(content), md5.Sum(content) //nolint:gosec // see the import
	return form{filename: filename, content: content, fields: map[string]string{
		":action": "file_upload", "protocol_version": "1", "name": name, "version": version, "filetype": filetype,
		"sha256_digest": hex.EncodeToString(s[:]), "md5_digest": hex.EncodeToString(m[:]), "metadata_version": "2.1",
	}}
}

// body encodes f as multipart/form-data, fields first, as twine does.
func (f form) body(t *testing.T) (io.Reader, string) {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	for k, v := range f.fields {
		must(t, mw.WriteField(k, v))
	}
	if f.filename != "" {
		w, err := mw.CreateFormFile("content", f.filename)
		must(t, err)
		_, err = w.Write(f.content)
		must(t, err)
	}
	must(t, mw.Close())
	return &buf, mw.FormDataContentType()
}

// post sends a form to POST /pypi/internal/.
func (e *env) post(t *testing.T, f form) *httptest.ResponseRecorder {
	t.Helper()
	body, ctype := f.body(t)
	req := httptest.NewRequest(http.MethodPost, "/pypi/internal/", body)
	req.Header.Set("Content-Type", ctype)
	rr := httptest.NewRecorder()
	e.h.ServeHTTP(rr, req)
	return rr
}

// TestUpload uploads a wheel and an sdist as twine does and finds them on the page; the same
// bytes again are 200, other bytes under the same file name are 409 "File already exists", the
// status twine --skip-existing skips.
func TestUpload(t *testing.T) {
	e := newEnv(t)
	whl, wi := testutil.Wheel(testutil.WheelOptions{Name: "My_Pkg", Version: "1.0"})
	tgz, si := testutil.Sdist(testutil.SdistOptions{Name: "My_Pkg", Version: "1.0"})
	for _, f := range []form{twineForm(wi.Filename, "My_Pkg", "1.0", PackagetypeWheel, whl), twineForm(si.Filename, "my-pkg", "1.0", PackagetypeSdist, tgz)} {
		if rr := e.post(t, f); rr.Code != http.StatusOK {
			t.Fatalf("upload %s: %d %s", f.filename, rr.Code, rr.Body)
		}
	}
	page := e.do(http.MethodGet, "simple/my-pkg/").Body.String()
	if !strings.Contains(page, wi.Filename) || !strings.Contains(page, si.Filename) {
		t.Errorf("the page after the uploads:\n%s", page)
	}
	if rr := e.post(t, twineForm(wi.Filename, "My_Pkg", "1.0", PackagetypeWheel, whl)); rr.Code != http.StatusOK {
		t.Errorf("the same bytes again: %d %s", rr.Code, rr.Body)
	}
	other, _ := testutil.Wheel(testutil.WheelOptions{Name: "My_Pkg", Version: "1.0", Summary: "rebuilt"})
	if rr := e.post(t, twineForm(wi.Filename, "My_Pkg", "1.0", PackagetypeWheel, other)); rr.Code != http.StatusConflict ||
		!strings.Contains(rr.Body.String(), "File already exists") {
		t.Errorf("other bytes under the file name: %d %s", rr.Code, rr.Body)
	}
}

// TestUploadRefuses checks the 400s: digests that are not the file's, md5 in base64 accepted,
// another action or protocol, no file, fields that disagree with the file, not a form; nothing
// is published by any of them.
func TestUploadRefuses(t *testing.T) {
	e := newEnv(t)
	whl, wi := testutil.Wheel(testutil.WheelOptions{Name: "tool", Version: "2.0"})
	good := func() form { return twineForm(wi.Filename, "tool", "2.0", PackagetypeWheel, whl) }
	for _, tt := range []struct {
		name string
		edit func(f *form)
	}{
		{"sha256 of other bytes", func(f *form) { f.fields["sha256_digest"] = strings.Repeat("ab", 32) }},
		{"md5 of other bytes", func(f *form) { f.fields["md5_digest"] = strings.Repeat("0", 32) }},
		{"blake2 of other bytes", func(f *form) { f.fields["blake2_256_digest"] = strings.Repeat("1", 64) }},
		{"another action", func(f *form) { f.fields[":action"] = "submit" }},
		{"another protocol", func(f *form) { f.fields["protocol_version"] = "2" }},
		{"no file", func(f *form) { f.filename = "" }},
		{"another name", func(f *form) { f.fields["name"] = "other" }},
		{"another version", func(f *form) { f.fields["version"] = "2.1" }},
		{"another filetype", func(f *form) { f.fields["filetype"] = PackagetypeSdist }},
		{"a file that is not a wheel", func(f *form) {
			f.content = []byte("not a zip")
			delete(f.fields, "sha256_digest")
			delete(f.fields, "md5_digest")
		}},
	} {
		f := good()
		tt.edit(&f)
		if rr := e.post(t, f); rr.Code != http.StatusBadRequest {
			t.Errorf("%s: %d %s, want 400", tt.name, rr.Code, rr.Body)
		}
	}
	req := httptest.NewRequest(http.MethodPost, "/pypi/internal/", strings.NewReader("name=tool"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rr := httptest.NewRecorder()
	e.h.ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Errorf("not multipart: %d", rr.Code)
	}
	if page := e.do(http.MethodGet, "simple/tool/"); page.Code != http.StatusNotFound {
		t.Errorf("a refused upload published: %d %s", page.Code, page.Body)
	}

	f := good()
	m := md5.Sum(whl) //nolint:gosec // see the import
	f.fields["md5_digest"] = base64.RawURLEncoding.EncodeToString(m[:])
	if rr := e.post(t, f); rr.Code != http.StatusOK {
		t.Errorf("md5 in URL-safe base64: %d %s", rr.Code, rr.Body)
	}
}

// TestUploadStreams sends a 200 MiB wheel generated as it is sent: the upload is spooled and
// read again without holding the file, so the bytes allocated are a fraction of it. The fake
// registry keeps no blob that large, so the range reads of Validate are served from a copy of
// the wheel the generator writes to disk, which allocates nothing either.
func TestUploadStreams(t *testing.T) {
	if testing.Short() {
		t.Skip("streams 200 MiB")
	}
	copyOnDisk, err := os.CreateTemp(t.TempDir(), "big-*.whl")
	must(t, err)
	t.Cleanup(func() { _ = copyOnDisk.Close() })
	e := newEnvWith(t, func(f *providertest.Fake) provider.Provider { return &fromDisk{Fake: f, file: copyOnDisk} })
	const size = 200 << 20
	pr, pw := io.Pipe()
	mw := multipart.NewWriter(pw)
	go func() {
		err := func() error {
			for k, v := range map[string]string{":action": "file_upload", "protocol_version": "1", "name": "big", "version": "1.0"} {
				if err := mw.WriteField(k, v); err != nil {
					return err
				}
			}
			w, err := mw.CreateFormFile("content", "big-1.0-py3-none-any.whl")
			if err != nil {
				return err
			}
			zw := zip.NewWriter(io.MultiWriter(w, copyOnDisk))
			blob, err := zw.CreateHeader(&zip.FileHeader{Name: "big/data.bin", Method: zip.Store})
			if err != nil {
				return err
			}
			chunk := make([]byte, 1<<20)
			for range size / len(chunk) {
				if _, err := blob.Write(chunk); err != nil {
					return err
				}
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
		}()
		pw.CloseWithError(err)
	}()
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	req := httptest.NewRequest(http.MethodPost, "/pypi/internal/", pr)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	rr := httptest.NewRecorder()
	e.h.ServeHTTP(rr, req)
	runtime.ReadMemStats(&after)
	if rr.Code != http.StatusOK {
		t.Fatalf("upload of 200 MiB: %d %s", rr.Code, rr.Body)
	}
	alloc := after.TotalAlloc - before.TotalAlloc
	t.Logf("the upload of %d MiB allocated %d MiB", size>>20, alloc>>20)
	if alloc > size/4 {
		t.Errorf("the upload of %d MiB allocated %d MiB", size>>20, alloc>>20)
	}
}

// fromDisk is the fake registry with the ranges of every blob it discarded served from file.
type fromDisk struct {
	*providertest.Fake
	file *os.File
}

func (d *fromDisk) OpenBlob(ctx context.Context, name string, dg digest.Digest, rangeHeader string, redirect bool) (provider.Blob, error) {
	b, err := d.Fake.OpenBlob(ctx, name, dg, rangeHeader, redirect)
	if err == nil || rangeHeader == "" {
		return b, err
	}
	var first, last int64
	if _, serr := fmt.Sscanf(rangeHeader, "bytes=%d-%d", &first, &last); serr != nil {
		return b, err
	}
	return provider.Blob{Status: http.StatusPartialContent, Size: last - first + 1, Digest: dg,
		Body: io.NopCloser(io.NewSectionReader(d.file, first, last-first+1))}, nil
}
