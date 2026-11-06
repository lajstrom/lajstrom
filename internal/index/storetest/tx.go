package storetest

import (
	"errors"
	"testing"

	"github.com/lajstrom/lajstrom/internal/index"
)

func testTx(t *testing.T, s index.Store) {
	ctx := t.Context()
	existing := newRepo(t, s, "raw", "existing")
	errBoom := errors.New("boom")
	for _, tt := range []struct {
		name    string
		fn      func(t *testing.T, w index.Writer) error
		wantErr error // nil: committed
	}{
		{"commit", func(t *testing.T, w index.Writer) error {
			r := newRepo(t, w, "raw", "commit")
			got, err := w.GetRepoByID(ctx, r.ID) // the writer sees its own writes
			if err != nil {
				return err
			}
			newPackage(t, w, got.ID, "pkg")
			return nil
		}, nil},
		{"rollback", func(t *testing.T, w index.Writer) error {
			r := newRepo(t, w, "raw", "rollback")
			newPackage(t, w, r.ID, "pkg")
			return errBoom
		}, errBoom},
		{"rollback after a conflict", func(t *testing.T, w index.Writer) error {
			newRepo(t, w, "raw", "rollback after a conflict")
			return w.CreateRepo(ctx, &index.Repo{Format: existing.Format, Name: existing.Name, Kind: "hosted"})
		}, index.ErrConflict},
	} {
		t.Run(tt.name, func(t *testing.T) {
			err := s.Tx(ctx, func(w index.Writer) error { return tt.fn(t, w) })
			if !errors.Is(err, tt.wantErr) || (tt.wantErr == nil) != (err == nil) {
				t.Fatalf("Tx: %v, want %v", err, tt.wantErr)
			}
			r, err := s.GetRepo(ctx, "raw", tt.name)
			switch {
			case tt.wantErr == nil && err != nil:
				t.Errorf("after commit: %v", err)
			case tt.wantErr == nil:
				if _, err := s.GetPackage(ctx, r.ID, "pkg"); err != nil {
					t.Errorf("after commit: %v", err)
				}
			case !errors.Is(err, index.ErrNotFound):
				t.Errorf("after rollback: GetRepo = %v, want ErrNotFound", err)
			}
		})
	}
}

// testCascades checks that deleting a repo, a package or a version removes the rows that refer
// to it, and nothing else.
func testCascades(t *testing.T, s index.Store) {
	ctx := t.Context()
	a, b := populate(t, s, "a"), populate(t, s, "b")
	alice := newUser(t, s, "alice")
	pat := index.Token{Kind: "pat", UserID: &alice.ID, Name: "laptop", Hash: "pat"}
	must(t, s.CreateToken(ctx, &pat))

	must(t, s.DeleteRepo(ctx, a.repo.ID))
	same(t, "derived rows of the deleted repo", derivedRows(0), derived(t, s, a))
	same(t, "derived rows of the other repo", derivedRows(1), derived(t, s, b))
	for _, tt := range []struct {
		name string
		err  error
	}{
		{"deploy token", second(s.GetTokenByHash(ctx, a.token.Hash))},
		{"session", second(s.GetSessionByID(ctx, a.session.ID))},
		{"key", second(s.GetKey(ctx, &a.repo.ID, a.key.Kind))},
	} {
		wantErr(t, tt.name+" of the deleted repo", tt.err, index.ErrNotFound)
	}
	for _, tt := range []struct {
		name string
		err  error
	}{
		{"deploy token of the other repo", second(s.GetTokenByHash(ctx, b.token.Hash))},
		{"session of the other repo", second(s.GetSessionByID(ctx, b.session.ID))},
		{"key of the other repo", second(s.GetKey(ctx, &b.repo.ID, b.key.Kind))},
		{"PAT", second(s.GetTokenByHash(ctx, pat.Hash))},
	} {
		if tt.err != nil {
			t.Errorf("%s after DeleteRepo: %v", tt.name, tt.err)
		}
	}

	must(t, s.DeleteVersion(ctx, b.version.ID))
	rows := derived(t, s, b)
	same(t, "derived rows after DeleteVersion", map[string]any{"packages": 1, "versions": 0, "files": 0,
		"oci_repos": 1, "index_cache": 1}, rows)
	must(t, s.DeletePackage(ctx, b.pkg.ID))
	pkgs, err := s.ListPackages(ctx, b.repo.ID, "")
	must(t, err)
	same(t, "packages after DeletePackage", []index.Package{}, pkgs)

	c := populate(t, s, "c")
	must(t, s.DeletePackage(ctx, c.pkg.ID))
	same(t, "derived rows after DeletePackage", map[string]any{"packages": 0, "versions": 0, "files": 0,
		"oci_repos": 1, "index_cache": 1}, derived(t, s, c))
}

