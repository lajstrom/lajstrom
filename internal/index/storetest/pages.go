package storetest

import (
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/lajstrom/lajstrom/internal/index"
)

// testPackagePages: ListPackagesAfter pages a repo's packages in byte order, the order of Go's
// strings, whatever the database's default collation: with every prefix and every limit, the
// pages are the whole list once. The cursor is a place, not a row: a package inserted before it
// is on no later page, one after it is, and a deleted one is on none, the cursor's own included.
func testPackagePages(t *testing.T, s index.Store) {
	ctx := t.Context()
	repo := newRepo(t, s, "raw", "a")
	other := newRepo(t, s, "raw", "b")
	names := []string{"lib-core", "lib-util", "app", "Lib-upper", "lib_x", "l%b", "ünicode", "ü-dash", "lib",
		"Z", "a", "é", "z", "1.10", "1.9"}
	pkgs := map[string]index.Package{}
	for _, name := range names {
		pkgs[name] = newPackage(t, s, repo.ID, name)
	}
	newPackage(t, s, other.ID, "lib-other") // another repo's: on no page

	pageNames := func(prefix, after string, limit int) []string {
		t.Helper()
		page, err := s.ListPackagesAfter(ctx, repo.ID, prefix, after, limit)
		must(t, err)
		return packageNames(page)
	}
	for _, prefix := range []string{"", "lib", "lib-", "ü", "zzz"} {
		want := []string{}
		for _, n := range names {
			if strings.HasPrefix(n, prefix) {
				want = append(want, n)
			}
		}
		slices.Sort(want)
		for limit := 1; limit <= len(want)+1; limit++ {
			got := []string{}
			for after := ""; ; {
				page := pageNames(prefix, after, limit)
				if len(page) > limit {
					t.Fatalf("prefix %q, limit %d: a page of %d", prefix, limit, len(page))
				}
				got = append(got, page...)
				if len(page) < limit || len(got) > len(names) {
					break
				}
				after = page[len(page)-1]
			}
			same(t, fmt.Sprintf("the pages of %d with prefix %q", limit, prefix), want, got)
		}
	}
	for _, tt := range []struct {
		prefix, after string
		limit         int
		want          []string
	}{
		{"lib", "lib-d", 10, []string{"lib-util", "lib_x"}}, // a cursor that is no name
		{"lib", "a", 2, []string{"lib", "lib-core"}},        // a cursor before the prefix
		{"", "ü-dash", 10, []string{"ünicode"}},
		{"", "ünicode", 10, []string{}},
		{"", "", 0, []string{}},
		{"", "", -1, []string{}},
	} {
		same(t, fmt.Sprintf("ListPackagesAfter(%q, %q, %d)", tt.prefix, tt.after, tt.limit), tt.want,
			pageNames(tt.prefix, tt.after, tt.limit))
	}

	same(t, "the first page", []string{"1.10", "1.9", "Lib-upper"}, pageNames("", "", 3))
	newPackage(t, s, repo.ID, "0-new")  // before the cursor
	newPackage(t, s, repo.ID, "Lib-zz") // after it
	must(t, s.DeletePackage(ctx, pkgs["Lib-upper"].ID))
	must(t, s.DeletePackage(ctx, pkgs["Z"].ID))
	same(t, "the page after inserts and deletes", []string{"Lib-zz", "a", "app"}, pageNames("", "Lib-upper", 3))
}

