package npm

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/lajstrom/lajstrom/internal/auth"
	"github.com/lajstrom/lajstrom/internal/index"
	"github.com/lajstrom/lajstrom/internal/oci/profile"
	"github.com/lajstrom/lajstrom/internal/testutil"
)

const widgetsURL = "/npm/internal/@acme%2fwidgets"

// forWrite returns the full packument as npm reads it before a change, ?write=true.
func forWrite(t *testing.T, h http.Handler) map[string]any {
	t.Helper()
	rr := get(h, widgetsURL+"?write=true", "Accept", acceptPnpm)
	if rr.Code != http.StatusOK {
		t.Fatalf("GET ?write=true: %d %s", rr.Code, rr.Body)
	}
	var doc map[string]any
	must(t, json.Unmarshal(rr.Body.Bytes(), &doc))
	return doc
}

// putDoc sends the packument doc to path, as npm sends its changes.
func putDoc(t *testing.T, h http.Handler, path string, doc map[string]any) *testResponse {
	t.Helper()
	b, err := json.Marshal(doc)
	must(t, err)
	rr := do(h, http.MethodPut, path, b, false)
	return &testResponse{rr.Code, rr.Body.String()}
}

type testResponse struct {
	code int
	body string
}

// versionsOf returns the versions of doc with their deprecation, "" for none.
func versionsOf(doc map[string]any) map[string]string {
	out := map[string]string{}
	for v, d := range doc["versions"].(map[string]any) {
		msg, _ := d.(map[string]any)["deprecated"].(string)
		out[v] = msg
	}
	return out
}

// TestDeprecate is npm deprecate and its undo through the packument update (#308): the body is
// the packument as read, with versions[v].deprecated set or "", and its _rev.
func TestDeprecate(t *testing.T) {
	p := newPipeline(t)
	h := p.install("https://lajstrom.example", nil)
	publish(t, h, "1.0.0", nil)
	publish(t, h, "1.1.0", nil)

	doc := forWrite(t, h)
	stale := doc["_rev"]
	doc["versions"].(map[string]any)["1.0.0"].(map[string]any)["deprecated"] = "use 1.1.0"
	if r := putDoc(t, h, widgetsURL, doc); r.code != http.StatusCreated {
		t.Fatalf("deprecate: %d %s", r.code, r.body)
	}
	after := forWrite(t, h)
	if d := cmp.Diff(map[string]string{"1.0.0": "use 1.1.0", "1.1.0": ""}, versionsOf(after)); d != "" {
		t.Errorf("deprecated (-want +got)\n%s", d)
	}
	if r := putDoc(t, h, widgetsURL, doc); r.code != http.StatusConflict {
		t.Errorf("the same change at the stale _rev %v: %d %s", stale, r.code, r.body)
	}
	after["versions"].(map[string]any)["1.0.0"].(map[string]any)["deprecated"] = ""
	if r := putDoc(t, h, widgetsURL, after); r.code != http.StatusCreated {
		t.Fatalf("undeprecate: %d %s", r.code, r.body)
	}
	if d := cmp.Diff(map[string]string{"1.0.0": "", "1.1.0": ""}, versionsOf(forWrite(t, h))); d != "" {
		t.Errorf("deprecated after the undo (-want +got)\n%s", d)
	}
	if n := p.seqOf(t); n != 4 {
		t.Errorf("%d journal entries, want the two publishes' and the deprecate and undeprecate", n)
	}

	cur := forWrite(t, h)
	cur["versions"].(map[string]any)["9.9.9"] = map[string]any{"name": "@acme/widgets", "version": "9.9.9"}
	if r := putDoc(t, h, widgetsURL, cur); r.code != http.StatusBadRequest {
		t.Errorf("a version the package does not have: %d %s", r.code, r.body)
	}
	cur = forWrite(t, h)
	cur["dist-tags"].(map[string]any)["next"] = "9.9.9"
	if r := putDoc(t, h, widgetsURL, cur); r.code != http.StatusBadRequest {
		t.Errorf("a dist-tag of a version not in the body: %d %s", r.code, r.body)
	}
	cur = forWrite(t, h)
	delete(cur, "_rev")
	if r := putDoc(t, h, widgetsURL, cur); r.code != http.StatusBadRequest {
		t.Errorf("without _rev: %d %s", r.code, r.body)
	}
	cur = forWrite(t, h)
	cur["versions"].(map[string]any)["1.1.0"].(map[string]any)["deprecated"] = "no"
	if r := putDoc(t, p.install("https://lajstrom.example", readOnly{}), widgetsURL, cur); r.code != http.StatusForbidden {
		t.Errorf("a deprecate without write: %d %s", r.code, r.body)
	}
}

