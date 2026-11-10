package npm

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lajstrom/lajstrom/internal/format"
	"github.com/lajstrom/lajstrom/internal/testutil"
)

// TestNormaliseCaptured publishes the bodies npm 10.9.9, npm 11.19.1 and yarn 4.18.1 sent for a
// string bin ("./cli.js", a scoped package), an object bin with ./, a directories.bin with a
// subdirectory and a dotfile, binding.gyp alone and with a postinstall. npm's documents differ
// from the tarball's package.json in what an install runs until both are normalised; yarn's are
// its package.json as it is. Every one is 201.
func TestNormaliseCaptured(t *testing.T) {
	p := newPipeline(t)
	h := p.routes(nil)
	files, err := filepath.Glob("testdata/normalise/*.json")
	must(t, err)
	if len(files) != 15 {
		t.Fatalf("%d captured bodies, want 15", len(files))
	}
	for _, f := range files {
		raw, err := os.ReadFile(f) //nolint:gosec // a captured body of this package's testdata
		must(t, err)
		var body struct {
			Name        string
			Versions    map[string]json.RawMessage
			Attachments map[string]struct{ Data []byte } `json:"_attachments"`
		}
		must(t, json.Unmarshal(raw, &body))
		for _, a := range body.Attachments {
			tb, err := ReadTarball(bytes.NewReader(a.Data))
			must(t, err)
			doc, err := ParsePackageJSON(body.Versions["1.0.0"])
			must(t, err)
			if npm := strings.HasPrefix(filepath.Base(f), "npm"); (checkInstall(doc, tb.PackageJSON) != nil) != npm {
				t.Errorf("%s: the document and package.json differ before normalisation: %v, want %v", f, !npm, npm)
			}
		}
		rr := do(h, http.MethodPut, "/npm/internal/"+strings.Replace(body.Name, "/", "%2f", 1), raw, false)
		if rr.Code != http.StatusCreated {
			t.Errorf("%s: %d %s", f, rr.Code, rr.Body)
		}
	}
}

// TestNormalise is each normalisation on a package.json of its own.
func TestNormalise(t *testing.T) {
	for _, tc := range []struct {
		name, pkg           string
		gyp                 bool
		wantBin, wantScript string
	}{
		{"a string bin of a scoped package", `"bin": "././cli.js"`, false, `{"cli": "cli.js"}`, ``},
		{"a string bin and directories.bin", `"bin": "cli.js", "directories": {"bin": "bin"}`, false, `{"cli": "cli.js"}`, ``},
		{"an object bin with ./", `"bin": {"a": "./a.js", "b": "b.js", "c": 1}`, false, `{"a": "a.js", "b": "b.js", "c": 1}`, ``},
		{"an empty bin and directories.bin", `"bin": "", "directories": {"bin": "bin/sub"}`, false, `{"nested": "bin/sub/nested"}`, ``},
		{"binding.gyp", ``, true, ``, `{"install": "node-gyp rebuild"}`},
		{"binding.gyp and a postinstall", `"scripts": {"postinstall": "z"}`, true, ``, `{"install": "node-gyp rebuild", "postinstall": "z"}`},
		{"binding.gyp and a preinstall", `"scripts": {"preinstall": "x"}`, true, ``, `{"preinstall": "x"}`},
		{"binding.gyp and an install", `"scripts": {"install": "y"}`, true, ``, `{"install": "y"}`},
		{"no binding.gyp", `"scripts": {"postinstall": "z"}`, false, ``, `{"postinstall": "z"}`},
	} {
		pkg := []byte(`{"name": "@acme/cli", "version": "1.0.0"` + strings.TrimSuffix(", "+tc.pkg, ", ") + `}`)
		b, _ := testutil.NpmTarball(testutil.NpmOptions{PackageJSON: pkg, BindingGyp: tc.gyp, Bin: map[string]string{"n": "bin/sub/nested"}})
		tb, err := ReadTarball(bytes.NewReader(b))
		must(t, err)
		before := string(tb.PackageJSON.Fields["bin"])
		got, err := tb.normalise(tb.PackageJSON)
		must(t, err)
		if !sameValue(got.Fields["bin"], json.RawMessage(tc.wantBin)) || !sameValue(got.Fields["scripts"], json.RawMessage(tc.wantScript)) {
			t.Errorf("%s: bin %s, scripts %s; want %s, %s", tc.name, got.Fields["bin"], got.Fields["scripts"], tc.wantBin, tc.wantScript)
		}
		if string(tb.PackageJSON.Fields["bin"]) != before {
			t.Errorf("%s: package.json changed", tc.name)
		}
	}
}

// TestNormaliseBinDir expands directories.bin as npm 10 and 11 do: every entry below the
// directory and the directories between, by base name, without dotfiles and dot directories;
// nothing out of the package. Past maxPaths, a directories.bin to expand is 400.
func TestNormaliseBinDir(t *testing.T) {
	files := map[string]string{"a": "bin/tool", "b": "bin/sub/nested", "c": "bin/.hidden", "d": "bin/.d/x", "e": "bin/sub/.x", "f": "lib/other"}
	b, _ := testutil.NpmTarball(testutil.NpmOptions{Bin: files})
	tb, err := ReadTarball(bytes.NewReader(b))
	must(t, err)
	all := map[string]any{"tool": "bin/tool", "sub": "bin/sub", "nested": "bin/sub/nested"}
	for dir, want := range map[string]map[string]any{
		"./bin": all, "bin/": all, "bin": all, "bin/sub": {"nested": "bin/sub/nested"},
		"../bin": {}, "/bin": {}, "none": {},
	} {
		got, err := tb.binDir(dir)
		must(t, err)
		expect(t, "directories.bin "+dir, got, want)
	}

	defer func(n int) { maxPaths = n }(maxPaths)
	maxPaths = 32
	tb, err = ReadTarball(bytes.NewReader(b))
	must(t, err)
	tb.PackageJSON.Fields["bin"], tb.PackageJSON.Fields["directories"] = nil, json.RawMessage(`{"bin": "bin"}`)
	if _, err := tb.normalise(tb.PackageJSON); !errors.Is(err, format.ErrInvalid) || !strings.Contains(err.Error(), "cannot be expanded") {
		t.Errorf("past maxPaths: %v", err)
	}
	tb.PackageJSON.Fields["bin"] = json.RawMessage(`{"x": "bin/tool"}`)
	if _, err := tb.normalise(tb.PackageJSON); err != nil {
		t.Errorf("past maxPaths with a bin: %v", err)
	}
}
