package storetest

import (
	"cmp"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
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

// testPackageQuery: ListPackagePage keeps the packages of the filters and orders them by each
// sort in both directions, a tie by name in the same direction; with every limit the pages,
// each after the key of the one before, are the whole list once. The text folds ASCII letters
// only and takes LIKE's wildcards as they are; the attrs of several keys hold on one row.
func testPackageQuery(t *testing.T, s index.Store) {
	ctx := t.Context()
	repo := newRepo(t, s, "apt", "a")
	other := newRepo(t, s, "apt", "b")
	pkgs := map[string]*index.Package{}
	for _, name := range []string{"alpha", "Beta-Lib", "gamma", "delta", "l%b", "Épée", "empty"} {
		p := index.Package{RepoID: repo.ID, Name: name, NameOCI: strings.ToLower(name), CreatedAt: at(1)}
		must(t, s.UpsertPackage(ctx, &p))
		pkgs[name] = &p
	}
	otherPkg := newPackage(t, s, other.ID, "alpha-other")
	deb := func(suite, arch string) map[string]string {
		return map[string]string{"lajstrom.deb.suite": suite, "lajstrom.deb.arch": arch}
	}
	putRows(t, s, []row{
		{pkg: pkgs["alpha"], version: "1.0", at: at(10), attrs: deb("jammy", "amd64"), summary: "The Alpha tool", sizes: []int64{10}},
		{pkg: pkgs["alpha"], version: "1.1", at: at(50), attrs: deb("noble", "arm64"), sizes: []int64{5, 5}},
		{pkg: pkgs["Beta-Lib"], version: "1.0", at: at(20), attrs: deb("jammy", "arm64"), summary: "beta: 100% pure", sizes: []int64{30}},
		{pkg: pkgs["gamma"], version: "1.0", at: at(30), attrs: deb("noble", "amd64"), sizes: []int64{1}},
		{pkg: pkgs["gamma"], version: "2.0", at: at(30), attrs: deb("noble", "amd64"), sizes: []int64{1}},
		{pkg: pkgs["delta"], version: "1.0", at: at(30), attrs: deb("Jammy", "amd64"), sizes: []int64{30}},
		{pkg: pkgs["l%b"], version: "1.0", at: at(5)},
		{pkg: pkgs["Épée"], version: "1.0", at: at(6), summary: "ÉPÉE cutlery", sizes: []int64{7}},
		{pkg: &otherPkg, version: "1.0", at: at(99), attrs: deb("jammy", "amd64"), summary: "alpha", sizes: []int64{1}},
	})
	// The value of each sort, per package.
	updated := map[string]time.Time{"alpha": at(50), "Beta-Lib": at(20), "gamma": at(30), "delta": at(30), "l%b": at(5),
		"Épée": at(6), "empty": at(1)}
	versions := map[string]int64{"alpha": 2, "Beta-Lib": 1, "gamma": 2, "delta": 1, "l%b": 1, "Épée": 1}
	sizes := map[string]int64{"alpha": 20, "Beta-Lib": 30, "gamma": 2, "delta": 30, "Épée": 7}
	keyOf := func(sort index.PackageSort, name string) index.PackageKey {
		switch sort {
		case index.SortUpdated:
			return index.PackageKey{Updated: updated[name], Name: name}
		case index.SortVersions:
			return index.PackageKey{N: versions[name], Name: name}
		case index.SortSize:
			return index.PackageKey{N: sizes[name], Name: name}
		case index.SortName:
		}
		return index.PackageKey{Name: name}
	}
	compare := func(sort index.PackageSort, a, b string) int {
		ka, kb := keyOf(sort, a), keyOf(sort, b)
		return cmp.Or(ka.Updated.Compare(kb.Updated), cmp.Compare(ka.N, kb.N), strings.Compare(a, b))
	}
	all := []string{"alpha", "Beta-Lib", "gamma", "delta", "l%b", "Épée", "empty"}
	for _, tt := range []struct {
		name string
		q    index.PackageQuery
		want []string // in any order
	}{
		{"every package", index.PackageQuery{}, all},
		{"a prefix", index.PackageQuery{Prefix: "Be"}, []string{"Beta-Lib"}},
		{"a name in another case", index.PackageQuery{Text: "LIB"}, []string{"Beta-Lib"}},
		{"a name or a summary", index.PackageQuery{Text: "alpha"}, []string{"alpha"}},
		{"a % as it is", index.PackageQuery{Text: "100%"}, []string{"Beta-Lib"}},
		{"a _ as it is", index.PackageQuery{Text: "100_"}, []string{}},
		{"no wildcard in a name", index.PackageQuery{Text: "l%b"}, []string{"l%b"}},
		{"É is not é", index.PackageQuery{Text: "épée"}, []string{}},
		{"É as it is", index.PackageQuery{Text: "Épée"}, []string{"Épée"}},
		{"ÉPÉE folds its ASCII", index.PackageQuery{Text: "ÉpÉe cut"}, []string{"Épée"}},
		{"an attr", index.PackageQuery{Attrs: map[string][]string{"lajstrom.deb.suite": {"jammy"}}}, []string{"alpha", "Beta-Lib"}},
		{"two keys on one row", index.PackageQuery{Attrs: map[string][]string{"lajstrom.deb.suite": {"jammy"}, "lajstrom.deb.arch": {"arm64"}}},
			[]string{"Beta-Lib"}},
		{"alternatives", index.PackageQuery{Attrs: map[string][]string{"lajstrom.deb.suite": {"jammy", "noble"}, "lajstrom.deb.arch": {"amd64"}}},
			[]string{"alpha", "gamma"}},
		{"a key that matches nothing", index.PackageQuery{Attrs: map[string][]string{`lajstrom.deb.suite"`: {"jammy"}}}, []string{}},
		{"every filter", index.PackageQuery{Prefix: "a", Text: "TOOL", Attrs: map[string][]string{"lajstrom.deb.arch": {"arm64"}}},
			[]string{"alpha"}},
	} {
		for _, sort := range []index.PackageSort{"", index.SortName, index.SortUpdated, index.SortVersions, index.SortSize} {
			for _, desc := range []bool{false, true} {
				want := slices.Clone(tt.want)
				slices.SortFunc(want, func(a, b string) int { return compare(sort, a, b) })
				if desc {
					slices.Reverse(want)
				}
				for limit := 1; limit <= len(want)+1; limit++ {
					q := tt.q
					q.Sort, q.Desc, q.Limit = sort, desc, limit
					what := fmt.Sprintf("%s, by %q, desc %v, pages of %d", tt.name, sort, desc, limit)
					same(t, what, want, packagePages(t, s, repo.ID, q, func(p index.PackagePlace) index.PackageKey {
						return keyOf(cmp.Or(sort, index.SortName), p.Package.Name)
					}))
				}
			}
		}
	}
	none, err := s.ListPackagePage(ctx, repo.ID, index.PackageQuery{})
	must(t, err)
	same(t, "a limit of 0", []index.PackagePlace{}, none)
	if _, err := s.ListPackagePage(ctx, repo.ID, index.PackageQuery{Sort: "stars", Limit: 1}); err == nil {
		t.Error("ListPackagePage by an unknown sort succeeded")
	}
}

// packagePages returns the names of the pages of q, each after the key of the last package of
// the one before, checking each key against want.
func packagePages(t *testing.T, s index.Store, repoID int64, q index.PackageQuery, want func(index.PackagePlace) index.PackageKey) []string {
	t.Helper()
	got := []string{}
	for range 100 {
		page, err := s.ListPackagePage(t.Context(), repoID, q)
		must(t, err)
		for _, p := range page {
			if k := want(p); p.Key != k {
				t.Errorf("by %q: the key of %s is %+v, want %+v", q.Sort, p.Package.Name, p.Key, k)
			}
			got = append(got, p.Package.Name)
		}
		if len(page) < q.Limit {
			break
		}
		q.After = &page[len(page)-1].Key
	}
	return got
}

// testAttrValues: ListAttrValues counts the packages of a repo with each value of a key, in byte
// order; a key that no row has, or that matches nothing, has none.
func testAttrValues(t *testing.T, s index.Store) {
	ctx := t.Context()
	repo := newRepo(t, s, "apt", "a")
	other := newRepo(t, s, "apt", "b")
	names := map[string]*index.Package{}
	for _, name := range []string{"a", "b", "c"} {
		p := newPackage(t, s, repo.ID, name)
		names[name] = &p
	}
	o := newPackage(t, s, other.ID, "o")
	suite := func(v string) map[string]string { return map[string]string{"lajstrom.deb.suite": v} }
	putRows(t, s, []row{
		{pkg: names["a"], version: "1", at: at(1), attrs: suite("jammy")},
		{pkg: names["a"], version: "2", at: at(2), attrs: suite("jammy")},
		{pkg: names["a"], version: "3", at: at(3), attrs: suite("noble")},
		{pkg: names["b"], version: "1", at: at(1), attrs: suite("jammy")},
		{pkg: names["c"], version: "1", at: at(1), attrs: suite("Jammy")},
		{pkg: names["c"], version: "2", at: at(2)},
		{pkg: &o, version: "1", at: at(1), attrs: suite("bookworm")},
	})
	for _, tt := range []struct {
		key  string
		want []index.AttrValue
	}{
		{"lajstrom.deb.suite", []index.AttrValue{{Value: "Jammy", Packages: 1}, {Value: "jammy", Packages: 2}, {Value: "noble", Packages: 1}}},
		{"lajstrom.deb.arch", []index.AttrValue{}},
		{`lajstrom.deb.suite"`, []index.AttrValue{}},
		{"", []index.AttrValue{}},
	} {
		got, err := s.ListAttrValues(ctx, repo.ID, tt.key)
		must(t, err)
		same(t, fmt.Sprintf("ListAttrValues(%q)", tt.key), tt.want, got)
	}
}

// testSearch: Search answers each rank in the order of the repo's format and name and the
// package's name, a package in the first of its ranks only, over the repos asked for; a digest
// matches by its prefix, manifests and files apart. With every limit the pages of a rank are
// its hits once.
func testSearch(t *testing.T, s index.Store) {
	ctx := t.Context()
	a, b, c := newRepo(t, s, "raw", "a"), newRepo(t, s, "raw", "b"), newRepo(t, s, "pypi", "c")
	pkg := func(repo index.Repo, name string) *index.Package {
		p := newPackage(t, s, repo.ID, name)
		return &p
	}
	tool, zz, aReq := pkg(a, "Requests-Tool"), pkg(a, "zz"), pkg(a, "requests")
	rows := putRows(t, s, []row{
		{pkg: aReq, version: "1.0", at: at(1), sizes: []int64{1}},
		{pkg: tool, version: "1.0", at: at(1), summary: "http client"},
		{pkg: pkg(a, "my-requests"), version: "1.0", at: at(1)},
		{pkg: zz, version: "1.0", at: at(1), summary: "Requests for humans"},
		{pkg: zz, version: "2.0", variant: "x", at: at(2), sizes: []int64{3, 4}},
		{pkg: pkg(b, "requests"), version: "1.0", at: at(1)},
		{pkg: pkg(c, "requests"), version: "1.0", at: at(1)},
	})
	names := func(hits []index.SearchHit) []string {
		out := []string{}
		for _, h := range hits {
			k := h.Key.Format + "/" + h.Key.Repo + "/" + h.Key.Package
			if h.Version != nil {
				k += " " + h.Key.Version + " " + h.Key.Variant
			}
			if h.File != nil {
				k += " " + h.Key.File
			}
			out = append(out, k)
		}
		return out
	}
	search := func(q index.SearchQuery) []string {
		t.Helper()
		all := []index.SearchHit{}
		limit := q.Limit
		for range 20 {
			hits, err := s.Search(ctx, q)
			must(t, err)
			all = append(all, hits...)
			if len(hits) < limit {
				break
			}
			q.After = &hits[len(hits)-1].Key
		}
		return names(all)
	}
	zzDigest := rows["zz/2.0_x"].Digest.String()
	fileDigest := dg("zz/2.0_xb").String() // the second file of zz 2.0
	for _, tt := range []struct {
		name string
		q    index.SearchQuery
		want []string
	}{
		{"exact", index.SearchQuery{Rank: index.RankExact, Text: "REQUESTS"}, []string{"pypi/c/requests", "raw/a/requests", "raw/b/requests"}},
		{"prefix", index.SearchQuery{Rank: index.RankPrefix, Text: "requests"}, []string{"raw/a/Requests-Tool"}},
		{"substring", index.SearchQuery{Rank: index.RankSubstring, Text: "requests"}, []string{"raw/a/my-requests"}},
		{"summary", index.SearchQuery{Rank: index.RankSummary, Text: "requests"}, []string{"raw/a/zz"}},
		{"a summary of a name already", index.SearchQuery{Rank: index.RankSummary, Text: "client"}, []string{"raw/a/Requests-Tool"}},
		{"one repo", index.SearchQuery{Rank: index.RankExact, Text: "requests", RepoIDs: []int64{b.ID}}, []string{"raw/b/requests"}},
		{"no repo", index.SearchQuery{Rank: index.RankExact, Text: "requests", RepoIDs: []int64{}}, []string{}},
		{"a wildcard as it is", index.SearchQuery{Rank: index.RankSubstring, Text: "%"}, []string{}},
		{"a manifest", index.SearchQuery{Rank: index.RankManifest, Text: zzDigest[:15]}, []string{"raw/a/zz 2.0 x"}},
		{"a whole digest", index.SearchQuery{Rank: index.RankManifest, Text: zzDigest}, []string{"raw/a/zz 2.0 x"}},
		{"a manifest of another repo", index.SearchQuery{Rank: index.RankManifest, Text: zzDigest[:15], RepoIDs: []int64{b.ID, c.ID}}, []string{}},
		{"a file", index.SearchQuery{Rank: index.RankFile, Text: fileDigest[:15]}, []string{"raw/a/zz 2.0 x 2.0_x-b"}},
		{"no manifest is a file", index.SearchQuery{Rank: index.RankFile, Text: zzDigest}, []string{}},
	} {
		for limit := 1; limit <= len(tt.want)+1; limit++ {
			q := tt.q
			q.Limit = limit
			same(t, fmt.Sprintf("%s, pages of %d", tt.name, limit), tt.want, search(q))
		}
	}
	hits, err := s.Search(ctx, index.SearchQuery{Rank: index.RankFile, Text: fileDigest, Limit: 1})
	must(t, err)
	if len(hits) != 1 || hits[0].File == nil || hits[0].File.Size != 4 || hits[0].Version.Digest != rows["zz/2.0_x"].Digest ||
		hits[0].Version.Metadata != nil || hits[0].Package.ID != zz.ID {
		t.Errorf("the hit of a file: %+v", hits)
	}
	none, err := s.Search(ctx, index.SearchQuery{Rank: index.RankExact, Text: "requests"})
	must(t, err)
	same(t, "a limit of 0", []index.SearchHit{}, none)
	if _, err := s.Search(ctx, index.SearchQuery{Rank: 99, Text: "x", Limit: 1}); err == nil {
		t.Error("Search of an unknown rank succeeded")
	}
}
