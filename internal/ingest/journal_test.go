package ingest_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"sync"
	"testing"

	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/lajstrom/lajstrom/internal/format"
	"github.com/lajstrom/lajstrom/internal/index"
	"github.com/lajstrom/lajstrom/internal/ingest"
	"github.com/lajstrom/lajstrom/internal/oci/profile"
	"github.com/lajstrom/lajstrom/internal/provider"
)

// npmEnv is an env with the hosted npm repo npm/internal and the package @acme/widgets, whose
// versions are rows of the index: the journal reads no version's manifest.
type npmEnv struct {
	*env
	npm index.Repo
	pkg index.Package
}

const widgetsPath = "acme/widgets"

func newNpmEnv(t *testing.T, versions ...string) *npmEnv {
	t.Helper()
	ctx := t.Context()
	e := &npmEnv{env: newEnv(t)}
	e.npm = index.Repo{Format: "npm", Name: "internal", Registry: "local", Prefix: "npm/internal", Kind: index.KindHosted}
	must(t, e.st.CreateRepo(ctx, &e.npm))
	e.pkg = index.Package{RepoID: e.npm.ID, Name: "@acme/widgets", NameOCI: widgetsPath}
	must(t, e.st.UpsertPackage(ctx, &e.pkg))
	for _, v := range versions {
		must(t, e.st.UpsertVersion(ctx, &index.Version{PackageID: e.pkg.ID, Version: v, Tag: profile.Escape(v),
			Digest: dg(v), ArtifactType: profile.ArtifactType("npm")}))
	}
	return e
}

// opAdd, opRm and opDeprecate are journal ops.
func opAdd(tag, version string) format.JournalOp {
	return format.JournalOp{ArtifactType: profile.ArtifactTypeNpmDistTags, Op: profile.JournalAdd, Tag: tag, Version: version}
}

func opRm(tag string) format.JournalOp {
	return format.JournalOp{ArtifactType: profile.ArtifactTypeNpmDistTags, Op: profile.JournalRm, Tag: tag}
}

func opDeprecate(version, message string) format.JournalOp {
	return format.JournalOp{ArtifactType: profile.ArtifactTypeNpmDeprecation, Op: profile.JournalDeprecate, Version: version, Message: message}
}

// state returns the package's journal_state.
func (e *npmEnv) state(t *testing.T) []index.JournalState {
	t.Helper()
	s, err := e.st.ListJournalState(t.Context(), e.pkg.ID)
	must(t, err)
	return s
}

// entries returns the package's journal rows.
func (e *npmEnv) entries(t *testing.T) []index.JournalEntry {
	t.Helper()
	rows, err := e.st.ListJournal(t.Context(), e.pkg.ID)
	must(t, err)
	return rows
}

// TestJournal appends entries: each op is a manifest under the next _j tag of the package's OCI
// repository, with a row, the folded state, an audit row and the repo's documents invalidated.
func TestJournal(t *testing.T) {
	ctx := t.Context()
	e := newNpmEnv(t, "1.0.0", "1.1.0-beta.1")
	must(t, e.st.PutIndexCache(ctx, &index.Cache{RepoID: e.npm.ID, Scope: "@acme/widgets", Kind: "packument", Content: []byte("{}")}))

	seqs, err := e.pipe.Journal(ctx, e.npm, widgetsPath, []format.JournalOp{opAdd("latest", "1.0.0"), opAdd("beta", "1.1.0-beta.1")}, alice)
	must(t, err)
	same(t, "seqs", []int64{1, 2}, seqs)
	seqs, err = e.pipe.Journal(ctx, e.npm, widgetsPath, []format.JournalOp{opDeprecate("1.0.0", "use 1.1"), opRm("beta")}, alice)
	must(t, err)
	same(t, "seqs of the second append", []int64{3, 4}, seqs)

	rows := e.entries(t)
	if len(rows) != 4 {
		t.Fatalf("%d journal rows, want 4", len(rows))
	}
	for i, r := range rows {
		tag := profile.JournalTag(int64(i + 1))
		if r.Seq != int64(i+1) || r.Tag != tag || e.tagged(t, "npm/internal/"+widgetsPath, tag) != r.Digest {
			t.Errorf("row %d: seq %d tag %s digest %s; the registry has %s under %s", i, r.Seq, r.Tag, r.Digest,
				e.tagged(t, "npm/internal/"+widgetsPath, tag), tag)
		}
	}
	same(t, "the attrs of entry 3", map[string]string{"lajstrom.npm.seq": "3", "lajstrom.npm.op": "deprecate",
		"lajstrom.npm.version": "1.0.0", "lajstrom.npm.message": "use 1.1"}, rows[2].Attrs)
	same(t, "state", []index.JournalState{
		{PackageID: e.pkg.ID, Kind: index.JournalDeprecated, Key: "1.0.0", Value: "use 1.1", Seq: 3},
		{PackageID: e.pkg.ID, Kind: index.JournalDistTag, Key: "latest", Value: "1.0.0", Seq: 1},
	}, e.state(t))

	m := e.manifest(t, profile.JournalTag(3))
	same(t, "annotations of entry 3", map[string]string{"lajstrom.schema": "1", "lajstrom.format": "npm", "lajstrom.repo": "internal",
		"lajstrom.name": "@acme/widgets", "lajstrom.npm.seq": "3", "lajstrom.npm.op": "deprecate", "lajstrom.npm.version": "1.0.0",
		"lajstrom.npm.message": "use 1.1", "lajstrom.uploaded-by": "user:alice", "org.opencontainers.image.created": "2026-11-07T12:00:00Z"},
		m.Annotations)

	audit := e.rows(t).Audit
	var got []string
	for _, ev := range slices.Backward(audit) {
		got = append(got, fmt.Sprintf("%s %s %t %s", ev.Action, ev.Subject, ev.Digest == rows[len(got)].Digest, ev.Details))
	}
	same(t, "audit rows", []string{
		`npm.dist-tag @acme/widgets latest true {"op":"add","seq":1,"tag":"latest","version":"1.0.0"}`,
		`npm.dist-tag @acme/widgets beta true {"op":"add","seq":2,"tag":"beta","version":"1.1.0-beta.1"}`,
		`npm.deprecate @acme/widgets 1.0.0 true {"op":"deprecate","seq":3,"version":"1.0.0"}`,
		`npm.dist-tag @acme/widgets beta true {"op":"rm","seq":4,"tag":"beta"}`,
	}, got)
	if _, err := e.st.GetIndexCache(ctx, e.npm.ID, "@acme/widgets", "packument"); !errors.Is(err, index.ErrNotFound) {
		t.Errorf("the packument is still cached: %v", err)
	}
}

