package npm

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/lajstrom/lajstrom/internal/auth"
	"github.com/lajstrom/lajstrom/internal/format"
	"github.com/lajstrom/lajstrom/internal/index"
	"github.com/lajstrom/lajstrom/internal/oci/profile"
	"github.com/lajstrom/lajstrom/internal/testutil"
)

// publish publishes version of @acme/widgets through h with the dist-tags of the body.
func publish(t *testing.T, h http.Handler, version string, tags map[string]any) {
	t.Helper()
	b, info := testutil.NpmTarball(testutil.NpmOptions{Name: "@acme/widgets", Version: version})
	body := npmBody(t, b, info, func(body, _ map[string]any) { body["dist-tags"] = tags })
	if rr := do(h, http.MethodPut, "/npm/internal/@acme%2fwidgets", body, false); rr.Code != http.StatusCreated {
		t.Fatalf("publish %s: %d %s", version, rr.Code, rr.Body)
	}
}

// packumentOf returns the full packument of @acme/widgets as h serves it.
func packumentOf(t *testing.T, h http.Handler, accept string) map[string]json.RawMessage {
	t.Helper()
	rr := get(h, "/npm/internal/@acme%2fwidgets", "Accept", accept)
	if rr.Code != http.StatusOK {
		t.Fatalf("packument: %d %s", rr.Code, rr.Body)
	}
	var doc map[string]json.RawMessage
	must(t, json.Unmarshal(rr.Body.Bytes(), &doc))
	return doc
}

// tagsOfDoc returns the dist-tags of a packument.
func tagsOfDoc(t *testing.T, doc map[string]json.RawMessage) map[string]string {
	t.Helper()
	var tags map[string]string
	must(t, json.Unmarshal(doc["dist-tags"], &tags))
	return tags
}

// seqOf returns the number of journal entries of @acme/widgets.
func (p *pipeline) seqOf(t *testing.T) int {
	t.Helper()
	pkg, err := p.st.GetPackage(t.Context(), p.repo.ID, "@acme/widgets")
	must(t, err)
	rows, err := p.st.ListJournal(t.Context(), pkg.ID)
	must(t, err)
	return len(rows)
}

// TestDistTags is npm dist-tag ls, add and rm (#307), with the dist-tags of a publish: each
// change is one journal entry, none when nothing changes, and the packument and the list show
// the state.
func TestDistTags(t *testing.T) {
	p := newPipeline(t)
	h := p.install("https://lajstrom.example", nil)
	publish(t, h, "1.0.0", map[string]any{"latest": "1.0.0"})
	publish(t, h, "2.0.0-beta.1", map[string]any{"beta": "2.0.0-beta.1"})
	publish(t, h, "1.0.0", map[string]any{"latest": "1.0.0"}) // the repeat: latest names it already
	if n := p.seqOf(t); n != 2 {
		t.Errorf("%d journal entries after the publishes, want 2", n)
	}
	doc := packumentOf(t, h, acceptNpm)
	if d := cmp.Diff(map[string]string{"latest": "1.0.0", "beta": "2.0.0-beta.1"}, tagsOfDoc(t, doc)); d != "" {
		t.Errorf("dist-tags (-want +got)\n%s", d)
	}
	if !strings.HasPrefix(string(doc["_rev"]), `"2-`) {
		t.Errorf("_rev %s, want the last seq 2", doc["_rev"])
	}

	steps := []struct {
		method, path, body string
		status             int
		tags               map[string]string // the list afterwards
	}{
		{http.MethodPut, "/npm/internal/-/package/@acme%2fwidgets/dist-tags/next", `"1.0.0"`, http.StatusCreated,
			map[string]string{"latest": "1.0.0", "beta": "2.0.0-beta.1", "next": "1.0.0"}},
		{http.MethodPut, "/npm/internal/-/package/@acme/widgets/dist-tags/next", `"1.0.0"`, http.StatusCreated, nil}, // no entry
		{http.MethodPut, "/npm/internal/-/package/@acme/widgets/dist-tags/latest", `"2.0.0-beta.1"`, http.StatusCreated,
			map[string]string{"latest": "2.0.0-beta.1", "beta": "2.0.0-beta.1", "next": "1.0.0"}},
		{http.MethodPut, "/npm/internal/-/package/@acme/widgets/dist-tags/next", `"9.9.9"`, http.StatusBadRequest, nil},
		{http.MethodPut, "/npm/internal/-/package/@acme/widgets/dist-tags/1next", `"1.0.0"`, http.StatusBadRequest, nil},
		{http.MethodPut, "/npm/internal/-/package/@acme/widgets/dist-tags/next", `1.0.0`, http.StatusBadRequest, nil},
		{http.MethodPut, "/npm/internal/-/package/@acme/gadgets/dist-tags/next", `"1.0.0"`, http.StatusNotFound, nil},
		{http.MethodDelete, "/npm/internal/-/package/@acme%2Fwidgets/dist-tags/next", "", http.StatusOK,
			map[string]string{"latest": "2.0.0-beta.1", "beta": "2.0.0-beta.1"}},
		{http.MethodDelete, "/npm/internal/-/package/@acme/widgets/dist-tags/next", "", http.StatusNotFound, nil},
		{http.MethodDelete, "/npm/internal/-/package/@acme/widgets/dist-tags/latest", "", http.StatusBadRequest, nil},
		{http.MethodDelete, "/npm/internal/-/package/@acme/widgets/dist-tags", "", http.StatusNotFound, nil},
		{http.MethodGet, "/npm/internal/-/package/@acme/widgets/dist-tags/beta", "", http.StatusNotFound, nil},
		{http.MethodGet, "/npm/internal/-/package/@acme/gadgets/dist-tags", "", http.StatusNotFound, nil},
		{http.MethodGet, "/npm/internal/-/token", "", http.StatusNotFound, nil},
	}
	for _, s := range steps {
		rr := do(h, s.method, s.path, []byte(s.body), false)
		if rr.Code != s.status || rr.Header().Get("Content-Type") != "application/json" {
			t.Errorf("%s %s %s: %d %s, want %d", s.method, s.path, s.body, rr.Code, rr.Body, s.status)
		}
		if s.tags == nil {
			continue
		}
		list := do(h, http.MethodGet, "/npm/internal/-/package/@acme%2fwidgets/dist-tags", nil, false)
		var got map[string]string
		must(t, json.Unmarshal(list.Body.Bytes(), &got))
		if d := cmp.Diff(s.tags, got); list.Code != http.StatusOK || d != "" {
			t.Errorf("after %s %s: %d (-want +got)\n%s", s.method, s.path, list.Code, d)
		}
	}
	if n := p.seqOf(t); n != 5 {
		t.Errorf("%d journal entries, want 5: two publishes, two adds and a rm", n)
	}
	if rr := get(h, "/npm/internal/@acme%2fwidgets/beta"); rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), `"version":"2.0.0-beta.1"`) {
		t.Errorf("the version of a dist-tag: %d %s", rr.Code, rr.Body)
	}
}

