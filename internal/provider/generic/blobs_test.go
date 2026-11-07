package generic

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/opencontainers/go-digest"
	"oras.land/oras-go/v2/content"

	"github.com/lajstrom/lajstrom/internal/config"
	"github.com/lajstrom/lajstrom/internal/provider"
)

// objectStore is another origin that serves the blobs of one repository of a fake registry, as
// the object storage behind a registry's redirect does: GET and HEAD of /bucket/<digest>, with
// ranges. It records each request with its Range and Authorization.
type objectStore struct {
	srv *httptest.Server

	mu       sync.Mutex
	requests []string // "GET /bucket/<digest> Range=<range> Authorization=<authorization>"
}

func newObjectStore(t *testing.T, f *fakeRegistry, repo string) *objectStore {
	t.Helper()
	s := &objectStore{}
	s.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		s.requests = append(s.requests, fmt.Sprintf("%s %s Range=%s Authorization=%s", r.Method, r.URL.Path,
			r.Header.Get("Range"), r.Header.Get("Authorization")))
		s.mu.Unlock()
		f.mu.Lock()
		b, ok := f.blobs[repo][digest.Digest(strings.TrimPrefix(r.URL.Path, "/bucket/"))]
		f.mu.Unlock()
		if !ok {
			http.Error(w, "<Error><Code>NoSuchKey</Code></Error>", http.StatusNotFound)
			return
		}
		http.ServeContent(w, r, "", time.Time{}, bytes.NewReader(b))
	}))
	t.Cleanup(s.srv.Close)
	return s
}

// log returns the requests so far.
func (s *objectStore) log() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.requests)
}

// redirectHook answers the blob requests of the fake registry without the query followed=1
// with status and the Location loc, and passes the others on.
func redirectHook(status int, loc string) func(w http.ResponseWriter, r *http.Request) bool {
	return func(w http.ResponseWriter, r *http.Request) bool {
		if !strings.Contains(r.URL.Path, "/blobs/") || r.URL.Query().Get("followed") == "1" {
			return false
		}
		if loc != "" {
			w.Header().Set("Location", loc)
		}
		w.WriteHeader(status)
		return true
	}
}

