package storetest

import (
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/lajstrom/lajstrom/internal/index"
)

// testPackageByNameOCI checks GetPackageByNameOCI and that NameOCI is unique within a repo:
// another name with a NameOCI that a package holds is ErrConflict naming both names and changes
// nothing, the holder upserts as before, the same NameOCI in another repo is a package of its
// own, and once the holder is deleted the other name may take the NameOCI.
func testPackageByNameOCI(t *testing.T, s index.Store) {
	ctx := t.Context()
	repo := newRepo(t, s, "raw", "a")
	other := newRepo(t, s, "raw", "b")
	first := index.Package{RepoID: repo.ID, Name: "Docs/Readme.MD", NameOCI: "docs/readme.md", CreatedAt: at(1)}
	must(t, s.UpsertPackage(ctx, &first))
	elsewhere := index.Package{RepoID: other.ID, Name: "docs/readme.md", NameOCI: "docs/readme.md", CreatedAt: at(2)}
	must(t, s.UpsertPackage(ctx, &elsewhere))
	onlyHere := newPackage(t, s, repo.ID, "only-a")

	got, err := s.GetPackageByNameOCI(ctx, repo.ID, "docs/readme.md")
	must(t, err)
	same(t, "GetPackageByNameOCI", first, *got)
	got, err = s.GetPackageByNameOCI(ctx, other.ID, "docs/readme.md")
	must(t, err)
	same(t, "GetPackageByNameOCI in the other repo", elsewhere, *got)

	twin := index.Package{RepoID: repo.ID, Name: "docs/readme.md", NameOCI: "docs/readme.md", CreatedAt: at(3)}
	err = s.UpsertPackage(ctx, &twin)
	if !errors.Is(err, index.ErrConflict) || !strings.Contains(err.Error(), `"docs/readme.md"`) ||
		!strings.Contains(err.Error(), `"Docs/Readme.MD"`) {
		t.Errorf("UpsertPackage of another name with a NameOCI that is held: %v, want ErrConflict naming both names", err)
	}
	list, err := s.ListPackages(ctx, repo.ID, "")
	must(t, err)
	same(t, "packages after the conflict", []index.Package{first, onlyHere}, list)
	again := index.Package{RepoID: repo.ID, Name: "Docs/Readme.MD", NameOCI: "docs/readme.md", CreatedAt: at(4)}
	must(t, s.UpsertPackage(ctx, &again))
	same(t, "UpsertPackage of the holder fills it from the row", first, again)

	for _, tt := range []struct {
		name string
		err  error
	}{
		{"GetPackageByNameOCI missing", second(s.GetPackageByNameOCI(ctx, repo.ID, "docs/other.md"))},
		{"GetPackageByNameOCI of a name", second(s.GetPackageByNameOCI(ctx, repo.ID, "Docs/Readme.MD"))},
		{"GetPackageByNameOCI other repo", second(s.GetPackageByNameOCI(ctx, other.ID, onlyHere.NameOCI))},
	} {
		wantErr(t, tt.name, tt.err, index.ErrNotFound)
	}

	must(t, s.DeletePackage(ctx, first.ID))
	twin.ID = 0
	must(t, s.UpsertPackage(ctx, &twin))
	got, err = s.GetPackageByNameOCI(ctx, repo.ID, "docs/readme.md")
	must(t, err)
	same(t, "GetPackageByNameOCI after the holder was deleted", twin, *got)
}

// testPackageNameOCIRace runs writers that upsert packages with one NameOCI at once, four per
// name, for a few rounds: the writers of the name that wins share its row, every other writer
// gets ErrConflict naming the winner, and none a driver error (a unique violation of the other
// index, which Postgres raises when ON CONFLICT names one).
func testPackageNameOCIRace(t *testing.T, s index.Store) {
	ctx := t.Context()
	repo := newRepo(t, s, "raw", "race")
	names := []string{"docs/readme.md", "Docs/Readme.MD", "DOCS/README.MD", "docs/README.md"}
	const rounds, writers = 20, 16
	for round := range rounds {
		dir := strconv.Itoa(round) + "/"
		errs := make([]error, writers)
		pkgs := make([]index.Package, writers)
		var wg sync.WaitGroup
		for i := range writers {
			wg.Go(func() {
				pkgs[i] = index.Package{RepoID: repo.ID, Name: dir + names[i%len(names)], NameOCI: dir + "docs/readme.md"}
				errs[i] = s.UpsertPackage(ctx, &pkgs[i])
			})
		}
		wg.Wait()
		stored, err := s.GetPackageByNameOCI(ctx, repo.ID, dir+"docs/readme.md")
		must(t, err)
		winners := 0
		for i, err := range errs {
			switch {
			case err == nil:
				winners++
				same(t, fmt.Sprintf("round %d, writer %d", round, i), *stored, pkgs[i])
			case !errors.Is(err, index.ErrConflict) || !strings.Contains(err.Error(), strconv.Quote(stored.Name)):
				t.Errorf("round %d, writer %d (%s): %v, want ErrConflict naming %q", round, i, pkgs[i].Name, err, stored.Name)
			}
		}
		if winners != writers/len(names) {
			t.Errorf("round %d: %d writers succeeded, want the %d of one name", round, winners, writers/len(names))
		}
	}
}

