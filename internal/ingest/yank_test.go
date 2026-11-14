package ingest_test

import (
	"errors"
	"fmt"
	"slices"
	"testing"

	"github.com/lajstrom/lajstrom/internal/format"
	"github.com/lajstrom/lajstrom/internal/index"
	"github.com/lajstrom/lajstrom/internal/ingest"
	"github.com/lajstrom/lajstrom/internal/oci/profile"
)

// opYank is the yank of a pypi version with reason ("" for none), and opUnyank its unyank.
func opYank(version, reason string) format.JournalOp {
	return format.JournalOp{ArtifactType: profile.ArtifactTypePypiYank, Op: profile.JournalYank, Version: version, Message: reason}
}

func opUnyank(version string) format.JournalOp {
	return format.JournalOp{ArtifactType: profile.ArtifactTypePypiYank, Op: profile.JournalUnyank, Version: version}
}

// newYankEnv is an npm env with the hosted pypi repo pypi/internal beside npm/internal, and its
// package requests, whose versions 2.32.0 and 2.32.3 each have a wheel and an sdist.
func newYankEnv(t *testing.T) (*npmEnv, index.Repo, index.Package) {
	t.Helper()
	ctx := t.Context()
	e := newNpmEnv(t, "1.0.0")
	pypi := index.Repo{Format: "pypi", Name: "internal", Registry: "local", Prefix: "pypi/internal", Kind: index.KindHosted}
	must(t, e.st.CreateRepo(ctx, &pypi))
	pkg := index.Package{RepoID: pypi.ID, Name: "requests", NameOCI: "requests"}
	must(t, e.st.UpsertPackage(ctx, &pkg))
	for _, v := range []string{"2.32.0", "2.32.3"} {
		for _, variant := range []string{"py3-none-any", "sdist"} {
			must(t, e.st.UpsertVersion(ctx, &index.Version{PackageID: pkg.ID, Version: v, Variant: variant, Tag: v + "__" + variant,
				Digest: dg(v + variant), ArtifactType: profile.ArtifactType("pypi")}))
		}
	}
	return e, pypi, pkg
}