// manifest returns the manifest under tag of the package's OCI repository.
func (e *npmEnv) manifest(t *testing.T, tag string) ocispec.Manifest {
	t.Helper()
	rr, err := e.prov.Repository(t.Context(), "npm/internal/"+widgetsPath)
	must(t, err)
	_, m := fetchFake(t, rr, tag)
	return m
}

// TestJournalRefused: what Journal refuses writes nothing, neither in the registry nor in the
// index.
func TestJournalRefused(t *testing.T) {
	ctx := t.Context()
	e := newNpmEnv(t, "1.0.0")
	proxy := e.npm
	proxy.Kind = index.KindProxy
	raw := e.repo
	tests := []struct {
		name    string
		repo    index.Repo
		pkgPath string
		ops     []format.JournalOp
		want    error
	}{
		{"rm latest", e.npm, widgetsPath, []format.JournalOp{opRm("latest")}, ingest.ErrInvalid},
		{"a tag that begins with a digit", e.npm, widgetsPath, []format.JournalOp{opAdd("1x", "1.0.0")}, ingest.ErrInvalid},
		{"a version the package does not have", e.npm, widgetsPath, []format.JournalOp{opAdd("beta", "1.0.0"), opAdd("next", "2.0.0")}, ingest.ErrInvalid},
		{"a package the repo does not have", e.npm, "acme/gadgets", []format.JournalOp{opAdd("beta", "1.0.0")}, index.ErrNotFound},
		{"a raw repo", raw, widgetsPath, []format.JournalOp{opAdd("beta", "1.0.0")}, ingest.ErrInvalid},
		{"a proxy repo", proxy, widgetsPath, []format.JournalOp{opAdd("beta", "1.0.0")}, format.ErrMethodNotAllowed},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := e.pipe.Journal(ctx, tt.repo, tt.pkgPath, tt.ops, alice)
			if !errors.Is(err, tt.want) {
				t.Errorf("Journal = %v, want %v", err, tt.want)
			}
		})
	}
	if len(e.pushRefs(profile.JournalTag(1))) != 0 || len(e.entries(t)) != 0 || len(e.state(t)) != 0 {
		t.Errorf("a refused append wrote: %d pushes, %d rows, %d state rows", len(e.pushRefs(profile.JournalTag(1))),
			len(e.entries(t)), len(e.state(t)))
	}
	if seqs, err := e.pipe.Journal(ctx, e.npm, widgetsPath, nil, alice); err != nil || len(seqs) != 0 {
		t.Errorf("Journal of no op = %v, %v", seqs, err)
	}
}

// pushEntry pushes the entry seq of @acme/widgets, as a writer that stopped before its index
// write left it, or bytes that are no entry.
func (e *npmEnv) pushEntry(t *testing.T, seq int64, op format.JournalOp, raw []byte) {
	t.Helper()
	ctx := t.Context()
	if raw == nil {
		m, err := profile.BuildJournal(profile.JournalEntry{JournalChange: profile.JournalChange(op), Format: "npm", Repo: "internal",
			Name: "@acme/widgets", Seq: seq, Created: now})
		must(t, err)
		raw, _, err = profile.Encode(m)
		must(t, err)
	}
	rr, err := e.prov.Repository(ctx, "npm/internal/"+widgetsPath)
	must(t, err)
	must(t, rr.Push(ctx, profile.EmptyConfig, bytes.NewReader(ocispec.DescriptorEmptyJSON.Data)))
	desc := ocispec.Descriptor{MediaType: ocispec.MediaTypeImageManifest, Digest: dgBytes(raw), Size: int64(len(raw))}
	must(t, rr.PushReference(ctx, desc, bytes.NewReader(raw), profile.JournalTag(seq)))
}