// testPackageRace runs concurrent first publishes of one package, as ingest writes them: each
// writer upserts the package and a version of its own in one transaction, for a few rounds.
// Every writer succeeds on the one package row, and every version is there; on YugabyteDB the
// writers deadlocked without the package lock (#629).
func testPackageRace(t *testing.T, s index.Store) {
	ctx := t.Context()
	repo := newRepo(t, s, "raw", "race")
	const rounds, writers = 20, 16
	for round := range rounds {
		name := strconv.Itoa(round) + "/tool"
		errs := make([]error, writers)
		pkgIDs := make([]int64, writers)
		var wg sync.WaitGroup
		for i := range writers {
			wg.Go(func() {
				errs[i] = s.Tx(ctx, func(w index.Writer) error {
					p := index.Package{RepoID: repo.ID, Name: name, NameOCI: name}
					if err := w.UpsertPackage(ctx, &p); err != nil {
						return err
					}
					pkgIDs[i] = p.ID
					tag := strconv.Itoa(i)
					return w.UpsertVersion(ctx, &index.Version{PackageID: p.ID, Version: tag, Tag: tag,
						Digest: dg(name + tag), ArtifactType: "application/vnd.lajstrom.raw.v1"})
				})
			})
		}
		wg.Wait()
		stored, err := s.GetPackage(ctx, repo.ID, name)
		must(t, err)
		for i, err := range errs {
			switch {
			case err != nil:
				t.Errorf("round %d, writer %d: %v, want nil", round, i, err)
			case pkgIDs[i] != stored.ID:
				t.Errorf("round %d, writer %d: package %d, want the stored row %d", round, i, pkgIDs[i], stored.ID)
			}
		}
		versions, err := s.ListVersions(ctx, stored.ID)
		must(t, err)
		if len(versions) != writers {
			t.Errorf("round %d: %d versions, want %d", round, len(versions), writers)
		}
	}
}

func testPackages(t *testing.T, s index.Store) {
	ctx := t.Context()
	repo := newRepo(t, s, "raw", "a")
	other := newRepo(t, s, "raw", "b")
	names := []string{"lib-core", "lib-util", "app", "Lib-upper", "lib_x", "l%b", "ünicode", "ü-dash"}
	pkgs := map[string]index.Package{}
	for i, name := range names {
		p := index.Package{RepoID: repo.ID, Name: name, NameOCI: "oci/" + name, CreatedAt: at(i)}
		must(t, s.UpsertPackage(ctx, &p))
		pkgs[name] = p
	}
	elsewhere := newPackage(t, s, other.ID, "lib-core")

	again := index.Package{RepoID: repo.ID, Name: "lib-core", NameOCI: "different", CreatedAt: at(99)}
	must(t, s.UpsertPackage(ctx, &again))
	same(t, "UpsertPackage of an existing package fills it from the row", pkgs["lib-core"], again)
	got, err := s.GetPackage(ctx, repo.ID, "lib-core")
	must(t, err)
	same(t, "GetPackage", pkgs["lib-core"], *got)
	got, err = s.GetPackage(ctx, other.ID, "lib-core")
	must(t, err)
	same(t, "GetPackage in the other repo", elsewhere, *got)

	for _, tt := range []struct {
		prefix string
		want   []string // byte order
	}{
		{"", []string{"Lib-upper", "app", "l%b", "lib-core", "lib-util", "lib_x", "ü-dash", "ünicode"}},
		{"lib", []string{"lib-core", "lib-util", "lib_x"}},
		{"lib_", []string{"lib_x"}},
		{"l%", []string{"l%b"}},
		{"ü", []string{"ü-dash", "ünicode"}},
		{"lib-core", []string{"lib-core"}},
		{"zzz", []string{}},
	} {
		list, err := s.ListPackages(ctx, repo.ID, tt.prefix)
		must(t, err)
		want := []index.Package{}
		for _, name := range tt.want {
			want = append(want, pkgs[name])
		}
		same(t, fmt.Sprintf("ListPackages(%q)", tt.prefix), want, list)
	}

	must(t, s.DeletePackage(ctx, pkgs["app"].ID))
	for _, tt := range []struct {
		name string
		err  error
		want error
	}{
		{"GetPackage deleted", second(s.GetPackage(ctx, repo.ID, "app")), index.ErrNotFound},
		{"GetPackage other repo", second(s.GetPackage(ctx, other.ID, "lib-util")), index.ErrNotFound},
		{"DeletePackage deleted", s.DeletePackage(ctx, pkgs["app"].ID), index.ErrNotFound},
	} {
		wantErr(t, tt.name, tt.err, tt.want)
	}
}

