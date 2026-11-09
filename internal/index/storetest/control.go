package storetest

import (
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/lajstrom/lajstrom/internal/index"
)

func testMigrate(t *testing.T, s index.Store) {
	ctx := t.Context()
	r := newRepo(t, s, "raw", "scratch")
	for i := range 2 {
		if err := s.Migrate(ctx); err != nil {
			t.Fatalf("Migrate #%d on a migrated store: %v", i+1, err)
		}
	}
	got, err := s.GetRepo(ctx, "raw", "scratch")
	must(t, err)
	same(t, "repo after Migrate", r, *got)
}

func testTimestamps(t *testing.T, s index.Store) {
	ctx := t.Context()
	local := time.Date(2026, 11, 6, 13, 0, 0, 123456789, time.FixedZone("UTC+3", 3*3600))
	want := time.Date(2026, 11, 6, 10, 0, 0, 123456000, time.UTC)
	r := index.Repo{Format: "raw", Name: "ts", Registry: "local", Kind: "hosted", CreatedAt: local, UpdatedAt: local}
	must(t, s.CreateRepo(ctx, &r))
	got, err := s.GetRepoByID(ctx, r.ID)
	must(t, err)
	for _, tt := range []struct {
		name string
		t    time.Time
	}{
		{"model after CreateRepo", r.CreatedAt},
		{"read back", got.CreatedAt},
		{"read back UpdatedAt", got.UpdatedAt},
	} {
		if !tt.t.Equal(want) || tt.t.Location() != time.UTC || tt.t.Nanosecond() != want.Nanosecond() {
			t.Errorf("%s: %v, want %v (UTC, truncated to microseconds)", tt.name, tt.t, want)
		}
	}

	before := time.Now().Truncate(time.Microsecond)
	u := index.User{Name: "zero", Kind: "local"}
	must(t, s.CreateUser(ctx, &u))
	if u.CreatedAt.Before(before) || u.CreatedAt.Location() != time.UTC {
		t.Errorf("zero CreatedAt became %v, want now (>= %v) in UTC", u.CreatedAt, before)
	}
}

func testRepos(t *testing.T, s index.Store) {
	ctx := t.Context()
	repos := []index.Repo{
		{Format: "raw", Name: "scratch", Registry: "local", Prefix: "raw/scratch", Kind: "hosted", AnonymousRead: true,
			Policy: json.RawMessage(`{"maxFileSize": 1048576, "maven": {"sessionTimeout": "1h"}}`), CreatedAt: at(1), UpdatedAt: at(2)},
		{Format: "apt", Name: "ubuntu", Registry: "zot", Prefix: "apt/ubuntu", Kind: "hosted"},
		{Format: "raw", Name: "a-first", Registry: "local", Prefix: "raw/a-first", Kind: "hosted", CreatedAt: at(3)},
	}
	for i := range repos {
		must(t, s.CreateRepo(ctx, &repos[i]))
		if repos[i].ID == 0 || repos[i].CreatedAt.IsZero() || repos[i].UpdatedAt.IsZero() {
			t.Fatalf("CreateRepo did not fill ID and timestamps: %+v", repos[i])
		}
	}
	if !repos[2].UpdatedAt.Equal(at(3)) {
		t.Errorf("zero UpdatedAt = %v, want CreatedAt %v", repos[2].UpdatedAt, at(3))
	}
	for _, want := range repos {
		got, err := s.GetRepo(ctx, want.Format, want.Name)
		must(t, err)
		same(t, "GetRepo "+want.Name, want, *got)
		got, err = s.GetRepoByID(ctx, want.ID)
		must(t, err)
		same(t, "GetRepoByID "+want.Name, want, *got)
	}
	for _, tt := range []struct {
		format string
		want   []int64
	}{
		{"", []int64{repos[1].ID, repos[2].ID, repos[0].ID}},
		{"raw", []int64{repos[2].ID, repos[0].ID}},
		{"maven", []int64{}},
	} {
		got, err := s.ListRepos(ctx, tt.format)
		must(t, err)
		same(t, fmt.Sprintf("ListRepos(%q)", tt.format), tt.want, ids(got, func(r index.Repo) int64 { return r.ID }))
	}

	before := time.Now().Truncate(time.Microsecond)
	u := repos[0]
	u.Name, u.CreatedAt = "renamed", time.Time{} // neither is stored
	u.Registry, u.Prefix, u.Kind, u.AnonymousRead, u.Policy = "zot", "raw/elsewhere", "proxy", false, nil
	must(t, s.UpdateRepo(ctx, &u))
	if u.Name != "scratch" || !u.CreatedAt.Equal(repos[0].CreatedAt) || u.UpdatedAt.Before(before) {
		t.Errorf("UpdateRepo filled %+v, want the stored name and CreatedAt and UpdatedAt now", u)
	}
	got, err := s.GetRepoByID(ctx, u.ID)
	must(t, err)
	same(t, "repo after UpdateRepo", u, *got)

	must(t, s.DeleteRepo(ctx, repos[1].ID))
	for _, tt := range []struct {
		name string
		err  error
		want error
	}{
		{"CreateRepo duplicate", s.CreateRepo(ctx, &index.Repo{Format: "raw", Name: "scratch", Kind: "hosted"}), index.ErrConflict},
		{"UpdateRepo missing", s.UpdateRepo(ctx, &index.Repo{ID: 999999}), index.ErrNotFound},
		{"DeleteRepo deleted", s.DeleteRepo(ctx, repos[1].ID), index.ErrNotFound},
		{"GetRepo missing", second(s.GetRepo(ctx, "raw", "nope")), index.ErrNotFound},
		{"GetRepo other format", second(s.GetRepo(ctx, "pypi", "scratch")), index.ErrNotFound},
		{"GetRepoByID deleted", second(s.GetRepoByID(ctx, repos[1].ID)), index.ErrNotFound},
	} {
		wantErr(t, tt.name, tt.err, tt.want)
	}
}

