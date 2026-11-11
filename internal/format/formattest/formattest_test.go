package formattest

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/lajstrom/lajstrom/internal/format"
	"github.com/lajstrom/lajstrom/internal/index"
)

func TestFake(t *testing.T) {
	ctx := context.Background()
	if n, at := (Fake{}).Name(), (Fake{}).ArtifactType(); n != "fake" || at != "application/vnd.lajstrom.fake.v1" {
		t.Errorf("zero Fake: %q %q", n, at)
	}
	f := Fake{FormatName: "other"}
	if n, at := f.Name(), f.ArtifactType(); n != "other" || at != "application/vnd.lajstrom.other.v1" {
		t.Errorf("Fake{other}: %q %q", n, at)
	}
	if got, err := f.EscapeName("Lib_Core"); got != "Lib_Core" || err != nil {
		t.Errorf("EscapeName = %q, %v", got, err)
	}
	if _, err := f.EscapeName(""); !errors.Is(err, format.ErrInvalid) {
		t.Errorf("EscapeName(\"\") = %v, want ErrInvalid", err)
	}
	if _, err := f.Validate(ctx, index.Repo{}, ocispec.Manifest{}, nil); !errors.Is(err, format.ErrUnsupported) {
		t.Errorf("Validate = %v", err)
	}
	if _, err := f.Build(ctx, index.Repo{}, format.Upload{}); !errors.Is(err, format.ErrUnsupported) {
		t.Errorf("Build = %v", err)
	}
	if _, err := f.RenderIndex(ctx, index.Repo{}, "s", nil); !errors.Is(err, format.ErrUnsupported) {
		t.Errorf("RenderIndex without Render = %v", err)
	}
	f.Render = func(context.Context, index.Repo, format.IndexScope) ([]format.Document, error) {
		return []format.Document{{Kind: "k"}}, nil
	}
	if docs, err := f.RenderIndex(ctx, index.Repo{}, "s", nil); err != nil || len(docs) != 1 {
		t.Errorf("RenderIndex = %v, %v", docs, err)
	}
}

func TestFakeHooks(t *testing.T) {
	ctx := context.Background()
	f := Fake{
		ValidateFunc: func(context.Context, index.Repo, ocispec.Manifest, format.BlobFetcher) (*format.Entry, error) {
			return &format.Entry{Name: "validated"}, nil
		},
		BuildFunc: func(_ context.Context, _ index.Repo, up format.Upload) (*format.Built, error) {
			return &format.Built{Tag: up.Params["tag"]}, nil
		},
	}
	if e, err := f.Validate(ctx, index.Repo{}, ocispec.Manifest{}, nil); err != nil || e.Name != "validated" {
		t.Errorf("Validate with ValidateFunc = %v, %v", e, err)
	}
	if b, err := f.Build(ctx, index.Repo{}, format.Upload{Params: map[string]string{"tag": "1"}}); err != nil || b.Tag != "1" {
		t.Errorf("Build with BuildFunc = %v, %v", b, err)
	}
}

func TestFakeRoutes(t *testing.T) {
	idx := &Index{Repos: []index.Repo{{ID: 1, Format: "fake", Name: "scratch"}}}
	mux := http.NewServeMux()
	f := Fake{Mount: func(r format.Router, _ format.Deps) {
		r.HandleFunc("GET /extra", func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "extra") })
	}}
	f.Routes(format.NewRouter(mux, f.Name(), format.Deps{Index: idx}), format.Deps{Index: idx})
	tests := []struct {
		path   string
		status int
		body   string // exact; "" skips the check
	}{
		{path: "/fake/ping", status: 200, body: "pong\n"},
		{path: "/fake/scratch/ping", status: 200, body: "pong fake/scratch\n"},
		{path: "/fake/extra", status: 200, body: "extra"},
		{path: "/fake/scratch/fail/invalid", status: 400, body: "formattest: fail invalid: format: invalid\n"},
		{path: "/fake/scratch/fail/unauthenticated", status: 401},
		{path: "/fake/scratch/fail/forbidden", status: 403},
		{path: "/fake/scratch/fail/notfound", status: 404},
		{path: "/fake/scratch/fail/conflict", status: 409},
		{path: "/fake/scratch/fail/unsupported", status: 501},
		{path: "/fake/scratch/fail/unauthorized", status: 502, body: "Bad Gateway\n"},
		{path: "/fake/scratch/fail/other", status: 500, body: "Internal Server Error\n"},
		{path: "/fake/scratch/fail/nonsense", status: 400, body: "format: invalid: unknown failure kind \"nonsense\"\n"},
	}
	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			rr := httptest.NewRecorder()
			mux.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, tt.path, nil))
			if rr.Code != tt.status || (tt.body != "" && rr.Body.String() != tt.body) {
				t.Errorf("GET %s = %d %q, want %d %q", tt.path, rr.Code, rr.Body, tt.status, tt.body)
			}
		})
	}
}