func testVersions(t *testing.T, s index.Store) {
	ctx := t.Context()
	repo := newRepo(t, s, "raw", "a")
	other := newRepo(t, s, "raw", "b")
	pkg := newPackage(t, s, repo.ID, "tool")
	const artifactType = "application/vnd.lajstrom.raw.v1"
	versions := []index.Version{
		{PackageID: pkg.ID, Version: "1.0.0", Tag: "1.0.0", Digest: dg("a"), ArtifactType: artifactType,
			Attrs: map[string]string{"lajstrom.raw.path": "tool"}, Metadata: json.RawMessage(`{"k": ["v"]}`),
			CreatedAt: at(10), IndexedAt: at(11)},
		{PackageID: pkg.ID, Version: "1.0.0", Variant: "linux-amd64", Tag: "1.0.0-linux-amd64", Digest: dg("b"),
			ArtifactType: artifactType, Attrs: map[string]string{}, CreatedAt: at(5)},
		{PackageID: pkg.ID, Version: "2.0.0", Tag: "2.0.0", Digest: dg("c"), ArtifactType: artifactType, CreatedAt: at(5)},
	}
	for i := range versions {
		must(t, s.UpsertVersion(ctx, &versions[i]))
		if versions[i].ID == 0 || versions[i].IndexedAt.IsZero() {
			t.Fatalf("UpsertVersion did not fill ID and IndexedAt: %+v", versions[i])
		}
	}
	one := func(v *index.Version, err error) index.Version {
		t.Helper()
		must(t, err)
		return *v
	}
	same(t, "GetVersion", versions[0], one(s.GetVersion(ctx, pkg.ID, "1.0.0", "")))
	same(t, "GetVersion with variant", versions[1], one(s.GetVersion(ctx, pkg.ID, "1.0.0", "linux-amd64")))
	same(t, "GetVersionByTag", versions[2], one(s.GetVersionByTag(ctx, pkg.ID, "2.0.0")))
	same(t, "GetVersionByDigest", versions[1], one(s.GetVersionByDigest(ctx, repo.ID, dg("b"))))
	list, err := s.ListVersions(ctx, pkg.ID)
	must(t, err)
	same(t, "ListVersions by CreatedAt, then ID", []index.Version{versions[1], versions[2], versions[0]}, list)

	again := versions[0]
	again.ID, again.Attrs, again.Metadata, again.IndexedAt = 0, map[string]string{"lajstrom.raw.path": "tool2"}, nil, at(20)
	must(t, s.UpsertVersion(ctx, &again))
	if again.ID != versions[0].ID {
		t.Errorf("re-upsert of the same digest: ID %d, want %d", again.ID, versions[0].ID)
	}
	same(t, "version after re-upsert", again, one(s.GetVersionByTag(ctx, pkg.ID, "1.0.0")))

	moved := versions[0]
	moved.ID, moved.Digest = 0, dg("z")
	err = s.UpsertVersion(ctx, &moved)
	var tc *index.TagConflictError
	if !errors.As(err, &tc) || tc.Tag != "1.0.0" || tc.Existing != dg("a") || !errors.Is(err, index.ErrConflict) {
		t.Errorf("UpsertVersion of a tag with another digest: %v, want a TagConflictError naming %s", err, dg("a"))
	}
	same(t, "version after the tag conflict", again, one(s.GetVersionByTag(ctx, pkg.ID, "1.0.0")))

	retagged := index.Version{PackageID: pkg.ID, Version: "2.0.0", Tag: "2.0.0-other", Digest: dg("d"), ArtifactType: artifactType}
	err = s.UpsertVersion(ctx, &retagged)
	if !errors.Is(err, index.ErrConflict) || errors.As(err, &tc) {
		t.Errorf("UpsertVersion of a version under another tag: %v, want ErrConflict and no TagConflictError", err)
	}

	must(t, s.DeleteVersion(ctx, versions[2].ID))
	for _, tt := range []struct {
		name string
		err  error
		want error
	}{
		{"GetVersion missing", second(s.GetVersion(ctx, pkg.ID, "9.9.9", "")), index.ErrNotFound},
		{"GetVersion deleted", second(s.GetVersion(ctx, pkg.ID, "2.0.0", "")), index.ErrNotFound},
		{"GetVersionByTag missing", second(s.GetVersionByTag(ctx, pkg.ID, "nope")), index.ErrNotFound},
		{"GetVersionByDigest missing", second(s.GetVersionByDigest(ctx, repo.ID, dg("nope"))), index.ErrNotFound},
		{"GetVersionByDigest other repo", second(s.GetVersionByDigest(ctx, other.ID, dg("a"))), index.ErrNotFound},
		{"DeleteVersion deleted", s.DeleteVersion(ctx, versions[2].ID), index.ErrNotFound},
	} {
		wantErr(t, tt.name, tt.err, tt.want)
	}
}