// second returns the error of a two-valued call.
func second[T any](_ T, err error) error { return err }

func testUsers(t *testing.T, s index.Store) {
	ctx := t.Context()
	users := []index.User{
		{Name: "bob", PasswordHash: passwordHash("bob"), Kind: "local", CreatedAt: at(1)},
		{Name: "alice", Kind: "local", Disabled: true}, // no password: NULL
	}
	for i := range users {
		must(t, s.CreateUser(ctx, &users[i]))
	}
	for _, want := range users {
		got, err := s.GetUser(ctx, want.Name)
		must(t, err)
		same(t, "GetUser "+want.Name, want, *got)
	}
	all, err := s.ListUsers(ctx)
	must(t, err)
	same(t, "ListUsers", []index.User{users[1], users[0]}, all)

	for _, u := range users {
		if !index.IsUID(u.UID) {
			t.Errorf("CreateUser gave %s the uid %q", u.Name, u.UID)
		}
	}
	got, err := s.GetUserByUID(ctx, users[0].UID)
	must(t, err)
	same(t, "GetUserByUID", users[0], *got)

	// A rename and a new password; the uid stays.
	u := users[1]
	u.Name, u.PasswordHash, u.Disabled, u.UID = "mallory", passwordHash("new"), false, "ignored"
	must(t, s.UpdateUser(ctx, &u))
	if u.UID != users[1].UID {
		t.Errorf("UpdateUser changed the uid to %q", u.UID)
	}
	got, err = s.GetUser(ctx, "mallory")
	must(t, err)
	same(t, "user after UpdateUser", u, *got)
	wantErr(t, "GetUser of the old name", second(s.GetUser(ctx, "alice")), index.ErrNotFound)

	for _, tt := range []struct {
		name string
		err  error
		want error
	}{
		{"CreateUser duplicate", s.CreateUser(ctx, &index.User{Name: "bob", Kind: "local"}), index.ErrConflict},
		{"UpdateUser to a name another has", s.UpdateUser(ctx, &index.User{ID: u.ID, Name: "bob", Kind: "local"}), index.ErrConflict},
		{"UpdateUser missing", s.UpdateUser(ctx, &index.User{ID: 999999, Name: "x", Kind: "local"}), index.ErrNotFound},
		{"GetUser missing", second(s.GetUser(ctx, "carol")), index.ErrNotFound},
		{"GetUserByUID missing", second(s.GetUserByUID(ctx, index.NewUID(at(0)))), index.ErrNotFound},
		{"GetUserByUID not a uid", second(s.GetUserByUID(ctx, "bob")), index.ErrNotFound},
	} {
		wantErr(t, tt.name, tt.err, tt.want)
	}
}

// testUsersDeleted: a deleted user keeps its row, by its uid, and frees its name: GetUser and
// ListUsers do not see it, and a new user of the name gets another uid.
func testUsersDeleted(t *testing.T, s index.Store) {
	ctx := t.Context()
	bob := newUser(t, s, "bob")
	deletedAt := at(5)
	bob.DeletedName, bob.Name, bob.DeletedAt, bob.Disabled = bob.Name, "~"+bob.UID, &deletedAt, true
	must(t, s.UpdateUser(ctx, &bob))
	wantErr(t, "GetUser of a deleted user", second(s.GetUser(ctx, "bob")), index.ErrNotFound)
	got, err := s.GetUserByUID(ctx, bob.UID)
	must(t, err)
	same(t, "GetUserByUID of a deleted user", bob, *got)
	all, err := s.ListUsers(ctx)
	must(t, err)
	if len(all) != 0 {
		t.Errorf("ListUsers = %+v, want no deleted user", all)
	}
	again := newUser(t, s, "bob")
	if again.UID == bob.UID {
		t.Errorf("the new bob has the uid of the deleted one")
	}
}

