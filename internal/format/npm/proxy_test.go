package npm

import (
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/lajstrom/lajstrom/internal/format"
	"github.com/lajstrom/lajstrom/internal/index"
)

// The packuments of testdata/proxy were captured from registry.npmjs.org on 2026-11-10, the
// full one with Accept: application/json and the abbreviated one with pnpm's Accept, of
// is-number (15 versions) and @isaacs/string-locale-compare (3). Personal data and what the
// tests do not read were removed: readme, readmeFilename, users, maintainers, contributors,
// _npmUser, _npmOperationalInternal, author.email and dist.npm-signature.
const (
	capturedIsNumber = "testdata/proxy/is-number.json"
	capturedScoped   = "testdata/proxy/string-locale-compare.json"
	scopedName       = "@isaacs/string-locale-compare"
)

// captured reads a captured packument; abbreviated reads the abbreviated one beside it.
func captured(t *testing.T, path string, abbreviated bool) []byte {
	t.Helper()
	if abbreviated {
		path = strings.TrimSuffix(path, ".json") + ".abbreviated.json"
	}
	b, err := os.ReadFile(path) //nolint:gosec // a captured packument of this package's testdata
	must(t, err)
	return b
}

// TestProxyRequests: the key of a name is the name; the upstream is asked for the full
// packument as npm asks it and for the abbreviated one as pnpm does, the scope's / as %2f.
func TestProxyRequests(t *testing.T) {
	p := Plugin{}
	for _, tc := range []struct{ name, kind, path, accept string }{
		{"is-number", kindPackument, "is-number", "application/json"},
		{"JSONStream", kindPackument, "JSONStream", "application/json"},
		{scopedName, kindAbbreviated, "@isaacs%2fstring-locale-compare", acceptAbbreviated},
	} {
		key, err := p.Key(tc.name)
		if err != nil || key != tc.name {
			t.Errorf("Key(%q) = %q, %v", tc.name, key, err)
		}
		req, err := p.MetadataRequest(tc.name, tc.kind)
		if err != nil || req.Path != tc.path || req.Accept != tc.accept {
			t.Errorf("MetadataRequest(%q, %s) = %+v, %v", tc.name, tc.kind, req, err)
		}
	}
	if _, err := p.Key("@isaacs"); !errors.Is(err, format.ErrInvalid) {
		t.Errorf("Key of a bare scope: %v", err)
	}
	if _, err := p.MetadataRequest("is-number", "simple"); !errors.Is(err, format.ErrInvalid) {
		t.Errorf("MetadataRequest of another kind: %v", err)
	}
}

// TestParseMetadataCaptured reads the captured packuments: an entry per version, its tarball
// at dist.tarball under the name Build gives it, the sha512 of dist.integrity and the sha1 of
// dist.shasum in hex, the publish time of the full packument (none in the abbreviated one).
func TestParseMetadataCaptured(t *testing.T) {
	p := Plugin{}
	for _, tc := range []struct {
		path, name, version, file, sha512, sha1 string
		versions                                int
		published                               time.Time
	}{
		{capturedIsNumber, "is-number", "7.0.0", "is-number-7.0.0.tgz",
			"e350a27e483a7bc4f2952a5db53a5e2d532abd20445734edb47bc4443ef8d7ea6767c00dbf4d34e0c44be3740a3c394af5c1af369e8d6566540656c65d8c719e",
			"7535345b896734d5f80c4d06c50955527a14f12b", 15, time.Date(2018, 7, 4, 15, 8, 58, 238e6, time.UTC)},
		{capturedScoped, scopedName, "1.1.0", "string-locale-compare-1.1.0.tgz",
			"490ecace187df83f995bd300d33918bf75578480cdc7e2f333a109fbfeb9237418f9e9d4e88b6d7bb1395d7ec45aba8b5b614de27d3a196cc19cfa02ded87669",
			"291c227e93fd407a96ecd59879a35809120e432b", 3, time.Date(2021, 9, 28, 18, 14, 13, 693e6, time.UTC)},
	} {
		for _, abbreviated := range []bool{false, true} {
			kind := map[bool]string{false: kindPackument, true: kindAbbreviated}[abbreviated]
			body := captured(t, tc.path, abbreviated)
			idx, err := p.ParseMetadata(tc.name, kind, body)
			must(t, err)
			if string(idx.Keep) != string(body) || len(idx.Entries) != tc.versions {
				t.Fatalf("%s %s: %d entries, kept %d bytes of %d", tc.name, kind, len(idx.Entries), len(idx.Keep), len(body))
			}
			var e format.UpstreamEntry
			for _, x := range idx.Entries {
				if x.Version == tc.version {
					e = x
				}
			}
			want := []format.UpstreamFile{{Name: tc.file, URL: "https://registry.npmjs.org/" + tc.name + "/-/" + tc.file,
				Checksums: map[string]string{"sha512": tc.sha512, "sha1": tc.sha1}}}
			if diff := cmp.Diff(want, e.Files); diff != "" {
				t.Errorf("%s %s %s (-want +got):\n%s", tc.name, kind, tc.version, diff)
			}
			if wantTime := map[bool]time.Time{false: tc.published}[abbreviated]; !e.Published.Equal(wantTime) {
				t.Errorf("%s %s published %v, want %v", tc.name, kind, e.Published, wantTime)
			}
		}
	}
	for _, body := range []string{`{"name": "left-pad", "versions": {}}`, `[]`, `{"name": "is-number", "versions": []}`} {
		if _, err := p.ParseMetadata("is-number", kindPackument, []byte(body)); !errors.Is(err, format.ErrInvalid) {
			t.Errorf("ParseMetadata(%s): %v", body, err)
		}
	}
}

