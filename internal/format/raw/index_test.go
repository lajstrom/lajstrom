package raw

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/opencontainers/go-digest"

	"github.com/lajstrom/lajstrom/internal/format"
	"github.com/lajstrom/lajstrom/internal/index"
	"github.com/lajstrom/lajstrom/internal/oci/profile"
)

// addVersion adds a version of path p with one file of size bytes, created at minute min of
// the day, to the store, and returns its manifest digest. files other than 1 adds that many.
func addVersion(t *testing.T, st index.Writer, repo index.Repo, p string, size int64, minute, files int) digest.Digest {
	t.Helper()
	ctx := t.Context()
	pkg := index.Package{RepoID: repo.ID, Name: p, NameOCI: escapePath(p)}
	must(t, st.UpsertPackage(ctx, &pkg))
	d := digest.FromString(p + time.Duration(minute).String())
	v := index.Version{PackageID: pkg.ID, Version: d.Encoded()[:16], Tag: d.Encoded()[:16], Digest: d,
		ArtifactType: profile.ArtifactType("raw"), CreatedAt: time.Date(2026, 11, 7, 9, minute, 0, 0, time.UTC)}
	must(t, st.UpsertVersion(ctx, &v))
	var fs []index.File
	for range files {
		fs = append(fs, index.File{Name: p, MediaType: profile.MediaTypeRawFile, Digest: digest.FromString(p), Size: size})
	}
	must(t, st.ReplaceFiles(ctx, v.ID, fs))
	return d
}

// TestRenderIndex checks the listing JSON: recursive below the prefix, one entry per version,
// sorted by path and then created, the manifest digest, an empty root, and a prefix without
// paths.
func TestRenderIndex(t *testing.T) {
	ctx := t.Context()
	st, repo := newStore(t, "")
	render := func(scope string) (string, error) {
		docs, err := Plugin{}.RenderIndex(ctx, repo, format.IndexScope(scope), st)
		if err != nil {
			return "", err
		}
		if len(docs) != 1 || docs[0].Kind != "listing" || docs[0].Path != scope || docs[0].ContentType != "application/json" {
			t.Errorf("documents of %q = %+v", scope, docs)
		}
		return string(docs[0].Content), nil
	}
	if got, err := render(""); err != nil || got != `{"entries":[]}` {
		t.Errorf("listing of an empty repo = %s, %v", got, err)
	}

	b2 := addVersion(t, st, repo, "docs/b.txt", 20, 30, 1)
	b1 := addVersion(t, st, repo, "docs/b.txt", 10, 5, 1)
	a := addVersion(t, st, repo, "docs/a.txt", 1, 40, 1)
	deep := addVersion(t, st, repo, "docs/sub/c.txt", 3, 1, 1)
	top := addVersion(t, st, repo, "top.txt", 0, 2, 1)
	addVersion(t, st, repo, "docs/two-files", 1, 3, 2) // not a raw version: left out
	entry := func(p string, size int64, d digest.Digest, minute string) string {
		return fmt.Sprintf(`{"path":%q,"size":%d,"digest":%q,"created":"2026-11-07T09:%s:00Z"}`, p, size, d, minute)
	}
	docs := entry("docs/a.txt", 1, a, "40") + "," + entry("docs/b.txt", 10, b1, "05") + "," + entry("docs/b.txt", 20, b2, "30") +
		"," + entry("docs/sub/c.txt", 3, deep, "01")
	for scope, want := range map[string]string{
		"":          `{"entries":[` + docs + "," + entry("top.txt", 0, top, "02") + `]}`,
		"docs/":     `{"entries":[` + docs + `]}`,
		"docs/sub/": `{"entries":[` + entry("docs/sub/c.txt", 3, deep, "01") + `]}`,
	} {
		if got, err := render(scope); err != nil || got != want {
			t.Errorf("listing of %q:\n got %s, %v\nwant %s", scope, got, err, want)
		}
	}
	for _, scope := range []string{"nothing/", "doc/", "docs"} {
		if _, err := (Plugin{}).RenderIndex(ctx, repo, format.IndexScope(scope), st); !errors.Is(err, index.ErrNotFound) {
			t.Errorf("RenderIndex(%q) = %v, want ErrNotFound", scope, err)
		}
	}
}