// TestDistTagsPackument: a dist-tag whose version is gone drops out, latest falls back to the
// highest release, and a deprecation shows in both packuments.
func TestDistTagsPackument(t *testing.T) {
	ctx := t.Context()
	p := newPipeline(t)
	h := p.install("https://lajstrom.example", nil)
	publish(t, h, "1.0.0", nil)
	publish(t, h, "1.1.0", nil)
	publish(t, h, "2.0.0-rc.1", map[string]any{"next": "2.0.0-rc.1"})
	_, err := p.pipe.Journal(ctx, p.repo, "acme/widgets", []format.JournalOp{
		{ArtifactType: profile.ArtifactTypeNpmDeprecation, Op: profile.JournalDeprecate, Version: "1.0.0", Message: "use 1.1.0"}}, alice)
	must(t, err)
	if _, err := p.pipe.Delete(ctx, p.repo, format.Deletion{Path: "acme/widgets", Reference: "1.1.0"}, alice); err != nil {
		t.Fatal(err)
	}
	full := packumentOf(t, h, acceptNpm)
	if d := cmp.Diff(map[string]string{"latest": "1.0.0", "next": "2.0.0-rc.1"}, tagsOfDoc(t, full)); d != "" {
		t.Errorf("dist-tags without 1.1.0, which latest named (-want +got)\n%s", d)
	}
	for _, accept := range []string{acceptNpm, acceptPnpm} {
		var doc struct {
			Versions map[string]struct{ Deprecated string } `json:"versions"`
		}
		rr := get(h, "/npm/internal/@acme%2fwidgets", "Accept", accept)
		must(t, json.Unmarshal(rr.Body.Bytes(), &doc))
		if doc.Versions["1.0.0"].Deprecated != "use 1.1.0" || doc.Versions["2.0.0-rc.1"].Deprecated != "" {
			t.Errorf("%s: deprecated %+v", accept, doc.Versions)
		}
	}
}

// readOnly lets alice read and nothing else.
type readOnly struct{}

func (readOnly) Allowed(_ context.Context, p auth.Principal, a auth.Action, _, _ string) (bool, error) {
	return p == alice && a == auth.ActionRead, nil
}

// TestDistTagsDenied: a change of a dist-tag needs write; the list, read.
func TestDistTagsDenied(t *testing.T) {
	p := newPipeline(t)
	publish(t, p.install("", nil), "1.0.0", nil)
	h := p.install("", readOnly{})
	for _, tc := range []struct {
		method, path string
		anonymous    bool
		status       int
	}{
		{http.MethodPut, "/npm/internal/-/package/@acme/widgets/dist-tags/next", false, http.StatusForbidden},
		{http.MethodDelete, "/npm/internal/-/package/@acme/widgets/dist-tags/latest", false, http.StatusForbidden},
		{http.MethodGet, "/npm/internal/-/package/@acme/widgets/dist-tags", false, http.StatusOK},
		{http.MethodGet, "/npm/internal/-/package/@acme/widgets/dist-tags", true, http.StatusUnauthorized},
	} {
		if rr := do(h, tc.method, tc.path, []byte(`"1.0.0"`), tc.anonymous); rr.Code != tc.status {
			t.Errorf("%s %s anonymous %v: %d %s, want %d", tc.method, tc.path, tc.anonymous, rr.Code, rr.Body, tc.status)
		}
	}
	if n := p.seqOf(t); n != 1 {
		t.Errorf("%d entries, want the publish's", n)
	}
}

// TestDistTagsProxy: a proxy repo's dist-tags are its upstream's: a change is 405 and writes
// nothing.
func TestDistTagsProxy(t *testing.T) {
	ctx := t.Context()
	p := newPipeline(t)
	proxy := index.Repo{Format: Name, Name: "npmjs", Registry: "local", Prefix: "npm/npmjs", Kind: index.KindProxy}
	must(t, p.st.CreateRepo(ctx, &proxy))
	h := p.install("", nil)
	for _, method := range []string{http.MethodPut, http.MethodDelete} {
		if rr := do(h, method, "/npm/npmjs/-/package/widgets/dist-tags/next", []byte(`"1.0.0"`), false); rr.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s of a dist-tag of a proxy: %d %s", method, rr.Code, rr.Body)
		}
	}
}
