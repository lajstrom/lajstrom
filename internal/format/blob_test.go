package format_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/lajstrom/lajstrom/internal/format"
	"github.com/lajstrom/lajstrom/internal/index"
	"github.com/lajstrom/lajstrom/internal/provider"
	"github.com/lajstrom/lajstrom/internal/provider/providertest"
)

// blobName is the OCI repository of the blobs of these tests.
const blobName = "raw/scratch/tool"

// blobProvider is the fake registry with OpenBlob and StatBlob replaced when open or stat is
// set.
type blobProvider struct {
	*providertest.Fake
	open func(ctx context.Context, name string, d digest.Digest, rangeHeader string, redirect bool) (provider.Blob, error)
	stat func(ctx context.Context, name string, d digest.Digest) (provider.Blob, error)
}

func (p *blobProvider) OpenBlob(ctx context.Context, name string, d digest.Digest, rangeHeader string, redirect bool) (provider.Blob, error) {
	if p.open != nil {
		return p.open(ctx, name, d, rangeHeader, redirect)
	}
	return p.Fake.OpenBlob(ctx, name, d, rangeHeader, redirect)
}

func (p *blobProvider) StatBlob(ctx context.Context, name string, d digest.Digest) (provider.Blob, error) {
	if p.stat != nil {
		return p.stat(ctx, name, d)
	}
	return p.Fake.StatBlob(ctx, name, d)
}

// newBlobFake returns the fake registry with content as a blob of blobName, and its digest.
func newBlobFake(t *testing.T, content string) (*providertest.Fake, digest.Digest) {
	t.Helper()
	f := &providertest.Fake{}
	rr, err := f.Repository(t.Context(), blobName)
	if err != nil {
		t.Fatal(err)
	}
	d := digest.FromString(content)
	if err := rr.Push(t.Context(), descriptorOf(d, len(content)), strings.NewReader(content)); err != nil {
		t.Fatal(err)
	}
	return f, d
}

// serveBlob runs ServeBlob for method with the header pairs hdr and returns the response and
// the error ServeBlob returned.
func serveBlob(t *testing.T, p provider.Provider, d digest.Digest, opts format.ServeBlobOptions, method string, hdr ...string) (*httptest.ResponseRecorder, error) {
	t.Helper()
	req := httptest.NewRequest(method, "/raw/scratch/tool", nil)
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	rr := httptest.NewRecorder()
	err := format.ServeBlob(rr, req, p, blobName, d, opts)
	return rr, err
}

// wantHeaders fails t when a header of rr is not the one in want; "" means absent.
func wantHeaders(t *testing.T, what string, rr *httptest.ResponseRecorder, want map[string]string) {
	t.Helper()
	for k, v := range want {
		if got := rr.Header().Get(k); got != v {
			t.Errorf("%s: %s = %q, want %q", what, k, got, v)
		}
	}
}