func testTokens(t *testing.T, s index.Store) {
	ctx := t.Context()
	alice := newUser(t, s, "alice")
	repo := newRepo(t, s, "raw", "scratch")
	tokens := []index.Token{
		{Kind: "pat", UserID: &alice.ID, Name: "laptop", Hash: "h1", Scopes: []string{"read", "write"},
			ExpiresAt: ptr(at(3600)), LastUsedAt: ptr(at(5)), CreatedAt: at(1)},
		{Kind: "pat", UserID: &alice.ID, Name: "full", Hash: "h2", Scopes: []string{}},
		{Kind: "deploy", RepoID: &repo.ID, Name: "deploy", Hash: "h3", Scopes: []string{"read"}, RevokedAt: ptr(at(7))},
		{Kind: "pat", UserID: &alice.ID, Name: "legacy", Hash: "h4"},
	}
	for i := range tokens {
		must(t, s.CreateToken(ctx, &tokens[i]))
	}
	for _, want := range tokens {
		got, err := s.GetTokenByHash(ctx, want.Hash)
		must(t, err)
		same(t, "GetTokenByHash "+want.Name, want, *got)
	}
	tokenID := func(tk index.Token) int64 { return tk.ID }
	for _, tt := range []struct {
		name           string
		userID, repoID *int64
		want           []index.Token
	}{
		{"all", nil, nil, tokens},
		{"of alice", &alice.ID, nil, []index.Token{tokens[0], tokens[1], tokens[3]}},
		{"of the repo", nil, &repo.ID, []index.Token{tokens[2]}},
		{"of alice and the repo", &alice.ID, &repo.ID, []index.Token{}},
	} {
		got, err := s.ListTokens(ctx, tt.userID, tt.repoID)
		must(t, err)
		same(t, "ListTokens "+tt.name, ids(tt.want, tokenID), ids(got, tokenID))
	}

	must(t, s.RevokeToken(ctx, tokens[0].ID, at(10)))
	must(t, s.TouchToken(ctx, tokens[1].ID, at(20)))
	revoked, err := s.GetTokenByHash(ctx, "h1")
	must(t, err)
	touched, err := s.GetTokenByHash(ctx, "h2")
	must(t, err)
	if revoked.RevokedAt == nil || !revoked.RevokedAt.Equal(at(10)) {
		t.Errorf("RevokedAt = %v, want %v", revoked.RevokedAt, at(10))
	}
	if touched.LastUsedAt == nil || !touched.LastUsedAt.Equal(at(20)) {
		t.Errorf("LastUsedAt = %v, want %v", touched.LastUsedAt, at(20))
	}

	for _, tt := range []struct {
		name string
		err  error
		want error
	}{
		{"CreateToken duplicate hash", s.CreateToken(ctx, &index.Token{Kind: "pat", UserID: &alice.ID, Hash: "h1"}), index.ErrConflict},
		{"RevokeToken missing", s.RevokeToken(ctx, 999999, at(1)), index.ErrNotFound},
		{"TouchToken missing", s.TouchToken(ctx, 999999, at(1)), index.ErrNotFound},
		{"GetTokenByHash missing", second(s.GetTokenByHash(ctx, "nope")), index.ErrNotFound},
	} {
		wantErr(t, tt.name, tt.err, tt.want)
	}
}

func testBindings(t *testing.T, s index.Store) {
	ctx := t.Context()
	bindings := []index.Binding{
		{Principal: "user:alice", Role: "writer", RepoMask: "raw/*", CreatedAt: at(1)},
		{Principal: "group:dev", Role: "reader", RepoMask: "*/*"},
		{Principal: "user:alice", Role: "admin", RepoMask: "*/*"},
		{Principal: "user:alice", Role: "reader", RepoMask: "*/*"},
	}
	for i := range bindings {
		must(t, s.CreateBinding(ctx, &bindings[i]))
	}
	dup := index.Binding{Principal: "user:alice", Role: "admin", RepoMask: "*/*"}
	wantErr(t, "CreateBinding of the same principal, role and mask", s.CreateBinding(ctx, &dup), index.ErrConflict)
	for _, tt := range []struct {
		principal string
		want      []index.Binding
	}{
		{"", bindings},
		{"user:alice", []index.Binding{bindings[0], bindings[2], bindings[3]}},
		{"anonymous", []index.Binding{}},
	} {
		got, err := s.ListBindings(ctx, tt.principal)
		must(t, err)
		same(t, fmt.Sprintf("ListBindings(%q)", tt.principal), tt.want, got)
	}
	must(t, s.DeleteBinding(ctx, bindings[3].ID))
	got, err := s.ListBindings(ctx, "")
	must(t, err)
	same(t, "bindings after DeleteBinding", bindings[:3], got)
	wantErr(t, "DeleteBinding deleted", s.DeleteBinding(ctx, bindings[3].ID), index.ErrNotFound)
}