// testVersionPages: ListVersionPage orders a package's versions by the earliest CreatedAt of
// their rows, then by version in byte order, both descending, ties and microseconds included;
// with every limit, the pages are every row once, a version's rows together on one page. The
// cursor is a place, as for packages, and a variant published later does not move its version.
func testVersionPages(t *testing.T, s index.Store) {
	ctx := t.Context()
	repo := newRepo(t, s, "raw", "a")
	pkg := newPackage(t, s, repo.ID, "tool")
	rows := map[string][]index.Version{}
	put := func(p index.Package, version, variant string, created time.Time) index.Version {
		t.Helper()
		v := index.Version{PackageID: p.ID, Version: version, Variant: variant, Tag: version + "_" + variant,
			Digest: dg(p.Name + version + variant), ArtifactType: "application/vnd.lajstrom.raw.v1", CreatedAt: created}
		must(t, s.UpsertVersion(ctx, &v))
		if p.ID == pkg.ID {
			rows[version] = append(rows[version], v)
		}
		return v
	}
	put(pkg, "1.0", "", at(10))
	put(pkg, "1.0", "linux", at(50)) // a later variant: the version stays at 10
	put(pkg, "1.1", "b", at(30))
	put(pkg, "1.1", "a", at(20))
	for _, v := range []string{"1.10", "1.9", "Z", "a", "z", "é"} { // as new as each other
		put(pkg, v, "", at(40))
	}
	put(pkg, "2.0", "x", at(60))
	put(pkg, "2.0", "y", at(60))
	put(pkg, "3.0", "", at(5))
	put(pkg, "m1", "", at(70))
	put(pkg, "m2", "", at(70).Add(time.Microsecond))
	put(newPackage(t, s, repo.ID, "other"), "9.9", "", at(100)) // another package's: on no page

	order := []string{"m2", "m1", "2.0", "é", "z", "a", "Z", "1.9", "1.10", "1.1", "1.0", "3.0"}
	want := []index.Version{}
	for _, v := range order {
		slices.SortFunc(rows[v], func(a, b index.Version) int { return strings.Compare(a.Variant, b.Variant) })
		want = append(want, rows[v]...)
	}
	page := func(after *index.VersionKey, limit int) []index.Version {
		t.Helper()
		list, err := s.ListVersionPage(ctx, pkg.ID, after, limit, index.VersionFilter{})
		must(t, err)
		return list
	}
	for limit := 1; limit <= len(order)+1; limit++ {
		got, seen := []index.Version{}, map[string]bool{}
		var after *index.VersionKey
		for n := 0; n <= len(order); n++ {
			list := page(after, limit)
			versions := versionsOf(list)
			if len(versions) > limit {
				t.Fatalf("limit %d: a page of %v", limit, versions)
			}
			for _, v := range versions {
				if seen[v] {
					t.Errorf("limit %d: %s on two pages", limit, v)
				}
				seen[v] = true
			}
			got = append(got, list...)
			if len(versions) < limit {
				break
			}
			after = lastKey(list)
		}
		same(t, fmt.Sprintf("the pages of %d", limit), want, got)
	}
	for _, tt := range []struct {
		after *index.VersionKey
		limit int
		want  []string
	}{
		{&index.VersionKey{CreatedAt: at(45), Version: "x"}, 2, []string{"é", "z"}}, // a key that is no version's
		{&index.VersionKey{CreatedAt: at(40), Version: "b"}, 2, []string{"a", "Z"}},
		{&index.VersionKey{CreatedAt: at(70), Version: "m1"}, 1, []string{"2.0"}},
		{&index.VersionKey{CreatedAt: at(5), Version: "3.0"}, 5, []string{}},
		{nil, 0, []string{}},
		{nil, -1, []string{}},
	} {
		same(t, fmt.Sprintf("ListVersionPage(%v, %d)", tt.after, tt.limit), tt.want, versionsOf(page(tt.after, tt.limit)))
	}

	first := page(nil, 3)
	same(t, "the first page", []string{"m2", "m1", "2.0"}, versionsOf(first))
	put(pkg, "4.0", "", at(80))   // newer than the cursor
	put(pkg, "2.0a", "", at(60))  // as new, a greater version: before the cursor
	put(pkg, "1.99", "", at(60))  // as new, a smaller version: after it
	put(pkg, "z", "late", at(90)) // a variant published later: z stays at 40
	for _, v := range append(rows["2.0"], rows["é"]...) {
		must(t, s.DeleteVersion(ctx, v.ID)) // the cursor's version, and one after it
	}
	next := page(lastKey(first), 3)
	same(t, "the page after inserts and deletes", []string{"1.99", "z", "z", "a"}, rowVersions(next))
}

// versionsOf returns the versions of rows, each once, in order.
func versionsOf(rows []index.Version) []string {
	return slices.Compact(rowVersions(rows))
}

// rowVersions returns the version of each row.
func rowVersions(rows []index.Version) []string {
	out := []string{}
	for _, v := range rows {
		out = append(out, v.Version)
	}
	return out
}

