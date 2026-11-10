package npm

import (
	"encoding/json"
	"errors"
	"maps"
	"slices"
	"strings"
	"testing"

	"github.com/lajstrom/lajstrom/internal/index"
	"github.com/lajstrom/lajstrom/internal/testutil"
)

// holdIsNumber uploads is-number 7.0.0, as other bytes than the upstream's, and 99.0.0, which
// the upstream does not list, and returns their facts by version.
func holdIsNumber(t *testing.T, p *pipeline) map[string]testutil.NpmInfo {
	t.Helper()
	held := map[string]testutil.NpmInfo{}
	for _, v := range []string{"7.0.0", "99.0.0"} {
		b, info := testutil.NpmTarball(testutil.NpmOptions{Name: "is-number", Version: v})
		held[v] = info
		if _, err := p.pipe.Upload(t.Context(), p.repo, upload(b, ""), alice); err != nil {
			t.Fatal(err)
		}
	}
	return held
}

// proxied is a packument RenderProxy rendered, decoded: its members, dist-tags and the dist of
// each version.
type proxied struct {
	members  []string
	distTags map[string]string
	dists    map[string]map[string]any
}

func decodeProxied(t *testing.T, content []byte) proxied {
	t.Helper()
	var doc struct {
		DistTags map[string]string                        `json:"dist-tags"`
		Versions map[string]struct{ Dist map[string]any } `json:"versions"`
	}
	var members map[string]json.RawMessage
	must(t, json.Unmarshal(content, &doc))
	must(t, json.Unmarshal(content, &members))
	out := proxied{members: slices.Sorted(maps.Keys(members)), distTags: doc.DistTags, dists: map[string]map[string]any{}}
	for v, vd := range doc.Versions {
		out.dists[v] = vd.Dist
	}
	return out
}

// renderBase is the base of the links the render tests ask for.
const renderBase = "https://registry.example/npm/internal/"

// TestRenderProxy renders the captured packuments of is-number for a repo that holds 7.0.0 and
// 99.0.0 (holdIsNumber): the upstream's members, dist-tags and versions, every tarball under the
// base, the held version with the dist of its bytes, 99.0.0 not listed, no link to the upstream
// (attestations dropped); the abbreviated kind with its members only.
func TestRenderProxy(t *testing.T) {
	p := newPipeline(t)
	held := holdIsNumber(t, p)
	const upstream6 = "sha512-Wu1VHeILBK8KAWJUAiSZQX94GmOE45Rg6/538fKwiloUu21KncEkYGPqob2oSZ5mUT73vLGrHQjKw3KMPwfDzg=="
	members := map[string][]string{
		kindPackument: {"_id", "_rev", "author", "bugs", "description", "dist-tags", "homepage", "keywords", "license", "name",
			"repository", "time", "versions"},
		kindAbbreviated: {"dist-tags", "modified", "name", "versions"}}
	for _, kind := range []string{kindPackument, kindAbbreviated} {
		kept := captured(t, capturedIsNumber, kind == kindAbbreviated)
		kept = []byte(strings.ReplaceAll(string(kept), `"fileCount":4,`,
			`"attestations":{"url":"https://registry.npmjs.org/-/npm/v1/attestations/x"},"fileCount":4,`))
		doc, err := Plugin{}.RenderProxy(t.Context(), p.repo, "is-number", kind, kept, p.st, renderBase)
		must(t, err)
		ct, _ := contentTypeOf(kind)
		if doc.Kind != kind || doc.ContentType != ct || doc.Path != "is-number" || strings.Contains(string(doc.Content), "registry.npmjs.org") {
			t.Fatalf("%s: %+v", kind, doc)
		}
		got := decodeProxied(t, doc.Content)
		expect(t, kind+" members", got.members, members[kind])
		expect(t, kind+" dist-tags", got.distTags, map[string]string{"latest": "7.0.0"})
		if _, ok := got.dists["99.0.0"]; ok || len(got.dists) != 15 {
			t.Errorf("%s: %d versions, 99.0.0 %v", kind, len(got.dists), ok)
		}
		for v, d := range got.dists {
			if d["tarball"] != renderBase+"is-number/-/is-number-"+v+".tgz" || d["attestations"] != nil {
				t.Errorf("%s %s: dist %v", kind, v, d)
			}
		}
		d7 := got.dists["7.0.0"]
		expect(t, kind+" the held integrity of 7.0.0", d7["integrity"], any(held["7.0.0"].Integrity))
		expect(t, kind+" the held shasum of 7.0.0", d7["shasum"], any(held["7.0.0"].Shasum))
		expect(t, kind+" the upstream's fileCount of 7.0.0", d7["fileCount"], any(4.0))
		expect(t, kind+" the upstream's integrity of 6.0.0", got.dists["6.0.0"]["integrity"], any(upstream6))
	}
}

// TestRenderProxyFallbacks: the abbreviated kind of a full packument (an upstream without the
// abbreviated form) keeps the abbreviated members of each version; without a kept packument
// the held versions are rendered as a hosted repo's, and a name the repo does not hold is
// index.ErrNotFound; a kept packument that is not an object is an error.
func TestRenderProxyFallbacks(t *testing.T) {
	p := newPipeline(t)
	holdIsNumber(t, p)
	full := captured(t, capturedIsNumber, false)
	doc, err := Plugin{}.RenderProxy(t.Context(), p.repo, "is-number", kindAbbreviated, full, p.st, renderBase)
	must(t, err)
	var short struct {
		Versions map[string]map[string]json.RawMessage
	}
	must(t, json.Unmarshal(doc.Content, &short))
	for v, vd := range short.Versions {
		extra := slices.DeleteFunc(slices.Collect(maps.Keys(vd)), func(k string) bool { return slices.Contains(abbreviatedKeys, k) })
		if len(extra) > 0 {
			t.Errorf("the abbreviated %s of a full packument has %v", v, extra)
		}
	}
	doc, err = Plugin{}.RenderProxy(t.Context(), p.repo, "is-number", kindPackument, nil, p.st, renderBase)
	must(t, err)
	got := decodeProxied(t, doc.Content)
	if len(got.dists) != 2 || got.dists["99.0.0"]["tarball"] != renderBase+"is-number/-/is-number-99.0.0.tgz" {
		t.Errorf("the held versions alone: %s", doc.Content)
	}
	if _, err := (Plugin{}).RenderProxy(t.Context(), p.repo, "left-pad", kindAbbreviated, nil, p.st, renderBase); !errors.Is(err, index.ErrNotFound) {
		t.Errorf("RenderProxy of a name the repo does not hold: %v", err)
	}
	if _, err := (Plugin{}).RenderProxy(t.Context(), p.repo, "is-number", kindPackument, []byte("null"), p.st, renderBase); err == nil {
		t.Error("RenderProxy of a kept packument that is not an object: no error")
	}
}
