package raw

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/lajstrom/lajstrom/internal/format"
	"github.com/lajstrom/lajstrom/internal/index"
	"github.com/lajstrom/lajstrom/internal/provider"
	"github.com/lajstrom/lajstrom/internal/provider/providertest"
)

// errBoom is the injected failure.
var errBoom = errors.New("boom")

// faults names the calls that fail with errBoom.
type faults map[string]bool

// faultyStore is an index.Store whose methods named in fail return errBoom. The routes read
// through it; ingest writes to the store under it.
type faultyStore struct {
	index.Store
	fail faults
}

func (s *faultyStore) GetPackage(ctx context.Context, repoID int64, name string) (*index.Package, error) {
	if s.fail["GetPackage"] {
		return nil, errBoom
	}
	return s.Store.GetPackage(ctx, repoID, name)
}

func (s *faultyStore) ListVersions(ctx context.Context, packageID int64) ([]index.Version, error) {
	if s.fail["ListVersions"] {
		return nil, errBoom
	}
	return s.Store.ListVersions(ctx, packageID)
}

func (s *faultyStore) ListFiles(ctx context.Context, versionID int64) ([]index.File, error) {
	if s.fail["ListFiles"] {
		return nil, errBoom
	}
	return s.Store.ListFiles(ctx, versionID)
}

func (s *faultyStore) ListPackages(ctx context.Context, repoID int64, prefix string) ([]index.Package, error) {
	if s.fail["ListPackages"] {
		return nil, errBoom
	}
	return s.Store.ListPackages(ctx, repoID, prefix)
}

func (s *faultyStore) GetVersionByDigest(ctx context.Context, repoID int64, d digest.Digest) (*index.Version, error) {
	if s.fail["GetVersionByDigest"] {
		return nil, errBoom
	}
	return s.Store.GetVersionByDigest(ctx, repoID, d)
}

func (s *faultyStore) Tx(ctx context.Context, fn func(index.Writer) error) error {
	if s.fail["Tx"] {
		return errBoom
	}
	return s.Store.Tx(ctx, fn)
}

// faultyProvider is the fake registry whose calls named in fail return errBoom; "stream" makes
// a fetched blob fail after its first byte.
type faultyProvider struct {
	*providertest.Fake
	fail faults
}

func (p *faultyProvider) Capabilities(ctx context.Context) (provider.Capabilities, error) {
	if p.fail["Capabilities"] {
		return provider.Capabilities{}, errBoom
	}
	return p.Fake.Capabilities(ctx)
}

func (p *faultyProvider) BlobRedirectURL(ctx context.Context, name string, d digest.Digest) (string, bool, error) {
	if p.fail["BlobRedirectURL"] {
		return "", false, errBoom
	}
	return p.Fake.BlobRedirectURL(ctx, name, d)
}

func (p *faultyProvider) Repository(ctx context.Context, name string) (provider.Repository, error) {
	if p.fail["Repository"] {
		return nil, errBoom
	}
	rr, err := p.Fake.Repository(ctx, name)
	return &faultyRepository{Repository: rr, fail: p.fail}, err
}

type faultyRepository struct {
	provider.Repository
	fail faults
}

func (r *faultyRepository) Fetch(ctx context.Context, d ocispec.Descriptor) (io.ReadCloser, error) {
	if r.fail["Fetch"] {
		return nil, errBoom
	}
	rc, err := r.Repository.Fetch(ctx, d)
	if err != nil || !r.fail["stream"] {
		return rc, err
	}
	return struct {
		io.Reader
		io.Closer
	}{io.MultiReader(io.LimitReader(rc, 1), failingReader{}), rc}, nil
}

func (r *faultyRepository) Delete(ctx context.Context, d ocispec.Descriptor) error {
	if r.fail["Delete"] {
		return errBoom
	}
	return r.Repository.Delete(ctx, d)
}

// failingReader fails every read.
type failingReader struct{}

func (failingReader) Read([]byte) (int, error) { return 0, errBoom }

