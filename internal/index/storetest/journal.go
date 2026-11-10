package storetest

import (
	"errors"
	"fmt"
	"slices"
	"strconv"
	"sync"
	"testing"

	"github.com/lajstrom/lajstrom/internal/index"
)

// journalEntry is the entry seq of package packageID, its digest made of digestOf.
func journalEntry(packageID, seq int64, digestOf string) index.JournalEntry {
	return index.JournalEntry{PackageID: packageID, Seq: seq, Tag: fmt.Sprintf("_j%08d", seq), Digest: dg(digestOf),
		ArtifactType: "application/vnd.lajstrom.npm.dist-tags.v1",
		Attrs:        map[string]string{"lajstrom.npm.seq": strconv.FormatInt(seq, 10), "lajstrom.npm.op": "add"},
		CreatedAt:    at(int(seq)), IndexedAt: at(100)}
}

// testJournal: the entries of an npm package's journal are listed in seq order, never rewritten
// (the same entry again is seen, another under its seq or its tag a TagConflictError), touched
// and deleted by id; the state is replaced whole; both go with their package.
func testJournal(t *testing.T, s index.Store) {
	ctx := t.Context()
	repo := newRepo(t, s, "npm", "internal")
	pkg := newPackage(t, s, repo.ID, "widgets")
	other := newPackage(t, s, repo.ID, "gadgets")

	entries := []index.JournalEntry{journalEntry(pkg.ID, 2, "two"), journalEntry(pkg.ID, 1, "one"), journalEntry(other.ID, 1, "o")}
	for i := range entries {
		must(t, s.UpsertJournal(ctx, &entries[i]))
		if entries[i].ID == 0 {
			t.Fatalf("UpsertJournal of seq %d did not fill ID", entries[i].Seq)
		}
	}
	got, err := s.ListJournal(ctx, pkg.ID)
	must(t, err)
	same(t, "journal in seq order", []index.JournalEntry{entries[1], entries[0]}, got)

	again := journalEntry(pkg.ID, 1, "one")
	again.CreatedAt, again.IndexedAt = at(50), at(200)
	must(t, s.UpsertJournal(ctx, &again))
	want := entries[1]
	want.IndexedAt = at(200)
	same(t, "the same entry again: seen, filled from the row", want, again)

	for _, e := range []index.JournalEntry{
		journalEntry(pkg.ID, 1, "another"),                                                   // another digest under the seq
		{PackageID: pkg.ID, Seq: 3, Tag: "_j00000001", Digest: dg("one"), ArtifactType: "x"}, // the tag under another seq
		{PackageID: pkg.ID, Seq: 1, Tag: "_j00000009", Digest: dg("one"), ArtifactType: "x"}, // another tag under the seq
	} {
		err := s.UpsertJournal(ctx, &e)
		wantTagConflict(t, fmt.Sprintf("seq %d tag %s digest %s", e.Seq, e.Tag, e.Digest), err, e.Tag, dg("one"))
	}
	got, err = s.ListJournal(ctx, pkg.ID)
	must(t, err)
	same(t, "journal after the conflicts", []index.JournalEntry{want, entries[0]}, got)

	must(t, s.TouchJournal(ctx, []int64{entries[0].ID, 1 << 40}, at(300)))
	got, err = s.ListJournal(ctx, pkg.ID)
	must(t, err)
	if got[1].IndexedAt != at(300) || got[0].IndexedAt != at(200) {
		t.Errorf("TouchJournal: indexed_at %v and %v, want %v and %v", got[0].IndexedAt, got[1].IndexedAt, at(200), at(300))
	}
	must(t, s.DeleteJournal(ctx, entries[0].ID))
	wantErr(t, "DeleteJournal of a deleted entry", s.DeleteJournal(ctx, entries[0].ID), index.ErrNotFound)
	got, err = s.ListJournal(ctx, pkg.ID)
	must(t, err)
	same(t, "journal after the delete", []index.JournalEntry{want}, got)
	wantErr(t, "an entry of a missing package", s.UpsertJournal(ctx, ptr(journalEntry(1<<40, 1, "x"))), index.ErrNotFound)
}

// journalStateFixture is a state of three rows, out of order.
var journalStateFixture = []index.JournalState{
	{Kind: index.JournalDistTag, Key: "latest", Value: "1.0.0", Seq: 1},
	{Kind: index.JournalDeprecated, Key: "0.9.0", Value: "use 1.0.0", Seq: 3},
	{Kind: index.JournalDistTag, Key: "beta", Value: "1.1.0-beta.1", Seq: 2},
}

