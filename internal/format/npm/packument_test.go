package npm

import (
	"encoding/json"
	"reflect"
	"regexp"
	"strings"
	"testing"

	"github.com/lajstrom/lajstrom/internal/format"
	"github.com/lajstrom/lajstrom/internal/testutil"
)

// TestCompareVersions orders the versions of SemVer 2.0.0 §11 and more.
func TestCompareVersions(t *testing.T) {
	order := []string{"0.9.0", "1.0.0-0", "1.0.0-0.0", "1.0.0-2", "1.0.0-11", "1.0.0-alpha", "1.0.0-alpha.1", "1.0.0-alpha.beta",
		"1.0.0-beta", "1.0.0-beta.2", "1.0.0-beta.11", "1.0.0-rc.1", "1.0.0", "1.0.1", "1.1.0", "1.10.0", "2.0.0", "10.0.0"}
	for i := range order {
		for j := range order {
			a, err := parseVersion(order[i])
			must(t, err)
			b, err := parseVersion(order[j])
			must(t, err)
			if got, want := a.compare(b), compareInts(i, j); got != want {
				t.Errorf("compare(%s, %s) = %d, want %d", order[i], order[j], got, want)
			}
		}
	}
}

func compareInts(a, b int) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

// publishVersions uploads the versions of the package name through the pipeline, each with
// npm's document (dependencies, a description of its own) and a readme.
func (p *pipeline) publishVersions(t *testing.T, name string, versions ...string) {
	t.Helper()
	for _, v := range versions {
		b, info := testutil.NpmTarball(testutil.NpmOptions{Name: name, Version: v, Description: "version " + v,
			Dependencies: map[string]string{"left-pad": "^1.3.0"}, Scripts: map[string]string{"install": "node x.js"}})
		up := upload(b, document(t, info, func(doc map[string]any) { doc["keywords"] = []string{"k" + v} }))
		if _, err := p.pipe.Upload(t.Context(), p.repo, up, alice); err != nil {
			t.Fatalf("upload %s@%s: %v", name, v, err)
		}
	}
}

// render renders the packuments of name and returns them decoded, the full one first.
func (p *pipeline) render(t *testing.T, name string) (full, abbreviated map[string]any, docs []format.Document) {
	t.Helper()
	docs, err := Plugin{ExternalURL: "https://registry.example/"}.RenderIndex(t.Context(), p.repo, format.IndexScope(name), p.st)
	must(t, err)
	if len(docs) != 2 || docs[0].Kind != kindPackument || docs[1].Kind != kindAbbreviated ||
		docs[0].ContentType != "application/json" || docs[1].ContentType != "application/vnd.npm.install-v1+json" {
		t.Fatalf("documents %+v", docs)
	}
	must(t, json.Unmarshal(docs[0].Content, &full))
	must(t, json.Unmarshal(docs[1].Content, &abbreviated))
	return full, abbreviated, docs
}