// testAuthVersion: every write of bindings and of repos increments auth_version in its
// transaction; a write that fails, a rolled back transaction and any other write leave it
// (docs/spec/rbac.md §3).
func testAuthVersion(t *testing.T, s index.Store) {
	ctx := t.Context()
	version := func() int64 {
		t.Helper()
		v, err := s.AuthVersion(ctx)
		must(t, err)
		return v
	}
	if v := version(); v < 1 {
		t.Fatalf("AuthVersion after Migrate = %d, want 1 or more", v)
	}
	r := index.Repo{Format: "raw", Name: "auth", Registry: "local", Prefix: "raw/auth", Kind: "hosted"}
	b := index.Binding{Principal: "user:alice", Role: "reader", RepoMask: "raw/*"}
	for _, step := range []struct {
		name  string
		write func() error
		moves bool
		err   error
	}{
		{"CreateRepo", func() error { return s.CreateRepo(ctx, &r) }, true, nil},
		{"CreateRepo of the same name", func() error { d := r; d.ID = 0; return s.CreateRepo(ctx, &d) }, false, index.ErrConflict},
		{"UpdateRepo", func() error { r.AnonymousRead = true; return s.UpdateRepo(ctx, &r) }, true, nil},
		{"UpdateRepo of a missing id", func() error { m := r; m.ID += 1000; return s.UpdateRepo(ctx, &m) }, false, index.ErrNotFound},
		{"CreateBinding", func() error { return s.CreateBinding(ctx, &b) }, true, nil},
		{"CreateBinding of the same binding", func() error { d := b; d.ID = 0; return s.CreateBinding(ctx, &d) }, false, index.ErrConflict},
		{"DeleteBinding", func() error { return s.DeleteBinding(ctx, b.ID) }, true, nil},
		{"DeleteBinding again", func() error { return s.DeleteBinding(ctx, b.ID) }, false, index.ErrNotFound},
		{"CreateUser", func() error { _ = newUser(t, s, "auth"); return nil }, false, nil},
		{"DeleteRepo", func() error { return s.DeleteRepo(ctx, r.ID) }, true, nil},
		{"DeleteRepo again", func() error { return s.DeleteRepo(ctx, r.ID) }, false, index.ErrNotFound},
	} {
		before := version()
		if err := step.write(); step.err == nil {
			must(t, err)
		} else {
			wantErr(t, step.name, err, step.err)
		}
		want := before
		if step.moves {
			want++
		}
		if got := version(); got != want {
			t.Errorf("AuthVersion after %s = %d, want %d", step.name, got, want)
		}
	}

	before := version()
	must(t, s.Tx(ctx, func(w index.Writer) error {
		for _, mask := range []string{"raw/a", "raw/b"} {
			if err := w.CreateBinding(ctx, &index.Binding{Principal: "user:bob", Role: "reader", RepoMask: mask}); err != nil {
				return err
			}
		}
		return nil
	}))
	if got := version(); got != before+2 {
		t.Errorf("AuthVersion after a Tx of two CreateBinding = %d, want %d", got, before+2)
	}
	errRollback := errors.New("roll back")
	err := s.Tx(ctx, func(w index.Writer) error {
		must(t, w.CreateBinding(ctx, &index.Binding{Principal: "user:bob", Role: "writer", RepoMask: "raw/a"}))
		return errRollback
	})
	wantErr(t, "a Tx that rolls back", err, errRollback)
	if got := version(); got != before+2 {
		t.Errorf("AuthVersion after a rolled back Tx = %d, want %d", got, before+2)
	}
}