// controlPlane is every control-plane row a test can read through the Reader.
type controlPlane struct {
	Repos      []index.Repo
	Users      []index.User
	Tokens     []index.Token
	Bindings   []index.Binding
	ServerKeys []index.Key
	RepoKeys   [][]index.Key
	Sessions   []index.Session
	Audit      []index.AuditEvent
}

func readControlPlane(t *testing.T, s index.Reader, fixtures ...fixture) controlPlane {
	t.Helper()
	ctx := t.Context()
	var cp controlPlane
	var err error
	cp.Repos, err = s.ListRepos(ctx, "")
	must(t, err)
	cp.Users, err = s.ListUsers(ctx)
	must(t, err)
	cp.Tokens, err = s.ListTokens(ctx, nil, nil)
	must(t, err)
	cp.Bindings, err = s.ListBindings(ctx, "")
	must(t, err)
	cp.ServerKeys, err = s.ListKeys(ctx, nil)
	must(t, err)
	cp.Audit, err = s.ListAudit(ctx, index.AuditFilter{})
	must(t, err)
	for _, f := range fixtures {
		keys, err := s.ListKeys(ctx, &f.repo.ID)
		must(t, err)
		cp.RepoKeys = append(cp.RepoKeys, keys)
		sess, err := s.GetSessionByID(ctx, f.session.ID)
		must(t, err)
		cp.Sessions = append(cp.Sessions, *sess)
	}
	return cp
}

// testResetDerived checks that ResetDerived of one repo and of all repos removes derived rows
// only.
func testResetDerived(t *testing.T, s index.Store) {
	ctx := t.Context()
	a, b := populate(t, s, "a"), populate(t, s, "b")
	newUser(t, s, "alice")
	must(t, s.CreateBinding(ctx, &index.Binding{Principal: "user:alice", Role: "admin", RepoMask: "*/*"}))
	must(t, s.CreateKey(ctx, &index.Key{Kind: "jwt", Public: []byte("p"), PrivateEnc: []byte("e"), Fingerprint: "jwt"}))
	must(t, s.AppendAudit(ctx, &index.AuditEvent{Principal: "user:alice", Action: "push", Repo: "raw/a"}))
	before := readControlPlane(t, s, a, b)
	if len(before.Repos) != 2 || len(before.Users) != 1 || len(before.Tokens) != 2 || len(before.Bindings) != 1 ||
		len(before.ServerKeys) != 1 || len(before.Audit) != 1 {
		t.Fatalf("control plane before ResetDerived: %+v", before)
	}

	must(t, s.ResetDerived(ctx, a.repo.ID))
	same(t, "derived rows of the reset repo", derivedRows(0), derived(t, s, a))
	same(t, "derived rows of the other repo", derivedRows(1), derived(t, s, b))
	same(t, "control plane after ResetDerived(repo)", before, readControlPlane(t, s, a, b))

	must(t, s.ResetDerived(ctx, 0))
	same(t, "derived rows after ResetDerived(0)", derivedRows(0), derived(t, s, b))
	same(t, "control plane after ResetDerived(0)", before, readControlPlane(t, s, a, b))
}
