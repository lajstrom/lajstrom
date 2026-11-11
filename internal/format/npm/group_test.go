package npm

import (
	"bytes"
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/lajstrom/lajstrom/internal/index"
	"github.com/lajstrom/lajstrom/internal/testutil"
)

// newGroupRepo is newProxyRepo with the group npm/all of the hosted npm/internal and the proxy
// npm/npmjs, in that order, and a group npm/proxies of the proxy alone.
func newGroupRepo(t *testing.T, externalURL string) *proxyRepo {
	t.Helper()
	p := newProxyRepo(t, externalURL)
	ctx := t.Context()
	for name, members := range map[string][]int64{"all": {p.pipeline.repo.ID, p.repo.ID}, "proxies": {p.repo.ID}} {
		g := index.Repo{Format: Name, Name: name, Prefix: "npm/" + name, Kind: index.KindGroup, Group: json.RawMessage(`{"merge": []}`)}
		must(t, p.st.CreateRepo(ctx, &g))
		must(t, p.st.SetGroupMembers(ctx, g.ID, members))
	}
	return p
}

// groupPackument gets a packument of a group and decodes it.
func (p *proxyRepo) groupPackument(t *testing.T, path, accept string) proxied {
	t.Helper()
	rr := get(p.h, "/npm/"+path, "Accept", accept)
	if rr.Code != http.StatusOK {
		t.Fatalf("GET %s: %d %s", path, rr.Code, rr.Body)
	}
	if strings.Contains(rr.Body.String(), p.up.srv.URL) || strings.Contains(rr.Body.String(), "/npm/npmjs/") ||
		strings.Contains(rr.Body.String(), "/npm/internal/") {
		t.Errorf("GET %s: a link that is not the group's:\n%s", path, rr.Body)
	}
	return decodeProxied(t, rr.Body.Bytes())
}

// sortedKeys returns the keys of m, sorted.
func sortedKeys[V any](m map[string]V) []string {
	return slices.Sorted(func(yield func(string) bool) {
		for k := range m {
			if !yield(k) {
				return
			}
		}
	})
}

// TestGroupInstall: one URL serves the hosted packages and the upstream's: a private scoped
// package from the hosted member, and a public one from the proxy, its tarball fetched through
// the group and served from the proxy's registry, every link the group's (#311).
func TestGroupInstall(t *testing.T) {
	p := newGroupRepo(t, "https://registry.example")
	private, info := testutil.NpmTarball(testutil.NpmOptions{Name: "@acme/widgets", Version: "1.0.0"})
	if rr := do(p.h, http.MethodPut, "/npm/internal/@acme%2fwidgets", npmBody(t, private, info, nil), false); rr.Code != http.StatusCreated {
		t.Fatalf("publish to the hosted member: %d %s", rr.Code, rr.Body)
	}
	public := p.up.publish(t, scopedName, "1.1.0", false)

	for _, accept := range []string{acceptNpm, acceptPnpm} {
		doc := p.groupPackument(t, "all/@acme%2fwidgets", accept)
		expect(t, "the private package's versions", sortedKeys(doc.dists), []string{"1.0.0"})
		expect(t, "its tarball", doc.dists["1.0.0"]["tarball"], any("https://registry.example/npm/all/@acme/widgets/-/widgets-1.0.0.tgz"))
		doc = p.groupPackument(t, "all/@isaacs%2fstring-locale-compare", accept)
		expect(t, "the public package's tarball", doc.dists["1.1.0"]["tarball"],
			any("https://registry.example/npm/all/@isaacs/string-locale-compare/-/string-locale-compare-1.1.0.tgz"))
		expect(t, "the public package's latest", doc.distTags["latest"], "1.1.0")
	}
	for path, want := range map[string][]byte{
		"/npm/all/@acme/widgets/-/widgets-1.0.0.tgz":                                 private,
		"/npm/all/@isaacs/string-locale-compare/-/string-locale-compare-1.1.0.tgz":   public,
		"/npm/all/@isaacs%2fstring-locale-compare/-/string-locale-compare-1.1.0.tgz": public,
	} {
		if rr := get(p.h, path); rr.Code != http.StatusOK || !bytes.Equal(rr.Body.Bytes(), want) {
			t.Errorf("GET %s: %d %s", path, rr.Code, rr.Body)
		}
	}
	expect(t, "tarball requests to the upstream", p.up.files.Load(), int32(1))
	if rr := get(p.h, "/npm/all/left-pad"); rr.Code != http.StatusNotFound {
		t.Errorf("a name no member has: %d %s", rr.Code, rr.Body)
	}
}