// TestJournalAdopts: entries in the registry after the index's last are adopted before the
// append's own, which get the next numbers; one that is not the package's entry stops the
// journal, and nothing is pushed.
func TestJournalAdopts(t *testing.T) {
	ctx := t.Context()
	e := newNpmEnv(t, "1.0.0", "2.0.0")
	e.pushEntry(t, 1, opAdd("next", "2.0.0"), nil)
	e.pushEntry(t, 2, opDeprecate("1.0.0", "old"), nil)
	seqs, err := e.pipe.Journal(ctx, e.npm, widgetsPath, []format.JournalOp{opAdd("latest", "2.0.0")}, alice)
	must(t, err)
	same(t, "seqs", []int64{3}, seqs)
	if rows := e.entries(t); len(rows) != 3 {
		t.Errorf("%d rows, want the two adopted and the new one", len(rows))
	}
	same(t, "state", []index.JournalState{
		{PackageID: e.pkg.ID, Kind: index.JournalDeprecated, Key: "1.0.0", Value: "old", Seq: 2},
		{PackageID: e.pkg.ID, Kind: index.JournalDistTag, Key: "latest", Value: "2.0.0", Seq: 3},
		{PackageID: e.pkg.ID, Kind: index.JournalDistTag, Key: "next", Value: "2.0.0", Seq: 1},
	}, e.state(t))

	e.pushEntry(t, 4, format.JournalOp{}, []byte(`{"schemaVersion":2}`))
	if _, err := e.pipe.Journal(ctx, e.npm, widgetsPath, []format.JournalOp{opAdd("beta", "1.0.0")}, alice); err == nil {
		t.Error("Journal over a broken entry succeeded")
	}
	if len(e.pushRefs(profile.JournalTag(5))) != 0 || len(e.entries(t)) != 3 {
		t.Error("Journal over a broken entry wrote")
	}
}

// TestJournalConcurrent: changes of one dist-tag through two pipelines at once, as through two
// replicas, under the store's real lock: no change is lost, the entries are numbered from 1
// without gaps in the order of the lock, and the state is the last one's.
func TestJournalConcurrent(t *testing.T) {
	ctx := t.Context()
	versions := []string{"1.0.0", "1.0.1", "1.0.2", "1.0.3", "1.0.4", "1.0.5", "1.0.6", "1.0.7"}
	e := newNpmEnv(t, versions...)
	pipes := []*ingest.Pipeline{e.pipe, ingest.New(e.options(e.prov))}
	seqs := make([]int64, len(versions))
	errs := make([]error, len(versions))
	var wg sync.WaitGroup
	for i, v := range versions {
		wg.Go(func() {
			s, err := pipes[i%2].Journal(ctx, e.npm, widgetsPath, []format.JournalOp{opAdd("latest", v)}, alice)
			if errs[i] = err; err == nil {
				seqs[i] = s[0]
			}
		})
	}
	wg.Wait()
	must(t, errors.Join(errs...))
	got := slices.Sorted(slices.Values(seqs))
	same(t, "seqs", []int64{1, 2, 3, 4, 5, 6, 7, 8}, got)
	rows := e.entries(t)
	if len(rows) != len(versions) {
		t.Fatalf("%d rows, want %d", len(rows), len(versions))
	}
	last := slices.Index(seqs, 8)
	same(t, "state: the change with the last seq", []index.JournalState{
		{PackageID: e.pkg.ID, Kind: index.JournalDistTag, Key: "latest", Value: versions[last], Seq: 8},
	}, e.state(t))
	for i, v := range versions {
		if got := rows[seqs[i]-1].Attrs["lajstrom.npm.version"]; got != v {
			t.Errorf("the entry of the change to %s, seq %d, names %s", v, seqs[i], got)
		}
	}
}

// dg is the digest of the string s.
func dg(s string) digest.Digest { return digest.FromString(s) }

// dgBytes is the digest of b.
func dgBytes(b []byte) digest.Digest { return digest.FromBytes(b) }

// fetchFake reads the manifest under tag of rr.
func fetchFake(t *testing.T, rr provider.Repository, tag string) ([]byte, ocispec.Manifest) {
	t.Helper()
	desc, err := rr.Resolve(t.Context(), tag)
	must(t, err)
	rc, err := rr.Fetch(t.Context(), desc)
	must(t, err)
	defer func() { _ = rc.Close() }()
	raw, err := io.ReadAll(rc)
	must(t, err)
	var m ocispec.Manifest
	must(t, json.Unmarshal(raw, &m))
	return raw, m
}