// TestOpenBlob checks OpenBlob against the fake registry: the first answer used as it is (200,
// 206, 416, a redirect to another origin returned), the redirects it follows (to the
// registry's own origin always, all of them with redirect false or capabilities.blobRedirect
// false, at most maxRedirects), and the errors.
func TestOpenBlob(t *testing.T) {
	const name = "raw/scratch/notes"
	const repo = "art/" + name
	data := []byte("0123456789abcdef")
	d := digest.FromBytes(data)
	tests := []struct {
		name     string
		rng      string
		noFollow bool // the redirect argument is false
		d        digest.Digest
		setup    func(f *fakeRegistry, store string)
		edit     func(c *config.Registry)
		want     openWant
	}{
		{name: "200", want: openWant{status: 200, body: string(data), size: 16, registry: 1}},
		{name: "206", rng: "bytes=2-5", want: openWant{status: 206, body: "2345", size: 4, contentRange: "bytes 2-5/16", registry: 1}},
		{name: "416", rng: "bytes=99-", want: openWant{status: 416, contentRange: "bytes */16", registry: 1}},
		{name: "404", d: digest.FromString("missing"), want: openWant{err: provider.ErrNotFound, registry: 1}},
		{
			name:  "a redirect to another origin is returned",
			rng:   "bytes=2-5",
			setup: func(f *fakeRegistry, store string) { f.redirect = store + "/bucket" },
			want:  openWant{redirect: func(store string) string { return store + "/bucket/" + d.String() }, size: -1, status: 307, registry: 1},
		},
		{
			name:     "with redirect false every redirect is followed",
			rng:      "bytes=2-5",
			noFollow: true,
			setup:    func(f *fakeRegistry, store string) { f.redirect = store + "/bucket" },
			want: openWant{status: 206, body: "2345", size: 4, contentRange: "bytes 2-5/16", registry: 1,
				store: []string{"GET /bucket/" + d.String() + " Range=bytes=2-5 Authorization="}},
		},
		{
			name:  "with capabilities.blobRedirect false every redirect is followed",
			setup: func(f *fakeRegistry, store string) { f.redirect = store + "/bucket" },
			edit:  func(c *config.Registry) { c.Capabilities.BlobRedirect = new(false) },
			want: openWant{status: 200, body: string(data), size: 16, registry: 1,
				store: []string{"GET /bucket/" + d.String() + " Range= Authorization="}},
		},
		{
			name: "a redirect into the registry's own API is followed",
			rng:  "bytes=-3",
			setup: func(f *fakeRegistry, _ string) {
				f.hook = redirectHook(307, "/v2/"+repo+"/blobs/"+d.String()+"?followed=1")
			},
			want: openWant{status: 206, body: "def", size: 3, contentRange: "bytes 13-15/16", registry: 2},
		},
		{
			name: "a redirect to the registry's origin outside the API is followed",
			setup: func(f *fakeRegistry, _ string) {
				f.hook = redirectHook(http.StatusFound, f.srv.URL+"/v2/"+repo+"/blobs/"+d.String()+"?followed=1")
			},
			want: openWant{status: 200, body: string(data), size: 16, registry: 2},
		},
		{
			name: "the registry's origin with its default port written is the same origin",
			setup: func(f *fakeRegistry, _ string) {
				f.hook = redirectHook(http.StatusSeeOther, "/v2/"+repo+"/blobs/"+d.String()+"?followed=1")
			},
			want: openWant{status: 200, body: string(data), size: 16, registry: 2},
		},
		{
			name: "more than maxRedirects",
			setup: func(f *fakeRegistry, _ string) {
				f.hook = redirectHook(http.StatusTemporaryRedirect, "/storage/blobs/again")
			},
			want: openWant{err: errAny, registry: maxRedirects + 1},
		},
		{
			name:  "a redirect without a Location",
			setup: func(f *fakeRegistry, _ string) { f.hook = redirectHook(http.StatusFound, "") },
			want:  openWant{err: errAny, registry: 1},
		},
		{
			name: "another digest than asked",
			setup: func(f *fakeRegistry, _ string) {
				f.hook = func(w http.ResponseWriter, _ *http.Request) bool {
					w.Header().Set("Docker-Content-Digest", digest.FromString("other").String())
					_, _ = w.Write(data)
					return true
				}
			},
			want: openWant{err: content.ErrMismatchedDigest, registry: 1},
		},
		{
			name: "a Docker-Content-Digest that does not parse",
			setup: func(f *fakeRegistry, _ string) {
				f.hook = func(w http.ResponseWriter, _ *http.Request) bool {
					w.Header().Set("Docker-Content-Digest", "sha256:nothex")
					_, _ = w.Write(data)
					return true
				}
			},
			want: openWant{err: content.ErrMismatchedDigest, registry: 1},
		},
		{
			name: "no Content-Length and no Docker-Content-Digest",
			setup: func(f *fakeRegistry, _ string) {
				f.hook = func(w http.ResponseWriter, _ *http.Request) bool {
					w.(http.Flusher).Flush() // chunked: no Content-Length
					_, _ = w.Write(data)
					return true
				}
			},
			want: openWant{status: 200, body: string(data), size: -1, registry: 1},
		},
		{
			name: "the registry refuses Lajstrom",
			setup: func(f *fakeRegistry, _ string) {
				f.hook = func(w http.ResponseWriter, _ *http.Request) bool {
					writeErr(w, http.StatusForbidden, "DENIED", "requested access to the resource is denied")
					return true
				}
			},
			want: openWant{err: provider.ErrUnauthorized, registry: 1},
		},
		{name: "invalid digest", d: "sha256:short", want: openWant{err: errAny}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFakeRegistry(t)
			f.blobs[repo] = map[digest.Digest][]byte{d: data}
			store := newObjectStore(t, f, repo)
			if tt.setup != nil {
				tt.setup(f, store.srv.URL)
			}
			asked := tt.d
			if asked == "" {
				asked = d
			}
			log, _ := testLogger()
			got, err := f.provider(t, log, tt.edit).OpenBlob(context.Background(), name, asked, tt.rng, !tt.noFollow)
			checkOpenBlob(t, f, store, got, err, asked, tt.want, tt.setup == nil)
		})
	}
}