// testJournalState: a package's state is replaced whole and listed by kind and key; a state with
// a key twice changes nothing, and so does the state of a missing package.
func testJournalState(t *testing.T, s index.Store) {
	ctx := t.Context()
	repo := newRepo(t, s, "npm", "internal")
	pkg := newPackage(t, s, repo.ID, "widgets")
	other := newPackage(t, s, repo.ID, "gadgets")
	state := slices.Clone(journalStateFixture)
	must(t, s.ReplaceJournalState(ctx, pkg.ID, state))
	must(t, s.ReplaceJournalState(ctx, other.ID, []index.JournalState{{Kind: index.JournalDistTag, Key: "latest", Value: "2.0.0", Seq: 1}}))
	gotState, err := s.ListJournalState(ctx, pkg.ID)
	must(t, err)
	same(t, "state ordered by kind and key", []index.JournalState{
		{PackageID: pkg.ID, Kind: index.JournalDeprecated, Key: "0.9.0", Value: "use 1.0.0", Seq: 3},
		{PackageID: pkg.ID, Kind: index.JournalDistTag, Key: "beta", Value: "1.1.0-beta.1", Seq: 2},
		{PackageID: pkg.ID, Kind: index.JournalDistTag, Key: "latest", Value: "1.0.0", Seq: 1},
	}, gotState)
	must(t, s.ReplaceJournalState(ctx, pkg.ID, []index.JournalState{{Kind: index.JournalDistTag, Key: "latest", Value: "1.1.0", Seq: 4}}))
	gotState, err = s.ListJournalState(ctx, pkg.ID)
	must(t, err)
	same(t, "state replaced", []index.JournalState{{PackageID: pkg.ID, Kind: index.JournalDistTag, Key: "latest", Value: "1.1.0", Seq: 4}}, gotState)

	dup := []index.JournalState{{Kind: index.JournalDistTag, Key: "latest", Value: "1", Seq: 5}, {Kind: index.JournalDistTag, Key: "latest", Value: "2", Seq: 6}}
	wantErr(t, "a state with one key twice", s.ReplaceJournalState(ctx, pkg.ID, dup), index.ErrConflict)
	gotState, err = s.ListJournalState(ctx, pkg.ID)
	must(t, err)
	if len(gotState) != 1 || gotState[0].Value != "1.1.0" {
		t.Errorf("a refused state changed the rows: %+v", gotState)
	}
	wantErr(t, "the state of a missing package", s.ReplaceJournalState(ctx, 1<<40, state), index.ErrNotFound)
}

// testJournalCascades: both tables go with their package, by DeletePackage, DeleteEmptyPackages
// and ResetDerived.
func testJournalCascades(t *testing.T, s index.Store) {
	ctx := t.Context()
	repo := newRepo(t, s, "npm", "internal")
	pkg := newPackage(t, s, repo.ID, "widgets")
	other := newPackage(t, s, repo.ID, "gadgets")
	for _, p := range []index.Package{pkg, other} {
		must(t, s.UpsertJournal(ctx, ptr(journalEntry(p.ID, 1, p.Name))))
		must(t, s.ReplaceJournalState(ctx, p.ID, journalStateFixture[:1]))
	}
	must(t, s.DeletePackage(ctx, pkg.ID))
	for what, n := range map[string]func() (int, error){
		"journal":       func() (int, error) { rows, err := s.ListJournal(ctx, pkg.ID); return len(rows), err },
		"journal_state": func() (int, error) { rows, err := s.ListJournalState(ctx, pkg.ID); return len(rows), err },
	} {
		if got, err := n(); err != nil || got != 0 {
			t.Errorf("%s of a deleted package: %d rows, %v", what, got, err)
		}
	}
	if _, err := s.DeleteEmptyPackages(ctx, repo.ID); err != nil {
		t.Fatal(err)
	}
	if rows, err := s.ListJournal(ctx, other.ID); err != nil || len(rows) != 0 {
		t.Errorf("journal of a package without versions after DeleteEmptyPackages: %d rows, %v", len(rows), err)
	}
	kept := newPackage(t, s, repo.ID, "kept")
	newVersion(t, s, kept.ID, "1.0.0")
	must(t, s.UpsertJournal(ctx, ptr(journalEntry(kept.ID, 1, "kept"))))
	must(t, s.ReplaceJournalState(ctx, kept.ID, journalStateFixture[:1]))
	must(t, s.ResetDerived(ctx, repo.ID))
	if rows, err := s.ListJournal(ctx, kept.ID); err != nil || len(rows) != 0 {
		t.Errorf("journal after ResetDerived: %d rows, %v", len(rows), err)
	}
	if rows, err := s.ListJournalState(ctx, kept.ID); err != nil || len(rows) != 0 {
		t.Errorf("journal_state after ResetDerived: %d rows, %v", len(rows), err)
	}
}

// testJournalRace: writers that insert one seq of a package at once end with one row; those of
// another digest get a TagConflictError naming it, never a driver error, and those of the same
// entry see the row.
func testJournalRace(t *testing.T, s index.Store) {
	ctx := t.Context()
	repo := newRepo(t, s, "npm", "race")
	pkg := newPackage(t, s, repo.ID, "widgets")
	const writers = 8
	for seq, digestOf := range map[int64]func(i int) string{1: strconv.Itoa, 2: func(int) string { return "same" }} {
		errs := make([]error, writers)
		var wg sync.WaitGroup
		for i := range writers {
			wg.Go(func() {
				e := journalEntry(pkg.ID, seq, digestOf(i))
				errs[i] = s.UpsertJournal(ctx, &e)
			})
		}
		wg.Wait()
		rows, err := s.ListJournal(ctx, pkg.ID)
		must(t, err)
		var stored *index.JournalEntry
		for i := range rows {
			if rows[i].Seq == seq {
				stored = &rows[i]
			}
		}
		if stored == nil {
			t.Fatalf("seq %d: no row after the race", seq)
		}
		winners := 0
		for i, err := range errs {
			var tc *index.TagConflictError
			switch {
			case err == nil:
				winners++
			case !errors.As(err, &tc) || tc.Existing != stored.Digest:
				t.Errorf("seq %d, writer %d: %v, want a TagConflictError naming %s", seq, i, err, stored.Digest)
			}
		}
		if want := map[int64]int{1: 1, 2: writers}[seq]; winners != want {
			t.Errorf("seq %d: %d writers succeeded, want %d", seq, winners, want)
		}
	}
}