// TestServeBlob checks the answers of ServeBlob over the fake registry: the stream with every
// header, a single range passed to the backend and its 206, the Ranges that are not sent, the
// 416, HEAD without a body and without OpenBlob, and the 304 before any request.
func TestServeBlob(t *testing.T) {
	const content = "0123456789abcdefghij"
	modified := time.Date(2026, 11, 7, 10, 0, 0, 0, time.UTC)
	lastModified := modified.Format(http.TimeFormat)
	tests := []struct {
		name      string
		method    string
		hdr       []string
		status    int
		body      string
		headers   map[string]string
		opened    []string // the Range of each OpenBlob; nil: no OpenBlob
		statCalls int
	}{
		{
			name: "200", method: http.MethodGet, status: 200, body: content, opened: []string{""},
			headers: map[string]string{"Content-Type": "text/plain", "Content-Length": "20", "Accept-Ranges": "bytes",
				"Last-Modified": lastModified, "Content-Range": "", "Location": "", "Content-Disposition": ""},
		},
		{
			name: "206", method: http.MethodGet, hdr: []string{"Range", "bytes=2-5"}, status: 206, body: "2345", opened: []string{"bytes=2-5"},
			headers: map[string]string{"Content-Range": "bytes 2-5/20", "Content-Length": "4", "Content-Type": "text/plain", "Accept-Ranges": "bytes"},
		},
		{name: "suffix", method: http.MethodGet, hdr: []string{"Range", "bytes=-3"}, status: 206, body: "hij", opened: []string{"bytes=-3"}},
		{name: "open-ended, unit in capitals", method: http.MethodGet, hdr: []string{"Range", " BYTES = 17-"}, status: 206, body: "hij", opened: []string{"bytes=17-"}},
		{name: "multi-range: not sent, 200 whole", method: http.MethodGet, hdr: []string{"Range", "bytes=0-1,4-5"}, status: 200, body: content, opened: []string{""}},
		{name: "first after last: not sent", method: http.MethodGet, hdr: []string{"Range", "bytes=5-2"}, status: 200, body: content, opened: []string{""}},
		{name: "not a number: not sent", method: http.MethodGet, hdr: []string{"Range", "bytes=a-"}, status: 200, body: content, opened: []string{""}},
		{name: "no positions: not sent", method: http.MethodGet, hdr: []string{"Range", "bytes=-"}, status: 200, body: content, opened: []string{""}},
		{name: "no dash: not sent", method: http.MethodGet, hdr: []string{"Range", "bytes=5"}, status: 200, body: content, opened: []string{""}},
		{name: "another unit: not sent", method: http.MethodGet, hdr: []string{"Range", "items=1-2"}, status: 200, body: content, opened: []string{""}},
		{
			name: "416", method: http.MethodGet, hdr: []string{"Range", "bytes=20-"}, status: 416, opened: []string{"bytes=20-"},
			headers: map[string]string{"Content-Range": "bytes */20", "Content-Type": "", "Content-Length": ""},
		},
		{
			name: "HEAD", method: http.MethodHead, status: 200, statCalls: 1,
			headers: map[string]string{"Content-Type": "text/plain", "Content-Length": "20", "Accept-Ranges": "bytes", "Last-Modified": lastModified},
		},
		{name: "If-None-Match", method: http.MethodGet, hdr: []string{"If-None-Match", "ETAG"}, status: 304},
		{name: "If-None-Match on HEAD", method: http.MethodHead, hdr: []string{"If-None-Match", "ETAG"}, status: 304},
		{name: "If-None-Match in a list, weak", method: http.MethodGet, hdr: []string{"If-None-Match", `"other", W/ETAG`}, status: 304},
		{name: "If-None-Match *", method: http.MethodGet, hdr: []string{"If-None-Match", "*"}, status: 304},
		{name: "If-None-Match of another", method: http.MethodGet, hdr: []string{"If-None-Match", `"other"`, "If-Modified-Since", lastModified}, status: 200, body: content, opened: []string{""}},
		{name: "If-None-Match that does not parse", method: http.MethodGet, hdr: []string{"If-None-Match", `other, ETAG`}, status: 200, body: content, opened: []string{""}},
		{name: "If-Modified-Since", method: http.MethodGet, hdr: []string{"If-Modified-Since", lastModified}, status: 304},
		{name: "If-Modified-Since before", method: http.MethodGet, hdr: []string{"If-Modified-Since", modified.Add(-time.Hour).Format(http.TimeFormat)}, status: 200, body: content, opened: []string{""}},
		{name: "If-Modified-Since that does not parse", method: http.MethodGet, hdr: []string{"If-Modified-Since", "yesterday"}, status: 200, body: content, opened: []string{""}},
		{name: "If-Match of another", method: http.MethodGet, hdr: []string{"If-Match", `"other"`}, status: 412},
		{name: "If-Match weak", method: http.MethodGet, hdr: []string{"If-Match", "W/ETAG"}, status: 412},
		{name: "If-Match", method: http.MethodGet, hdr: []string{"If-Match", "ETAG"}, status: 200, body: content, opened: []string{""}},
		{name: "If-Match *", method: http.MethodGet, hdr: []string{"If-Match", "*"}, status: 200, body: content, opened: []string{""}},
		{name: "If-Unmodified-Since before", method: http.MethodGet, hdr: []string{"If-Unmodified-Since", modified.Add(-time.Hour).Format(http.TimeFormat)}, status: 412},
		{name: "If-Unmodified-Since", method: http.MethodGet, hdr: []string{"If-Unmodified-Since", lastModified}, status: 200, body: content, opened: []string{""}},
		{name: "If-Range with the ETag", method: http.MethodGet, hdr: []string{"Range", "bytes=0-1", "If-Range", "ETAG"}, status: 206, body: "01", opened: []string{"bytes=0-1"}},
		{name: "If-Range with another ETag", method: http.MethodGet, hdr: []string{"Range", "bytes=0-1", "If-Range", `"other"`}, status: 200, body: content, opened: []string{""}},
		{name: "If-Range weak", method: http.MethodGet, hdr: []string{"Range", "bytes=0-1", "If-Range", "W/ETAG"}, status: 200, body: content, opened: []string{""}},
		{name: "If-Range with the date", method: http.MethodGet, hdr: []string{"Range", "bytes=0-1", "If-Range", lastModified}, status: 206, body: "01", opened: []string{"bytes=0-1"}},
		{name: "If-Range with another date", method: http.MethodGet, hdr: []string{"Range", "bytes=0-1", "If-Range", modified.Add(time.Hour).Format(http.TimeFormat)}, status: 200, body: content, opened: []string{""}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f, d := newBlobFake(t, content)
			etag := `"` + d.String() + `"`
			hdr := make([]string, len(tt.hdr))
			for i, v := range tt.hdr {
				hdr[i] = strings.ReplaceAll(v, "ETAG", etag)
			}
			rr, err := serveBlob(t, f, d, format.ServeBlobOptions{ContentType: "text/plain", LastModified: modified.Add(500 * time.Millisecond), Redirect: true},
				tt.method, hdr...)
			if err != nil {
				t.Errorf("ServeBlob returned %v", err)
			}
			if rr.Code != tt.status || rr.Body.String() != tt.body {
				t.Errorf("response = %d %q, want %d %q", rr.Code, rr.Body, tt.status, tt.body)
			}
			if rr.Header().Get("ETag") != etag {
				t.Errorf("ETag = %q, want %q", rr.Header().Get("ETag"), etag)
			}
			if tt.status != 304 && tt.status != 412 && rr.Header().Get("Docker-Content-Digest") != d.String() {
				t.Errorf("Docker-Content-Digest = %q", rr.Header().Get("Docker-Content-Digest"))
			}
			wantHeaders(t, tt.name, rr, tt.headers)
			wantBlobReads(t, f, d, tt.opened, tt.statCalls)
		})
	}
}

