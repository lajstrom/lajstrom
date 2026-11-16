package maven

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/lajstrom/lajstrom/internal/format"
	"github.com/lajstrom/lajstrom/internal/index"
)

// TestProxyRequests: the requests of the documents of a name (docs/spec/http-routing.md, maven,
// Proxy).
func TestProxyRequests(t *testing.T) {
	p := Plugin{}
	for kind, want := range map[string]string{
		kindMetadata:                     "com/example/lib-core/maven-metadata.xml",
		"maven-metadata:1.0-SNAPSHOT":    "com/example/lib-core/1.0-SNAPSHOT/maven-metadata.xml",
		"file:1.2.0/lib-core-1.2.0.jar":  "com/example/lib-core/1.2.0/lib-core-1.2.0.jar.sha1",
		"file:1.0-SNAPSHOT/lib-core.pom": "com/example/lib-core/1.0-SNAPSHOT/lib-core.pom.sha1",
	} {
		req, err := p.MetadataRequest("com.example:lib-core", kind)
		if err != nil || req.Path != want {
			t.Errorf("MetadataRequest of %s: %q, %v; want %q", kind, req.Path, err, want)
		}
	}
	for _, kind := range []string{"packument", "maven-metadata:1.0", "file:lib-core-1.2.0.jar"} {
		if _, err := p.MetadataRequest("com.example:lib-core", kind); !errors.Is(err, format.ErrInvalid) {
			t.Errorf("MetadataRequest of %s: %v, want ErrInvalid", kind, err)
		}
	}
	if _, err := p.Key("com.example"); !errors.Is(err, format.ErrInvalid) {
		t.Errorf("Key of a name without an artifactId: %v", err)
	}
}

// TestProxyFileEntry: the path of a file is the artifact of that one file, listed by the entry
// of its .sha1, which keeps the lowercase hex of its first word.
func TestProxyFileEntry(t *testing.T) {
	p := Plugin{}
	ref, err := p.FileRequest("com/example/lib-core/1.0-SNAPSHOT/lib-core-1.0-20261114.101112-3-sources.jar")
	want := format.ArtifactRef{Name: "com.example:lib-core", Version: "1.0-20261114.101112-3", Variant: "jar:sources",
		File: "lib-core-1.0-20261114.101112-3-sources.jar", Kind: "file:1.0-SNAPSHOT/lib-core-1.0-20261114.101112-3-sources.jar"}
	if err != nil || ref != want {
		t.Errorf("FileRequest of a build's file: %+v, %v", ref, err)
	}
	for _, path := range []string{"com/example/lib-core/1.0-SNAPSHOT/lib-core-1.0-SNAPSHOT.jar", "com/example/lib-core/maven-metadata.xml",
		"com/example/lib-core/1.2.0/lib-core-1.2.0.jar.sha1"} {
		if _, err := p.FileRequest(path); !errors.Is(err, index.ErrNotFound) {
			t.Errorf("FileRequest of %s: %v, want ErrNotFound", path, err)
		}
	}

	sum := strings.Repeat("ab", 20)
	parsed, err := p.ParseMetadata("com.example:lib-core", ref.Kind, []byte(strings.ToUpper(sum)+" *lib-core-1.0-20261114.101112-3-sources.jar\n"))
	if err != nil || string(parsed.Keep) != sum || len(parsed.Entries) != 1 {
		t.Fatalf("ParseMetadata of a .sha1: %+v, %v", parsed, err)
	}
	e := parsed.Entries[0]
	got := format.ArtifactRef{Name: ref.Name, Version: e.Version, Variant: e.Variant, Kind: ref.Kind}
	if len(e.Files) == 1 {
		got.File = e.Files[0].Name
		expect(t, "the checksums and the path of the entry", []string{e.Files[0].Checksums[SHA1], e.Params[ParamPath]},
			[]string{sum, "com/example/lib-core/" + strings.TrimPrefix(ref.Kind, "file:")})
	}
	expect(t, "the entry of a .sha1", got, ref)
	again, err := p.ParseMetadata("com.example:lib-core", ref.Kind, parsed.Keep)
	if err != nil || again.Entries[0].Files[0].Checksums[SHA1] != sum {
		t.Errorf("ParseMetadata of what it kept: %+v, %v", again, err)
	}
	for _, body := range []string{"", "not hex", strings.Repeat("ab", 32)} {
		if _, err := p.ParseMetadata("com.example:lib-core", ref.Kind, []byte(body)); !errors.Is(err, format.ErrInvalid) {
			t.Errorf("ParseMetadata of the .sha1 %q: %v, want ErrInvalid", body, err)
		}
	}
}

