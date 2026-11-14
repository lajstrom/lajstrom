package deb

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"

	"github.com/lajstrom/lajstrom/internal/testutil"
)

// contentsLine is a line of a Contents index: the path padded as contentsOf pads it, then names.
func contentsLine(path, names string) string {
	return path + strings.Repeat(" ", max(1, contentsColumn-len(path))) + names + "\n"
}

// TestRenderContents renders the Contents of a suite: hello 1.0.0-1 and 1.1.0-1 for amd64 (one
// path in each version alone, the shared ones once), hello-tools for arm64 with a symbolic
// link, hello-data for all in the index of every architecture and alone in Contents-all,
// libfoo2 in universe without a file list (its md5sums), and a source package, which has no
// paths; the Release lists every Contents and its .gz.
func TestRenderContents(t *testing.T) {
	p := newPipeline(t)
	for _, u := range []struct {
		opts             testutil.DebOptions
		suite, component string
	}{
		{testutil.DebOptions{Files: map[string]string{"usr/bin/hello": "1", "usr/share/doc/hello/README": "r"}}, "jammy", "main"},
		{testutil.DebOptions{Version: "1.1.0-1", Files: map[string]string{"usr/bin/hello": "2", "usr/share/hello/new": "n"}}, "jammy", "main"},
		{testutil.DebOptions{Name: "hello-tools", Arch: "arm64", Files: map[string]string{"usr/bin/hello-tool": "t"},
			Links: map[string]string{"usr/bin/ht": "hello-tool"}}, "jammy", "main"},
		{testutil.DebOptions{Name: "hello-data", Arch: "all", Files: map[string]string{"usr/share/hello/greetings": "g", "usr/bin/hello": "x"}}, "jammy", "main"},
	} {
		deb, _ := testutil.Deb(u.opts)
		p.uploadTo(t, deb, u.suite, u.component)
	}
	p.addVersion(t, version{name: "libfoo2", version: "2.1-3", suite: "jammy", component: "universe", arch: "amd64",
		metadata: `{"control": {"Package": "libfoo2", "Version": "2.1-3", "Architecture": "amd64", "Section": "libs"},` +
			`"filename": "libfoo2_2.1-3_amd64.deb", "md5sums": {"usr/lib/libfoo.so.2": "b026324c6904b2a9cb4b88d6d61c81d1"},` +
			`"size": 42, "sha256": "` + strings.Repeat("a", 64) + `"}`})
	if _, err := p.pipe.Upload(t.Context(), p.repo, upload(sourceFiles(testutil.Dsc(testutil.DscOptions{}))), alice); err != nil {
		t.Fatal(err)
	}
	docs := render(t, p, fixedClock(p), "jammy")
	for kind, want := range map[string]string{
		"main/Contents-amd64": contentsLine("usr/bin/hello", "misc/hello,misc/hello-data") + contentsLine("usr/share/doc/hello/README", "misc/hello") +
			contentsLine("usr/share/hello/greetings", "misc/hello-data") + contentsLine("usr/share/hello/new", "misc/hello"),
		"main/Contents-arm64": contentsLine("usr/bin/hello", "misc/hello-data") + contentsLine("usr/bin/hello-tool", "misc/hello-tools") +
			contentsLine("usr/bin/ht", "misc/hello-tools") + contentsLine("usr/share/hello/greetings", "misc/hello-data"),
		"main/Contents-all":       contentsLine("usr/bin/hello", "misc/hello-data") + contentsLine("usr/share/hello/greetings", "misc/hello-data"),
		"universe/Contents-amd64": contentsLine("usr/lib/libfoo.so.2", "libs/libfoo2"),
		"universe/Contents-arm64": "",
	} {
		if got := string(docs[kind]); got != want {
			t.Errorf("%s:\n%s\nwant\n%s", kind, got, want)
		}
		if !bytes.Equal(gunzip(t, docs[kind+".gz"]), docs[kind]) {
			t.Errorf("%s.gz is not %s", kind, kind)
		}
	}
	checkRelease(t, docs[kindRelease], docs)
}

// TestContentsRoutes serves the Contents of a component by name and by hash, as the Release
// lists them, and 404 for one it does not list.
func TestContentsRoutes(t *testing.T) {
	e := newAptEnv(t, envOptions{})
	hello, _ := testutil.Deb(testutil.DebOptions{})
	uploaded(t, e.put("suite=jammy&component=main", hello))
	sums := listed(e.get("dists/jammy/Release").Body.Bytes())
	for _, kind := range []string{"main/Contents-amd64", "main/Contents-amd64.gz"} {
		body := e.get("dists/jammy/" + kind).Body.Bytes()
		sum := sha256.Sum256(body)
		if hex.EncodeToString(sum[:]) != sums[kind] {
			t.Errorf("%s is not the file the Release lists", kind)
		}
		if got := e.get("dists/jammy/main/by-hash/SHA256/" + sums[kind]).Body.Bytes(); !bytes.Equal(got, body) {
			t.Errorf("by-hash of %s is another file", kind)
		}
	}
	if !strings.HasPrefix(e.get("dists/jammy/main/Contents-amd64").Body.String(), "usr/share/doc/hello/README ") {
		t.Error("Contents-amd64 does not start with the README of hello")
	}
	for _, path := range []string{"dists/jammy/main/Contents-arm64", "dists/jammy/main/by-hash/SHA256/" + strings.Repeat("0", 64),
		"dists/jammy/main/by-hash/SHA256/" + sums["main/binary-amd64/Packages"]} {
		if rr := e.do("GET", path); rr.Code != 404 {
			t.Errorf("GET %s = %d, want 404", path, rr.Code)
		}
	}
}
