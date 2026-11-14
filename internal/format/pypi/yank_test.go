package pypi

import (
	"log/slog"
	"net/http"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"

	"github.com/lajstrom/lajstrom/internal/auth"
	"github.com/lajstrom/lajstrom/internal/format"
	"github.com/lajstrom/lajstrom/internal/index"
	"github.com/lajstrom/lajstrom/internal/index/reindex"
	"github.com/lajstrom/lajstrom/internal/oci/profile"
	"github.com/lajstrom/lajstrom/internal/provider"
	"github.com/lajstrom/lajstrom/internal/testutil"
)

// yank appends a yank of version of my-pkg with reason ("" for none), or an unyank when unyank
// is set, through the pipeline, as the management API does.
func (e *env) yank(version, reason string, unyank bool) {
	e.t.Helper()
	op := format.JournalOp{ArtifactType: profile.ArtifactTypePypiYank, Op: profile.JournalYank, Version: version, Message: reason}
	if unyank {
		op.Op = profile.JournalUnyank
	}
	if _, err := e.pipe.Journal(e.t.Context(), e.repo, "my-pkg", []format.JournalOp{op}, auth.Principal{Kind: auth.KindUser, Name: "alice"}); err != nil {
		e.t.Fatalf("%s %s: %v", op.Op, version, err)
	}
}

// pages returns the HTML and the JSON page of my-pkg.
func (e *env) pages() (string, []byte) {
	e.t.Helper()
	h := e.do(http.MethodGet, "simple/my-pkg/")
	j := e.do(http.MethodGet, "simple/my-pkg/", "Accept", contentTypeJSON)
	if h.Code != http.StatusOK || j.Code != http.StatusOK {
		e.t.Fatalf("the pages: %d, %d", h.Code, j.Code)
	}
	return h.Body.String(), j.Body.Bytes()
}

// TestYankReindex: the yanks of a package are entries of its journal in the registry, so with the
// derived tables dropped reindex restores its rows, its state and the pages the simple index
// renders from them; the next run sees every entry unchanged.
func TestYankReindex(t *testing.T) {
	ctx := t.Context()
	e := newPublished(t)
	next, ni := testutil.Wheel(testutil.WheelOptions{Name: "My_Pkg", Version: "1.1"})
	e.publish(ni.Filename, next)
	e.yank("1.0", "broken", false)
	e.yank("1.1", "", false)
	e.yank("1.1", "", true)
	e.yank("1.1", "the wheel is empty", false)
	pkg, err := e.st.GetPackage(ctx, e.repo.ID, "my-pkg")
	must(t, err)
	journalOf := func() ([]index.JournalEntry, []index.JournalState) {
		rows, err := e.st.ListJournal(ctx, pkg.ID)
		must(t, err)
		state, err := e.st.ListJournalState(ctx, pkg.ID)
		must(t, err)
		for i := range rows {
			rows[i].ID, rows[i].PackageID = 0, 0
		}
		for i := range state {
			state[i].PackageID = 0
		}
		return rows, state
	}
	rows, state := journalOf()
	if d := cmp.Diff([]index.JournalState{
		{Kind: index.JournalYanked, Key: "1.0", Value: "broken", Seq: 1},
		{Kind: index.JournalYanked, Key: "1.1", Value: "the wheel is empty", Seq: 4},
	}, state); d != "" {
		t.Fatalf("the state before reindex (-want +got)\n%s", d)
	}
	html, page := e.pages()

	must(t, e.st.ResetDerived(ctx, e.repo.ID))
	w := reindex.New(reindex.Options{Index: e.st, Providers: provider.Map{"local": e.prov}, Inspector: e.pipe, Log: slog.New(slog.DiscardHandler)})
	req := reindex.Request{Repo: &e.repo, Prune: true, Source: "cli", Principal: "user:admin"}
	run := func() reindex.RepoReport {
		r, err := w.Start(ctx, req)
		must(t, err)
		report, err := w.Run(ctx, r, req)
		must(t, err)
		return report.Repos[0]
	}
	if r := run(); r.Added != 4+4 || len(r.Errors) != 0 || len(r.OrphanJournal) != 0 {
		t.Errorf("reindex: %+v", r)
	}
	if pkg, err = e.st.GetPackage(ctx, e.repo.ID, "my-pkg"); err != nil {
		t.Fatal(err)
	}
	gotRows, gotState := journalOf()
	if d := cmp.Diff(rows, gotRows, cmpopts.IgnoreFields(index.JournalEntry{}, "IndexedAt")); d != "" {
		t.Errorf("the journal after reindex (-before +after)\n%s", d)
	}
	if d := cmp.Diff(state, gotState); d != "" {
		t.Errorf("the state after reindex (-before +after)\n%s", d)
	}
	gotHTML, gotPage := e.pages()
	if gotHTML != html || string(gotPage) != string(page) {
		t.Errorf("the pages after reindex:\n%s\n%s\nwant\n%s\n%s", gotHTML, gotPage, html, page)
	}
	if r := run(); r.Unchanged != 4+4 || r.Added != 0 || r.Removed != 0 {
		t.Errorf("reindex again: %+v", r)
	}
}