// openWant is what an OpenBlob of TestOpenBlob should give.
type openWant struct {
	status       int
	body         string
	size         int64
	contentRange string
	redirect     func(store string) string // nil: no RedirectURL
	err          error
	registry     int      // GETs the registry gets
	store        []string // the object store's requests
}

// checkOpenBlob fails t unless the answer got, err of an OpenBlob of the blob asked is w, and
// the registry and the object store got the requests w says. With ranges, a 200 or 206 must
// say Accept-Ranges, as the fake registry does.
func checkOpenBlob(t *testing.T, f *fakeRegistry, store *objectStore, got provider.Blob, err error, asked digest.Digest, w openWant, ranges bool) {
	t.Helper()
	wantErr(t, "OpenBlob", err, w.err)
	body := readBody(t, got)
	wantRedirect := ""
	if w.redirect != nil {
		wantRedirect = w.redirect(store.srv.URL)
	}
	if err == nil && (got.Status != w.status || body != w.body || got.Size != w.size || got.ContentRange != w.contentRange ||
		got.RedirectURL != wantRedirect || got.Digest != asked) {
		t.Errorf("OpenBlob = %+v with %q; want %+v and %q", got, body, w, wantRedirect)
	}
	if err == nil && (w.status == 200 || w.status == 206) && !got.AcceptRanges && ranges {
		t.Errorf("AcceptRanges false; the fake registry sends Accept-Ranges: bytes")
	}
	checkRequests(t, f, store, w)
}

// checkRequests fails t unless the registry got w.registry GETs and the object store the
// requests w.store.
func checkRequests(t *testing.T, f *fakeRegistry, store *objectStore, w openWant) {
	t.Helper()
	if n := f.count("GET "); n != w.registry {
		t.Errorf("%d GETs to the registry, want %d:\n%s", n, w.registry, strings.Join(f.log(), "\n"))
	}
	if s := store.log(); !slices.Equal(s, w.store) {
		t.Errorf("object store requests %q, want %q", s, w.store)
	}
}

// readBody reads and closes the body of b; "" without one.
func readBody(t *testing.T, b provider.Blob) string {
	t.Helper()
	if b.Body == nil {
		return ""
	}
	all, err := io.ReadAll(b.Body)
	_ = b.Body.Close()
	if err != nil {
		t.Errorf("read the body: %v", err)
	}
	return string(all)
}

// TestOpenBlobRepositoryName checks that a name the registry would refuse is an error without
// a request.
func TestOpenBlobRepositoryName(t *testing.T) {
	f := newFakeRegistry(t)
	log, _ := testLogger()
	p := f.provider(t, log, nil)
	if _, err := p.OpenBlob(context.Background(), "Raw/Upper", digest.FromString("x"), "", true); err == nil {
		t.Error("OpenBlob of an invalid repository name succeeded")
	}
	if _, err := p.StatBlob(context.Background(), "raw/x", "sha256:short"); err == nil {
		t.Error("StatBlob of an invalid digest succeeded")
	}
	if len(f.log()) != 0 {
		t.Errorf("requests %q, want none", f.log())
	}
}

