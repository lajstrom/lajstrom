// Package storetest is the conformance suite of index.Store (docs/spec/layout.md §4): every
// implementation runs Run in its tests, SQLite always and Postgres when
// LAJSTROM_TEST_POSTGRES_DSN is set. It is imported only by tests.
package storetest

import (
	"encoding/json"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/opencontainers/go-digest"

	"github.com/lajstrom/lajstrom/internal/index"
)

// Run runs the conformance suite against the stores open returns. open returns a fresh,
// migrated, empty store for each subtest and registers its Close with t.Cleanup; subtests run
// in parallel, each on its own store.
func Run(t *testing.T, open func(t *testing.T) index.Store) {
	tests := []struct {
		name string
		run  func(t *testing.T, s index.Store)
	}{
		{"Migrate", testMigrate},
		{"Timestamps", testTimestamps},
		{"Repos", testRepos},
		{"Users", testUsers},
		{"Tokens", testTokens},
		{"Bindings", testBindings},
		{"AuthVersion", testAuthVersion},
		{"Keys", testKeys},
		{"KeysActive", testKeysActive},
		{"KeyRace", testKeyRace},
		{"UsersDeleted", testUsersDeleted},
		{"AuditPrincipalName", testAuditPrincipalName},
		{"Sessions", testSessions},
		{"SessionsTouch", testSessionsTouch},
		{"SessionsConcurrent", testSessionsConcurrent},
		{"Audit", testAudit},
		{"AuditDefaultLimit", testAuditDefaultLimit},
		{"TagClaims", testTagClaims},
		{"TagClaimRace", testTagClaimRace},
		{"TagClaimsLifecycle", testTagClaimsLifecycle},
		{"ReferrersLock", testReferrersLock},
		{"Packages", testPackages},
		{"PackageByNameOCI", testPackageByNameOCI},
		{"PackageNameOCIRace", testPackageNameOCIRace},
		{"Versions", testVersions},
		{"VersionTagRace", testVersionTagRace},
		{"Files", testFiles},
		{"IndexCache", testIndexCache},
		{"OCIRepos", testOCIRepos},
		{"MissingParent", testMissingParent},
		{"Tx", testTx},
		{"Cascades", testCascades},
		{"ResetDerived", testResetDerived},
		{"PackageCreatedAt", testPackageCreatedAt},
		{"PackageCreatedAtRace", testPackageCreatedAtRace},
		{"Degraded", testDegraded},
		{"Sweep", testSweep},
		{"ReindexRuns", testReindexRuns},
		{"ReindexRunsKept", testReindexRunsKept},
		{"ReindexRunRace", testReindexRunRace},
		{"ProxyMeta", testProxyMeta},
		{"Groups", testGroups},
		{"FetchClaims", testFetchClaims},
		{"FetchClaimRace", testFetchClaimRace},
		{"Journal", testJournal},
		{"JournalState", testJournalState},
		{"JournalCascades", testJournalCascades},
		{"JournalRace", testJournalRace},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			tt.run(t, open(t))
		})
	}
}

// t0 is the base of the timestamps of the fixtures.
var t0 = time.Date(2026, 11, 6, 10, 0, 0, 0, time.UTC)

// at returns t0 plus sec seconds.
func at(sec int) time.Time { return t0.Add(time.Duration(sec) * time.Second) }

// ptr returns a pointer to v.
func ptr[T any](v T) *T { return &v }

// dg returns the sha256 digest of s.
func dg(s string) digest.Digest { return digest.FromString(s) }

// passwordHash returns a fake argon2id hash for a user, built at run time so that gosec does
// not take a string literal for a credential.
func passwordHash(name string) string { return "$argon2id$v=19$" + name }

// opts compare json.RawMessage by value: Postgres jsonb does not keep the spelling. The filter
// keeps the comparer off []byte fields, which are assignable to json.RawMessage.
var opts = cmp.Options{
	cmp.FilterPath(func(p cmp.Path) bool { return p.Last().Type() == reflect.TypeFor[json.RawMessage]() },
		cmp.Comparer(func(a, b json.RawMessage) bool {
			if a == nil || b == nil {
				return a == nil && b == nil
			}
			var x, y any
			return json.Unmarshal(a, &x) == nil && json.Unmarshal(b, &y) == nil && cmp.Equal(x, y)
		})),
}

// same fails the test when got differs from want.
func same(t *testing.T, what string, want, got any) {
	t.Helper()
	if d := cmp.Diff(want, got, opts); d != "" {
		t.Errorf("%s: (-want +got)\n%s", what, d)
	}
}

// must stops the test on an error.
func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// wantErr fails the test unless err wraps target.
func wantErr(t *testing.T, what string, err, target error) {
	t.Helper()
	if !errors.Is(err, target) {
		t.Errorf("%s: error = %v, want %v", what, err, target)
	}
}