// TestJournalYank: the yanks of a pypi package are entries of its journal (#81), under the _j
// tags of its OCI repository, with the lajstrom.pypi.* keys, a row each, the folded state, an
// audit row pypi.yank each and the repo's documents invalidated.
func TestJournalYank(t *testing.T) {
	ctx := t.Context()
	e, pypi, pkg := newYankEnv(t)
	putCache(t, e.st, &index.Cache{RepoID: pypi.ID, Scope: "simple/requests", Kind: "simple.json", Content: []byte("{}")})

	seqs, err := e.pipe.Journal(ctx, pypi, "requests", []format.JournalOp{opYank("2.32.0", "breaks proxies"), opYank("2.32.3", "")}, alice)
	must(t, err)
	same(t, "seqs", []int64{1, 2}, seqs)
	seqs, err = e.pipe.Journal(ctx, pypi, "requests", []format.JournalOp{opUnyank("2.32.3")}, alice)
	must(t, err)
	same(t, "seqs of the second append", []int64{3}, seqs)

	rows, err := e.st.ListJournal(ctx, pkg.ID)
	must(t, err)
	if len(rows) != 3 {
		t.Fatalf("%d journal rows, want 3", len(rows))
	}
	for i, r := range rows {
		if tag := profile.JournalTag(int64(i + 1)); r.Tag != tag || r.ArtifactType != profile.ArtifactTypePypiYank ||
			e.tagged(t, "pypi/internal/requests", tag) != r.Digest {
			t.Errorf("row %d: tag %s artifactType %s digest %s; the registry has %s under %s", i, r.Tag, r.ArtifactType, r.Digest,
				e.tagged(t, "pypi/internal/requests", tag), tag)
		}
	}
	same(t, "the attrs of the entries", []map[string]string{
		{"lajstrom.pypi.seq": "1", "lajstrom.pypi.op": "yank", "lajstrom.pypi.version": "2.32.0", "lajstrom.pypi.reason": "breaks proxies"},
		{"lajstrom.pypi.seq": "2", "lajstrom.pypi.op": "yank", "lajstrom.pypi.version": "2.32.3"},
		{"lajstrom.pypi.seq": "3", "lajstrom.pypi.op": "unyank", "lajstrom.pypi.version": "2.32.3"},
	}, []map[string]string{rows[0].Attrs, rows[1].Attrs, rows[2].Attrs})
	state, err := e.st.ListJournalState(ctx, pkg.ID)
	must(t, err)
	same(t, "state", []index.JournalState{{PackageID: pkg.ID, Kind: index.JournalYanked, Key: "2.32.0", Value: "breaks proxies", Seq: 1}}, state)

	rr, err := e.prov.Repository(ctx, "pypi/internal/requests")
	must(t, err)
	_, m := fetchFake(t, rr, profile.JournalTag(1))
	same(t, "annotations of entry 1", map[string]string{"lajstrom.schema": "1", "lajstrom.format": "pypi", "lajstrom.repo": "internal",
		"lajstrom.name": "requests", "lajstrom.pypi.seq": "1", "lajstrom.pypi.op": "yank", "lajstrom.pypi.version": "2.32.0",
		"lajstrom.pypi.reason": "breaks proxies", "lajstrom.uploaded-by": "user:alice", "org.opencontainers.image.created": "2026-11-07T12:00:00Z"},
		m.Annotations)

	var got []string
	for _, ev := range slices.Backward(e.rows(t).Audit) {
		if ev.Repo == "pypi/internal" {
			got = append(got, fmt.Sprintf("%s %s %t %s", ev.Action, ev.Subject, ev.Digest == rows[len(got)].Digest, ev.Details))
		}
	}
	same(t, "audit rows", []string{
		`pypi.yank requests 2.32.0 true {"op":"yank","seq":1,"version":"2.32.0"}`,
		`pypi.yank requests 2.32.3 true {"op":"yank","seq":2,"version":"2.32.3"}`,
		`pypi.yank requests 2.32.3 true {"op":"unyank","seq":3,"version":"2.32.3"}`,
	}, got)
	if _, err := e.st.GetIndexCache(ctx, pypi.ID, "simple/requests", "simple.json"); !errors.Is(err, index.ErrNotFound) {
		t.Errorf("the project page is still cached: %v", err)
	}
}

// TestJournalYankRefused: an op of npm's journal is refused in a pypi repo, a yank in an npm
// repo, and a yank of a version the package does not have; nothing is written.
func TestJournalYankRefused(t *testing.T) {
	ctx := t.Context()
	e, pypi, pkg := newYankEnv(t)
	for _, tt := range []struct {
		name    string
		repo    index.Repo
		pkgPath string
		op      format.JournalOp
	}{
		{"a dist-tag in a pypi repo", pypi, "requests", opAdd("beta", "2.32.0")},
		{"a deprecation in a pypi repo", pypi, "requests", opDeprecate("2.32.0", "old")},
		{"a yank in an npm repo", e.npm, widgetsPath, opYank("1.0.0", "")},
		{"a yank of a version the package does not have", pypi, "requests", opYank("2.31.0", "")},
		{"an unyank with a reason", pypi, "requests", format.JournalOp{ArtifactType: profile.ArtifactTypePypiYank, Op: profile.JournalUnyank,
			Version: "2.32.0", Message: "why"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := e.pipe.Journal(ctx, tt.repo, tt.pkgPath, []format.JournalOp{tt.op}, alice); !errors.Is(err, ingest.ErrInvalid) {
				t.Errorf("Journal = %v, want ErrInvalid", err)
			}
		})
	}
	if rows, err := e.st.ListJournal(ctx, pkg.ID); err != nil || len(rows) != 0 || len(e.entries(t)) != 0 {
		t.Errorf("a refused append wrote: %d pypi rows, %d npm rows, %v", len(rows), len(e.entries(t)), err)
	}
}