// testVersionTagRace runs writers that upsert one tag at once: with different digests exactly
// one wins and the others get the TagConflictError; with the same digest all succeed on one
// row.
func testVersionTagRace(t *testing.T, s index.Store) {
	ctx := t.Context()
	repo := newRepo(t, s, "raw", "race")
	pkg := newPackage(t, s, repo.ID, "tool")
	const writers = 8
	for _, tt := range []struct {
		tag        string
		digest     func(i int) string
		wantWinner int // writers that get nil
	}{
		{"1.0.0", strconv.Itoa, 1},
		{"2.0.0", func(int) string { return "same" }, writers},
	} {
		errs := make([]error, writers)
		rowIDs := make([]int64, writers)
		var wg sync.WaitGroup
		for i := range writers {
			wg.Go(func() {
				v := index.Version{PackageID: pkg.ID, Version: tt.tag, Tag: tt.tag, Digest: dg(tt.digest(i)),
					ArtifactType: "application/vnd.lajstrom.raw.v1"}
				errs[i] = s.UpsertVersion(ctx, &v)
				rowIDs[i] = v.ID
			})
		}
		wg.Wait()
		stored, err := s.GetVersionByTag(ctx, pkg.ID, tt.tag)
		must(t, err)
		winners := 0
		for i, err := range errs {
			var tc *index.TagConflictError
			switch {
			case err == nil:
				winners++
				if rowIDs[i] != stored.ID {
					t.Errorf("tag %s, writer %d: ID %d, want the stored row %d", tt.tag, i, rowIDs[i], stored.ID)
				}
			case !errors.As(err, &tc) || tc.Existing != stored.Digest:
				t.Errorf("tag %s, writer %d: %v, want a TagConflictError naming %s", tt.tag, i, err, stored.Digest)
			}
		}
		if winners != tt.wantWinner {
			t.Errorf("tag %s: %d writers succeeded, want %d", tt.tag, winners, tt.wantWinner)
		}
	}
}

