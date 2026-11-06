package format_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/lajstrom/lajstrom/internal/format"
	"github.com/lajstrom/lajstrom/internal/format/formattest"
	"github.com/lajstrom/lajstrom/internal/index"
)

// renderer is a Fake whose Render returns docs and err and counts its calls.
type renderer struct {
	docs  []format.Document
	err   error
	calls int
}

func (r *renderer) plugin() format.Plugin {
	return formattest.Fake{Render: func(context.Context, index.Repo, format.IndexScope) ([]format.Document, error) {
		r.calls++
		return r.docs, r.err
	}}
}

var (
	cacheRepo = index.Repo{ID: 7, Format: "fake", Name: "scratch"}
	simple    = format.Document{Kind: "simple", Path: "simple/requests/", ContentType: "text/html", Content: []byte("<a>requests-2.32.3.tar.gz</a>")}
	simpleAlt = format.Document{Kind: "simple-json", Path: "simple/requests/", ContentType: "application/vnd.pypi.simple.v1+json", Content: []byte(`{"files":[]}`)}
)

func hexSHA256(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// checkCached fails unless idx holds d for scope as CachedDocument stores it.
func checkCached(t *testing.T, idx *formattest.Index, scope string, d format.Document, from, to time.Time) {
	t.Helper()
	c, err := idx.GetIndexCache(context.Background(), cacheRepo.ID, scope, d.Kind)
	if err != nil {
		t.Fatalf("index_cache %s: %v", d.Kind, err)
	}
	if string(c.Content) != string(d.Content) || c.SHA256 != hexSHA256(d.Content) || c.Scope != scope || c.RepoID != cacheRepo.ID {
		t.Errorf("index_cache %s = %+v", d.Kind, c)
	}
	if c.RenderedAt.Location() != time.UTC || c.RenderedAt.Before(from) || c.RenderedAt.After(to) {
		t.Errorf("RenderedAt = %v, want UTC between %v and %v", c.RenderedAt, from, to)
	}
}

func TestCachedDocument(t *testing.T) {
	ctx := context.Background()
	idx := &formattest.Index{}
	r := &renderer{docs: []format.Document{simple, simpleAlt}}
	p := r.plugin()

	from := time.Now()
	c, err := format.CachedDocument(ctx, idx, p, cacheRepo, "simple/requests", "simple")
	to := time.Now()
	if err != nil {
		t.Fatalf("miss: %v", err)
	}
	if c.Kind != "simple" || string(c.Content) != string(simple.Content) || c.SHA256 != hexSHA256(simple.Content) {
		t.Errorf("miss returned %+v", c)
	}
	if r.calls != 1 {
		t.Errorf("miss rendered %d times, want 1", r.calls)
	}
	checkCached(t, idx, "simple/requests", simple, from, to)
	checkCached(t, idx, "simple/requests", simpleAlt, from, to)

	c2, err := format.CachedDocument(ctx, idx, p, cacheRepo, "simple/requests", "simple-json")
	if err != nil || string(c2.Content) != string(simpleAlt.Content) {
		t.Fatalf("hit = %+v, %v", c2, err)
	}
	if r.calls != 1 {
		t.Errorf("hit rendered: %d renders, want 1", r.calls)
	}

	if err := idx.InvalidateIndexCache(ctx, cacheRepo.ID, "simple/requests"); err != nil {
		t.Fatal(err)
	}
	if _, err := format.CachedDocument(ctx, idx, p, cacheRepo, "simple/requests", "simple"); err != nil || r.calls != 2 {
		t.Errorf("after invalidation: %v, %d renders, want 2", err, r.calls)
	}
}

func TestCachedDocumentErrors(t *testing.T) {
	renderErr := errors.New("fake: render failed")
	tests := []struct {
		name   string
		r      *renderer
		kind   string
		want   error
		stored []format.Document // documents in index_cache afterwards
	}{
		{name: "kind not rendered", r: &renderer{docs: []format.Document{simpleAlt}}, kind: "simple", want: index.ErrNotFound, stored: []format.Document{simpleAlt}},
		{name: "nothing rendered", r: &renderer{}, kind: "simple", want: index.ErrNotFound},
		{name: "render error", r: &renderer{docs: []format.Document{simple}, err: renderErr}, kind: "simple", want: renderErr},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			idx := &formattest.Index{}
			from := time.Now()
			c, err := format.CachedDocument(ctx, idx, tt.r.plugin(), cacheRepo, "simple/requests", tt.kind)
			to := time.Now()
			if !errors.Is(err, tt.want) || c != nil {
				t.Fatalf("CachedDocument = %+v, %v; want %v", c, err, tt.want)
			}
			for _, d := range tt.stored {
				checkCached(t, idx, "simple/requests", d, from, to)
			}
			if len(tt.stored) == 0 {
				if _, err := idx.GetIndexCache(ctx, cacheRepo.ID, "simple/requests", "simple"); !errors.Is(err, index.ErrNotFound) {
					t.Errorf("index_cache has a document after %s", tt.name)
				}
			}
		})
	}
}

func TestServeDocument(t *testing.T) {
	renderedAt := time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)
	c := &index.Cache{Content: []byte("Package: nginx\n"), SHA256: "abc123", RenderedAt: renderedAt}
	lastModified := renderedAt.Format(http.TimeFormat)
	tests := []struct {
		name   string
		header map[string]string
		status int
		body   string
	}{
		{name: "full", status: 200, body: "Package: nginx\n"},
		{name: "If-None-Match", header: map[string]string{"If-None-Match": `"abc123"`}, status: 304},
		{name: "If-None-Match other", header: map[string]string{"If-None-Match": `"def"`}, status: 200, body: "Package: nginx\n"},
		{name: "If-Modified-Since", header: map[string]string{"If-Modified-Since": lastModified}, status: 304},
		{name: "Range", header: map[string]string{"Range": "bytes=0-6"}, status: 206, body: "Package"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/fake/scratch/Packages", nil)
			for k, v := range tt.header {
				req.Header.Set(k, v)
			}
			rr := httptest.NewRecorder()
			format.ServeDocument(rr, req, c, "text/plain; charset=utf-8")
			if rr.Code != tt.status || rr.Body.String() != tt.body {
				t.Errorf("response = %d %q, want %d %q", rr.Code, rr.Body, tt.status, tt.body)
			}
			h := rr.Header()
			if h.Get("ETag") != `"abc123"` {
				t.Errorf("ETag = %q", h.Get("ETag"))
			}
			if tt.status == http.StatusNotModified { // a 304 keeps the ETag and drops the rest (RFC 9110 §15.4.5)
				return
			}
			if h.Get("Last-Modified") != lastModified || h.Get("Content-Type") != "text/plain; charset=utf-8" {
				t.Errorf("Last-Modified %q, Content-Type %q", h.Get("Last-Modified"), h.Get("Content-Type"))
			}
		})
	}
}