// wantBlobReads fails t unless the fake got OpenBlob calls for d with the ranges opened, and
// stats StatBlob calls.
func wantBlobReads(t *testing.T, f *providertest.Fake, d digest.Digest, opened []string, stats int) {
	t.Helper()
	var got []string
	for _, o := range f.OpenBlobs() {
		if o.Name != blobName || o.Digest != d {
			t.Errorf("OpenBlob of %s@%s", o.Name, o.Digest)
		}
		got = append(got, o.Range)
	}
	if fmt.Sprint(got) != fmt.Sprint(opened) || len(got) != len(opened) {
		t.Errorf("OpenBlob ranges %q, want %q", got, opened)
	}
	if n := len(f.StatBlobs()); n != stats {
		t.Errorf("%d StatBlob calls, want %d", n, stats)
	}
}

// TestServeBlobRedirect checks the policy switch: a 307 with Location and the
// Docker-Content-Digest when the fake redirects and Redirect is true, the stream when Redirect
// is false although the fake could redirect, and HEAD, which never redirects.
func TestServeBlobRedirect(t *testing.T) {
	const content = "redirected bytes"
	f, d := newBlobFake(t, content)
	f.RedirectURL = func(name string, d digest.Digest) (string, bool) {
		return "https://objects.test/" + name + "?sig=" + d.Encoded(), true
	}
	manifest := digest.FromString("the manifest")
	opts := format.ServeBlobOptions{ContentType: "application/octet-stream", ManifestDigest: manifest, Redirect: true}

	rr, err := serveBlob(t, f, d, opts, http.MethodGet, "Range", "bytes=1-2")
	if err != nil || rr.Code != http.StatusTemporaryRedirect || rr.Body.Len() != 0 {
		t.Errorf("GET with Redirect = %d %q, %v; want 307", rr.Code, rr.Body, err)
	}
	wantHeaders(t, "307", rr, map[string]string{"Location": "https://objects.test/" + blobName + "?sig=" + d.Encoded(),
		"Docker-Content-Digest": manifest.String(), "Content-Length": ""})

	opts.Redirect = false
	rr, err = serveBlob(t, f, d, opts, http.MethodGet)
	if err != nil || rr.Code != http.StatusOK || rr.Body.String() != content {
		t.Errorf("GET without Redirect = %d %q, %v; want the stream", rr.Code, rr.Body, err)
	}
	wantHeaders(t, "stream", rr, map[string]string{"Location": "", "Docker-Content-Digest": manifest.String()})

	opts.Redirect = true
	rr, err = serveBlob(t, f, d, opts, http.MethodHead)
	if err != nil || rr.Code != http.StatusOK || rr.Body.Len() != 0 {
		t.Errorf("HEAD with Redirect = %d %q, %v; want 200 without a body", rr.Code, rr.Body, err)
	}
	if opened := f.OpenBlobs(); len(opened) != 2 || opened[0].Range != "bytes=1-2" {
		t.Errorf("OpenBlobs = %+v", opened)
	}
}