func TestIndexGetRepo(t *testing.T) {
	ctx := context.Background()
	idx := &Index{Repos: []index.Repo{{ID: 1, Format: "fake", Name: "scratch"}}}

	r, err := idx.GetRepo(ctx, "fake", "scratch")
	if err != nil || r.ID != 1 {
		t.Fatalf("GetRepo = %v, %v", r, err)
	}
	r.Name = "changed"
	if idx.Repos[0].Name != "scratch" {
		t.Error("GetRepo returned a pointer into Repos")
	}
	if _, err := idx.GetRepo(ctx, "pypi", "scratch"); !errors.Is(err, index.ErrNotFound) {
		t.Errorf("GetRepo(pypi/scratch) = %v, want ErrNotFound", err)
	}
	idx.GetRepoErr = errors.New("down")
	if _, err := idx.GetRepo(ctx, "fake", "scratch"); !errors.Is(err, idx.GetRepoErr) {
		t.Errorf("GetRepo with GetRepoErr = %v", err)
	}
	if n := idx.GetRepoCalls.Load(); n != 3 {
		t.Errorf("GetRepoCalls = %d, want 3", n)
	}
}

func TestIndexCache(t *testing.T) {
	ctx := context.Background()
	idx := &Index{}
	content := []byte("v1")
	put := func(w index.Writer, scope, kind string) {
		t.Helper()
		if ok, err := w.PutIndexCache(ctx, &index.Cache{RepoID: 1, Scope: scope, Kind: kind, Content: content}, 0); err != nil || !ok {
			t.Fatalf("PutIndexCache: %t, %v", ok, err)
		}
	}
	err := idx.Tx(ctx, func(w index.Writer) error {
		put(w, "a", "Packages")
		put(w, "a", "Release")
		put(w, "b", "Packages")
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	content[1] = '2'
	if c, err := idx.GetIndexCache(ctx, 1, "a", "Packages"); err != nil || string(c.Content) != "v1" {
		t.Errorf("GetIndexCache = %v, %v; want the content as it was put", c, err)
	}
	cached := func(scope, kind string) bool {
		_, err := idx.GetIndexCache(ctx, 1, scope, kind)
		return err == nil
	}
	if err := idx.InvalidateIndexCache(ctx, 1, "a"); err != nil || cached("a", "Packages") || cached("a", "Release") || !cached("b", "Packages") {
		t.Errorf("InvalidateIndexCache(a): %v; a/Packages %v a/Release %v b/Packages %v", err, cached("a", "Packages"), cached("a", "Release"), cached("b", "Packages"))
	}
	if err := idx.InvalidateIndexCache(ctx, 1, ""); err != nil || cached("b", "Packages") {
		t.Errorf("InvalidateIndexCache(all): %v; b/Packages %v", err, cached("b", "Packages"))
	}
}

// TestIndexCacheGeneration checks that every invalidation moves the repo's generation, and that a
// put of another generation stores nothing.
func TestIndexCacheGeneration(t *testing.T) {
	ctx := context.Background()
	idx := &Index{}
	for range 2 {
		if err := idx.InvalidateIndexCache(ctx, 1, ""); err != nil {
			t.Fatal(err)
		}
	}
	if gen, err := idx.IndexCacheGeneration(ctx, 1); err != nil || gen != 2 {
		t.Errorf("IndexCacheGeneration after two invalidations = %d, %v; want 2", gen, err)
	}
	for _, gen := range []int64{1, 2} {
		want := gen == 2
		ok, err := idx.PutIndexCache(ctx, &index.Cache{RepoID: 1, Scope: "a", Kind: "Packages"}, gen)
		_, getErr := idx.GetIndexCache(ctx, 1, "a", "Packages")
		if err != nil || ok != want || (getErr == nil) != want {
			t.Errorf("PutIndexCache at generation %d of 2: %t, %v; stored %t, want %t", gen, ok, err, getErr == nil, want)
		}
	}
}
