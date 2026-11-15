package pypi

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/lajstrom/lajstrom/internal/format"
	"github.com/lajstrom/lajstrom/internal/index"
	"github.com/lajstrom/lajstrom/internal/testutil"
)

// newGroupEnv is newProxyEnv with the groups pypi/all of the hosted pypi/internal and the proxy
// pypi/pypi, in that order; pypi/merged of the same members, which merges my-pkg; and
// pypi/proxies of the proxy alone.
func newGroupEnv(t *testing.T) *proxyEnv {
	t.Helper()
	p := newProxyEnv(t)
	ctx := t.Context()
	for name, g := range map[string]struct {
		settings string
		members  []int64
	}{
		"all":     {`{"merge": []}`, []int64{p.repo.ID, p.proxy.ID}},
		"merged":  {`{"merge": ["my-pkg"]}`, []int64{p.repo.ID, p.proxy.ID}},
		"proxies": {`{"merge": []}`, []int64{p.proxy.ID}},
	} {
		r := index.Repo{Format: Name, Name: name, Prefix: "pypi/" + name, Kind: index.KindGroup, Group: json.RawMessage(g.settings)}
		must(t, p.st.CreateRepo(ctx, &r))
		must(t, p.st.SetGroupMembers(ctx, r.ID, g.members))
	}
	return p
}

// postTo sends a form to POST /pypi/<repo>/.
func (p *proxyEnv) postTo(t *testing.T, repo string, f form) *httptest.ResponseRecorder {
	t.Helper()
	body, ctype := f.body(t)
	req := httptest.NewRequest(http.MethodPost, "/pypi/"+repo+"/", body)
	req.Header.Set("Content-Type", ctype)
	rr := httptest.NewRecorder()
	p.h.ServeHTTP(rr, req)
	return rr
}

// TestGroupInstall: one URL serves the hosted projects and the upstream's: a private project
// from the hosted member, a public one from the proxy, its file fetched through the group and
// served from the proxy's registry, every link relative to the group's page; an upload through
// the group is the hosted member's, and a group without a hosted member takes none.
func TestGroupInstall(t *testing.T) {
	p := newGroupEnv(t)
	private, pi := testutil.Wheel(testutil.WheelOptions{Name: "acme-tool", Version: "1.0"})
	if rr := p.postTo(t, "all", twineForm(pi.Filename, "acme-tool", "1.0", PackagetypeWheel, private)); rr.Code != http.StatusOK {
		t.Fatalf("an upload through the group: %d %s", rr.Code, rr.Body)
	}
	if page := p.page("internal/simple/acme-tool/"); len(page.Files) != 1 {
		t.Errorf("the hosted member after the upload through the group: %v", page.filenames())
	}
	bodies, _, pure, native, sdist := upstreamProject(p.up)

	page := p.page("all/simple/acme-tool/")
	expect(t, "the private project through the group", []any{page.filenames(), page.Files[0].URL},
		[]any{[]string{pi.Filename}, "../../files/acme-tool/1.0/" + pi.Filename})
	page = p.page("all/simple/my-pkg/")
	expect(t, "the public project through the group", page.filenames(), []string{pure, native, sdist})
	for path, want := range map[string][]byte{"all/files/my-pkg/1.0/" + pure: bodies[pure], "all/files/acme-tool/1.0/" + pi.Filename: private,
		"all/files/acme-tool/1.0/" + pi.Filename + ".metadata": pi.Metadata} {
		if rr := p.get(path); rr.Code != http.StatusOK || !bytes.Equal(rr.Body.Bytes(), want) {
			t.Errorf("GET %s through the group: %d %s", path, rr.Code, rr.Body)
		}
	}
	expect(t, "file requests to the upstream", p.up.fetches.Load(), int32(1))
	if _, err := p.st.GetPackage(t.Context(), p.proxy.ID, "my-pkg"); err != nil {
		t.Errorf("the fetched file's package in the proxy: %v", err)
	}
	if rr := p.postTo(t, "proxies", twineForm(pi.Filename, "acme-tool", "1.0", PackagetypeWheel, private)); rr.Code != http.StatusMethodNotAllowed {
		t.Errorf("an upload to a group without a hosted member: %d %s", rr.Code, rr.Body)
	}
}

