package storetest

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/lajstrom/lajstrom/internal/index"
)

// The package list, one package and search (docs/spec/index-schema.md, The package list, one
// package and search).

// row is a versions row of a test of this file, with the sizes of its files.
type row struct {
	pkg              *index.Package
	version, variant string
	at               time.Time
	attrs            map[string]string
	summary          string
	sizes            []int64
}

// putRows writes the versions rows and their files; a row's tag is its version and variant, its
// digest the digest of the tag in its package.
func putRows(t *testing.T, s index.Store, rows []row) map[string]index.Version {
	t.Helper()
	out := map[string]index.Version{}
	for _, r := range rows {
		tag := r.version + "_" + r.variant
		v := index.Version{PackageID: r.pkg.ID, Version: r.version, Variant: r.variant, Tag: tag, Digest: dg(r.pkg.Name + "/" + tag),
			ArtifactType: "application/vnd.lajstrom.raw.v1", Attrs: r.attrs, Metadata: json.RawMessage(`{"tag": "` + tag + `"}`),
			Summary: r.summary, CreatedAt: r.at, IndexedAt: r.at}
		must(t, s.UpsertVersion(t.Context(), &v))
		files := make([]index.File, len(r.sizes))
		for i, size := range r.sizes {
			files[i] = index.File{Name: tag + "-" + string(rune('a'+i)), MediaType: "application/octet-stream",
				Digest: dg(r.pkg.Name + "/" + tag + string(rune('a'+i))), Size: size}
		}
		must(t, s.ReplaceFiles(t.Context(), v.ID, files))
		out[r.pkg.Name+"/"+tag] = v
	}
	return out
}

// testPackageFacts: ListPackageFacts counts a package's versions and manifests, adds up the
// sizes of its files and takes the newest publish time; its last published version is the first
// by VersionKey, a variant published later not moving it, and so is its latest version without a
// LatestFunc; the head is that version's first variant, without metadata. A package without
// versions, or that does not exist, has the zero facts.
func testPackageFacts(t *testing.T, s index.Store) {
	ctx := t.Context()
	repo := newRepo(t, s, "raw", "a")
	tool := newPackage(t, s, repo.ID, "tool")
	tie := newPackage(t, s, repo.ID, "tie")
	empty := newPackage(t, s, repo.ID, "empty")
	attrs := map[string]string{"lajstrom.deb.arch": "amd64"}
	rows := putRows(t, s, []row{
		{pkg: &tool, version: "1.0", at: at(10), sizes: []int64{3, 4}},
		{pkg: &tool, version: "2.0", variant: "b-arm", at: at(20), sizes: []int64{5}},
		{pkg: &tool, version: "2.0", variant: "a-amd", at: at(30), attrs: attrs, summary: "Two", sizes: []int64{6}},
		{pkg: &tool, version: "1.5", at: at(15)},
		{pkg: &tool, version: "1.0", variant: "late", at: at(40), sizes: []int64{100}}, // 1.0 stays older than 2.0
		{pkg: &tie, version: "1.0", at: at(5)},
		{pkg: &tie, version: "1.1", at: at(5)},
	})
	head := func(key string) *index.Version {
		v := rows[key]
		v.Metadata = nil
		return &v
	}
	got, err := s.ListPackageFacts(ctx, []int64{tool.ID, empty.ID, tie.ID, 1 << 40}, nil)
	must(t, err)
	same(t, "ListPackageFacts", []index.PackageFacts{
		{PackageID: tool.ID, Versions: 3, Manifests: 5, Size: 118, UpdatedAt: at(40), Latest: "2.0",
			LatestVariants: []string{"a-amd", "b-arm"}, LastPublished: "2.0", Head: head("tool/2.0_a-amd")},
		{PackageID: empty.ID},
		{PackageID: tie.ID, Versions: 2, Manifests: 2, UpdatedAt: at(5), Latest: "1.1", LatestVariants: []string{""},
			LastPublished: "1.1", Head: head("tie/1.1_")},
		{PackageID: 1 << 40},
	}, got)

	// A LatestFunc picks among the versions, newest first, with the journal state; the dist-tags
	// that name a version of the package are its tags. A version it does not have is none, and
	// the head is then the last published version's.
	must(t, s.ReplaceJournalState(ctx, tool.ID, []index.JournalState{
		{Kind: index.JournalDistTag, Key: "latest", Value: "1.5", Seq: 3},
		{Kind: index.JournalDistTag, Key: "gone", Value: "9.9", Seq: 2},
		{Kind: index.JournalDeprecated, Key: "1.0", Value: "old", Seq: 1},
	}))
	state, err := s.ListJournalState(ctx, tool.ID)
	must(t, err)
	seen := map[int64][]string{}
	latest := func(id int64, versions []string, st []index.JournalState) string {
		seen[id] = versions
		for _, s := range st {
			if s.Kind == index.JournalDistTag && s.Key == "latest" {
				return s.Value
			}
		}
		return "none"
	}
	got, err = s.ListPackageFacts(ctx, []int64{tool.ID, tie.ID}, latest)
	must(t, err)
	same(t, "ListPackageFacts with a LatestFunc", []index.PackageFacts{
		{PackageID: tool.ID, Versions: 3, Manifests: 5, Size: 118, UpdatedAt: at(40), Latest: "1.5",
			LatestVariants: []string{""}, LastPublished: "2.0", Tags: []index.JournalState{state[2]}, Head: head("tool/1.5_")},
		{PackageID: tie.ID, Versions: 2, Manifests: 2, UpdatedAt: at(5), LastPublished: "1.1", Head: head("tie/1.1_")},
	}, got)
	same(t, "the versions the LatestFunc saw", map[int64][]string{tool.ID: {"2.0", "1.5", "1.0"}, tie.ID: {"1.1", "1.0"}}, seen)
	none, err := s.ListPackageFacts(ctx, nil, latest)
	must(t, err)
	same(t, "ListPackageFacts of none", []index.PackageFacts{}, none)
}
