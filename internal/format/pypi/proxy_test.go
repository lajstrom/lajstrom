package pypi

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/lajstrom/lajstrom/internal/format"
	"github.com/lajstrom/lajstrom/internal/index"
)

// TestProxyRequests: the key of a name is its PEP 503 form, the page of a project is asked at
// <project>/ for the JSON form first, and a file's path maps to its project, version and
// variant only when its file name says the same project and version.
func TestProxyRequests(t *testing.T) {
	p := Plugin{}
	if key, err := p.Key("My_Pkg.Extra"); err != nil || key != "my-pkg-extra" {
		t.Errorf("Key: %q, %v", key, err)
	}
	if _, err := p.Key("-bad"); !errors.Is(err, format.ErrInvalid) {
		t.Errorf("Key of a name that is not a project's: %v", err)
	}
	req, err := p.MetadataRequest("My_Pkg", kindJSON)
	must(t, err)
	expect(t, "the page's request", req, format.UpstreamRequest{Path: "my-pkg/", Accept: acceptUpstream})
	if _, err := p.MetadataRequest("my-pkg", kindHTML); !errors.Is(err, format.ErrInvalid) {
		t.Errorf("a request of the HTML kind: %v", err)
	}
	for path, want := range map[string]format.ArtifactRef{
		"files/My_Pkg/1.0/my_pkg-1.0-py3-none-any.whl": {Name: "my-pkg", Version: "1.0", Variant: "py3-none-any",
			File: "my_pkg-1.0-py3-none-any.whl", Kind: kindJSON},
		"files/my-pkg/1.0/my_pkg-1.0-2-cp311-cp311-win_amd64.whl": {Name: "my-pkg", Version: "1.0", Variant: "2-cp311-cp311-win_amd64",
			File: "my_pkg-1.0-2-cp311-cp311-win_amd64.whl", Kind: kindJSON},
		"files/my-pkg/1.0/my-pkg-1.0.zip": {Name: "my-pkg", Version: "1.0", Variant: VariantSdist, File: "my-pkg-1.0.zip", Kind: kindJSON},
	} {
		got, err := p.FileRequest(path)
		if err != nil || got != want {
			t.Errorf("FileRequest(%s) = %+v, %v; want %+v", path, got, err, want)
		}
	}
	for _, path := range []string{"files/my-pkg/1.1/my_pkg-1.0.tar.gz", "files/other/1.0/my_pkg-1.0.tar.gz", "files/my-pkg/1.0/my_pkg-1.0.egg",
		"simple/my-pkg/", "files/my-pkg/1.0", "files/-x/1.0/x-1.0.tar.gz"} {
		if _, err := p.FileRequest(path); !errors.Is(err, index.ErrNotFound) {
			t.Errorf("FileRequest(%s): %v, want ErrNotFound", path, err)
		}
	}
}

