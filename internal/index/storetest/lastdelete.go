package storetest

import (
	"errors"
	"testing"
	"time"

	"github.com/lajstrom/lajstrom/internal/index"
)

// testPackageLastDelete: packages.last_delete_at (#1419; docs/spec/index-schema.md, The time of
// a package's last delete). A new package has none, whatever UpsertPackage was given, and no
// version write sets it. DeleteVersion sets it, in its transaction, to the later of its clock
// and the deleted version's CreatedAt, and never moves it back; a delete that finds no row, or
// whose transaction rolls back, sets nothing, nor does it touch another package. Every read of
// the package carries it.
func testPackageLastDelete(t *testing.T, s index.Store) {
	ctx := t.Context()
	repo := newRepo(t, s, "raw", "deletes")
	p := index.Package{RepoID: repo.ID, Name: "file.txt", NameOCI: "file.txt", LastDeleteAt: ptr(at(1))}
	must(t, s.UpsertPackage(ctx, &p))
	other := newPackage(t, s, repo.ID, "other.txt")
	lastDelete := func(name string) *time.Time {
		t.Helper()
		got, err := s.GetPackage(ctx, repo.ID, name)
		must(t, err)
		return got.LastDeleteAt
	}
	version := func(pkg index.Package, v string, created time.Time) index.Version {
		t.Helper()
		ver := index.Version{PackageID: pkg.ID, Version: v, Tag: v, Digest: dg(pkg.Name + v), ArtifactType: "a", CreatedAt: created}
		must(t, s.UpsertVersion(ctx, &ver))
		must(t, s.ReplaceFiles(ctx, ver.ID, []index.File{{Name: pkg.Name, MediaType: "m", Digest: dg(pkg.Name + v + "file")}}))
		return ver
	}
	if p.LastDeleteAt != nil {
		t.Errorf("UpsertPackage of a new package: LastDeleteAt %v, want none", p.LastDeleteAt)
	}
	v1, v2 := version(p, "1", at(0)), version(p, "2", at(10))
	o1 := version(other, "1", at(0))
	again := v1
	again.IndexedAt = at(20)
	must(t, s.UpsertVersion(ctx, &again))
	if got := lastDelete("file.txt"); got != nil {
		t.Errorf("a package without a delete: LastDeleteAt %v, want none", got)
	}

	before := time.Now().Truncate(time.Microsecond)
	must(t, s.DeleteVersion(ctx, v2.ID))
	after := time.Now()
	first := lastDelete("file.txt")
	if first == nil || first.Before(before) || first.After(after) || first.Location() != time.UTC {
		t.Fatalf("after a delete: LastDeleteAt %v, want a UTC time within [%v, %v]", first, before, after)
	}
	if got := lastDelete("other.txt"); got != nil {
		t.Errorf("another package after the delete: LastDeleteAt %v, want none", got)
	}
	wantErr(t, "DeleteVersion of a deleted version", s.DeleteVersion(ctx, v2.ID), index.ErrNotFound)
	same(t, "after a delete that found no row", first, lastDelete("file.txt"))

	errBoom := errors.New("boom")
	err := s.Tx(ctx, func(w index.Writer) error {
		if err := w.DeleteVersion(ctx, o1.ID); err != nil {
			return err
		}
		return errBoom
	})
	wantErr(t, "a delete rolled back", err, errBoom)
	if got := lastDelete("other.txt"); got != nil {
		t.Errorf("after a delete rolled back: LastDeleteAt %v, want none", got)
	}
	must(t, s.Tx(ctx, func(w index.Writer) error { return w.DeleteVersion(ctx, o1.ID) }))
	if got := lastDelete("other.txt"); got == nil || got.Before(*first) {
		t.Errorf("after a delete in a transaction: LastDeleteAt %v, want one not before %v", got, first)
	}

	// A version created ahead of the clock (a manifest pushed through /v2): its delete records
	// its CreatedAt, and a later delete of an older version keeps it.
	ahead := time.Now().UTC().Add(time.Hour).Truncate(time.Second)
	v3 := version(p, "3", ahead)
	must(t, s.DeleteVersion(ctx, v3.ID))
	same(t, "after the delete of a version created ahead of the clock", &ahead, lastDelete("file.txt"))
	must(t, s.DeleteVersion(ctx, v1.ID))
	same(t, "after the delete of an older version, later", &ahead, lastDelete("file.txt"))

	// Every read of the package carries it.
	v4 := version(p, "4", at(30))
	want, err := s.GetPackage(ctx, repo.ID, "file.txt")
	must(t, err)
	packageReads(t, s, *want, v4.ID)
}

// packageReads checks that the reads of a package other than GetPackage return want, which has
// the one file of its version versionID, whose name is the package's.
func packageReads(t *testing.T, s index.Store, want index.Package, versionID int64) {
	t.Helper()
	ctx := t.Context()
	byOCI, err := s.GetPackageByNameOCI(ctx, want.RepoID, want.NameOCI)
	must(t, err)
	same(t, "GetPackageByNameOCI", want, *byOCI)
	again := index.Package{RepoID: want.RepoID, Name: want.Name, NameOCI: want.NameOCI}
	must(t, s.UpsertPackage(ctx, &again))
	same(t, "UpsertPackage of the row", want, again)
	list, err := s.ListPackages(ctx, want.RepoID, want.Name)
	must(t, err)
	same(t, "ListPackages", []index.Package{want}, list)
	after, err := s.ListPackagesAfter(ctx, want.RepoID, want.Name, "", 1)
	must(t, err)
	same(t, "ListPackagesAfter", []index.Package{want}, after)
	for _, sort := range []index.PackageSort{index.SortName, index.SortUpdated} {
		page, err := s.ListPackagePage(ctx, want.RepoID, index.PackageQuery{Prefix: want.Name, Sort: sort, Limit: 1})
		must(t, err)
		if len(page) != 1 {
			t.Fatalf("ListPackagePage by %s: %d packages, want 1", sort, len(page))
		}
		same(t, "ListPackagePage by "+string(sort), want, page[0].Package)
	}
	files, err := s.FindFiles(ctx, want.RepoID, want.Name)
	must(t, err)
	if len(files) != 1 || files[0].Version.ID != versionID {
		t.Fatalf("FindFiles: %+v, want the file of version %d", files, versionID)
	}
	same(t, "FindFiles", want, files[0].Package)
	unseen, err := s.ListUnseenVersions(ctx, want.RepoID, time.Now().Add(time.Hour))
	must(t, err)
	for _, m := range unseen {
		if m.Package.ID == want.ID {
			same(t, "ListUnseenVersions", want, m.Package)
		}
	}
}