func testFiles(t *testing.T, s index.Store) {
	ctx := t.Context()
	repo := newRepo(t, s, "apt", "main")
	other := newRepo(t, s, "apt", "other")
	pkgA, pkgB := newPackage(t, s, repo.ID, "a"), newPackage(t, s, repo.ID, "b")
	verA, verB := newVersion(t, s, pkgA.ID, "1.0"), newVersion(t, s, pkgB.ID, "2.0")
	pkgA.CreatedAt, pkgB.CreatedAt = verA.CreatedAt, verB.CreatedAt // the earliest version's
	filesA := []index.File{
		{Name: "z.deb", MediaType: "application/vnd.debian.binary-package", Digest: dg("z"), Size: 30,
			Attrs: map[string]string{"org.opencontainers.image.title": "z.deb"}},
		{Name: "common.txt", MediaType: "text/plain", Digest: dg("ca"), Size: 4},
	}
	filesB := []index.File{{Name: "common.txt", MediaType: "text/plain", Digest: dg("cb"), Size: 5, Attrs: map[string]string{}}}
	must(t, s.ReplaceFiles(ctx, verA.ID, filesA))
	must(t, s.ReplaceFiles(ctx, verB.ID, filesB))
	for _, f := range append(filesA, filesB...) {
		if f.ID == 0 || f.VersionID == 0 {
			t.Fatalf("ReplaceFiles did not fill ID and VersionID: %+v", f)
		}
	}
	listed, err := s.ListFiles(ctx, verA.ID)
	must(t, err)
	same(t, "ListFiles in manifest order", filesA, listed)

	found, err := s.FindFiles(ctx, repo.ID, "common.txt")
	must(t, err)
	same(t, "FindFiles across packages", []index.FileMatch{
		{File: filesA[1], Version: verA, Package: pkgA},
		{File: filesB[0], Version: verB, Package: pkgB},
	}, found)
	found, err = s.FindFiles(ctx, other.ID, "common.txt")
	must(t, err)
	same(t, "FindFiles in another repo", []index.FileMatch{}, found)

	replaced := []index.File{{Name: "new.deb", MediaType: "application/vnd.debian.binary-package", Digest: dg("n"), Size: 1}}
	must(t, s.ReplaceFiles(ctx, verA.ID, replaced))
	listed, err = s.ListFiles(ctx, verA.ID)
	must(t, err)
	same(t, "ListFiles after ReplaceFiles", replaced, listed)
	found, err = s.FindFiles(ctx, repo.ID, "common.txt")
	must(t, err)
	same(t, "FindFiles after ReplaceFiles", []index.FileMatch{{File: filesB[0], Version: verB, Package: pkgB}}, found)

	must(t, s.ReplaceFiles(ctx, verA.ID, nil))
	listed, err = s.ListFiles(ctx, verA.ID)
	must(t, err)
	same(t, "ListFiles after ReplaceFiles(nil)", []index.File{}, listed)
	wantErr(t, "ReplaceFiles of a missing version", s.ReplaceFiles(ctx, 999999, replaced), index.ErrNotFound)
}

func testIndexCache(t *testing.T, s index.Store) {
	ctx := t.Context()
	repo := newRepo(t, s, "apt", "main")
	other := newRepo(t, s, "pypi", "main")
	docs := []index.Cache{
		{RepoID: repo.ID, Scope: "jammy/main/amd64", Kind: "Packages", Content: []byte("Package: a\n"), SHA256: "s1", RenderedAt: at(1)},
		{RepoID: repo.ID, Scope: "jammy/main/amd64", Kind: "InRelease", Content: []byte("signed"), SHA256: "s2",
			RenderedAt: at(2), SignedAt: ptr(at(3))},
		{RepoID: repo.ID, Scope: "jammy", Kind: "Release", Content: []byte("Suite: jammy\n"), SHA256: "s3"},
		{RepoID: other.ID, Scope: "jammy/main/amd64", Kind: "Packages", Content: []byte("other"), SHA256: "s4", RenderedAt: at(4)},
	}
	for i := range docs {
		if ok, err := s.PutIndexCache(ctx, &docs[i], 0); err != nil || !ok {
			t.Fatalf("PutIndexCache %s at generation 0: %t, %v", docs[i].Kind, ok, err)
		}
	}
	get := func(c index.Cache) (index.Cache, error) {
		got, err := s.GetIndexCache(ctx, c.RepoID, c.Scope, c.Kind)
		if err != nil {
			return index.Cache{}, err
		}
		return *got, nil
	}
	for _, want := range docs {
		got, err := get(want)
		must(t, err)
		same(t, "GetIndexCache "+want.Kind, want, got)
	}

	replaced := docs[0]
	replaced.ID, replaced.Content, replaced.SHA256, replaced.RenderedAt = 0, []byte("Package: b\n"), "s5", at(5)
	must(t, second(s.PutIndexCache(ctx, &replaced, 0)))
	if replaced.ID != docs[0].ID {
		t.Errorf("PutIndexCache of an existing key: ID %d, want %d", replaced.ID, docs[0].ID)
	}
	got, err := get(docs[0])
	must(t, err)
	same(t, "document after PutIndexCache replaced it", replaced, got)

	present := func(want ...bool) []bool { return want }
	check := func(what string, want []bool) {
		t.Helper()
		var have []bool
		for _, d := range docs {
			_, err := get(d)
			if err != nil && !errors.Is(err, index.ErrNotFound) {
				t.Fatal(err)
			}
			have = append(have, err == nil)
		}
		same(t, what, want, have)
	}
	must(t, s.InvalidateIndexCache(ctx, repo.ID, "jammy/main/amd64"))
	check("after invalidating a scope", present(false, false, true, true))
	must(t, s.InvalidateIndexCache(ctx, repo.ID, ""))
	check("after invalidating the repo", present(false, false, false, true))

	// Every invalidation moves the repo's generation, and a put of another generation stores
	// nothing.
	generation := func(repoID int64) int64 {
		t.Helper()
		gen, err := s.IndexCacheGeneration(ctx, repoID)
		must(t, err)
		return gen
	}
	same(t, "generations after two invalidations of one repo", []int64{2, 0}, []int64{generation(repo.ID), generation(other.ID)})
	for _, gen := range []int64{0, 1, 3} {
		stored, err := s.PutIndexCache(ctx, &docs[0], gen)
		must(t, err)
		if stored {
			t.Errorf("PutIndexCache at generation %d of a repo at 2 stored the document", gen)
		}
	}
	check("after puts of other generations", present(false, false, false, true))
	if stored, err := s.PutIndexCache(ctx, &docs[0], 2); err != nil || !stored {
		t.Errorf("PutIndexCache at the current generation: %t, %v", stored, err)
	}
	check("after a put of the current generation", present(true, false, false, true))
	must(t, s.ResetDerived(ctx, repo.ID))
	same(t, "generations after ResetDerived of one repo", []int64{3, 0}, []int64{generation(repo.ID), generation(other.ID)})
	must(t, s.ResetDerived(ctx, 0))
	same(t, "generations after ResetDerived of every repo", []int64{4, 1}, []int64{generation(repo.ID), generation(other.ID)})
}