// TestOpenBlobContext checks that a cancelled context is an error of OpenBlob.
func TestOpenBlobContext(t *testing.T) {
	f := newFakeRegistry(t)
	log, _ := testLogger()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := f.provider(t, log, nil).OpenBlob(ctx, "raw/scratch/notes", digest.FromString("x"), "", true)
	if !errors.Is(err, context.Canceled) {
		t.Errorf("OpenBlob with a cancelled context: %v", err)
	}
}

// TestStatBlob checks StatBlob: one HEAD with the size, the digest and Accept-Ranges; a
// redirect followed to the object store, which sends no Docker-Content-Digest; 404, and a
// Docker-Content-Digest of another blob.
func TestStatBlob(t *testing.T) {
	const name = "raw/scratch/notes"
	const repo = "art/" + name
	data := []byte("0123456789")
	d := digest.FromBytes(data)
	ctx := context.Background()
	log, _ := testLogger()

	f := newFakeRegistry(t)
	f.blobs[repo] = map[digest.Digest][]byte{d: data}
	p := f.provider(t, log, nil)
	want := provider.Blob{Status: http.StatusOK, Size: 10, Digest: d, AcceptRanges: true}
	if got, err := p.StatBlob(ctx, name, d); err != nil || got != want {
		t.Errorf("StatBlob = %+v, %v; want %+v", got, err, want)
	}
	if got := f.log(); !slices.Equal(got, []string{"HEAD /v2/" + repo + "/blobs/" + d.String()}) {
		t.Errorf("requests %q, want one HEAD", got)
	}
	if _, err := p.StatBlob(ctx, name, digest.FromString("missing")); !errors.Is(err, provider.ErrNotFound) {
		t.Errorf("StatBlob of a missing blob: %v, want ErrNotFound", err)
	}

	store := newObjectStore(t, f, repo)
	f.redirect = store.srv.URL + "/bucket"
	if got, err := p.StatBlob(ctx, name, d); err != nil || got != want {
		t.Errorf("StatBlob through a redirect = %+v, %v; want %+v", got, err, want)
	}
	if got := store.log(); !slices.Equal(got, []string{"HEAD /bucket/" + d.String() + " Range= Authorization="}) {
		t.Errorf("object store requests %q", got)
	}

	f.redirect = ""
	f.hook = func(w http.ResponseWriter, _ *http.Request) bool {
		w.Header().Set("Docker-Content-Digest", digest.FromString("other").String())
		return true
	}
	if _, err := p.StatBlob(ctx, name, d); !errors.Is(err, content.ErrMismatchedDigest) {
		t.Errorf("StatBlob with another digest: %v, want ErrMismatchedDigest", err)
	}
	f.hook = redirectHook(http.StatusFound, "")
	if _, err := p.StatBlob(ctx, name, d); err == nil {
		t.Error("StatBlob of a redirect without a Location succeeded")
	}
}

// TestSameOrigin checks the origin rule of the redirects.
func TestSameOrigin(t *testing.T) {
	tests := []struct {
		base, loc string
		want      bool
	}{
		{"http://registry:5000", "http://registry:5000/v2/x", true},
		{"http://registry:5000", "http://REGISTRY:5000/storage", true},
		{"http://registry:5000", "https://registry:5000/v2/x", false},
		{"http://registry:5000", "http://registry:5001/v2/x", false},
		{"http://registry:5000", "http://objects.example/b", false},
		{"https://registry.example", "https://registry.example:443/v2/x", true},
		{"http://registry.example:80", "http://registry.example/v2/x", true},
		{"https://registry.example", "s3://registry.example/x", false},
	}
	for _, tt := range tests {
		p, err := New(config.Registry{Name: "r", Type: Type, URL: tt.base}, nil)
		if err != nil {
			t.Fatal(err)
		}
		u, err := url.Parse(tt.loc)
		if err != nil {
			t.Fatal(err)
		}
		if got := p.sameOrigin(u); got != tt.want {
			t.Errorf("sameOrigin(%s, %s) = %v, want %v", tt.base, tt.loc, got, tt.want)
		}
	}
}
