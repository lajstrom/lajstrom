package maven

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/opencontainers/go-digest"

	"github.com/lajstrom/lajstrom/internal/format"
	"github.com/lajstrom/lajstrom/internal/index"
	"github.com/lajstrom/lajstrom/internal/index/sqlite"
	"github.com/lajstrom/lajstrom/internal/ingest"
)

// snapshotRows writes the versions of com.example:lib-core into a new store of the repo
// maven/snapshots, each with the files of its names: the metadata layer lists them, or, for a
// name list that starts with "degraded", the rows have no metadata and only the files.
func snapshotRows(t *testing.T, versions map[string][]string) (*sqlite.Store, index.Repo) {
	t.Helper()
	ctx := t.Context()
	st, err := sqlite.Open(ctx, "file:maven-"+strings.ReplaceAll(t.Name(), "/", "-")+"?mode=memory&cache=shared")
	must(t, err)
	t.Cleanup(func() { _ = st.Close() })
	must(t, st.Migrate(ctx))
	r := index.Repo{Format: Name, Name: "snapshots", Registry: "local", Prefix: "maven/snapshots", Kind: "hosted"}
	must(t, st.CreateRepo(ctx, &r))
	at := time.Date(2026, 11, 14, 9, 0, 0, 0, time.UTC)
	i := 0
	for v, names := range versions {
		c := Coordinates{GroupID: "com.example", ArtifactID: "lib-core", Version: v}
		md := metadata{GroupID: c.GroupID, ArtifactID: c.ArtifactID, Version: v, Packaging: "jar"}
		degraded := len(names) > 0 && names[0] == "degraded"
		if degraded {
			names = names[1:]
		}
		var files []index.File
		for _, n := range names {
			f, err := ParseFilename(c, n)
			must(t, err)
			md.Files = append(md.Files, fileMetadata{Name: n, Classifier: f.Classifier, Extension: f.Extension})
			files = append(files, index.File{Name: n, MediaType: "application/vnd.lajstrom.maven.artifact.v1", Digest: digest.FromString(v + n)})
		}
		raw, err := json.Marshal(md)
		must(t, err)
		if degraded {
			raw = nil
		}
		i++
		rows := ingest.Rows{
			Package: index.Package{RepoID: r.ID, Name: c.Name(), NameOCI: "com.example/lib-core"},
			Version: index.Version{Version: v, Tag: v, Digest: digest.FromString(v), ArtifactType: "application/vnd.lajstrom.maven.v1",
				CreatedAt: at.Add(time.Duration(i) * time.Minute), Metadata: raw, Degraded: degraded},
			Files:   files,
			OCIRepo: "maven/snapshots/com.example/lib-core",
		}
		must(t, ingest.WriteRows(ctx, st, &rows, rows.Version.CreatedAt))
	}
	return st, r
}