// testIndexCacheRace checks a render against an invalidation that comes while it runs (INV-27).
// The render is a transaction like format.CachedDocument's: the generation, a read of the
// versions it renders, the store of the document. The invalidation is a transaction like a
// publish's: a version and InvalidateIndexCache. It comes after the render's read, committed
// before the store, or not yet committed when the render stores (it holds its transaction a
// while after the invalidation). Either way no document without the new version is left in
// index_cache. On SQLite every transaction holds the write lock from its start, so the
// invalidation waits for the render, which goes on after a short wait; on Postgres (read
// committed) the two interleave.
func testIndexCacheRace(t *testing.T, s index.Store) {
	ctx := t.Context()
	repo := newRepo(t, s, "raw", "race")
	pkg := newPackage(t, s, repo.ID, "pkg")
	newVersion(t, s, pkg.ID, "1")
	const wait = 200 * time.Millisecond // what the render waits for the invalidation
	for i, committed := range []bool{true, false} {
		version, when := strconv.Itoa(i+2), map[bool]string{true: "committed before", false: "not committed at"}[committed]
		read, invalidated := make(chan struct{}), make(chan struct{})
		done := make(chan error, 1)
		go func() {
			<-read
			done <- s.Tx(ctx, func(w index.Writer) error {
				v := index.Version{PackageID: pkg.ID, Version: version, Tag: version, Digest: dg(version), ArtifactType: "a"}
				if err := w.UpsertVersion(ctx, &v); err != nil {
					return err
				}
				if err := w.InvalidateIndexCache(ctx, repo.ID, ""); err != nil {
					return err
				}
				if !committed {
					close(invalidated)
					time.Sleep(wait / 2)
				}
				return nil
			})
			if committed {
				close(invalidated)
			}
		}()
		err := s.Tx(ctx, func(w index.Writer) error {
			gen, err := w.IndexCacheGeneration(ctx, repo.ID)
			if err != nil {
				return err
			}
			versions, err := w.ListVersions(ctx, pkg.ID)
			if err != nil {
				return err
			}
			close(read)
			select {
			case <-invalidated:
			case <-time.After(wait):
			}
			var doc []string
			for _, v := range versions {
				doc = append(doc, v.Version)
			}
			c := index.Cache{RepoID: repo.ID, Scope: "", Kind: "listing", Content: []byte(strings.Join(doc, ",")), SHA256: "s"}
			_, err = w.PutIndexCache(ctx, &c, gen)
			return err
		})
		must(t, err)
		must(t, <-done)
		c, err := s.GetIndexCache(ctx, repo.ID, "", "listing")
		switch {
		case errors.Is(err, index.ErrNotFound):
		case err != nil:
			t.Fatal(err)
		case !strings.Contains(string(c.Content), version):
			t.Errorf("an invalidation %s the store: index_cache keeps %q, rendered before version %s", when, c.Content, version)
		}
	}
}

