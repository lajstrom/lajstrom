package api

import (
	"context"
	"maps"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/opencontainers/go-digest"

	"github.com/lajstrom/lajstrom/internal/auth"
	"github.com/lajstrom/lajstrom/internal/format"
	"github.com/lajstrom/lajstrom/internal/index"
)

// searchFixture has pypi/libs and raw/open, which alice reads, and pypi/secret, which only
// admin does: each with a package that requests names, and in pypi/secret a file whose
// digest is known.
func searchFixture(t *testing.T) (*fixture, digest.Digest) {
	t.Helper()
	g := map[string]bool{"user:alice read pypi/libs": true, "user:alice read raw/open": true}
	for k, v := range grants {
		g[k] = v
	}
	f := newFixture(t, Options{Auth: fakeAuth{grants: g}})
	libs, secret, open := f.create(t, "pypi", "libs"), f.create(t, "pypi", "secret"), f.create(t, "raw", "open")
	ctx := context.Background()
	requests := publish(t, f.st, libs.ID, "requests", "2.32.3", testNow)
	v, err := f.st.GetVersion(ctx, requests.ID, "2.32.3", "")
	must(t, err)
	v.Summary = "Python HTTP for Humans."
	must(t, f.st.UpsertVersion(ctx, v))
	publish(t, f.st, libs.ID, "requests-toolbelt", "1.0", testNow)
	publish(t, f.st, libs.ID, "httpx", "0.27", testNow)
	publish(t, f.st, open.ID, "docs/requests.txt", "0123456789abcdef", testNow)
	hidden := publish(t, f.st, secret.ID, "requests", "9.9", testNow)
	hv, err := f.st.GetVersion(ctx, hidden.ID, "9.9", "")
	must(t, err)
	file := digest.FromString("the hidden wheel")
	must(t, f.st.ReplaceFiles(ctx, hv.ID, []index.File{{Name: "requests-9.9.whl", MediaType: "application/zip", Digest: file, Size: 9}}))
	return f, file
}

// hitNames are the hits of a search answer as format/repo/name, with the version and what
// matched for a version.
func hitNames(t *testing.T, f *fixture, target string, p auth.Principal) ([]string, string) {
	t.Helper()
	var res struct {
		Hits []map[string]any `json:"hits"`
		Next string           `json:"next"`
	}
	decodeJSON(t, f.do(http.MethodGet, target, "", p), http.StatusOK, &res)
	out := []string{}
	for _, h := range res.Hits {
		n := h["format"].(string) + "/" + h["repo"].(string) + "/" + h["name"].(string)
		if h["kind"] == "version" {
			n += " " + h["version"].(string) + " " + h["matched"].(string)
		}
		out = append(out, n)
	}
	return out, res.Next
}

// TestSearch: the ranks of a name, the facts of a package hit, and a page at a time with the
// cursor of the last hit across ranks; ?format keeps one format.
func TestSearch(t *testing.T) {
	f, _ := searchFixture(t)
	got, _ := hitNames(t, f, "/api/v1/search?q=Requests", admin)
	want := []string{"pypi/libs/requests", "pypi/secret/requests", "pypi/libs/requests-toolbelt", "raw/open/docs/requests.txt"}
	if !slices.Equal(got, want) {
		t.Errorf("hits %v, want %v", got, want)
	}
	var paged []string
	for next, first := "", true; first || next != ""; first = false {
		target := "/api/v1/search?q=requests&limit=1"
		if next != "" {
			target += "&cursor=" + url.QueryEscape(next)
		}
		var page []string
		page, next = hitNames(t, f, target, admin)
		paged = append(paged, page...)
		if len(paged) > 10 {
			break
		}
	}
	if !slices.Equal(paged, want) {
		t.Errorf("the pages of one hit: %v, want %v", paged, want)
	}
	if got, _ := hitNames(t, f, "/api/v1/search?q=requests&format=raw", admin); !slices.Equal(got, []string{"raw/open/docs/requests.txt"}) {
		t.Errorf("format=raw: %v", got)
	}
	if got, _ := hitNames(t, f, "/api/v1/search?q=humans", admin); !slices.Equal(got, []string{"pypi/libs/requests"}) {
		t.Errorf("a summary: %v", got)
	}
	var one any
	decodeJSON(t, f.do(http.MethodGet, "/api/v1/search?q=requests&limit=1", "", admin), http.StatusOK, &one)
	m := one.(map[string]any)
	delete(m, "next")
	wantJSON(t, "a package hit", m, `{"hits": [{"kind": "package", "format": "pypi", "repo": "libs", "name": "requests",
		"latestVersion": "2.32.3", "summary": "Python HTTP for Humans.", "updatedAt": "2026-01-02T12:00:00Z"}]}`)
}