func testKeys(t *testing.T, s index.Store) {
	ctx := t.Context()
	repo := newRepo(t, s, "apt", "ubuntu")
	keys := []index.Key{
		{Kind: "jwt", Public: []byte("pub1"), PrivateEnc: []byte("enc1"), Fingerprint: "fp1", CreatedAt: at(1)},
		{RepoID: &repo.ID, Kind: "gpg", Public: []byte("pub3"), PrivateEnc: []byte("enc3"), Fingerprint: "fp3", CreatedAt: at(3)},
		{Kind: "jwt", Public: []byte("pub0"), PrivateEnc: []byte("enc0"), Fingerprint: "fp0", CreatedAt: at(9), RetiredAt: ptr(at(9))},
	}
	for i := range keys {
		must(t, s.CreateKey(ctx, &keys[i]))
	}
	getKey := func(repoID *int64, kind string) (index.Key, error) {
		k, err := s.GetKey(ctx, repoID, kind)
		if err != nil {
			return index.Key{}, err
		}
		return *k, nil
	}
	k, err := getKey(nil, "jwt")
	must(t, err)
	same(t, "server jwt key", keys[0], k)
	k, err = getKey(&repo.ID, "gpg")
	must(t, err)
	same(t, "repo gpg key", keys[1], k)

	must(t, s.RetireKey(ctx, keys[0].ID, at(5)))
	keys[0].RetiredAt = ptr(at(5))
	wantErr(t, "GetKey after RetireKey of the one not retired", second(getKey(nil, "jwt")), index.ErrNotFound)
	newer := index.Key{Kind: "jwt", Public: []byte("pub2"), PrivateEnc: []byte("enc2"), Fingerprint: "fp2", CreatedAt: at(6)}
	must(t, s.CreateKey(ctx, &newer))
	k, err = getKey(nil, "jwt")
	must(t, err)
	same(t, "server jwt key after a rotation", newer, k)
	must(t, s.RetireKey(ctx, newer.ID, time.Time{}))

	server, err := s.ListKeys(ctx, nil)
	must(t, err)
	if len(server) != 3 || server[2].RetiredAt == nil {
		t.Errorf("ListKeys(nil) = %+v, want 3 keys, the last retired now", server)
	} else {
		newer.RetiredAt = server[2].RetiredAt
		same(t, "ListKeys(nil)", []index.Key{keys[0], keys[2], newer}, server)
	}
	ofRepo, err := s.ListKeys(ctx, &repo.ID)
	must(t, err)
	same(t, "ListKeys(repo)", []index.Key{keys[1]}, ofRepo)

	for _, tt := range []struct {
		name string
		err  error
		want error
	}{
		{"GetKey all retired", second(getKey(nil, "jwt")), index.ErrNotFound},
		{"GetKey of another kind", second(getKey(&repo.ID, "jwt")), index.ErrNotFound},
		{"GetKey of a repo without keys", second(getKey(ptr(repo.ID+1000), "gpg")), index.ErrNotFound},
		{"RetireKey missing", s.RetireKey(ctx, 999999, at(1)), index.ErrNotFound},
	} {
		wantErr(t, tt.name, tt.err, tt.want)
	}
}

// testKeysActive checks the unique index of migration 0004: one key that is not retired per
// kind and owner, the owner a repo or the server; retired keys do not count, and a key retired
// makes room for the next.
func testKeysActive(t *testing.T, s index.Store) {
	ctx := t.Context()
	a, b := newRepo(t, s, "apt", "a"), newRepo(t, s, "apt", "b")
	key := func(repoID *int64, kind, fp string, retired *time.Time) *index.Key {
		return &index.Key{RepoID: repoID, Kind: kind, Public: []byte("pub-" + fp), PrivateEnc: []byte("enc-" + fp),
			Fingerprint: fp, RetiredAt: retired}
	}
	first := key(&a.ID, "gpg", "a1", nil)
	must(t, s.CreateKey(ctx, first))
	for _, tt := range []struct {
		name string
		key  *index.Key
		want error // nil: created
	}{
		{"a second gpg key of a", key(&a.ID, "gpg", "a2", nil), index.ErrConflict},
		{"a jwt key of a", key(&a.ID, "jwt", "a3", nil), nil},
		{"a retired gpg key of a", key(&a.ID, "gpg", "a4", ptr(at(1))), nil},
		{"the gpg key of b", key(&b.ID, "gpg", "b1", nil), nil},
		{"a server jwt key", key(nil, "jwt", "s1", nil), nil},
		{"a second server jwt key", key(nil, "jwt", "s2", nil), index.ErrConflict},
		{"a server gpg key", key(nil, "gpg", "s3", nil), nil},
	} {
		err := s.CreateKey(ctx, tt.key)
		if tt.want == nil {
			if err != nil {
				t.Errorf("CreateKey of %s: %v", tt.name, err)
			}
			continue
		}
		wantErr(t, "CreateKey of "+tt.name, err, tt.want)
	}
	must(t, s.RetireKey(ctx, first.ID, at(2)))
	next := key(&a.ID, "gpg", "a5", nil)
	must(t, s.CreateKey(ctx, next))
	got, err := s.GetKey(ctx, &a.ID, "gpg")
	must(t, err)
	same(t, "the gpg key of a after a rotation", *next, *got)
	keys, err := s.ListKeys(ctx, &a.ID)
	must(t, err)
	var fps []string
	for _, k := range keys {
		fps = append(fps, k.Fingerprint)
	}
	same(t, "the keys of a", []string{"a1", "a3", "a4", "a5"}, fps)
}

