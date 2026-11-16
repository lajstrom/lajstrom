package storetest

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/lajstrom/lajstrom/internal/index"
)

// Bench sizes: a repo of 10 000 manifests, as the reindex of #29 measures, in 1 000 packages
// of 5 versions of 2 variants, each manifest with 2 files.
const (
	benchPackages = 1000
	benchVersions = 5
)

// Bench seeds s with the repo of the bench sizes, and a second repo as large that no read of
// the package list asks for, then measures the reads of the package list on it and of the
// search over both, each as the API makes them: a page of 100 packages and their facts, a page
// of 100 hits (docs/spec/index-schema.md, The package list, one package and search).
func Bench(b *testing.B, s index.Store) {
	ctx := b.Context()
	repo := seedBench(b, s, "a")
	seedBench(b, s, "b")
	page := func(q index.PackageQuery) func() error {
		return func() error {
			q.Limit = 101
			places, err := s.ListPackagePage(ctx, repo.ID, q)
			if err != nil {
				return err
			}
			ids := make([]int64, 0, len(places))
			for _, p := range places[:min(len(places), 100)] {
				ids = append(ids, p.Package.ID)
			}
			_, err = s.ListPackageFacts(ctx, ids, nil)
			return err
		}
	}
	// search runs the ranks of the search over both repos, as the API does for a page of 100.
	search := func(text string, ranks ...index.SearchRank) func() error {
		return func() error {
			n := 0
			for _, rank := range ranks {
				hits, err := s.Search(ctx, index.SearchQuery{Rank: rank, Text: text, Limit: 101 - n})
				if err != nil {
					return err
				}
				if n += len(hits); n > 100 {
					break
				}
			}
			return nil
		}
	}
	names := []index.SearchRank{index.RankExact, index.RankPrefix, index.RankSubstring, index.RankSummary}
	digest := dg("a" + "pkg-0500" + "1.0__jammy_amd64").String()[:15]
	jammy := map[string][]string{"lajstrom.deb.suite": {"jammy"}}
	for _, bb := range []struct {
		name string
		read func() error
	}{
		{"page by name", page(index.PackageQuery{})},
		{"page by name after a key", page(index.PackageQuery{After: &index.PackageKey{Name: "pkg-0500"}})},
		{"page by updated", page(index.PackageQuery{Sort: index.SortUpdated, Desc: true})},
		{"page by versions", page(index.PackageQuery{Sort: index.SortVersions, Desc: true})},
		{"page by size", page(index.PackageQuery{Sort: index.SortSize, Desc: true})},
		{"page of a text", page(index.PackageQuery{Text: "TOOL 07"})},
		{"page of an attr", page(index.PackageQuery{Attrs: jammy})},
		{"page of an attr by updated", page(index.PackageQuery{Attrs: jammy, Sort: index.SortUpdated, Desc: true})},
		{"facet", func() error { _, err := s.ListAttrValues(ctx, repo.ID, "lajstrom.deb.suite"); return err }},
		{"search a name", search("pkg-0500", names...)},
		{"search a prefix", search("PKG-05", names...)},
		{"search a summary", search("of the bench", names...)},
		{"search nothing", search("nothing", names...)},
		{"search a digest", search(digest, append([]index.SearchRank{index.RankManifest, index.RankFile}, names...)...)},
	} {
		b.Run(bb.name, func(b *testing.B) {
			for b.Loop() {
				if err := bb.read(); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// seedBench writes a repo of the bench sizes: package pkg-NNNN, versions 1.N, variants
// jammy/amd64 and noble/arm64, each with its summary and 2 files.
func seedBench(b *testing.B, s index.Store, name string) index.Repo {
	b.Helper()
	ctx := context.Background()
	repo := index.Repo{Format: "apt", Name: name, Registry: "local", Prefix: "apt/" + name, Kind: "hosted"}
	benchMust(b, s.CreateRepo(ctx, &repo))
	start := time.Now()
	for i := range benchPackages {
		benchMust(b, s.Tx(ctx, func(w index.Writer) error {
			pkg := index.Package{RepoID: repo.ID, Name: fmt.Sprintf("pkg-%04d", i), NameOCI: fmt.Sprintf("pkg-%04d", i)}
			if err := w.UpsertPackage(ctx, &pkg); err != nil {
				return err
			}
			for v := range benchVersions {
				for _, sa := range [][2]string{{"jammy", "amd64"}, {"noble", "arm64"}} {
					tag := fmt.Sprintf("1.%d__%s_%s", v, sa[0], sa[1])
					row := index.Version{PackageID: pkg.ID, Version: fmt.Sprintf("1.%d", v), Variant: sa[0] + "/main/" + sa[1], Tag: tag,
						Digest: dg(name + pkg.Name + tag), ArtifactType: "application/vnd.lajstrom.deb.v1",
						Attrs:   map[string]string{"lajstrom.deb.suite": sa[0], "lajstrom.deb.component": "main", "lajstrom.deb.arch": sa[1]},
						Summary: fmt.Sprintf("Tool %03d of the bench", i%1000), CreatedAt: t0.Add(time.Duration(i*benchVersions+v) * time.Minute)}
					if err := w.UpsertVersion(ctx, &row); err != nil {
						return err
					}
					if err := w.ReplaceFiles(ctx, row.ID, []index.File{
						{Name: pkg.Name + ".deb", MediaType: "application/vnd.lajstrom.deb.package.v1", Digest: dg(name + pkg.Name + tag + "deb"), Size: int64(1000 + i)},
						{Name: pkg.Name + ".dsc", MediaType: "application/vnd.lajstrom.deb.dsc.v1", Digest: dg(name + pkg.Name + tag + "dsc"), Size: 100},
					}); err != nil {
						return err
					}
				}
			}
			return nil
		}))
	}
	b.Logf("seeded %s with %d manifests in %s", repo.Prefix, benchPackages*benchVersions*2, time.Since(start).Round(time.Millisecond))
	return repo
}

// benchMust stops the benchmark on an error.
func benchMust(b *testing.B, err error) {
	b.Helper()
	if err != nil {
		b.Fatal(err)
	}
}
