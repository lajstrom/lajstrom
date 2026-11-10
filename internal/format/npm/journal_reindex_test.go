package npm

import (
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"

	"github.com/lajstrom/lajstrom/internal/format"
	"github.com/lajstrom/lajstrom/internal/index"
	"github.com/lajstrom/lajstrom/internal/oci/profile"
	"github.com/lajstrom/lajstrom/internal/testutil"
)

// journalOf returns the journal rows and the state of a package, without ids and the times of
// indexing.
func (p *pipeline) journalOf(t *testing.T, name string) ([]index.JournalEntry, []index.JournalState) {
	t.Helper()
	ctx := t.Context()
	pkg, err := p.st.GetPackage(ctx, p.repo.ID, name)
	must(t, err)
	rows, err := p.st.ListJournal(ctx, pkg.ID)
	must(t, err)
	state, err := p.st.ListJournalState(ctx, pkg.ID)
	must(t, err)
	for i := range rows {
		rows[i].ID, rows[i].PackageID, rows[i].IndexedAt = 0, 0, rows[i].IndexedAt.Truncate(0)
	}
	for i := range state {
		state[i].PackageID = 0
	}
	return rows, state
}

// journalOps is a journal of every op: two dist-tags moved and one removed, a deprecation set
// and one cleared.
var journalOps = []format.JournalOp{
	{ArtifactType: profile.ArtifactTypeNpmDistTags, Op: profile.JournalAdd, Tag: "latest", Version: "1.0.0"},
	{ArtifactType: profile.ArtifactTypeNpmDistTags, Op: profile.JournalAdd, Tag: "beta", Version: "2.0.0-beta.1"},
	{ArtifactType: profile.ArtifactTypeNpmDistTags, Op: profile.JournalAdd, Tag: "next", Version: "2.0.0-beta.1"},
	{ArtifactType: profile.ArtifactTypeNpmDeprecation, Op: profile.JournalDeprecate, Version: "1.0.0", Message: "use 1.1.0"},
	{ArtifactType: profile.ArtifactTypeNpmDistTags, Op: profile.JournalAdd, Tag: "latest", Version: "1.1.0"},
	{ArtifactType: profile.ArtifactTypeNpmDeprecation, Op: profile.JournalDeprecate, Version: "1.1.0", Message: "oops"},
	{ArtifactType: profile.ArtifactTypeNpmDeprecation, Op: profile.JournalUndeprecate, Version: "1.1.0"},
	{ArtifactType: profile.ArtifactTypeNpmDistTags, Op: profile.JournalRm, Tag: "next"},
}

// journalPipeline is a pipeline with the versions 1.0.0, 1.1.0 and 2.0.0-beta.1 of
// @acme/widgets and the journal of journalOps, one append each.
func journalPipeline(t *testing.T) *pipeline {
	t.Helper()
	p := newPipeline(t)
	for _, v := range []string{"1.0.0", "1.1.0", "2.0.0-beta.1"} {
		b, _ := testutil.NpmTarball(testutil.NpmOptions{Name: "@acme/widgets", Version: v})
		p.push(t, p.build(t, b))
	}
	for _, op := range journalOps {
		if _, err := p.pipe.Journal(t.Context(), p.repo, "acme/widgets", []format.JournalOp{op}, alice); err != nil {
			t.Fatal(err)
		}
	}
	return p
}

// TestJournalReindex is the acceptance criterion of #307: with the derived tables dropped,
// reindex replays the journal and restores every dist-tag and deprecation, and the next run
// sees every entry unchanged.
func TestJournalReindex(t *testing.T) {
	p := journalPipeline(t)
	rows, state := p.journalOf(t, "@acme/widgets")
	if d := cmp.Diff([]index.JournalState{
		{Kind: index.JournalDeprecated, Key: "1.0.0", Value: "use 1.1.0", Seq: 4},
		{Kind: index.JournalDistTag, Key: "beta", Value: "2.0.0-beta.1", Seq: 2},
		{Kind: index.JournalDistTag, Key: "latest", Value: "1.1.0", Seq: 5},
	}, state); d != "" {
		t.Fatalf("the state before reindex (-want +got)\n%s", d)
	}

	must(t, p.st.ResetDerived(t.Context(), p.repo.ID))
	if r := p.reindex(t); r.Added != 3+len(journalOps) || len(r.Errors) != 0 || len(r.OrphanJournal) != 0 {
		t.Errorf("reindex: %+v", r)
	}
	gotRows, gotState := p.journalOf(t, "@acme/widgets")
	if d := cmp.Diff(rows, gotRows, cmpopts.IgnoreFields(index.JournalEntry{}, "IndexedAt")); d != "" {
		t.Errorf("the journal after reindex (-before +after)\n%s", d)
	}
	if d := cmp.Diff(state, gotState); d != "" {
		t.Errorf("the state after reindex (-before +after)\n%s", d)
	}
	if r := p.reindex(t); r.Unchanged != 3+len(journalOps) || r.Added != 0 || r.Removed != 0 {
		t.Errorf("reindex again: %+v", r)
	}
}

// TestJournalReindexLost: the registry loses entry 5, the move of latest to 1.1.0. Reindex
// removes its row, reports the gap and replays the rest: latest is 1.0.0 again, as entry 1 set
// it.
func TestJournalReindexLost(t *testing.T) {
	ctx := t.Context()
	p := journalPipeline(t)
	rr, err := p.fake.Repository(ctx, "npm/internal/acme/widgets")
	must(t, err)
	desc, err := rr.Resolve(ctx, profile.JournalTag(5))
	must(t, err)
	must(t, rr.Delete(ctx, desc))
	r := p.reindex(t)
	if r.Removed != 1 || len(r.Errors) != 1 || r.Errors[0].Tag != profile.JournalTag(5) {
		t.Errorf("reindex after a lost entry: %+v", r)
	}
	_, state := p.journalOf(t, "@acme/widgets")
	if want := (index.JournalState{Kind: index.JournalDistTag, Key: "latest", Value: "1.0.0", Seq: 1}); len(state) != 3 || state[2] != want {
		t.Errorf("the state without entry 5: %+v", state)
	}
}

// TestJournalReindexOrphans: with every version deleted, the package goes, and its entries are
// orphans that reindex reports and does not index.
func TestJournalReindexOrphans(t *testing.T) {
	ctx := t.Context()
	p := journalPipeline(t)
	for _, v := range []string{"1.0.0", "1.1.0", "2.0.0-beta.1"} {
		if _, err := p.pipe.Delete(ctx, p.repo, format.Deletion{Path: "acme/widgets", Reference: v}, alice); err != nil {
			t.Fatal(err)
		}
	}
	if r := p.reindex(t); len(r.OrphanJournal) != len(journalOps) || len(r.Errors) != 0 {
		t.Errorf("reindex of a journal without versions: %d orphans, %+v", len(r.OrphanJournal), r)
	}
	if _, err := p.st.GetPackage(ctx, p.repo.ID, "@acme/widgets"); err == nil {
		t.Error("the package of the orphans is indexed")
	}
}