// TestFailures checks that a failure of the index or the registry is a 500 whose cause is
// logged and not answered, on GET, DELETE and the listing; that a PUT whose version cannot be
// read back is still 201, without created; and that a stream that breaks after the headers is
// logged.
func TestFailures(t *testing.T) {
	tests := []struct {
		name, method, target string
		fail                 string
		status               int
		log                  string
	}{
		{"GET package", http.MethodGet, target(fileTxt), "GetPackage", 500, "raw: request failed"},
		{"GET versions", http.MethodGet, target(fileTxt), "ListVersions", 500, "raw: request failed"},
		{"GET files", http.MethodGet, target(fileTxt), "ListFiles", 500, "raw: request failed"},
		{"GET pinned", http.MethodGet, target(fileTxt) + "?digest=" + digest.FromString("x").String(), "GetVersionByDigest", 500, "raw: request failed"},
		{"GET redirect", http.MethodGet, target(fileTxt), "BlobRedirectURL", 500, "raw: request failed"},
		{"GET repository", http.MethodGet, target(fileTxt), "Repository", 500, "raw: request failed"},
		{"GET fetch", http.MethodGet, target(fileTxt), "Fetch", 500, "raw: request failed"},
		{"GET stream", http.MethodGet, target(fileTxt), "stream", 200, "the blob stream failed after the response started"},
		{"listing", http.MethodGet, "/raw/scratch/", "Tx", 500, "raw: request failed"},
		{"DELETE package", http.MethodDelete, target(fileTxt), "GetPackage", 500, "raw: request failed"},
		{"DELETE versions", http.MethodDelete, target(fileTxt), "ListVersions", 500, "raw: request failed"},
		{"DELETE capabilities", http.MethodDelete, target(fileTxt), "Capabilities", 500, "raw: request failed"},
		{"DELETE repository", http.MethodDelete, target(fileTxt), "Repository", 500, "raw: request failed"},
		{"DELETE manifest", http.MethodDelete, target(fileTxt), "Delete", 500, "raw: request failed"},
		{"DELETE rows", http.MethodDelete, target(fileTxt), "Tx", 500, "after the registry deleted"},
		{"PUT read back", http.MethodPut, target("new.txt"), "GetVersionByDigest", 201, "published version not read back"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fail := faults{}
			prov := &faultyProvider{Fake: &providertest.Fake{RegistryName: "local"}, fail: fail}
			e := newEnv(t, envOptions{registry: prov, store: func(st index.Store) index.Store { return &faultyStore{Store: st, fail: fail} }})
			e.put(fileTxt, "content")
			fail[tt.fail] = true
			rr := e.do(tt.method, tt.target, strings.NewReader("new"))
			if rr.Code != tt.status || !strings.Contains(e.logs.String(), tt.log) {
				t.Errorf("%s %s = %d %q, want %d; log:\n%s", tt.method, tt.target, rr.Code, rr.Body, tt.status, e.logs)
			}
			if tt.status == http.StatusInternalServerError && strings.Contains(rr.Body.String(), "boom") {
				t.Errorf("the cause reached the client: %q", rr.Body)
			}
		})
	}
}

// TestRenderIndexFailures checks that a failure of the index while rendering is returned.
func TestRenderIndexFailures(t *testing.T) {
	st, repo := newStore(t, "")
	addVersion(t, st, repo, "docs/a.txt", 1, 1, 1)
	for _, call := range []string{"ListPackages", "ListVersions", "ListFiles"} {
		r := &faultyStore{Store: st, fail: faults{call: true}}
		if _, err := (Plugin{}).RenderIndex(t.Context(), repo, "", r); !errors.Is(err, errBoom) {
			t.Errorf("RenderIndex with %s failing: %v", call, err)
		}
	}
}

// TestHandlerEdges covers what the router normally guarantees: a handler without a repo in the
// context, a PUT without a publisher, Routes without a logger; and the small helpers.
func TestHandlerEdges(t *testing.T) {
	st, repo := newStore(t, "")
	h := &handlers{deps: format.Deps{Index: st, Log: slog.New(slog.DiscardHandler)}}
	rr := httptest.NewRecorder()
	h.get(rr, httptest.NewRequest(http.MethodGet, "/raw/scratch/x", nil))
	if rr.Code != http.StatusInternalServerError {
		t.Errorf("GET without a repo in the context = %d", rr.Code)
	}
	mux := http.NewServeMux()
	deps := format.Deps{Index: st}
	Plugin{}.Routes(format.NewRouter(mux, Name, deps), deps)
	rr = httptest.NewRecorder()
	mux.ServeHTTP(rr, httptest.NewRequest(http.MethodPut, "/raw/scratch/x", strings.NewReader("x")))
	if rr.Code != http.StatusInternalServerError {
		t.Errorf("PUT without a publisher = %d", rr.Code)
	}
	if got := ociName(index.Repo{Format: "raw", Name: "scratch"}, "a/b"); got != "raw/scratch/a/b" {
		t.Errorf("ociName without a prefix = %q", got)
	}
	if got := ociName(repo, "a"); got != "raw/scratch/a" {
		t.Errorf("ociName = %q", got)
	}
	if err := tooLarge(repo, errBoom); !errors.Is(err, errBoom) || err.Error() != "boom" {
		t.Errorf("tooLarge of another error = %v", err)
	}
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "@"
	if got := clientIP(req); got != "@" {
		t.Errorf("clientIP of an address without a port = %q", got)
	}
}