// TestProxyParseMetadata: an upstream's maven-metadata.xml is kept when it names the coordinates
// of its path, or none of them, as a group's plugin prefixes do, and is refused otherwise.
func TestProxyParseMetadata(t *testing.T) {
	p := Plugin{}
	plugins := `<metadata><plugins><plugin><name>Maven Dependency Plugin</name><prefix>dependency</prefix>` +
		`<artifactId>maven-dependency-plugin</artifactId></plugin></plugins></metadata>`
	for _, tt := range []struct {
		name, kind, body string
		ok               bool
	}{
		{"com.example:lib-core", kindMetadata, string(upstreamDoc("1.2.0", "1.2.0", "1.2.0")), true},
		{"com.example:lib-core", kindMetadata, `<metadata><groupId>com.example</groupId><artifactId>lib-core</artifactId><version>1.2.0</version></metadata>`, true},
		{"org.apache.maven:plugins", kindMetadata, plugins, true},
		{"org.apache.maven:plugins", kindMetadata, `<metadata><groupId>org.apache.maven.plugins</groupId>` + plugins[len("<metadata>"):], true},
		{"com.example:lib-api", kindMetadata, string(upstreamDoc("1.2.0", "1.2.0", "1.2.0")), false},
		{"com.example:lib-core", "maven-metadata:1.0-SNAPSHOT", `<metadata><version>1.1-SNAPSHOT</version></metadata>`, false},
		{"com.example:lib-core", kindMetadata, `<project/>`, false},
		{"com.example:lib-core", kindMetadata, `{"versions": []}`, false},
	} {
		parsed, err := p.ParseMetadata(tt.name, tt.kind, []byte(tt.body))
		switch {
		case tt.ok && (err != nil || string(parsed.Keep) != tt.body || len(parsed.Entries) != 0):
			t.Errorf("%s %s: %+v, %v", tt.name, tt.body, parsed, err)
		case !tt.ok && !errors.Is(err, format.ErrInvalid):
			t.Errorf("%s %s: %v, want ErrInvalid", tt.name, tt.body, err)
		}
	}
}

// TestProxyKeep: the versions a repo does not take leave the listing, and latest and release
// are chosen anew among the others when they were among them.
func TestProxyKeep(t *testing.T) {
	for _, tt := range []struct {
		policy, latest, release string
		versions                []string
		want                    []string // the versions, latest, release
	}{
		{PolicyRelease, "2.0-SNAPSHOT", "1.1", []string{"1.0", "1.1", "2.0-rc1", "2.0-SNAPSHOT"}, []string{"1.0 1.1 2.0-rc1", "2.0-rc1", "1.1"}},
		{PolicyRelease, "1.1", "", []string{"1.0", "1.1"}, []string{"1.0 1.1", "1.1", "1.1"}},
		{PolicySnapshot, "1.1", "1.1", []string{"1.0-SNAPSHOT", "1.1"}, []string{"1.0-SNAPSHOT", "1.0-SNAPSHOT", ""}},
	} {
		doc, err := readRepoMetadata(upstreamDoc(tt.latest, tt.release, tt.versions...))
		must(t, err)
		repo := index.Repo{Name: "central", Policy: []byte(`{"maven": {"versionPolicy": "` + tt.policy + `"}}`)}
		doc.keep(func(v string) bool { return takes(repo, v) })
		v := doc.Versioning
		if got := []string{strings.Join(v.Versions, " "), v.Latest, v.Release}; !slices.Equal(got, tt.want) {
			t.Errorf("%s of %v, latest %s, release %s: %q, want %q", tt.policy, tt.versions, tt.latest, tt.release, got, tt.want)
		}
	}
}