// TestSearchDigest: a digest's prefix, with sha256: or without and in either case, finds the
// manifest and the file whose digest starts with it, a file hit naming the file.
func TestSearchDigest(t *testing.T) {
	f, file := searchFixture(t)
	manifest := digest.FromString("requests" + "2.32.3")
	for _, q := range []string{manifest.String()[:20], strings.ToUpper(manifest.Encoded()[:8]), manifest.String()} {
		if got, _ := hitNames(t, f, "/api/v1/search?q="+url.QueryEscape(q), admin); !slices.Equal(got, []string{"pypi/libs/requests 2.32.3 manifest"}) {
			t.Errorf("q=%s: %v", q, got)
		}
	}
	var res any
	decodeJSON(t, f.do(http.MethodGet, "/api/v1/search?q="+file.Encoded()[:12], "", admin), http.StatusOK, &res)
	wantJSON(t, "a file hit", res, `{"hits": [{"kind": "version", "format": "pypi", "repo": "secret", "name": "requests",
		"version": "9.9", "variant": "", "digest": "`+digest.FromString("requests9.9").String()+`", "matched": "file",
		"file": {"name": "requests-9.9.whl", "digest": "`+file.String()+`"}}]}`)
	if got, _ := hitNames(t, f, "/api/v1/search?q="+file.Encoded()[:7], admin); len(got) != 0 {
		t.Errorf("7 hex digits are no digest: %v", got)
	}
}

// TestSearchAccess: the search runs over the repos the caller may read (docs/spec/rbac.md §3):
// pypi/secret adds no hit for alice, by name or by digest, and her answer is the one she would
// get if it did not exist; bob and anonymous get 200 without hits.
func TestSearchAccess(t *testing.T) {
	f, file := searchFixture(t)
	aliceHits, _ := hitNames(t, f, "/api/v1/search?q=requests", alice)
	if want := []string{"pypi/libs/requests", "pypi/libs/requests-toolbelt", "raw/open/docs/requests.txt"}; !slices.Equal(aliceHits, want) {
		t.Errorf("alice: %v, want %v", aliceHits, want)
	}
	if got, _ := hitNames(t, f, "/api/v1/search?q="+file.Encoded()[:12], alice); len(got) != 0 {
		t.Errorf("alice, the digest of a file of pypi/secret: %v", got)
	}
	if got, _ := hitNames(t, f, "/api/v1/search?q=requests&format=pypi", alice); !slices.Equal(got, aliceHits[:2]) {
		t.Errorf("alice, format=pypi: %v", got)
	}
	for _, p := range []auth.Principal{bob, auth.Anonymous} {
		if got, next := hitNames(t, f, "/api/v1/search?q=requests", p); len(got) != 0 || next != "" {
			t.Errorf("%s: %v %q", p, got, next)
		}
	}
	must(t, f.repos.Delete(context.Background(), "pypi", "secret", true, Origin{}))
	if got, _ := hitNames(t, f, "/api/v1/search?q=requests", alice); !slices.Equal(got, aliceHits) {
		t.Errorf("alice, without pypi/secret: %v, want %v", got, aliceHits)
	}
}

// TestSearchInvalid: q is required and at most 256 bytes, and a cursor must be a search's.
func TestSearchInvalid(t *testing.T) {
	f, _ := searchFixture(t)
	for _, tt := range []struct{ target, want string }{
		{"/api/v1/search", "q: required"},
		{"/api/v1/search?q=", "q: required"},
		{"/api/v1/search?q=" + strings.Repeat("a", 257), "q: required, 1 to 256 bytes"},
		{"/api/v1/search?q=a&cursor=" + cursor("not json"), "cursor: "},
		{"/api/v1/search?q=a&limit=0", "limit: "},
	} {
		wantError(t, f.do(http.MethodGet, tt.target, "", admin), http.StatusBadRequest, codeInvalid, tt.want)
	}
}

// TestSearchLatest: the latest version of a package hit is the one the plugin of its repo's
// format picks; a repo of a format without a picker has its newest.
func TestSearchLatest(t *testing.T) {
	f := newFixture(t, Options{})
	f.repos.Latest = func(name string) (format.LatestPicker, bool) { return &pickOldest{}, name == "pypi" }
	libs, open := f.create(t, "pypi", "libs"), f.create(t, "raw", "open")
	publish(t, f.st, libs.ID, "requests", "1.0", testNow.Add(-time.Hour))
	publish(t, f.st, libs.ID, "requests", "2.0", testNow)
	publish(t, f.st, open.ID, "requests.txt", "a1", testNow.Add(-time.Hour))
	publish(t, f.st, open.ID, "requests.txt", "b2", testNow)
	var res struct {
		Hits []PackageHit `json:"hits"`
	}
	decodeJSON(t, f.do(http.MethodGet, "/api/v1/search?q=requests", "", admin), http.StatusOK, &res)
	got := map[string]string{}
	for _, h := range res.Hits {
		got[h.Format+"/"+h.Name] = *h.LatestVersion
	}
	if want := map[string]string{"pypi/requests": "1.0", "raw/requests.txt": "b2"}; !maps.Equal(got, want) {
		t.Errorf("latest versions %v, want %v", got, want)
	}
}