// lastKey returns the key of the last version of a page, whose rows are all on it.
func lastKey(rows []index.Version) *index.VersionKey {
	last := rows[len(rows)-1]
	k := &index.VersionKey{CreatedAt: last.CreatedAt, Version: last.Version}
	for _, v := range rows {
		if v.Version == k.Version && v.CreatedAt.Before(k.CreatedAt) {
			k.CreatedAt = v.CreatedAt
		}
	}
	return k
}

// testVersionPagesWithout: ListVersionPage with attachment kinds keeps the rows without an
// attachment of any of them (#89, ?without=): a version is listed with those of its variants, at
// the key of the first of them, and each kind given is one more condition; the cursor pages the
// filtered list.
func testVersionPagesWithout(t *testing.T, s index.Store) {
	ctx := t.Context()
	repo := newRepo(t, s, "raw", "a")
	pkg := newPackage(t, s, repo.ID, "tool")
	put := func(version, variant string, created time.Time, kinds ...string) index.Version {
		t.Helper()
		v := index.Version{PackageID: pkg.ID, Version: version, Variant: variant, Tag: version + "_" + variant,
			Digest: dg(version + variant), ArtifactType: "application/vnd.lajstrom.raw.v1", CreatedAt: created}
		must(t, s.UpsertVersion(ctx, &v))
		for _, k := range kinds {
			a := attachment(version+variant+k, k+" layer", 1, 1)
			a.Kind = k
			must(t, s.ReplaceAttachments(ctx, v.ID, a.Digest, []index.Attachment{a}))
		}
		return v
	}
	put("1.0", "", at(10), index.AttachmentSBOM, index.AttachmentSignature)
	put("2.0", "a", at(20), index.AttachmentSBOM)
	put("2.0", "b", at(25))
	put("3.0", "", at(30), index.AttachmentSignature)
	put("4.0", "", at(40))
	rowsOf := func(after *index.VersionKey, limit int, without ...string) []string {
		t.Helper()
		list, err := s.ListVersionPage(ctx, pkg.ID, after, limit, index.VersionFilter{Without: without})
		must(t, err)
		out := []string{}
		for _, v := range list {
			out = append(out, v.Version+"/"+v.Variant)
		}
		return out
	}
	same(t, "without", []string{"4.0/", "3.0/", "2.0/a", "2.0/b", "1.0/"}, rowsOf(nil, 10))
	same(t, "without sbom", []string{"4.0/", "3.0/", "2.0/b"}, rowsOf(nil, 10, index.AttachmentSBOM))
	same(t, "without a signature", []string{"4.0/", "2.0/a", "2.0/b"}, rowsOf(nil, 10, index.AttachmentSignature))
	same(t, "without sbom and signature", []string{"4.0/", "2.0/b"}, rowsOf(nil, 10, index.AttachmentSBOM, index.AttachmentSignature))
	same(t, "without a scan", []string{"4.0/", "3.0/", "2.0/a", "2.0/b", "1.0/"}, rowsOf(nil, 10, index.AttachmentScan))
	same(t, "the first page without sbom", []string{"4.0/", "3.0/"}, rowsOf(nil, 2, index.AttachmentSBOM))
	// 2.0 without an SBOM is its variant b alone, at 25: its key is b's.
	same(t, "the next page without sbom", []string{"2.0/b"}, rowsOf(&index.VersionKey{CreatedAt: at(30), Version: "3.0"}, 2, index.AttachmentSBOM))
	same(t, "after 2.0 without sbom", []string{}, rowsOf(&index.VersionKey{CreatedAt: at(25), Version: "2.0"}, 2, index.AttachmentSBOM))
}