// testKeyRace creates the gpg key of a new repo from several writers at once, as replicas do
// on a repo's first signed request: one insert wins, the others are ErrConflict, and GetKey
// returns the winner.
func testKeyRace(t *testing.T, s index.Store) {
	ctx := t.Context()
	const rounds, writers = 10, 8
	for round := range rounds {
		repo := newRepo(t, s, "apt", fmt.Sprintf("race-%d", round))
		errs := make([]error, writers)
		keys := make([]index.Key, writers)
		var wg sync.WaitGroup
		for i := range writers {
			wg.Go(func() {
				fp := fmt.Sprintf("fp-%d-%d", round, i)
				keys[i] = index.Key{RepoID: &repo.ID, Kind: "gpg", Public: []byte(fp), PrivateEnc: []byte(fp), Fingerprint: fp}
				errs[i] = s.CreateKey(ctx, &keys[i])
			})
		}
		wg.Wait()
		stored, err := s.GetKey(ctx, &repo.ID, "gpg")
		must(t, err)
		winners := 0
		for i, err := range errs {
			switch {
			case err == nil:
				winners++
				same(t, fmt.Sprintf("round %d, writer %d", round, i), *stored, keys[i])
			case !errors.Is(err, index.ErrConflict):
				t.Errorf("round %d, writer %d: %v, want ErrConflict", round, i, err)
			}
		}
		if winners != 1 {
			t.Errorf("round %d: %d writers created the key, want 1", round, winners)
		}
	}
}

func testSessions(t *testing.T, s index.Store) {
	ctx := t.Context()
	repo := newRepo(t, s, "maven", "releases")
	sess := index.Session{RepoID: repo.ID, Key: "com.example:lib:1.0", Principal: "user:alice", State: "open",
		Files: json.RawMessage(`[{"name": "lib-1.0.jar", "digest": "sha256:aa"}]`), ExpiresAt: at(100)}
	must(t, s.UpsertSession(ctx, &sess))
	if sess.ID == 0 || sess.CreatedAt.IsZero() || !sess.UpdatedAt.Equal(sess.CreatedAt) {
		t.Fatalf("UpsertSession insert filled %+v", sess)
	}
	got, err := s.GetSession(ctx, repo.ID, sess.Key)
	must(t, err)
	same(t, "GetSession", sess, *got)
	got, err = s.GetSessionByID(ctx, sess.ID)
	must(t, err)
	same(t, "GetSessionByID", sess, *got)

	u := sess
	u.State, u.Files, u.ExpiresAt, u.Key = "committed", nil, at(200), "ignored"
	must(t, s.UpsertSession(ctx, &u))
	if u.ID != sess.ID || u.Key != sess.Key || !u.CreatedAt.Equal(sess.CreatedAt) || u.UpdatedAt.Before(sess.UpdatedAt) {
		t.Errorf("UpsertSession update filled %+v, want the row's key and CreatedAt and a newer UpdatedAt", u)
	}
	got, err = s.GetSessionByID(ctx, sess.ID)
	must(t, err)
	same(t, "session after update", u, *got)

	soon := index.Session{RepoID: repo.ID, Key: "k1", Principal: "user:alice", State: "open", ExpiresAt: at(10)}
	later := index.Session{RepoID: repo.ID, Key: "k2", Principal: "user:alice", State: "open", ExpiresAt: at(1000)}
	for _, x := range []*index.Session{&soon, &later} {
		must(t, s.UpsertSession(ctx, x))
	}
	n, err := s.ExpireSessions(ctx, at(500))
	must(t, err)
	if n != 1 {
		t.Errorf("ExpireSessions = %d, want 1 (the committed session is not open)", n)
	}
	for _, tt := range []struct {
		id    int64
		state string
	}{{soon.ID, "expired"}, {later.ID, "open"}, {sess.ID, "committed"}} {
		got, err := s.GetSessionByID(ctx, tt.id)
		must(t, err)
		if got.State != tt.state {
			t.Errorf("session %d: state %q, want %q", tt.id, got.State, tt.state)
		}
	}
	must(t, s.DeleteSession(ctx, later.ID))

	for _, tt := range []struct {
		name string
		err  error
		want error
	}{
		{"GetSession not open", second(s.GetSession(ctx, repo.ID, sess.Key)), index.ErrNotFound},
		{"GetSession expired", second(s.GetSession(ctx, repo.ID, "k1")), index.ErrNotFound},
		{"GetSessionByID deleted", second(s.GetSessionByID(ctx, later.ID)), index.ErrNotFound},
		{"UpsertSession missing id", s.UpsertSession(ctx, &index.Session{ID: 999999, State: "open"}), index.ErrNotFound},
		{"DeleteSession deleted", s.DeleteSession(ctx, later.ID), index.ErrNotFound},
	} {
		wantErr(t, tt.name, tt.err, tt.want)
	}
}

