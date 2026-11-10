package npm

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/lajstrom/lajstrom/internal/format"
	"github.com/lajstrom/lajstrom/internal/index"
	"github.com/lajstrom/lajstrom/internal/oci/profile"
	"github.com/lajstrom/lajstrom/internal/testutil"
)

var repo = index.Repo{ID: 1, Format: Name, Name: "internal", Registry: "local", Prefix: "npm/internal", Kind: "hosted"}

// upload is the Upload of a tarball with the version document doc ("" for none).
func upload(b []byte, doc string) format.Upload {
	return format.Upload{Params: map[string]string{ParamUploadedBy: "user:alice", ParamDocument: doc}, Files: []format.UploadedFile{{
		Name: "x.tgz", Size: int64(len(b)), Open: func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(b)), nil },
	}}}
}

// document returns the version document npm sends for a tarball: its package.json with what npm
// adds, the readme, _id and the like, and dist.
func document(t *testing.T, info testutil.NpmInfo, change func(map[string]any)) string {
	t.Helper()
	var doc map[string]any
	must(t, json.Unmarshal(info.PackageJSON, &doc))
	doc["readme"] = "# " + info.Name
	doc["_id"] = info.Name + "@" + info.Version
	doc["_nodeVersion"] = "22.11.0"
	doc["dist"] = map[string]any{"integrity": info.Integrity, "shasum": info.Shasum, "tarball": "http://placeholder/x.tgz"}
	if change != nil {
		change(doc)
	}
	b, err := json.Marshal(doc)
	must(t, err)
	return string(b)
}

// buildCase is a version to build: unscoped and scoped, with and without a version document.
type buildCase struct {
	name, path string
	opts       testutil.NpmOptions
	withDoc    bool
	readme     string // the body's top-level readme
	wantReadme string
	shrinkwrap bool
	installs   bool
}

var buildCases = []buildCase{
	{name: "unscoped", path: "npm/internal/widgets", withDoc: true, wantReadme: "# widgets"},
	{name: "scoped", path: "npm/internal/acme/widgets", opts: testutil.NpmOptions{Name: "@acme/widgets", Version: "2.0.0-rc.1",
		Dependencies: map[string]string{"widgets": "^1.0.0"}}, withDoc: true, wantReadme: "# @acme/widgets"},
	{name: "package.json alone", path: "npm/internal/widgets", opts: testutil.NpmOptions{Shrinkwrap: true, BindingGyp: true},
		readme: "from the body", wantReadme: "from the body", shrinkwrap: true, installs: true},
	{name: "an old name", path: "npm/internal/jsonstream", opts: testutil.NpmOptions{Name: "JSONStream",
		Scripts: map[string]string{"install": "node x.js"}}, readme: readmePlaceholder, installs: true},
}

// build builds the version of tc.
func (tc buildCase) build(t *testing.T) (*format.Built, []byte, testutil.NpmInfo) {
	t.Helper()
	b, info := testutil.NpmTarball(tc.opts)
	up := upload(b, "")
	if tc.withDoc {
		up = upload(b, document(t, info, nil))
	}
	up.Params[ParamReadme] = tc.readme
	built, err := Plugin{}.Build(t.Context(), repo, up)
	must(t, err)
	return built, b, info
}

// TestBuild builds versions, unscoped and scoped, with and without a document, and checks the
// manifest: the annotations, the tag, the path, the tarball layer, the metadata layer, whose
// manifest is the document without dist, readme and the keys that begin with _.
func TestBuild(t *testing.T) {
	for _, tc := range buildCases {
		t.Run(tc.name, func(t *testing.T) {
			built, b, info := tc.build(t)
			a := built.Manifest.Annotations
			for k, want := range map[string]string{profile.AnnotationName: info.Name, profile.AnnotationVersion: info.Version,
				profile.AnnotationFormat: Name, profile.AnnotationRepo: "internal", profile.AnnotationUploadedBy: "user:alice"} {
				if a[k] != want {
					t.Errorf("%s = %q, want %q", k, a[k], want)
				}
			}
			if built.Tag != info.Version || built.RepoPath != tc.path || built.Manifest.ArtifactType != "application/vnd.lajstrom.npm.v1" {
				t.Errorf("tag %q, path %q, artifactType %q", built.Tag, built.RepoPath, built.Manifest.ArtifactType)
			}
			l := built.Manifest.Layers[0]
			if l.MediaType != profile.MediaTypeNpmTarball || l.Digest != digest.FromBytes(b) || l.Annotations[ocispec.AnnotationTitle] != info.Filename ||
				l.Annotations[profile.AnnotationNpmIntegrity] != info.Integrity || l.Annotations[profile.AnnotationNpmShasum] != info.Shasum {
				t.Errorf("tarball layer %+v", l)
			}
			checkMetadata(t, built.Entry.Metadata, metadata{Name: info.Name, Version: info.Version, Readme: tc.wantReadme, Filename: info.Filename,
				Size: info.Size, Integrity: info.Integrity, Shasum: info.Shasum, SHA256: info.SHA256, HasShrinkwrap: tc.shrinkwrap, HasInstallScript: tc.installs})
		})
	}
}