// TestGroupDependencyConfusion: a public package published under a private name leaks none of
// its upstream versions into the group: the hosted member's name hides the proxy, which is not
// asked for the packument nor for a tarball of the name (docs/spec/proxy-and-group.md §8.2).
func TestGroupDependencyConfusion(t *testing.T) {
	p := newGroupRepo(t, "https://registry.example")
	p.up.publish(t, "is-number", "7.0.0", false) // the public package, 15 versions upstream
	private, info := testutil.NpmTarball(testutil.NpmOptions{Name: "is-number", Version: "1.0.0"})
	if rr := do(p.h, http.MethodPut, "/npm/internal/is-number", npmBody(t, private, info, nil), false); rr.Code != http.StatusCreated {
		t.Fatalf("publish to the hosted member: %d %s", rr.Code, rr.Body)
	}
	for _, accept := range []string{acceptNpm, acceptPnpm} {
		doc := p.groupPackument(t, "all/is-number", accept)
		expect(t, "versions through the group", sortedKeys(doc.dists), []string{"1.0.0"})
		expect(t, "dist-tags through the group", doc.distTags, map[string]string{"latest": "1.0.0"})
	}
	if rr := get(p.h, "/npm/all/is-number/-/is-number-7.0.0.tgz"); rr.Code != http.StatusNotFound {
		t.Errorf("the upstream's tarball of a hosted name: %d %s", rr.Code, rr.Body)
	}
	expect(t, "upstream requests for a hosted name", p.up.packuments.Load()+p.up.files.Load(), int32(0))
	if doc := p.groupPackument(t, "proxies/is-number", acceptNpm); len(doc.dists) != 15 {
		t.Errorf("a group without the hosted member: %d versions, want the upstream's 15", len(doc.dists))
	}
}

// mustDo sends a request to the routes and fails the test unless it is answered with status.
func (p *proxyRepo) mustDo(t *testing.T, method, path string, body []byte, status int) {
	t.Helper()
	if rr := do(p.h, method, path, body, false); rr.Code != status {
		t.Fatalf("%s %s: %d %s, want %d", method, path, rr.Code, rr.Body, status)
	}
}

// groupPublishBody is npm's publish body of name@version.
func groupPublishBody(t *testing.T, name, version string) []byte {
	b, info := testutil.NpmTarball(testutil.NpmOptions{Name: name, Version: version})
	return npmBody(t, b, info, nil)
}

// TestGroupWrites: a publish, a dist-tag and an unpublish through the group go to its hosted
// member, with the group in their audit rows; ?write=true is the hosted member's packument
// (docs/spec/proxy-and-group.md §8.4).
func TestGroupWrites(t *testing.T) {
	p := newGroupRepo(t, "https://registry.example")
	ctx := t.Context()
	p.mustDo(t, http.MethodPut, "/npm/all/@acme%2fwidgets", groupPublishBody(t, "@acme/widgets", "1.0.0"), http.StatusCreated)
	if _, err := p.st.GetPackage(ctx, p.pipeline.repo.ID, "@acme/widgets"); err != nil {
		t.Fatalf("the hosted member after a publish through the group: %v", err)
	}
	p.mustDo(t, http.MethodPut, "/npm/all/@acme%2fwidgets", groupPublishBody(t, "@acme/widgets", "2.0.0"), http.StatusCreated)
	p.mustDo(t, http.MethodPut, "/npm/all/-/package/@acme%2fwidgets/dist-tags/stable", []byte(`"1.0.0"`), http.StatusCreated)
	doc := p.groupPackument(t, "all/@acme%2fwidgets", acceptNpm)
	expect(t, "dist-tags", doc.distTags, map[string]string{"latest": "2.0.0", "stable": "1.0.0"})

	rr := get(p.h, "/npm/all/@acme%2fwidgets?write=true", "Accept", acceptPnpm)
	var write struct {
		Rev      string                     `json:"_rev"`
		Versions map[string]json.RawMessage `json:"versions"`
	}
	must(t, json.Unmarshal(rr.Body.Bytes(), &write))
	if rr.Code != http.StatusOK || write.Rev == "" || len(write.Versions) != 2 || !strings.Contains(rr.Body.String(), "/npm/all/@acme/widgets/-/") {
		t.Fatalf("?write=true through the group: %d %s", rr.Code, rr.Body)
	}
	p.mustDo(t, http.MethodDelete, "/npm/all/@acme%2fwidgets/-/widgets-1.0.0.tgz/-rev/"+write.Rev, nil, http.StatusOK)
	if doc := p.groupPackument(t, "all/@acme%2fwidgets", acceptNpm); len(doc.dists) != 1 {
		t.Errorf("after the unpublish: %v", sortedKeys(doc.dists))
	}
	for _, action := range []string{"push", "delete", "npm.dist-tag"} {
		evs, err := p.st.ListAudit(ctx, index.AuditFilter{Action: action})
		must(t, err)
		if len(evs) == 0 || evs[0].Repo != "npm/internal" || !strings.Contains(string(evs[0].Details), `"via":"npm/all"`) {
			t.Errorf("the %s row of a write through the group: %+v", action, evs)
		}
	}
}