// TestRenderSnapshot renders the maven-metadata.xml of a SNAPSHOT version from the builds the
// index has of it, written in no order: snapshot names the latest by Maven's order, the
// timestamp then the build number (two builds of one number by two clients, the later timestamp
// wins; build 10 after build 9 of one second); lastUpdated is its timestamp; snapshotVersions
// has one entry per classifier and extension, from the newest build that has the file (the
// sources only in an older build, a degraded build read from its files), sorted. Builds of
// another SNAPSHOT version and releases are not in it, and a SNAPSHOT version without a build
// renders nothing.
func TestRenderSnapshot(t *testing.T) {
	st, r := snapshotRows(t, map[string][]string{
		"1.0-20261114.101112-1": {"lib-core-1.0-20261114.101112-1.jar", "lib-core-1.0-20261114.101112-1-sources.jar", "lib-core-1.0-20261114.101112-1.pom"},
		"1.0-20261114.120000-9": {"lib-core-1.0-20261114.120000-9.jar", "lib-core-1.0-20261114.120000-9.pom"},
		"1.0-20261114.120000-10": {"degraded", "lib-core-1.0-20261114.120000-10.jar", "lib-core-1.0-20261114.120000-10.pom",
			"lib-core-1.0-20261114.120000-10.pom.asc"},
		"1.0-20261114.110000-10": {"lib-core-1.0-20261114.110000-10.jar", "lib-core-1.0-20261114.110000-10-javadoc.jar"},
		"2.0-20261115.000000-1":  {"lib-core-2.0-20261115.000000-1.jar"},
		"0.9":                    {"lib-core-0.9.jar"},
	})
	docs, err := Plugin{}.RenderIndex(t.Context(), r, "com.example/lib-core/1.0-SNAPSHOT", st)
	must(t, err)
	if len(docs) != 5 || docs[0].Path != "com/example/lib-core/1.0-SNAPSHOT/maven-metadata.xml" || docs[0].Kind != kindMetadata ||
		docs[2].Path != "com/example/lib-core/1.0-SNAPSHOT/maven-metadata.xml.sha1" {
		t.Fatalf("documents %+v, want the document and its four checksums", docs)
	}
	golden(t, "snapshot-maven-metadata.xml", docs[0].Content)
	if sums := sumsOf(docs[0].Content); string(docs[2].Content) != sums[SHA1] {
		t.Errorf("the sha1 file %q, the document's %s", docs[2].Content, sums[SHA1])
	}

	for _, tt := range []struct {
		scope format.IndexScope
		docs  int
	}{
		{"com.example/lib-core/2.0-SNAPSHOT", 5},
		{"com.example/lib-core/3.0-SNAPSHOT", 0},
		{"com.example/nothing/1.0-SNAPSHOT", 0},
	} {
		docs, err := Plugin{}.RenderIndex(t.Context(), r, tt.scope, st)
		if err != nil || len(docs) != tt.docs {
			t.Errorf("RenderIndex(%q) = %d documents, %v; want %d", tt.scope, len(docs), err, tt.docs)
		}
	}
	for _, scope := range []format.IndexScope{"com.example/lib-core/1.0", "com.example/lib-core/1.0-snapshot",
		"com.example/lib-core/1.0-20261114.101112-1", "com.example/lib-core/1.0-SNAPSHOT/x"} {
		if _, err := (Plugin{}).RenderIndex(t.Context(), r, scope, st); err == nil {
			t.Errorf("RenderIndex(%q): no error, want index.ErrNotFound", scope)
		}
	}
}

// TestRenderArtifactWithBuilds: the maven-metadata.xml of an artifact lists a SNAPSHOT version
// once for all its builds, in Maven's order among the releases; latest may be it, release never.
func TestRenderArtifactWithBuilds(t *testing.T) {
	for _, tt := range []struct {
		name     string
		versions []string
		want     string
	}{
		{"builds only", []string{"1.0-20261114.101112-1", "1.0-20261114.111112-2", "1.1-20261114.121112-1"},
			"<latest>1.1-SNAPSHOT</latest>\n    <versions>\n      <version>1.0-SNAPSHOT</version>\n      <version>1.1-SNAPSHOT</version>\n    </versions>"},
		{"builds and releases", []string{"0.9", "1.0-20261114.101112-1", "1.0", "1.1-20261114.101112-1"},
			"<latest>1.1-SNAPSHOT</latest>\n    <release>1.0</release>\n    <versions>\n      <version>0.9</version>\n      <version>1.0-SNAPSHOT</version>\n      <version>1.0</version>\n      <version>1.1-SNAPSHOT</version>"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			vs := map[string][]string{}
			for _, v := range tt.versions {
				vs[v] = []string{"lib-core-" + v + ".jar"}
			}
			st, r := snapshotRows(t, vs)
			docs, err := Plugin{}.RenderIndex(t.Context(), r, "com.example/lib-core", st)
			must(t, err)
			if body := string(docs[0].Content); !strings.Contains(body, tt.want) {
				t.Errorf("the document:\n%s\nwant it to have\n%s", body, tt.want)
			}
		})
	}
}