// TestServeBlobOptions checks ETag, ManifestDigest and Filename, and that a redirect's body is
// closed.
func TestServeBlobOptions(t *testing.T) {
	const content = "a file"
	f, d := newBlobFake(t, content)
	rr, _ := serveBlob(t, f, d, format.ServeBlobOptions{ContentType: "application/x-tar", ETag: `"v1"`,
		ManifestDigest: digest.FromString("m"), Filename: "tool-1.0.tar"}, http.MethodGet)
	wantHeaders(t, "options", rr, map[string]string{"ETag": `"v1"`, "Docker-Content-Digest": digest.FromString("m").String(),
		"Content-Disposition": `attachment; filename=tool-1.0.tar`, "Last-Modified": ""})
	rr, _ = serveBlob(t, f, d, format.ServeBlobOptions{ContentType: "text/plain", Filename: "файл.txt"}, http.MethodHead)
	wantHeaders(t, "a name that is not ASCII", rr, map[string]string{"Content-Disposition": `attachment; filename*=utf-8''%D1%84%D0%B0%D0%B9%D0%BB.txt`})
	if rr, _ := serveBlob(t, f, d, format.ServeBlobOptions{ContentType: "text/plain", ETag: `"v1"`}, http.MethodGet, "If-None-Match", `"v1"`); rr.Code != http.StatusNotModified {
		t.Errorf("If-None-Match with the ETag of the options = %d", rr.Code)
	}

	closed := false
	p := &blobProvider{Fake: f, open: func(context.Context, string, digest.Digest, string, bool) (provider.Blob, error) {
		return provider.Blob{RedirectURL: "https://objects.test/x", Body: closer{&closed}, Digest: d}, nil
	}}
	if rr, _ := serveBlob(t, p, d, format.ServeBlobOptions{ContentType: "text/plain", Redirect: true}, http.MethodGet); rr.Code != http.StatusTemporaryRedirect || !closed {
		t.Errorf("a redirect with a body = %d, body closed %v", rr.Code, closed)
	}
}

// closer is a body that records that it was closed.
type closer struct{ closed *bool }

func (closer) Read([]byte) (int, error) { return 0, io.EOF }
func (c closer) Close() error           { *c.closed = true; return nil }

// brokenBody gives n bytes and then fails with err, or ends early when err is nil.
type brokenBody struct {
	n   int
	err error
}

func (b *brokenBody) Read(p []byte) (int, error) {
	if b.n == 0 {
		if b.err == nil {
			return 0, io.EOF
		}
		return 0, b.err
	}
	k := min(len(p), b.n)
	clear(p[:k])
	b.n -= k
	return k, nil
}

func (*brokenBody) Close() error { return nil }

// TestServeBlobErrors checks that the errors before the response started go through Fail,
// WriteError without one.
func TestServeBlobErrors(t *testing.T) {
	f, d := newBlobFake(t, "content")
	boom := errors.New("boom")
	opts := format.ServeBlobOptions{ContentType: "text/plain"}

	rr, err := serveBlob(t, f, digest.FromString("missing"), opts, http.MethodGet)
	if err != nil || rr.Code != http.StatusNotFound || !strings.Contains(rr.Body.String(), "not found") {
		t.Errorf("GET of a missing blob = %d %q, %v; want 404 from WriteError", rr.Code, rr.Body, err)
	}
	rr, _ = serveBlob(t, f, digest.FromString("missing"), opts, http.MethodHead)
	if rr.Code != http.StatusNotFound {
		t.Errorf("HEAD of a missing blob = %d, want 404", rr.Code)
	}

	var failed []error
	withFail := opts
	withFail.Fail = func(w http.ResponseWriter, err error) {
		failed = append(failed, err)
		w.WriteHeader(599)
	}
	failing := &blobProvider{Fake: f,
		open: func(context.Context, string, digest.Digest, string, bool) (provider.Blob, error) {
			return provider.Blob{}, boom
		},
		stat: func(context.Context, string, digest.Digest) (provider.Blob, error) { return provider.Blob{}, boom },
	}
	for _, method := range []string{http.MethodGet, http.MethodHead} {
		if rr, err := serveBlob(t, failing, d, withFail, method); err != nil || rr.Code != 599 {
			t.Errorf("%s with a failing provider = %d, %v; want Fail's answer", method, rr.Code, err)
		}
	}
	noBody := &blobProvider{Fake: f, open: func(context.Context, string, digest.Digest, string, bool) (provider.Blob, error) {
		return provider.Blob{Status: http.StatusOK, Size: 3, Digest: d}, nil
	}}
	if rr, _ := serveBlob(t, noBody, d, withFail, http.MethodGet); rr.Code != 599 {
		t.Errorf("a provider answer without a body = %d", rr.Code)
	}
	if rr, _ := serveBlob(t, f, d, format.ServeBlobOptions{Fail: withFail.Fail}, http.MethodGet); rr.Code != 599 {
		t.Errorf("no ContentType = %d", rr.Code)
	}
	if rr, _ := serveBlob(t, nil, d, opts, http.MethodGet); rr.Code != http.StatusInternalServerError {
		t.Errorf("no provider = %d, want 500 from WriteError", rr.Code)
	}
	if len(failed) != 4 || !errors.Is(failed[0], boom) || !errors.Is(failed[1], boom) {
		t.Errorf("Fail got %v", failed)
	}
}