// ids returns the IDs of a list of models.
func ids[T any](items []T, id func(T) int64) []int64 {
	out := []int64{}
	for _, it := range items {
		out = append(out, id(it))
	}
	return out
}

func newRepo(t *testing.T, s index.Writer, format, name string) index.Repo {
	t.Helper()
	r := index.Repo{Format: format, Name: name, Registry: "local", Prefix: format + "/" + name, Kind: "hosted"}
	must(t, s.CreateRepo(t.Context(), &r))
	return r
}

func newUser(t *testing.T, s index.Writer, name string) index.User {
	t.Helper()
	u := index.User{Name: name, PasswordHash: passwordHash(name), Kind: "local"}
	must(t, s.CreateUser(t.Context(), &u))
	return u
}

func newPackage(t *testing.T, s index.Writer, repoID int64, name string) index.Package {
	t.Helper()
	p := index.Package{RepoID: repoID, Name: name, NameOCI: name}
	must(t, s.UpsertPackage(t.Context(), &p))
	return p
}

func newVersion(t *testing.T, s index.Writer, packageID int64, version string) index.Version {
	t.Helper()
	v := index.Version{PackageID: packageID, Version: version, Tag: version, Digest: dg(version),
		ArtifactType: "application/vnd.lajstrom.raw.v1", Attrs: map[string]string{"lajstrom.raw.path": version}}
	must(t, s.UpsertVersion(t.Context(), &v))
	return v
}

// fixture is one repo with a row in every table that refers to it.
type fixture struct {
	repo    index.Repo
	token   index.Token // deploy token
	key     index.Key
	session index.Session
	pkg     index.Package
	version index.Version
	files   []index.File
	cache   index.Cache
	ociRepo index.OCIRepo
}

// populate creates a repo and a row in every table that refers to it.
func populate(t *testing.T, s index.Writer, name string) fixture {
	t.Helper()
	ctx := t.Context()
	f := fixture{repo: newRepo(t, s, "raw", name)}
	f.token = index.Token{Kind: "deploy", RepoID: &f.repo.ID, Name: "ci", Hash: "hash-" + name, Scopes: []string{"read"}}
	must(t, s.CreateToken(ctx, &f.token))
	f.key = index.Key{RepoID: &f.repo.ID, Kind: "gpg", Public: []byte("pub"), PrivateEnc: []byte("enc"), Fingerprint: "fp-" + name}
	must(t, s.CreateKey(ctx, &f.key))
	f.session = index.Session{RepoID: f.repo.ID, Key: "g:a:1", Principal: "user:alice", State: "open", ExpiresAt: at(3600)}
	must(t, s.UpsertSession(ctx, &f.session))
	f.pkg = newPackage(t, s, f.repo.ID, "pkg")
	f.version = newVersion(t, s, f.pkg.ID, "1.0.0")
	f.files = []index.File{{Name: name + ".bin", MediaType: "application/octet-stream", Digest: dg(name), Size: 1}}
	must(t, s.ReplaceFiles(ctx, f.version.ID, f.files))
	f.cache = index.Cache{RepoID: f.repo.ID, Scope: "simple/pkg", Kind: "simple", Content: []byte("<html>"), SHA256: "s"}
	must(t, s.PutIndexCache(ctx, &f.cache))
	f.ociRepo = index.OCIRepo{RepoID: f.repo.ID, Path: "raw/" + name + "/pkg"}
	must(t, s.UpsertOCIRepo(ctx, &f.ociRepo))
	return f
}

// derived returns the derived rows of a fixture as the store has them now.
func derived(t *testing.T, s index.Reader, f fixture) map[string]any {
	t.Helper()
	ctx := t.Context()
	pkgs, err := s.ListPackages(ctx, f.repo.ID, "")
	must(t, err)
	versions, err := s.ListVersions(ctx, f.pkg.ID)
	must(t, err)
	files, err := s.ListFiles(ctx, f.version.ID)
	must(t, err)
	oci, err := s.ListOCIRepos(ctx, f.repo.ID)
	must(t, err)
	_, cacheErr := s.GetIndexCache(ctx, f.repo.ID, f.cache.Scope, f.cache.Kind)
	if cacheErr != nil && !errors.Is(cacheErr, index.ErrNotFound) {
		t.Fatal(cacheErr)
	}
	return map[string]any{"packages": len(pkgs), "versions": len(versions), "files": len(files),
		"oci_repos": len(oci), "index_cache": map[bool]int{true: 1, false: 0}[cacheErr == nil]}
}

// derivedRows is what derived returns for a populated fixture, or for an emptied one.
func derivedRows(n int) map[string]any {
	return map[string]any{"packages": n, "versions": n, "files": n, "oci_repos": n, "index_cache": n}
}