// TestUnpublishVersion is npm unpublish of one version: the packument update without the
// version and the dist-tags that named it, at -rev/<rev>, then the DELETE of its tarball's path.
func TestUnpublishVersion(t *testing.T) {
	ctx := t.Context()
	p := newPipeline(t)
	h := p.install("https://lajstrom.example", nil)
	publish(t, h, "1.0.0", nil)
	publish(t, h, "1.1.0", nil)
	publish(t, h, "2.0.0-beta.1", map[string]any{"beta": "2.0.0-beta.1"})

	doc := forWrite(t, h)
	delete(doc["versions"].(map[string]any), "2.0.0-beta.1")
	delete(doc["dist-tags"].(map[string]any), "beta")
	if r := putDoc(t, h, widgetsURL+"/-rev/"+doc["_rev"].(string), doc); r.code != http.StatusCreated {
		t.Fatalf("the packument update: %d %s", r.code, r.body)
	}
	again := forWrite(t, h)
	tarball := "/npm/internal/@acme/widgets/-/widgets-2.0.0-beta.1.tgz/-rev/" + again["_rev"].(string)
	for range 2 {
		if rr := do(h, http.MethodDelete, tarball, nil, false); rr.Code != http.StatusOK {
			t.Errorf("DELETE of the tarball's path: %d %s", rr.Code, rr.Body)
		}
	}
	after := forWrite(t, h)
	if d := cmp.Diff(map[string]any{"latest": "1.1.0"}, after["dist-tags"]); d != "" {
		t.Errorf("dist-tags (-want +got)\n%s", d)
	}
	if _, ok := after["versions"].(map[string]any)["2.0.0-beta.1"]; ok {
		t.Error("the version is still in the packument")
	}
	audit, err := p.st.ListAudit(ctx, index.AuditFilter{Action: "delete"})
	must(t, err)
	if len(audit) != 1 || !strings.Contains(audit[0].Subject, "2.0.0-beta.1") {
		t.Errorf("the audit of the unpublish: %+v", audit)
	}

	// The tarball's DELETE alone, as npm sends it for 1.1.0, which latest named: latest falls back.
	doc = forWrite(t, h)
	if rr := do(h, http.MethodDelete, "/npm/internal/@acme/widgets/-/widgets-1.1.0.tgz/-rev/"+doc["_rev"].(string), nil, false); rr.Code != http.StatusOK {
		t.Errorf("DELETE of 1.1.0's tarball: %d %s", rr.Code, rr.Body)
	}
	if d := cmp.Diff(map[string]any{"latest": "1.0.0"}, forWrite(t, h)["dist-tags"]); d != "" {
		t.Errorf("dist-tags without 1.1.0 (-want +got)\n%s", d)
	}
	if rr := do(p.install("", readOnly{}), http.MethodDelete, "/npm/internal/@acme/widgets/-/widgets-1.0.0.tgz/-rev/x", nil, false); rr.Code != http.StatusForbidden {
		t.Errorf("an unpublish without delete: %d", rr.Code)
	}
}

// writer may read and write, and not delete.
type writer struct{}

func (writer) Allowed(_ context.Context, _ auth.Principal, a auth.Action, _, _ string) (bool, error) {
	return a == auth.ActionRead || a == auth.ActionWrite, nil
}