// TestGroupWritesWithoutHosted: a group without a hosted member answers a write, and the read
// a write starts from, 405 (docs/spec/proxy-and-group.md §8.4).
func TestGroupWritesWithoutHosted(t *testing.T) {
	p := newGroupRepo(t, "https://registry.example")
	for _, path := range []string{"/npm/proxies/@acme%2fwidgets", "/npm/proxies/-/package/@acme%2fwidgets/dist-tags/x"} {
		rr := do(p.h, http.MethodPut, path, groupPublishBody(t, "@acme/widgets", "1.0.0"), false)
		if rr.Code != http.StatusMethodNotAllowed || rr.Header().Get("Allow") != "GET, HEAD" {
			t.Errorf("PUT %s on a group without a hosted member: %d %s", path, rr.Code, rr.Body)
		}
	}
	if rr := get(p.h, "/npm/proxies/@acme%2fwidgets?write=true"); rr.Code != http.StatusMethodNotAllowed {
		t.Errorf("?write=true on a group without a hosted member: %d", rr.Code)
	}
}

// TestGroupSearch: a search of a group looks through its members' packages, the hosted
// member's first.
func TestGroupSearch(t *testing.T) {
	p := newGroupRepo(t, "https://registry.example")
	b, info := testutil.NpmTarball(testutil.NpmOptions{Name: "@acme/widgets", Version: "1.0.0"})
	if rr := do(p.h, http.MethodPut, "/npm/internal/@acme%2fwidgets", npmBody(t, b, info, nil), false); rr.Code != http.StatusCreated {
		t.Fatalf("publish: %d %s", rr.Code, rr.Body)
	}
	p.up.publish(t, "is-number", "7.0.0", false)
	if rr := get(p.h, "/npm/npmjs/is-number/-/is-number-7.0.0.tgz"); rr.Code != http.StatusOK {
		t.Fatalf("the proxy's fetch: %d", rr.Code)
	}
	rr := get(p.h, "/npm/all/-/v1/search?text=")
	var res struct {
		Objects []struct {
			Package struct{ Name, Version string } `json:"package"`
		} `json:"objects"`
		Total int `json:"total"`
	}
	must(t, json.Unmarshal(rr.Body.Bytes(), &res))
	if rr.Code != http.StatusOK || res.Total != 2 || res.Objects[0].Package.Name != "@acme/widgets" || res.Objects[1].Package.Name != "is-number" {
		t.Errorf("search of the group: %d %s", rr.Code, rr.Body)
	}
}

// TestGroupAudit: the bulk audit of a group goes to its proxy member without the names its
// hosted member holds, which never leave the server, and is {} when none is left; npm 6's two
// routes are 501 (docs/spec/http-routing.md, npm, Audit).
func TestGroupAudit(t *testing.T) {
	p := newGroupRepo(t, "https://registry.example")
	p.mustDo(t, http.MethodPut, "/npm/internal/@acme%2fwidgets", groupPublishBody(t, "@acme/widgets", "1.0.0"), http.StatusCreated)
	rr := as(p.h, alice, http.MethodPost, "/npm/all/-/"+auditBulk, []byte(`{"@acme/widgets":["1.0.0"],"lodash":["4.17.15"]}`))
	if rr.Code != http.StatusOK || rr.Body.String() != upstreamAdvisories {
		t.Fatalf("the bulk audit of the group: %d %s", rr.Code, rr.Body)
	}
	p.up.mu.Lock()
	seen := slices.Clone(p.up.audits)
	p.up.mu.Unlock()
	if len(seen) != 1 || seen[0].body != `{"lodash":["4.17.15"]}` {
		t.Errorf("the upstream got %+v, want lodash alone", seen)
	}
	rr = as(p.h, alice, http.MethodPost, "/npm/all/-/"+auditBulk, []byte(`{"@acme/widgets":["1.0.0"]}`))
	if rr.Code != http.StatusOK || strings.TrimSpace(rr.Body.String()) != "{}" || len(p.up.audits) != 1 {
		t.Errorf("a bulk audit of private names only: %d %s, %d upstream requests", rr.Code, rr.Body, len(p.up.audits))
	}
	if rr := as(p.h, alice, http.MethodPost, "/npm/all/-/"+auditQuick, []byte(`{}`)); rr.Code != http.StatusNotImplemented {
		t.Errorf("npm 6's audit of a group: %d %s", rr.Code, rr.Body)
	}
	// The proxy member's deny list counts too, for names the hosted member does not hold (#514).
	p.repo.Upstream = json.RawMessage(`{"name": "npmjs", "deny": ["@corp/*"]}`)
	must(t, p.st.UpdateRepo(t.Context(), &p.repo))
	rr = as(p.h, alice, http.MethodPost, "/npm/all/-/"+auditBulk, []byte(`{"@acme/widgets":["1.0.0"],"@corp/tools":["2.0.0"],"lodash":["4.17.15"]}`))
	p.up.mu.Lock()
	seen = slices.Clone(p.up.audits)
	p.up.mu.Unlock()
	if rr.Code != http.StatusOK || len(seen) != 2 || seen[1].body != `{"lodash":["4.17.15"]}` {
		t.Errorf("the bulk audit of the group with a denied name: %d %s, the upstream got %+v", rr.Code, rr.Body, seen)
	}
}