// checkMetadata compares the metadata layer b with want, but its manifest, which must have the
// name and a description and no dist, readme or key that begins with _.
func checkMetadata(t *testing.T, b []byte, want metadata) {
	t.Helper()
	var md metadata
	must(t, json.Unmarshal(b, &md))
	want.Manifest = md.Manifest
	if d := cmp.Diff(want, md); d != "" {
		t.Errorf("metadata (-want +got)\n%s", d)
	}
	var manifest map[string]any
	must(t, json.Unmarshal(md.Manifest, &manifest))
	for k := range manifest {
		if k == "dist" || k == "readme" || strings.HasPrefix(k, "_") {
			t.Errorf("the manifest has %s", k)
		}
	}
	if manifest["name"] != want.Name || manifest["description"] == nil {
		t.Errorf("manifest %v", manifest)
	}
}

// TestBuildRefuses is the version documents and tarballs Build refuses: ErrInvalid each, the
// key named for the manifest confusion; bundledDependencies is bundleDependencies.
func TestBuildRefuses(t *testing.T) {
	ctx := t.Context()
	b, info := testutil.NpmTarball(testutil.NpmOptions{Dependencies: map[string]string{"lodash": "^4.17.21"},
		Bin: map[string]string{"widgets": "cli.js"}, Scripts: map[string]string{"postinstall": "node setup.js"}})
	doc := func(change func(map[string]any)) format.Upload { return upload(b, document(t, info, change)) }
	for _, tc := range []struct {
		name, want string
		up         format.Upload
	}{
		{"another name", "of other@1.0.0", doc(func(d map[string]any) { d["name"] = "other" })},
		{"another version", "of widgets@1.0.1", doc(func(d map[string]any) { d["version"] = "1.0.1" })},
		{"a dependency more", "differ in dependencies", doc(func(d map[string]any) { d["dependencies"].(map[string]any)["evil"] = "*" })},
		{"no dependencies", "differ in dependencies", doc(func(d map[string]any) { delete(d, "dependencies") })},
		{"an optional dependency", "differ in optionalDependencies", doc(func(d map[string]any) { d["optionalDependencies"] = map[string]any{"x": "1"} })},
		{"a peer dependency", "differ in peerDependencies", doc(func(d map[string]any) { d["peerDependencies"] = map[string]any{"x": "1"} })},
		{"a bundled dependency", "differ in bundleDependencies", doc(func(d map[string]any) { d["bundledDependencies"] = []any{"lodash"} })},
		{"another bin", "differ in bin", doc(func(d map[string]any) { d["bin"] = map[string]any{"widgets": "evil.js"} })},
		{"bin as a string of another file", "differ in bin", doc(func(d map[string]any) { d["bin"] = "evil.js" })},
		{"directories.bin in place of bin", "differ in bin", doc(func(d map[string]any) {
			delete(d, "bin")
			d["directories"] = map[string]any{"bin": "."}
		})},
		{"no postinstall", "differ in scripts.postinstall", doc(func(d map[string]any) { delete(d, "scripts") })},
		{"a preinstall", "differ in scripts.preinstall", doc(func(d map[string]any) { d["scripts"].(map[string]any)["preinstall"] = "curl x | sh" })},
		{"a document that is no object", "not a JSON object", upload(b, `["x"]`)},
		{"a readme of 3 MiB", "at most 2097152", doc(func(d map[string]any) { d["readme"] = strings.Repeat("x", 3<<20) })},
		{"two files", "one tarball", func() format.Upload { u := upload(b, ""); u.Files = append(u.Files, u.Files[0]); return u }()},
		{"a size that is not the declared one", "the upload says 3", func() format.Upload { u := upload(b, ""); u.Files[0].Size = 3; return u }()},
		{"no tarball", "not gzip", upload([]byte("not a tarball"), "")},
	} {
		_, err := Plugin{}.Build(ctx, repo, tc.up)
		if !errors.Is(err, format.ErrInvalid) || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: %v, want ErrInvalid with %q", tc.name, err, tc.want)
		}
	}
	for name, change := range map[string]func(map[string]any){
		"the scripts npm does not run on install": func(d map[string]any) { d["scripts"].(map[string]any)["test"] = "jest" },
		"empty optionalDependencies":              func(d map[string]any) { d["optionalDependencies"] = map[string]any{} },
		"bundleDependencies false":                func(d map[string]any) { d["bundleDependencies"] = false },
		"another description":                     func(d map[string]any) { d["description"] = "other" },
		"bin as a string of the same file":        func(d map[string]any) { d["bin"] = "./cli.js" },
	} {
		if _, err := (Plugin{}).Build(ctx, repo, doc(change)); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
	for _, o := range []testutil.NpmOptions{{Version: "1.0.0+build"}, {Name: "node_modules"}, {Version: "01.0.0"}} {
		tgz, _ := testutil.NpmTarball(o)
		if _, err := (Plugin{}).Build(ctx, repo, upload(tgz, "")); !errors.Is(err, format.ErrInvalid) {
			t.Errorf("%+v: %v, want ErrInvalid", o, err)
		}
	}
}