// TestUnpublishPackage is npm unpublish of a whole package: the current _rev deletes every
// version and every journal entry, after which nothing is left, not even orphans for reindex; a
// stale one is 409, a registry that does not delete manifests 501, and neither changes anything.
func TestUnpublishPackage(t *testing.T) {
	ctx := t.Context()
	p := newPipeline(t)
	h := p.install("https://lajstrom.example", nil)
	publish(t, h, "1.0.0", nil)
	publish(t, h, "1.1.0", map[string]any{"latest": "1.1.0", "next": "1.1.0"})
	rev := forWrite(t, h)["_rev"].(string)

	for _, tc := range []struct {
		name   string
		h      http.Handler
		rev    string
		status int
	}{
		{"a stale rev", h, "1-0123", http.StatusConflict},
		{"without delete", p.install("https://lajstrom.example", writer{}), rev, http.StatusForbidden},
	} {
		if rr := do(tc.h, http.MethodDelete, widgetsURL+"/-rev/"+tc.rev, nil, false); rr.Code != tc.status {
			t.Errorf("%s: %d %s", tc.name, rr.Code, rr.Body)
		}
	}
	p.fake.NoDeleteManifest = true
	if rr := do(h, http.MethodDelete, widgetsURL+"/-rev/"+rev, nil, false); rr.Code != http.StatusNotImplemented {
		t.Errorf("a registry without deletes: %d %s", rr.Code, rr.Body)
	}
	p.fake.NoDeleteManifest = false
	if n := p.seqOf(t); n != 3 || forWrite(t, h)["_rev"] != rev {
		t.Fatalf("the refusals changed the package: %d entries", n)
	}

	if rr := do(h, http.MethodDelete, widgetsURL+"/-rev/"+rev, nil, false); rr.Code != http.StatusOK {
		t.Fatalf("unpublish: %d %s", rr.Code, rr.Body)
	}
	if rr := get(h, widgetsURL); rr.Code != http.StatusNotFound {
		t.Errorf("the packument after the unpublish: %d", rr.Code)
	}
	rr, err := p.fake.Repository(ctx, "npm/internal/acme/widgets")
	must(t, err)
	for _, tag := range []string{"1.0.0", "1.1.0", profile.JournalTag(1), profile.JournalTag(2), profile.JournalTag(3)} {
		if _, err := rr.Resolve(ctx, tag); err == nil {
			t.Errorf("%s is still in the registry", tag)
		}
	}
	if r := p.reindex(t); len(r.OrphanJournal) != 0 || r.Added != 0 || len(r.Errors) != 0 {
		t.Errorf("reindex after the unpublish: %+v", r)
	}
}

// TestUnpublishProxy: a proxy repo takes no unpublish, 405, and keeps what it holds.
func TestUnpublishProxy(t *testing.T) {
	ctx := t.Context()
	p := newPipeline(t)
	proxy := index.Repo{Format: Name, Name: "npmjs", Registry: "local", Prefix: "npm/npmjs", Kind: index.KindProxy}
	must(t, p.st.CreateRepo(ctx, &proxy))
	b, _ := testutil.NpmTarball(testutil.NpmOptions{Name: "left-pad", Version: "1.3.0"})
	if _, err := p.pipe.Upload(ctx, proxy, upload(b, ""), alice); err != nil {
		t.Fatal(err)
	}
	h := p.install("", nil)
	for _, path := range []string{"/npm/npmjs/left-pad/-rev/1-x", "/npm/npmjs/left-pad/-/left-pad-1.3.0.tgz/-rev/1-x"} {
		if rr := do(h, http.MethodDelete, path, nil, false); rr.Code != http.StatusMethodNotAllowed {
			t.Errorf("DELETE %s: %d %s", path, rr.Code, rr.Body)
		}
	}
	if _, err := p.st.GetPackage(ctx, proxy.ID, "left-pad"); err != nil {
		t.Errorf("the proxy lost what it holds: %v", err)
	}
}