// testVersionPagesUnmet: ListVersionPage with a requirement keeps the rows that have no
// attachment verified of its predicate type, or of one that continues it after a "/", signed by
// one of its names (#1287, ?unmet=; docs/spec/policy.md §11): a signature counts as cosign's
// predicate type, an untrusted row or another signer meets nothing, nor does a type that only
// starts like it; with kinds too, a row meets both; the cursor pages the filtered list.
func testVersionPagesUnmet(t *testing.T, s index.Store) {
	ctx := t.Context()
	repo := newRepo(t, s, "raw", "a")
	pkg := newPackage(t, s, repo.ID, "tool")
	const spdx, sign = "https://spdx.dev/Document", "https://sigstore.dev/cosign/sign/v1"
	put := func(version, variant string, created int, kind, predicateType, signedBy string) {
		t.Helper()
		v := index.Version{PackageID: pkg.ID, Version: version, Variant: variant, Tag: version + "_" + variant,
			Digest: dg(version + variant), ArtifactType: "application/vnd.lajstrom.raw.v1", CreatedAt: at(created)}
		must(t, s.UpsertVersion(ctx, &v))
		if kind == "" {
			return
		}
		a := attachment(version+variant, version+variant+" layer", 1, 1)
		a.Kind, a.PredicateType, a.SignedBy = kind, predicateType, signedBy
		if signedBy == "" {
			a.Verification, a.Reason = index.AttachmentUntrusted, "no trusted key verifies its signature"
		}
		must(t, s.ReplaceAttachments(ctx, v.ID, a.Digest, []index.Attachment{a}))
	}
	put("1.0", "", 10, index.AttachmentSBOM, spdx, "release")
	put("2.0", "a", 20, index.AttachmentSBOM, spdx, "other")
	put("2.0", "b", 25, index.AttachmentSBOM, spdx, "")
	put("3.0", "", 30, index.AttachmentAttestation, spdx+"X", "release")
	put("4.0", "", 40, index.AttachmentSBOM, "https://cyclonedx.org/bom/v1.6", "release")
	put("5.0", "", 50, index.AttachmentSignature, "", "release")
	put("6.0", "", 60, "", "", "")
	rowsOf := func(after *index.VersionKey, limit int, f index.VersionFilter) []string {
		t.Helper()
		list, err := s.ListVersionPage(ctx, pkg.ID, after, limit, f)
		must(t, err)
		out := []string{}
		for _, v := range list {
			out = append(out, v.Version+"/"+v.Variant)
		}
		return out
	}
	unmet := func(predicateType string, names ...string) index.VersionFilter {
		return index.VersionFilter{Unmet: &index.Requirement{PredicateType: predicateType, SignedBy: names}}
	}
	all := []string{"6.0/", "5.0/", "4.0/", "3.0/", "2.0/a", "2.0/b", "1.0/"}
	same(t, "an spdx of release", []string{"6.0/", "5.0/", "4.0/", "3.0/", "2.0/a", "2.0/b"}, rowsOf(nil, 10, unmet(spdx, "release")))
	same(t, "an spdx of release or other", []string{"6.0/", "5.0/", "4.0/", "3.0/", "2.0/b"}, rowsOf(nil, 10, unmet(spdx, "release", "other")))
	same(t, "a signature of release", []string{"6.0/", "4.0/", "3.0/", "2.0/a", "2.0/b", "1.0/"}, rowsOf(nil, 10, unmet(sign, "release")))
	same(t, "a cyclonedx bom of release, any version", []string{"6.0/", "5.0/", "3.0/", "2.0/a", "2.0/b", "1.0/"},
		rowsOf(nil, 10, unmet("https://cyclonedx.org/bom", "release")))
	same(t, "a cyclonedx bom of another version", all, rowsOf(nil, 10, unmet("https://cyclonedx.org/bom/v1.5", "release")))
	same(t, "a type with the characters of a pattern", all, rowsOf(nil, 10, unmet("https://spdx.dev/Doc%", "release")))
	same(t, "a requirement without a name", all, rowsOf(nil, 10, unmet(spdx)))
	same(t, "without an sbom and a signature of release", []string{"6.0/", "3.0/"},
		rowsOf(nil, 10, index.VersionFilter{Without: []string{index.AttachmentSBOM}, Unmet: unmet(sign, "release").Unmet}))
	same(t, "the first page", []string{"6.0/", "5.0/"}, rowsOf(nil, 2, unmet(spdx, "release")))
	same(t, "the next page", []string{"4.0/", "3.0/"}, rowsOf(&index.VersionKey{CreatedAt: at(50), Version: "5.0"}, 2, unmet(spdx, "release")))
	// 2.0 that misses an spdx of release or other is its variant b alone, at 25: its key is b's.
	same(t, "the last page", []string{"2.0/b"}, rowsOf(&index.VersionKey{CreatedAt: at(30), Version: "3.0"}, 2, unmet(spdx, "release", "other")))
}