// TestRenderIndex renders the packuments of a scoped package of four versions: the versions in
// SemVer order, latest the highest release, not the release candidate above it; each version
// its document with _id, dist (the absolute tarball URL under the external URL, the scope's /
// as it is), _hasShrinkwrap and hasInstallScript; time; the root members of latest; the
// abbreviated form with its keys only. _rev changes with a version.
func TestRenderIndex(t *testing.T) {
	p := newPipeline(t)
	p.publishVersions(t, "@acme/widgets", "1.2.0", "1.10.0", "2.0.0-rc.1", "1.0.0")
	full, short, docs := p.render(t, "@acme/widgets")

	expect(t, "the order of versions", objectKeys(t, docs[0].Content, "versions"), []string{"1.0.0", "1.2.0", "1.10.0", "2.0.0-rc.1"})
	expect(t, "_rev of the form <seq>-<hex>", regexp.MustCompile(`^0-[0-9a-f]{32}$`).MatchString(full["_rev"].(string)), true)
	v := full["versions"].(map[string]any)["1.10.0"].(map[string]any)
	dist := v["dist"].(map[string]any)
	times := full["time"].(map[string]any)
	sv := short["versions"].(map[string]any)["1.10.0"].(map[string]any)
	for _, c := range []struct {
		what      string
		got, want any
	}{
		{"_id", full["_id"], "@acme/widgets"},
		{"name", full["name"], "@acme/widgets"},
		{"dist-tags", full["dist-tags"], map[string]any{"latest": "1.10.0"}},
		{"description", full["description"], "version 1.10.0"},
		{"keywords", full["keywords"], []any{"k1.10.0"}},
		{"readme", full["readme"], "# @acme/widgets"},
		{"the version's _id", v["_id"], "@acme/widgets@1.10.0"},
		{"hasInstallScript", v["hasInstallScript"], true},
		{"_hasShrinkwrap", v["_hasShrinkwrap"], false},
		{"the version's readme", v["readme"], nil},
		{"_nodeVersion", v["_nodeVersion"], nil},
		{"dist.tarball", dist["tarball"], "https://registry.example/npm/internal/@acme/widgets/-/widgets-1.10.0.tgz"},
		{"dist.integrity", strings.HasPrefix(dist["integrity"].(string), "sha512-"), true},
		{"dist.shasum", len(dist["shasum"].(string)), 40},
		{"time", len(times), 6},
		{"time of 2.0.0-rc.1", times["2.0.0-rc.1"] != nil, true},
		{"abbreviated name", short["name"], "@acme/widgets"},
		{"abbreviated modified", short["modified"], times["modified"]},
		{"abbreviated dist-tags", short["dist-tags"], full["dist-tags"]},
		{"abbreviated description", short["description"], nil},
		{"abbreviated version description", sv["description"], nil},
		{"abbreviated version keywords", sv["keywords"], nil},
		{"abbreviated version dependencies", sv["dependencies"], map[string]any{"left-pad": "^1.3.0"}},
		{"abbreviated version dist", sv["dist"], v["dist"]},
		{"abbreviated version hasInstallScript", sv["hasInstallScript"], true},
	} {
		expect(t, c.what, c.got, c.want)
	}

	p.publishVersions(t, "@acme/widgets", "1.11.0")
	if again, _, _ := p.render(t, "@acme/widgets"); again["_rev"] == full["_rev"] {
		t.Error("_rev did not change with a version")
	}
}

// expect fails t when got is not want.
func expect(t *testing.T, what string, got, want any) {
	t.Helper()
	if !reflect.DeepEqual(got, want) {
		t.Errorf("%s = %v, want %v", what, got, want)
	}
}

// TestRenderIndexFallbacks: latest of a package of pre-releases only is the highest; a version
// without metadata is {name, version, dist}; a package the repo has not renders nothing.
func TestRenderIndexFallbacks(t *testing.T) {
	p := newPipeline(t)
	p.publishVersions(t, "widgets", "1.0.0-beta.2", "1.0.0-beta.10")
	pkg, err := p.st.GetPackage(t.Context(), p.repo.ID, "widgets")
	must(t, err)
	v, err := p.st.GetVersion(t.Context(), pkg.ID, "1.0.0-beta.2", "")
	must(t, err)
	v.Metadata, v.Degraded = nil, true
	must(t, p.st.UpsertVersion(t.Context(), v))

	full, _, _ := p.render(t, "widgets")
	if full["dist-tags"].(map[string]any)["latest"] != "1.0.0-beta.10" {
		t.Errorf("dist-tags %v", full["dist-tags"])
	}
	degraded := full["versions"].(map[string]any)["1.0.0-beta.2"].(map[string]any)
	dist := degraded["dist"].(map[string]any)
	if len(degraded) != 3 || degraded["name"] != "widgets" || dist["tarball"] != "https://registry.example/npm/internal/widgets/-/widgets-1.0.0-beta.2.tgz" ||
		!strings.HasPrefix(dist["integrity"].(string), "sha512-") {
		t.Errorf("a degraded version: %v", degraded)
	}
	for _, name := range []string{"other", "@acme/widgets", "Not A Name"} {
		if docs, err := (Plugin{}).RenderIndex(t.Context(), p.repo, format.IndexScope(name), p.st); err != nil || docs != nil {
			t.Errorf("%s: %v, %v", name, docs, err)
		}
	}
}

// objectKeys returns the keys of the object at key of the JSON object b, in their order.
func objectKeys(t *testing.T, b []byte, key string) []string {
	t.Helper()
	var outer map[string]json.RawMessage
	must(t, json.Unmarshal(b, &outer))
	dec := json.NewDecoder(strings.NewReader(string(outer[key])))
	var keys []string
	if _, err := dec.Token(); err != nil {
		t.Fatal(err)
	}
	for dec.More() {
		k, err := dec.Token()
		must(t, err)
		keys = append(keys, k.(string))
		var skip json.RawMessage
		must(t, dec.Decode(&skip))
	}
	return keys
}