// TestParseMetadataEntries: a version that is not SemVer or not an object is no entry; a
// version without dist.tarball is fetched at <name>/-/<file>; the checksums of integrity and
// shasum, a token that does not decode left out.
func TestParseMetadataEntries(t *testing.T) {
	body := `{"name": "@acme/w", "time": {"1.0.0": "not a time"}, "versions": {
		"1.0.0": {"dist": {"integrity": "sha512-AAAA sha1-AAAAAAAAAAAAAAAAAAAAAAAAAAA= sha256-` + strings.Repeat("A", 43) + `=?opt sha512-x",
			"shasum": "ffffffffffffffffffffffffffffffffffffffff"}},
		"2.0.0": {"dist": {"shasum": "FFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFF", "integrity": "sha384-AAAA"}},
		"v3": {}, "4.0.0": "four", "5.0.0": null}}`
	idx, err := Plugin{}.ParseMetadata("@acme/w", kindPackument, []byte(body))
	must(t, err)
	zero32 := strings.Repeat("00", 32)
	want := []format.UpstreamEntry{
		{Version: "1.0.0", Files: []format.UpstreamFile{{Name: "w-1.0.0.tgz", URL: "@acme%2fw/-/w-1.0.0.tgz",
			Checksums: map[string]string{"sha1": strings.Repeat("00", 20), "sha256": zero32}}}},
		{Version: "2.0.0", Files: []format.UpstreamFile{{Name: "w-2.0.0.tgz", URL: "@acme%2fw/-/w-2.0.0.tgz",
			Checksums: map[string]string{"sha1": strings.Repeat("ff", 20)}}}},
	}
	if diff := cmp.Diff(want, idx.Entries); diff != "" {
		t.Errorf("entries (-want +got):\n%s", diff)
	}
}

// TestPrefetchHooks: a prefetch reads both packuments, the full one first, and without
// allVersions takes the targets of its dist-tags, each once, sorted; a target that is not a
// string is left out, and a body that is not a packument or a kind npm lacks is ErrInvalid.
func TestPrefetchHooks(t *testing.T) {
	if diff := cmp.Diff([]string{kindPackument, kindAbbreviated}, Plugin{}.PrefetchKinds()); diff != "" {
		t.Errorf("PrefetchKinds (-want +got):\n%s", diff)
	}
	got, err := Plugin{}.Pointed("is-number", kindPackument, captured(t, capturedIsNumber, false))
	must(t, err)
	if diff := cmp.Diff([]string{"7.0.0"}, got); diff != "" {
		t.Errorf("Pointed of is-number (-want +got):\n%s", diff)
	}
	body := `{"name": "@acme/w", "dist-tags": {"latest": "1.0.0", "next": "2.0.0-rc.1", "stable": "1.0.0", "odd": 3, "empty": ""}}`
	got, err = Plugin{}.Pointed("@acme/w", kindAbbreviated, []byte(body))
	must(t, err)
	if diff := cmp.Diff([]string{"1.0.0", "2.0.0-rc.1"}, got); diff != "" {
		t.Errorf("Pointed (-want +got):\n%s", diff)
	}
	if got, err := (Plugin{}).Pointed("@acme/w", kindPackument, []byte(`{"name": "@acme/w"}`)); err != nil || len(got) != 0 {
		t.Errorf("Pointed without dist-tags = %v, %v; want none", got, err)
	}
	for _, c := range []struct{ kind, body string }{{kindPackument, `[1]`}, {kindPackument, `{`}, {"other", `{}`}} {
		if _, err := (Plugin{}).Pointed("@acme/w", c.kind, []byte(c.body)); !errors.Is(err, format.ErrInvalid) {
			t.Errorf("Pointed(%s, %s) = %v, want ErrInvalid", c.kind, c.body, err)
		}
	}
}

// TestFileRequest maps the tarball paths npm, pnpm and yarn ask to the version; any other
// path is index.ErrNotFound.
func TestFileRequest(t *testing.T) {
	for _, tc := range []struct{ path, name, version, file string }{
		{"is-number/-/is-number-7.0.0.tgz", "is-number", "7.0.0", "is-number-7.0.0.tgz"},
		{"@acme/widgets/-/widgets-1.2.0-rc.1.tgz", "@acme/widgets", "1.2.0-rc.1", "widgets-1.2.0-rc.1.tgz"},
		{"@acme%2fwidgets/-/widgets-1.2.0.tgz", "@acme/widgets", "1.2.0", "widgets-1.2.0.tgz"},
		{"@acme%2Fwidgets/-/widgets-1.2.0.tgz", "@acme/widgets", "1.2.0", "widgets-1.2.0.tgz"},
	} {
		ref, err := Plugin{}.FileRequest(tc.path)
		want := format.ArtifactRef{Name: tc.name, Version: tc.version, File: tc.file, Kind: kindPackument}
		if err != nil || ref != want {
			t.Errorf("FileRequest(%s) = %+v, %v", tc.path, ref, err)
		}
	}
	for _, path := range []string{"is-number", "is-number/7.0.0", "is-number/-/other-7.0.0.tgz", "is-number/-/is-number-7.0.tgz",
		"is-number/-/is-number-7.0.0.zip", "@acme/-/widgets-1.0.0.tgz", "-/v1/search", "is-number/-/is-number-7.0.0.tgz/x"} {
		if _, err := (Plugin{}).FileRequest(path); !errors.Is(err, index.ErrNotFound) {
			t.Errorf("FileRequest(%s): %v", path, err)
		}
	}
}