func testOCIRepos(t *testing.T, s index.Store) {
	ctx := t.Context()
	repo := newRepo(t, s, "raw", "a")
	other := newRepo(t, s, "raw", "b")
	oci := []index.OCIRepo{
		{RepoID: repo.ID, Path: "raw/a/z", SeenAt: at(1)},
		{RepoID: repo.ID, Path: "raw/a/b", SeenAt: at(2)},
		{RepoID: repo.ID, Path: "raw/a/m", SeenAt: at(3)},
		{RepoID: other.ID, Path: "raw/b/x", SeenAt: at(1)},
	}
	for i := range oci {
		must(t, s.UpsertOCIRepo(ctx, &oci[i]))
	}
	seen := index.OCIRepo{RepoID: repo.ID, Path: "raw/a/z", SeenAt: at(10)}
	must(t, s.UpsertOCIRepo(ctx, &seen))
	if seen.ID != oci[0].ID {
		t.Errorf("UpsertOCIRepo of an existing path: ID %d, want %d", seen.ID, oci[0].ID)
	}
	list, err := s.ListOCIRepos(ctx, repo.ID)
	must(t, err)
	same(t, "ListOCIRepos by path", []index.OCIRepo{oci[1], oci[2], seen}, list)

	n, err := s.PruneOCIRepos(ctx, repo.ID, at(3))
	must(t, err)
	if n != 1 {
		t.Errorf("PruneOCIRepos = %d, want 1", n)
	}
	list, err = s.ListOCIRepos(ctx, repo.ID)
	must(t, err)
	same(t, "ListOCIRepos after PruneOCIRepos", []index.OCIRepo{oci[2], seen}, list)
	list, err = s.ListOCIRepos(ctx, other.ID)
	must(t, err)
	same(t, "ListOCIRepos of the other repo", []index.OCIRepo{oci[3]}, list)

	// DeleteOCIRepo removes the one row of its repo and path: the other repo's row of the same
	// path stays.
	twin := index.OCIRepo{RepoID: other.ID, Path: oci[2].Path, SeenAt: at(4)}
	must(t, s.UpsertOCIRepo(ctx, &twin))
	must(t, s.DeleteOCIRepo(ctx, repo.ID, oci[2].Path))
	list, err = s.ListOCIRepos(ctx, repo.ID)
	must(t, err)
	same(t, "ListOCIRepos after DeleteOCIRepo", []index.OCIRepo{seen}, list)
	list, err = s.ListOCIRepos(ctx, other.ID)
	must(t, err)
	same(t, "ListOCIRepos of the other repo after DeleteOCIRepo", []index.OCIRepo{twin, oci[3]}, list)
	wantErr(t, "DeleteOCIRepo of a path without a row", s.DeleteOCIRepo(ctx, repo.ID, oci[2].Path), index.ErrNotFound)
	wantErr(t, "DeleteOCIRepo in a missing repo", s.DeleteOCIRepo(ctx, 999999, seen.Path), index.ErrNotFound)
	// The upsert after it, a push's, records the repository again.
	again := index.OCIRepo{RepoID: repo.ID, Path: oci[2].Path, SeenAt: at(5)}
	must(t, s.UpsertOCIRepo(ctx, &again))
	list, err = s.ListOCIRepos(ctx, repo.ID)
	must(t, err)
	same(t, "ListOCIRepos after an upsert of the deleted path", []index.OCIRepo{again, seen}, list)
}

// testMissingParent checks that a row referring to a missing row is index.ErrNotFound.
func testMissingParent(t *testing.T, s index.Store) {
	ctx := t.Context()
	const missing = int64(999999)
	for _, tt := range []struct {
		name string
		err  error
	}{
		{"UpsertPackage", s.UpsertPackage(ctx, &index.Package{RepoID: missing, Name: "p"})},
		{"UpsertVersion", s.UpsertVersion(ctx, &index.Version{PackageID: missing, Version: "1", Tag: "1", Digest: dg("1")})},
		{"CreateToken", s.CreateToken(ctx, &index.Token{Kind: "pat", UserID: ptr(missing), Hash: "h"})},
		{"CreateKey", s.CreateKey(ctx, &index.Key{RepoID: ptr(missing), Kind: "gpg"})},
		{"UpsertSession", s.UpsertSession(ctx, &index.Session{RepoID: missing, Key: "k", State: "open"})},
		{"PutIndexCache", second(s.PutIndexCache(ctx, &index.Cache{RepoID: missing, Scope: "s", Kind: "k"}, 0))},
		{"UpsertOCIRepo", s.UpsertOCIRepo(ctx, &index.OCIRepo{RepoID: missing, Path: "p"})},
	} {
		wantErr(t, tt.name, tt.err, index.ErrNotFound)
	}
}

