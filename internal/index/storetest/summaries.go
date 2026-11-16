package storetest

import (
	"encoding/json"
	"testing"

	"github.com/lajstrom/lajstrom/internal/index"
)

// testVersionSummaries: a row the store writes has its summary written, empty for none, when it
// keeps its metadata, so ListVersionsWithoutSummary lists none of them and SetVersionSummaries
// writes over none of them; a row without metadata is neither listed nor written. The rows of
// before migration 0029, whose summary is NULL, are in the test of the sqlite package
// (docs/spec/index-schema.md, The summaries of the rows before 0029).
func testVersionSummaries(t *testing.T, s index.Store) {
	ctx := t.Context()
	repo := newRepo(t, s, "pypi", "a")
	pkg := newPackage(t, s, repo.ID, "requests")
	rows := []index.Version{
		{Version: "1", Metadata: json.RawMessage(`{"summary": "One"}`), Summary: "One"},
		{Version: "2", Metadata: json.RawMessage(`{"name": "requests"}`)},
		{Version: "3"},
	}
	summaries := map[int64]string{1 << 40: "none"}
	for i := range rows {
		v := &rows[i]
		v.PackageID, v.Tag, v.Digest, v.ArtifactType, v.CreatedAt = pkg.ID, v.Version, dg(v.Version), "application/vnd.lajstrom.pypi.v1", at(i)
		must(t, s.UpsertVersion(ctx, v))
		summaries[v.ID] = "other"
	}
	got, err := s.ListVersionsWithoutSummary(ctx, 0, 10)
	must(t, err)
	same(t, "ListVersionsWithoutSummary", []index.Version{}, got)
	n, err := s.SetVersionSummaries(ctx, summaries)
	must(t, err)
	if n != 0 {
		t.Errorf("SetVersionSummaries wrote %d rows, want none", n)
	}
	for _, want := range rows {
		v, err := s.GetVersion(ctx, pkg.ID, want.Version, "")
		must(t, err)
		if v.Summary != want.Summary {
			t.Errorf("version %s: summary %q, want %q", want.Version, v.Summary, want.Summary)
		}
	}
	none, err := s.ListVersionsWithoutSummary(ctx, 0, 0)
	must(t, err)
	same(t, "ListVersionsWithoutSummary with no limit", []index.Version{}, none)
}