// testSessionsTouch: TouchSession moves the expiry of an open session that has not expired, and
// finds no other; a key has one open session at a time.
func testSessionsTouch(t *testing.T, s index.Store) {
	ctx := t.Context()
	repo := newRepo(t, s, "maven", "releases")
	open := index.Session{RepoID: repo.ID, Key: "k2", Principal: "user:alice", State: "open", ExpiresAt: at(1000)}
	must(t, s.UpsertSession(ctx, &open))
	touched, err := s.TouchSession(ctx, open.ID, at(500), at(2000))
	must(t, err)
	if touched.ID != open.ID || touched.State != "open" || !touched.ExpiresAt.Equal(at(2000)) || touched.Key != "k2" {
		t.Errorf("TouchSession = %+v, want session %d open until at(2000)", touched, open.ID)
	}

	// One open session per key: a second is a conflict, one after it closed is not.
	first := index.Session{RepoID: repo.ID, Key: "k3", Principal: "user:alice", State: "open", ExpiresAt: at(1000)}
	must(t, s.UpsertSession(ctx, &first))
	dup := index.Session{RepoID: repo.ID, Key: "k3", Principal: "user:bob", State: "open", ExpiresAt: at(1000)}
	wantErr(t, "UpsertSession of a second open session", s.UpsertSession(ctx, &dup), index.ErrConflict)
	first.State = "aborted"
	must(t, s.UpsertSession(ctx, &first))
	must(t, s.UpsertSession(ctx, &dup))
	must(t, s.DeleteSession(ctx, open.ID))

	for _, tt := range []struct {
		name string
		err  error
	}{
		{"expired at", second(s.TouchSession(ctx, dup.ID, at(1000), at(3000)))},
		{"not open", second(s.TouchSession(ctx, first.ID, at(0), at(3000)))},
		{"deleted", second(s.TouchSession(ctx, open.ID, at(0), at(3000)))},
	} {
		wantErr(t, "TouchSession "+tt.name, tt.err, index.ErrNotFound)
	}
}

// testSessionsConcurrent: transactions that each start with TouchSession, then read, change
// and write the files of one session, at once, all land: TouchSession holds the row.
func testSessionsConcurrent(t *testing.T, s index.Store) {
	ctx := t.Context()
	repo := newRepo(t, s, "maven", "releases")
	sess := index.Session{RepoID: repo.ID, Key: "com.example:lib:1.0", Principal: "user:alice", State: "open",
		Files: json.RawMessage(`[]`), ExpiresAt: at(1000)}
	must(t, s.UpsertSession(ctx, &sess))
	const n = 8
	var wg sync.WaitGroup
	for i := range n {
		wg.Go(func() {
			err := s.Tx(ctx, func(w index.Writer) error {
				cur, err := w.TouchSession(ctx, sess.ID, at(0), at(1000))
				if err != nil {
					return err
				}
				var files []string
				if err := json.Unmarshal(cur.Files, &files); err != nil {
					return err
				}
				if cur.Files, err = json.Marshal(append(files, fmt.Sprintf("f%d", i))); err != nil {
					return err
				}
				return w.UpsertSession(ctx, cur)
			})
			if err != nil {
				t.Errorf("transaction %d: %v", i, err)
			}
		})
	}
	wg.Wait()
	got, err := s.GetSessionByID(ctx, sess.ID)
	must(t, err)
	var files []string
	must(t, json.Unmarshal(got.Files, &files))
	if len(files) != n {
		t.Errorf("%d files after %d concurrent changes, want all: %v", len(files), n, files)
	}
}