// TestGroupPages: the project list of a group is its members'; the HTML form of a page is the
// JSON one converted, and answers a conditional GET with 304; a project no member has is 404.
func TestGroupPages(t *testing.T) {
	p := newGroupEnv(t)
	_, _, pure, _, _ := upstreamProject(p.up)
	private, pi := testutil.Wheel(testutil.WheelOptions{Name: "acme-tool", Version: "1.0"})
	p.publish(pi.Filename, private)
	if rr := p.get("all/files/my-pkg/1.0/" + pure); rr.Code != http.StatusOK {
		t.Fatalf("GET %s through the group: %d %s", pure, rr.Code, rr.Body)
	}
	for accept, want := range map[string]string{contentTypeJSON: `"projects":[{"name":"acme-tool"},{"name":"my-pkg"}]`,
		"text/html": `<a href="acme-tool/">acme-tool</a><br/>` + "\n" + `    <a href="my-pkg/">my-pkg</a>`} {
		if rr := p.get("all/simple/", "Accept", accept); rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), want) {
			t.Errorf("the group's project list as %s: %d %s", accept, rr.Code, rr.Body)
		}
	}
	rr := p.get("all/simple/my-pkg/")
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), `<a href="../../files/my-pkg/1.0/`+pure+`#sha256=`) {
		t.Errorf("the HTML page through the group: %d %s", rr.Code, rr.Body)
	}
	if again := p.get("all/simple/my-pkg/", "If-None-Match", rr.Header().Get("ETag")); again.Code != http.StatusNotModified {
		t.Errorf("a conditional GET of the group's page: %d", again.Code)
	}
	if rr := p.get("all/simple/left-pad/"); rr.Code != http.StatusNotFound {
		t.Errorf("a project no member has: %d %s", rr.Code, rr.Body)
	}
}

// TestGroupHostedHides: a public project published under a private name leaks none of its
// upstream files into the group: the hosted member's project hides the proxy, which is not asked
// for the page nor for a file of the name (docs/spec/proxy-and-group.md §8.2), unless a merge
// pattern of the group matches it, and then the files are merged, the hosted member's first.
func TestGroupHostedHides(t *testing.T) {
	p := newGroupEnv(t)
	_, _, pure, native, sdist := upstreamProject(p.up)
	private, pi := testutil.Wheel(testutil.WheelOptions{Name: "my-pkg", Version: "0.5"})
	p.publish(pi.Filename, private)

	expect(t, "the files through the group", p.page("all/simple/my-pkg/").filenames(), []string{pi.Filename})
	if rr := p.get("all/files/my-pkg/1.0/" + pure); rr.Code != http.StatusNotFound {
		t.Errorf("the upstream's file of a hosted name: %d %s", rr.Code, rr.Body)
	}
	expect(t, "upstream requests for a hosted name", p.up.pages.Load()+p.up.fetches.Load(), int32(0))
	expect(t, "the files through a group without the hosted member", p.page("proxies/simple/my-pkg/").filenames(),
		[]string{pure, native, sdist})
	merged := p.page("merged/simple/my-pkg/")
	expect(t, "the files through a group that merges the name", merged.filenames(), []string{pi.Filename, pure, native, sdist})
	expect(t, "its versions", merged.Versions, []string{"0.5", "1.0"})
}

// TestMergeGroup merges the pages of two members: the union of the files by file name, the
// first member's file of a name that both list, the union of the versions in order, and the API
// version 1.0 when a member's page is of 1.0.
func TestMergeGroup(t *testing.T) {
	first := `{"meta":{"api-version":"1.1"},"name":"my-pkg","versions":["1.0"],"files":[` +
		`{"filename":"my_pkg-1.0.tar.gz","url":"../../files/my-pkg/1.0/my_pkg-1.0.tar.gz","hashes":{"sha256":"aa"},"size":3}]}`
	second := `{"meta":{"api-version":"1.0"},"name":"my-pkg","versions":["0.9","1.0"],"files":[` +
		`{"filename":"my_pkg-0.9.tar.gz","url":"../../files/my-pkg/0.9/my_pkg-0.9.tar.gz","hashes":{"sha256":"bb"}},` +
		`{"filename":"my_pkg-1.0.tar.gz","url":"../../files/my-pkg/1.0/my_pkg-1.0.tar.gz","hashes":{"sha256":"cc"}}]}`
	d, err := Plugin{}.MergeGroup(t.Context(), index.Repo{}, "My_Pkg", kindJSON, []format.MemberDocument{
		{Repo: index.Repo{Kind: index.KindHosted}, Content: []byte(first)}, {Repo: index.Repo{Kind: index.KindProxy}, Content: []byte(second)}},
		"https://pypi.example/pypi/all/")
	must(t, err)
	want := `{"meta":{"api-version":"1.0"},"name":"my-pkg","versions":["1.0","0.9"],"files":[` +
		`{"filename":"my_pkg-1.0.tar.gz","url":"../../files/my-pkg/1.0/my_pkg-1.0.tar.gz","hashes":{"sha256":"aa"},"size":3},` +
		`{"filename":"my_pkg-0.9.tar.gz","url":"../../files/my-pkg/0.9/my_pkg-0.9.tar.gz","hashes":{"sha256":"bb"}}]}` + "\n"
	if string(d.Content) != want || d.Kind != kindJSON {
		t.Errorf("the merged page:\n%s\nwant\n%s", d.Content, want)
	}
	if _, err := (Plugin{}).MergeGroup(t.Context(), index.Repo{}, "my-pkg", kindHTML, nil, ""); err == nil {
		t.Error("a merge of the HTML kind: no error")
	}
}