// TestServeBlobStreamErrors checks that a stream that breaks after the headers is the error
// ServeBlob returns, a body shorter than its size too, and a client that went away none.
func TestServeBlobStreamErrors(t *testing.T) {
	f, d := newBlobFake(t, "content")
	boom := errors.New("boom")
	opts := format.ServeBlobOptions{ContentType: "text/plain"}
	for _, tt := range []struct {
		name string
		body *brokenBody
		want error
	}{
		{"a read error", &brokenBody{n: 3, err: boom}, boom},
		{"a body shorter than its size", &brokenBody{n: 3}, io.ErrUnexpectedEOF},
	} {
		broken := &blobProvider{Fake: f, open: func(context.Context, string, digest.Digest, string, bool) (provider.Blob, error) {
			return provider.Blob{Body: tt.body, Size: 10, Digest: d}, nil // Status 0 is 200
		}}
		rr, err := serveBlob(t, broken, d, opts, http.MethodGet)
		if !errors.Is(err, tt.want) || rr.Code != http.StatusOK || rr.Body.Len() != 3 || rr.Header().Get("Content-Length") != "10" {
			t.Errorf("%s: %d with %d bytes, %v; want 200 with 3 bytes and %v", tt.name, rr.Code, rr.Body.Len(), err, tt.want)
		}
	}

	ctx, cancel := context.WithCancel(t.Context())
	gone := &blobProvider{Fake: f, open: func(context.Context, string, digest.Digest, string, bool) (provider.Blob, error) {
		cancel() // the client goes away while the bytes flow
		return provider.Blob{Status: http.StatusOK, Body: &brokenBody{n: 3, err: context.Canceled}, Size: 10, Digest: d}, nil
	}}
	req := httptest.NewRequestWithContext(ctx, http.MethodGet, "/raw/scratch/tool", nil)
	if err := format.ServeBlob(httptest.NewRecorder(), req, gone, blobName, d, opts); err != nil {
		t.Errorf("a client that went away: %v, want nil", err)
	}
}

// TestBlobRedirect checks the lenient reading of policy.blobRedirect.
func TestBlobRedirect(t *testing.T) {
	tests := []struct {
		policy string
		want   bool
	}{
		{"", true},
		{`{}`, true},
		{`{"blobRedirect": "auto"}`, true},
		{`{"blobRedirect": "never"}`, false},
		{`{"maxFileSize": 10, "blobRedirect": "never"}`, false},
		{`{"blobRedirect": "Never"}`, true},
		{`{"blobRedirect": false}`, true},
		{`{"blobRedirect": null}`, true},
		{`[1, 2]`, true},
		{`{"blobRedirect": "never"`, true},
	}
	for _, tt := range tests {
		repo := index.Repo{Format: "raw", Name: "scratch"}
		if tt.policy != "" {
			repo.Policy = json.RawMessage(tt.policy)
		}
		if got := format.BlobRedirect(repo); got != tt.want {
			t.Errorf("BlobRedirect(%s) = %v, want %v", tt.policy, got, tt.want)
		}
	}
}

// descriptorOf describes a blob of size bytes for a push to the fake.
func descriptorOf(d digest.Digest, size int) ocispec.Descriptor {
	return ocispec.Descriptor{MediaType: "application/octet-stream", Digest: d, Size: int64(size)}
}