func testAudit(t *testing.T, s index.Store) {
	ctx := t.Context()
	events := []index.AuditEvent{
		{At: at(1), Principal: "user:alice", Action: "repo.create", Repo: "raw/scratch", Subject: "raw/scratch", IP: "10.0.0.1"},
		{At: at(2), Principal: "user:alice", Action: "push", Repo: "raw/scratch", Subject: "pkg 1.0", Digest: dg("m1"),
			IP: "10.0.0.1", Details: json.RawMessage(`{"requestId": "r-2"}`)},
		{At: at(3), Principal: "token:7", Action: "push", Repo: "pypi/main", Subject: "requests 2.0", Digest: dg("m2"), IP: "10.0.0.2"},
		{At: at(3), Principal: "user:bob", Action: "delete", Repo: "raw/scratch", Subject: "pkg 1.0", IP: "10.0.0.3"},
		{At: at(4), Principal: "user:alice", Action: "token.create", Subject: "alice", IP: "10.0.0.1"},
	}
	for i := range events {
		must(t, s.AppendAudit(ctx, &events[i]))
	}
	ev := func(i ...int) []index.AuditEvent {
		out := []index.AuditEvent{}
		for _, j := range i {
			out = append(out, events[j])
		}
		return out
	}
	for _, tt := range []struct {
		name string
		f    index.AuditFilter
		want []index.AuditEvent
	}{
		{"all, newest first", index.AuditFilter{}, ev(4, 3, 2, 1, 0)},
		{"principal", index.AuditFilter{Principal: "user:alice"}, ev(4, 1, 0)},
		{"repo", index.AuditFilter{Repo: "raw/scratch"}, ev(3, 1, 0)},
		{"action", index.AuditFilter{Action: "push"}, ev(2, 1)},
		{"principal and action", index.AuditFilter{Principal: "user:alice", Action: "push"}, ev(1)},
		{"since, inclusive", index.AuditFilter{Since: at(3)}, ev(4, 3, 2)},
		{"until, exclusive", index.AuditFilter{Until: at(3)}, ev(1, 0)},
		{"since and until", index.AuditFilter{Since: at(2), Until: at(4)}, ev(3, 2, 1)},
		{"limit", index.AuditFilter{Limit: 2}, ev(4, 3)},
		{"limit and offset", index.AuditFilter{Limit: 2, Offset: 2}, ev(2, 1)},
		{"offset past the end", index.AuditFilter{Offset: 10}, ev()},
		{"no match", index.AuditFilter{Principal: "anonymous"}, ev()},
	} {
		got, err := s.ListAudit(ctx, tt.f)
		must(t, err)
		same(t, "ListAudit "+tt.name, tt.want, got)
	}

	before := time.Now().Truncate(time.Microsecond)
	now := index.AuditEvent{Principal: "anonymous", Action: "system"}
	must(t, s.AppendAudit(ctx, &now))
	if now.At.Before(before) {
		t.Errorf("zero At became %v, want now", now.At)
	}
}

// testAuditPrincipalName: an event of user:<uid> gets the user's name of its time, a rename
// changes the name of the events after it and not of those before, a deleted user's events get
// the name it had, and a name given is kept; other principals get none.
func testAuditPrincipalName(t *testing.T, s index.Store) {
	ctx := t.Context()
	alice := newUser(t, s, "alice")
	p := "user:" + alice.UID
	evs := []*index.AuditEvent{{Principal: p, Action: "push"}}
	must(t, s.AppendAudit(ctx, evs[0]))
	alice.Name = "alicia"
	must(t, s.UpdateUser(ctx, &alice))
	evs = append(evs, &index.AuditEvent{Principal: p, Action: "push"})
	must(t, s.AppendAudit(ctx, evs[1]))
	deletedAt := at(9)
	alice.DeletedName, alice.Name, alice.DeletedAt = alice.Name, "~"+alice.UID, &deletedAt
	must(t, s.UpdateUser(ctx, &alice))
	evs = append(evs, &index.AuditEvent{Principal: p, Action: "user.delete"},
		&index.AuditEvent{Principal: p, PrincipalName: "given", Action: "push"},
		&index.AuditEvent{Principal: "token:3", Action: "push"},
		&index.AuditEvent{Principal: "user:alice", Action: "push"})
	for _, ev := range evs[2:] {
		must(t, s.AppendAudit(ctx, ev))
	}
	want := []string{"alice", "alicia", "alicia", "given", "", ""}
	for i, ev := range evs {
		if ev.PrincipalName != want[i] {
			t.Errorf("event %d of %s: PrincipalName %q, want %q", i, ev.Principal, ev.PrincipalName, want[i])
		}
	}
	listed, err := s.ListAudit(ctx, index.AuditFilter{Principal: p})
	must(t, err)
	if len(listed) != 4 || listed[3].PrincipalName != "alice" {
		t.Errorf("ListAudit of %s: %+v, want 4 events, the oldest of alice", p, listed)
	}
}

func testAuditDefaultLimit(t *testing.T, s index.Store) {
	ctx := t.Context()
	must(t, s.Tx(ctx, func(w index.Writer) error {
		for i := range 101 {
			if err := w.AppendAudit(ctx, &index.AuditEvent{At: at(i), Principal: "user:alice", Action: "push"}); err != nil {
				return err
			}
		}
		return nil
	}))
	got, err := s.ListAudit(ctx, index.AuditFilter{})
	must(t, err)
	if len(got) != 100 || !got[0].At.Equal(at(100)) {
		t.Errorf("ListAudit without a limit: %d events, the first at %v; want 100, the newest first", len(got), got[0].At)
	}
}
