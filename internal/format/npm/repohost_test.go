package npm

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/lajstrom/lajstrom/internal/auth"
	"github.com/lajstrom/lajstrom/internal/server"
	"github.com/lajstrom/lajstrom/internal/testutil"
)

// onHost is get for a request that came on the repo host of repo in the FQDN mode, below
// art.example, as the server rewrites it: path is the path form, and the context has the
// RepoHost.
func onHost(h http.Handler, repo, path string, kv ...string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, path, nil)
	host := server.RepoHost{Format: Name, Repo: repo, URL: "https://" + repo + ".art.example/",
		PathURL: "https://registry.example/npm/" + repo + "/"}
	req = req.WithContext(server.WithRepoHost(auth.WithPrincipal(req.Context(), alice), host))
	for i := 0; i+1 < len(kv); i += 2 {
		req.Header.Set(kv[i], kv[i+1])
	}
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	return rr
}

// tarballs returns the dist.tarball of each version of a packument or of one version.
func tarballs(t *testing.T, body []byte) []string {
	t.Helper()
	var doc struct {
		Versions map[string]struct{ Dist struct{ Tarball string } }
		Dist     struct{ Tarball string }
	}
	must(t, json.Unmarshal(body, &doc))
	var out []string
	for _, v := range doc.Versions {
		out = append(out, v.Dist.Tarball)
	}
	if doc.Dist.Tarball != "" {
		out = append(out, doc.Dist.Tarball)
	}
	return out
}

// TestRepoHostPackuments: on the repo host of the FQDN mode (docs/spec/http-routing.md §1.1),
// every dist.tarball of a hosted repo's packuments, full, abbreviated and for writing, and of one
// version, is below the repo host, and fetches the tarball there; the ETag is that of the bytes
// served and answers If-None-Match. The main host still gets the links under the external URL,
// from the same index_cache row.
func TestRepoHostPackuments(t *testing.T) {
	p := newPipeline(t)
	h := p.install("https://registry.example", nil)
	files := publishWidgets(t, h)
	const want = "https://internal.art.example/@acme/widgets/-/widgets-1.0.0.tgz"
	main := get(h, "/npm/internal/@acme/widgets", "Accept", acceptNpm)
	for _, tc := range []struct{ path, accept string }{
		{"/npm/internal/@acme/widgets", acceptNpm},
		{"/npm/internal/@acme%2fwidgets", acceptPnpm},
		{"/npm/internal/@acme/widgets?write=true", acceptPnpm},
		{"/npm/internal/@acme/widgets/1.0.0", ""},
		{"/npm/internal/@acme/widgets/latest", ""},
	} {
		rr := onHost(h, "internal", tc.path, "Accept", tc.accept)
		if rr.Code != http.StatusOK {
			t.Fatalf("%s: %d %s", tc.path, rr.Code, rr.Body)
		}
		expect(t, tc.path+" dist.tarball", tarballs(t, rr.Body.Bytes()), []string{want})
		if strings.Contains(rr.Body.String(), "registry.example") {
			t.Errorf("%s: a link under the external URL:\n%s", tc.path, rr.Body)
		}
	}
	rr := onHost(h, "internal", "/npm/internal/@acme/widgets", "Accept", acceptNpm)
	etag := rr.Header().Get("ETag")
	if etag == "" || etag == main.Header().Get("ETag") {
		t.Errorf("ETag on the repo host %q, on the main host %q: want another one", etag, main.Header().Get("ETag"))
	}
	expect(t, "If-None-Match on the repo host",
		onHost(h, "internal", "/npm/internal/@acme/widgets", "Accept", acceptNpm, "If-None-Match", etag).Code, http.StatusNotModified)
	expect(t, "the main host", tarballs(t, get(h, "/npm/internal/@acme/widgets", "Accept", acceptNpm).Body.Bytes()),
		[]string{"https://registry.example/npm/internal/@acme/widgets/-/widgets-1.0.0.tgz"})
	expect(t, "the main host's packument", get(h, "/npm/internal/@acme/widgets", "Accept", acceptNpm).Body.String(), main.Body.String())
	// The server rewrites https://internal.art.example/<path> to /npm/internal/<path>.
	got := onHost(h, "internal", "/npm/internal/"+strings.TrimPrefix(want, "https://internal.art.example/"))
	if got.Code != http.StatusOK || !bytes.Equal(got.Body.Bytes(), files["@acme/widgets"]) {
		t.Errorf("the tarball at its link on the repo host: %d, %d bytes", got.Code, got.Body.Len())
	}
}

// TestRepoHostGroup: on the host of a group, the group's links, its hosted member's and its
// proxy's packages alike, are below the group's host; on a proxy's host, below the proxy's.
func TestRepoHostGroup(t *testing.T) {
	p := newGroupRepo(t, "https://registry.example")
	private, info := testutil.NpmTarball(testutil.NpmOptions{Name: "@acme/widgets", Version: "1.0.0"})
	if rr := do(p.h, http.MethodPut, "/npm/internal/@acme%2fwidgets", npmBody(t, private, info, nil), false); rr.Code != http.StatusCreated {
		t.Fatalf("publish to the hosted member: %d %s", rr.Code, rr.Body)
	}
	p.up.publish(t, scopedName, "1.1.0", false)
	for _, tc := range []struct{ repo, path, want string }{
		{"all", "/npm/all/@acme%2fwidgets", "https://all.art.example/@acme/widgets/-/widgets-1.0.0.tgz"},
		{"all", "/npm/all/@isaacs%2fstring-locale-compare",
			"https://all.art.example/@isaacs/string-locale-compare/-/string-locale-compare-1.1.0.tgz"},
		{"npmjs", "/npm/npmjs/@isaacs%2fstring-locale-compare",
			"https://npmjs.art.example/@isaacs/string-locale-compare/-/string-locale-compare-1.1.0.tgz"},
	} {
		for _, accept := range []string{acceptNpm, acceptPnpm} {
			rr := onHost(p.h, tc.repo, tc.path, "Accept", accept)
			if rr.Code != http.StatusOK {
				t.Fatalf("%s: %d %s", tc.path, rr.Code, rr.Body)
			}
			links := tarballs(t, rr.Body.Bytes())
			if !slices.Contains(links, tc.want) || slices.ContainsFunc(links, func(l string) bool {
				return !strings.HasPrefix(l, "https://"+tc.repo+".art.example/")
			}) {
				t.Errorf("%s with %q: dist.tarball %q, want %s among them, all below the host", tc.path, accept, links, tc.want)
			}
		}
	}
}
