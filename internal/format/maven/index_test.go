package maven

import (
	"errors"
	"flag"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/opencontainers/go-digest"

	"github.com/lajstrom/lajstrom/internal/format"
	"github.com/lajstrom/lajstrom/internal/index"
	"github.com/lajstrom/lajstrom/internal/index/sqlite"
	"github.com/lajstrom/lajstrom/internal/ingest"
)

var update = flag.Bool("update", false, "rewrite the golden files of testdata/")

// golden compares got with testdata/name, or writes it with -update.
func golden(t *testing.T, name string, got []byte) {
	t.Helper()
	p := filepath.Join("testdata", name)
	if *update {
		must(t, os.MkdirAll("testdata", 0o750))
		must(t, os.WriteFile(p, got, 0o600))
		return
	}
	want, err := os.ReadFile(p) //nolint:gosec // a golden file of this package's testdata
	must(t, err)
	if string(want) != string(got) {
		t.Errorf("%s differs from the golden file (go test -run %s -update to rewrite):\n%s", name, t.Name(), got)
	}
}

// TestRenderIndexGolden renders the maven-metadata.xml of an artifact from index rows written in
// an order that is not Maven's, with fixed times: the versions in Maven's order (1.0-rc1 before
// 1.0), latest the greatest (a pre-release), release the greatest that is not a pre-release,
// lastUpdated the newest created whichever version it is, and the four checksum files those of
// the document's bytes. Another artifact of the group is not in it, and one with pre-releases
// only has no release; an artifact the index does not have renders nothing, and a scope that is
// not <groupId>/<artifactId> is index.ErrNotFound.
func TestRenderIndexGolden(t *testing.T) {
	ctx := t.Context()
	st, err := sqlite.Open(ctx, "file:maven-"+t.Name()+"?mode=memory&cache=shared")
	must(t, err)
	t.Cleanup(func() { _ = st.Close() })
	must(t, st.Migrate(ctx))
	r := index.Repo{Format: Name, Name: "releases", Registry: "local", Prefix: "maven/releases", Kind: "hosted"}
	must(t, st.CreateRepo(ctx, &r))
	at := time.Date(2026, 11, 9, 10, 0, 0, 0, time.UTC)
	write := func(name, nameOCI, version string, created time.Time) {
		t.Helper()
		rows := ingest.Rows{
			Package: index.Package{RepoID: r.ID, Name: name, NameOCI: nameOCI},
			Version: index.Version{Version: version, Tag: version, Digest: digest.FromString(name + version),
				ArtifactType: "application/vnd.lajstrom.maven.v1", CreatedAt: created},
			OCIRepo: "maven/releases/" + nameOCI,
		}
		must(t, ingest.WriteRows(ctx, st, &rows, created))
	}
	for i, v := range []string{"1.10", "1.0-alpha-1", "2.0-rc1", "1.0", "1.0-rc1", "1.0.1", "1.0-beta", "1.0-sp1"} {
		created := at.Add(time.Duration(i) * time.Hour)
		if v == "1.0.1" { // the newest, and not the greatest version
			created = at.Add(24*time.Hour + 30*time.Minute + 15*time.Second)
		}
		write("com.example:lib-core", "com.example/lib-core", v, created)
	}
	write("com.example:lib-extra", "com.example/lib-extra", "9.0", at.Add(48*time.Hour))
	write("com.example:lib-next", "com.example/lib-next", "1.0-beta", at)
	write("com.example:lib-next", "com.example/lib-next", "1.0-rc1", at)

	docs, err := Plugin{}.RenderIndex(ctx, r, "com.example/lib-core", st)
	must(t, err)
	var kinds []string
	for _, d := range docs {
		kinds = append(kinds, d.Kind+" "+d.Path+" "+d.ContentType)
	}
	want := []string{
		"maven-metadata com/example/lib-core/maven-metadata.xml application/xml",
		"maven-metadata.md5 com/example/lib-core/maven-metadata.xml.md5 text/plain",
		"maven-metadata.sha1 com/example/lib-core/maven-metadata.xml.sha1 text/plain",
		"maven-metadata.sha256 com/example/lib-core/maven-metadata.xml.sha256 text/plain",
		"maven-metadata.sha512 com/example/lib-core/maven-metadata.xml.sha512 text/plain",
	}
	if !slices.Equal(kinds, want) {
		t.Fatalf("documents %q, want %q", kinds, want)
	}
	golden(t, "maven-metadata.xml", docs[0].Content)
	sums := sumsOf(docs[0].Content)
	for i, a := range []string{MD5, SHA1, SHA256, SHA512} {
		if got := string(docs[i+1].Content); got != sums[a] {
			t.Errorf("the %s file is %q, the document's %s is %s", a, got, a, sums[a])
		}
	}

	next, err := Plugin{}.RenderIndex(ctx, r, "com.example/lib-next", st)
	must(t, err)
	if body := string(next[0].Content); !strings.Contains(body, "<latest>1.0-rc1</latest>") || strings.Contains(body, "<release>") {
		t.Errorf("the metadata of pre-releases only:\n%s", body)
	}

	for _, scope := range []format.IndexScope{"com.example/nothing", "org.other/lib-core"} {
		if docs, err := (Plugin{}).RenderIndex(ctx, r, scope, st); err != nil || docs != nil {
			t.Errorf("RenderIndex(%q) = %d documents, %v; want none", scope, len(docs), err)
		}
	}
	for _, scope := range []format.IndexScope{"", "com.example", "com.example/lib-core/1.0", "com..example/lib-core", "com.example:lib-core"} {
		if _, err := (Plugin{}).RenderIndex(ctx, r, scope, st); !errors.Is(err, index.ErrNotFound) {
			t.Errorf("RenderIndex(%q): %v, want index.ErrNotFound", scope, err)
		}
	}
}