// TestParseUpstreamPage reads a JSON page and an HTML page into what a proxy keeps: the files it
// can fetch, their URLs without fragment and relative to the page as the upstream wrote them, and
// the entries with their URLs below the upstream's; the kept page parses to the same entries.
func TestParseUpstreamPage(t *testing.T) {
	sum := strings.Repeat("ab", 32)
	other := strings.Repeat("CD", 32)
	pages := map[string]string{
		"json": `{"meta": {"_last-serial": 26054547, "api-version": "1.1"}, "name": "My-Pkg", "versions": ["1.0"], "files": [
			{"filename": "my_pkg-1.0-py3-none-any.whl", "url": "https://files.example/p/my_pkg-1.0-py3-none-any.whl#sha256=` + sum + `",
			 "hashes": {"sha256": "` + sum + `", "md5": "00"}, "requires-python": ">=3.9", "dist-info-metadata": {"sha256": "` + other + `"},
			 "size": 10, "upload-time": "2026-01-02T03:04:05.000006Z", "yanked": "bad"},
			{"filename": "my_pkg-1.0.tar.gz", "url": "../../packages/my_pkg-1.0.tar.gz", "hashes": {"sha256": "` + other + `"},
			 "core-metadata": {"sha256": "` + sum + `"}, "yanked": false},
			{"filename": "my_pkg-1.0.zip", "url": "my_pkg-1.0.zip", "hashes": {"sha256": "` + sum + `"}},
			{"filename": "my_pkg-0.9.zip", "url": "/packages/my_pkg-0.9.zip", "hashes": {"sha256": "` + sum + `"}, "yanked": ""},
			{"filename": "my_pkg-0.9.exe", "url": "my_pkg-0.9.exe", "hashes": {"sha256": "` + sum + `"}},
			{"filename": "my_pkg-0.8.tar.gz", "url": "my_pkg-0.8.tar.gz", "hashes": {"md5": "00"}},
			{"filename": "my_pkg-0.7.tar.gz", "url": "ftp://x/my_pkg-0.7.tar.gz", "hashes": {"sha256": "` + sum + `"}}]}`,
		"html": `<html><body><h1>Links for my-pkg</h1>
			<a href="https://files.example/p/my_pkg-1.0-py3-none-any.whl#sha256=` + sum + `" data-requires-python="&gt;=3.9"
			   data-dist-info-metadata="sha256=` + other + `" data-yanked="bad">my_pkg-1.0-py3-none-any.whl</a>
			<a data-core-metadata=sha256=` + sum + ` href=../../packages/my_pkg-1.0.tar.gz#sha256=` + other + `>my_pkg-1.0.tar.gz</a>
			<a href='my_pkg-1.0.zip#sha256=` + sum + `'>my_pkg-1.0.zip</a>
			<a href="/packages/my_pkg-0.9.zip#sha256=` + sum + `" data-yanked>x</a><a href="my_pkg-0.9.exe#sha256=` + sum + `">x</a>
			<a href="my_pkg-0.8.tar.gz#md5=00">x</a><!-- <a href="my_pkg-0.6.tar.gz#sha256=` + sum + `">x</a> -->
			</body></html>`,
	}
	wantURLs := []string{"https://files.example/p/my_pkg-1.0-py3-none-any.whl", "my-pkg/../../packages/my_pkg-1.0.tar.gz", "/packages/my_pkg-0.9.zip"}
	for form, body := range pages {
		got, err := Plugin{}.ParseMetadata("my_pkg", kindJSON, []byte(body))
		must(t, err)
		var urls []string
		for _, e := range got.Entries {
			urls = append(urls, e.Files[0].URL)
		}
		expect(t, form+": the entries' URLs", urls, wantURLs)
		expect(t, form+": the wheel's entry", got.Entries[0], format.UpstreamEntry{Version: "1.0", Variant: "py3-none-any",
			Files:     []format.UpstreamFile{{Name: "my_pkg-1.0-py3-none-any.whl", URL: wantURLs[0], Checksums: map[string]string{"sha256": sum}}},
			Published: map[string]time.Time{"json": time.Date(2026, 1, 2, 3, 4, 5, 6000, time.UTC)}[form]})
		again, err := Plugin{}.ParseMetadata("my-pkg", kindJSON, got.Keep)
		must(t, err)
		expect(t, form+": the kept page parsed again", again, got)
		var kept upstreamPage
		must(t, json.Unmarshal(got.Keep, &kept))
		f := kept.Files[0]
		expect(t, form+": the kept wheel", []any{f.URL, f.RequiresPython, coreHash(f.CoreMetadata, nil), string(f.Yanked)},
			[]any{wantURLs[0], ">=3.9", strings.ToLower(other), `"bad"`})
		expect(t, form+": the kept sdists", []any{kept.Files[1].CoreMetadata, string(kept.Files[2].Yanked)}, []any{nil, "true"})
	}
	for name, body := range map[string]string{"another project's page": `{"name": "other", "files": []}`, "broken JSON": `{"name": `} {
		if _, err := (Plugin{}).ParseMetadata("my-pkg", kindJSON, []byte(body)); !errors.Is(err, format.ErrInvalid) {
			t.Errorf("%s: %v, want ErrInvalid", name, err)
		}
	}
}

// FuzzUpstreamPage feeds ParseMetadata a project's page as an upstream may send it, HTML or
// JSON. It must not fail but with format.ErrInvalid, and what it keeps parses again to the same
// entries, each a file of the project that FileRequest maps back to its version and variant.
func FuzzUpstreamPage(f *testing.F) {
	sum := strings.Repeat("0f", 32)
	for _, s := range []string{
		`<a href="../../p/my_pkg-1.0-py3-none-any.whl#sha256=` + sum + `" data-yanked data-requires-python="&lt;4">x</a>`,
		`<A HREF=my_pkg-1.0.tar.gz#sha256=` + sum + ` data-core-metadata=sha256=` + sum + `>`, `<a href='x'<a href="y">`, "<!-- <a", "<a",
		`{"name": "my-pkg", "files": [{"filename": "my_pkg-1.0.zip", "url": "u", "hashes": {"sha256": "` + sum + `"}, "yanked": "r"}]}`,
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		got, err := Plugin{}.ParseMetadata("my-pkg", kindJSON, []byte(s))
		if err != nil {
			if !errors.Is(err, format.ErrInvalid) {
				t.Fatalf("ParseMetadata: %v, want ErrInvalid", err)
			}
			return
		}
		again, err := Plugin{}.ParseMetadata("my-pkg", kindJSON, got.Keep)
		if err != nil || !reflect.DeepEqual(again.Entries, got.Entries) {
			t.Fatalf("the kept page parses again to %+v, %v; want %+v", again, err, got.Entries)
		}
		for _, e := range got.Entries {
			ref, err := Plugin{}.FileRequest(filePath("my-pkg", e.Version, e.Files[0].Name))
			if err != nil || ref.Version != e.Version || ref.Variant != e.Variant {
				t.Fatalf("the entry %+v maps back to %+v, %v", e, ref, err)
			}
		}
	})
}