// testVersionOCIRepo checks versions.oci_repo (#642): a version that reindex found at another
// form of its package's OCI name keeps that form in OCIRepo through every read of a version,
// ManifestPath says where its manifest is, an upsert of the same digest writes OCIRepo as given
// (ingest's, "", clears it), and PackageOf finds the package from either form.
func testVersionOCIRepo(t *testing.T, s index.Store) {
	ctx := t.Context()
	repo := newRepo(t, s, "npm", "a")
	pkg := index.Package{RepoID: repo.ID, Name: "@netlify/blobs", NameOCI: "netlify/blob_73"}
	must(t, s.UpsertPackage(ctx, &pkg))
	const artifactType = "application/vnd.lajstrom.npm.v1"
	split := index.Version{PackageID: pkg.ID, Version: "1.0.0", Tag: "1.0.0", Digest: dg("one"), ArtifactType: artifactType,
		CreatedAt: at(1), IndexedAt: at(2), OCIRepo: "netlify/blobs"}
	here := index.Version{PackageID: pkg.ID, Version: "2.0.0", Tag: "2.0.0", Digest: dg("two"), ArtifactType: artifactType,
		CreatedAt: at(3), IndexedAt: at(4)}
	must(t, s.UpsertVersion(ctx, &split))
	must(t, s.UpsertVersion(ctx, &here))
	files := []index.File{{Name: "blobs-1.0.0.tgz", MediaType: "application/gzip", Digest: dg("tgz"), Size: 3}}
	must(t, s.ReplaceFiles(ctx, split.ID, files))
	stored, err := s.GetPackage(ctx, repo.ID, pkg.Name) // CreatedAt: its earliest version's
	must(t, err)
	pkg = *stored

	one := func(v *index.Version, err error) index.Version {
		t.Helper()
		must(t, err)
		return *v
	}
	same(t, "GetVersion", split, one(s.GetVersion(ctx, pkg.ID, "1.0.0", "")))
	same(t, "GetVersionByTag", split, one(s.GetVersionByTag(ctx, pkg.ID, "1.0.0")))
	same(t, "GetVersionByDigest", split, one(s.GetVersionByDigest(ctx, repo.ID, dg("one"))))
	list, err := s.ListVersions(ctx, pkg.ID)
	must(t, err)
	same(t, "ListVersions", []index.Version{split, here}, list)
	page, err := s.ListVersionPage(ctx, pkg.ID, nil, 10)
	must(t, err)
	same(t, "ListVersionPage", []index.Version{here, split}, page)
	unseen, err := s.ListUnseenVersions(ctx, repo.ID, at(10))
	must(t, err)
	same(t, "ListUnseenVersions", []index.VersionMatch{{Version: split, Package: pkg}, {Version: here, Package: pkg}}, unseen)
	found, err := s.FindFiles(ctx, repo.ID, "blobs-1.0.0.tgz")
	must(t, err)
	if len(found) != 1 || found[0].Version.OCIRepo != "netlify/blobs" {
		t.Errorf("FindFiles = %+v, want the file of the version at netlify/blobs", found)
	}
	for _, tc := range []struct {
		v    index.Version
		want string
	}{{split, "netlify/blobs"}, {here, "netlify/blob_73"}} {
		if got := index.ManifestPath(pkg, tc.v); got != tc.want {
			t.Errorf("ManifestPath of %s = %q, want %q", tc.v.Version, got, tc.want)
		}
	}

	again := split
	again.ID, again.OCIRepo, again.IndexedAt = 0, "", at(5)
	must(t, s.UpsertVersion(ctx, &again))
	same(t, "the version upserted again without OCIRepo", again, one(s.GetVersionByTag(ctx, pkg.ID, "1.0.0")))
	again.ID, again.OCIRepo, again.IndexedAt = 0, "netlify/blobs", at(6)
	must(t, s.UpsertVersion(ctx, &again))
	same(t, "the version upserted again with OCIRepo", again, one(s.GetVersionByTag(ctx, pkg.ID, "1.0.0")))

	for _, path := range []string{"netlify/blob_73", "netlify/blobs"} {
		got, err := index.PackageOf(ctx, s, repo.ID, path)
		must(t, err)
		if got.ID != pkg.ID {
			t.Errorf("PackageOf %s = %+v, want %+v", path, got, pkg)
		}
	}
	_, err = index.PackageOf(ctx, s, repo.ID, "netlify/other")
	wantErr(t, "PackageOf of a path without a package", err, index.ErrNotFound)
}
