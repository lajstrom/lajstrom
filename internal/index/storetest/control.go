package storetest

import (
	"encoding/json"
	"fmt"
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

	u := users[1]
	u.Name, u.PasswordHash, u.Disabled = "mallory", passwordHash("new"), false
	must(t, s.UpdateUser(ctx, &u))
	if u.Name != "alice" {
		t.Errorf("UpdateUser changed the name to %q", u.Name)
	}
	got, err := s.GetUser(ctx, "alice")
	must(t, err)
	same(t, "user after UpdateUser", u, *got)

	for _, tt := range []struct {
		name string
		err  error
		want error
	}{
		{"CreateUser duplicate", s.CreateUser(ctx, &index.User{Name: "bob", Kind: "local"}), index.ErrConflict},
		{"UpdateUser missing", s.UpdateUser(ctx, &index.User{ID: 999999, Kind: "local"}), index.ErrNotFound},
		{"GetUser missing", second(s.GetUser(ctx, "carol")), index.ErrNotFound},
	} {
		wantErr(t, tt.name, tt.err, tt.want)
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
		{Principal: "user:alice", Role: "admin", RepoMask: "*/*"}, // no unique constraint
	}
	for i := range bindings {
		must(t, s.CreateBinding(ctx, &bindings[i]))
	}
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

func testKeys(t *testing.T, s index.Store) {
	ctx := t.Context()
	repo := newRepo(t, s, "apt", "ubuntu")
	keys := []index.Key{
		{Kind: "jwt", Public: []byte("pub1"), PrivateEnc: []byte("enc1"), Fingerprint: "fp1", CreatedAt: at(1)},
		{Kind: "jwt", Public: []byte("pub2"), PrivateEnc: []byte("enc2"), Fingerprint: "fp2", CreatedAt: at(2)},
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
	same(t, "server jwt key", keys[1], k)
	k, err = getKey(&repo.ID, "gpg")
	must(t, err)
	same(t, "repo gpg key", keys[2], k)

	must(t, s.RetireKey(ctx, keys[1].ID, at(5)))
	keys[1].RetiredAt = ptr(at(5))
	k, err = getKey(nil, "jwt")
	must(t, err)
	same(t, "server jwt key after RetireKey of the newest", keys[0], k)
	must(t, s.RetireKey(ctx, keys[0].ID, time.Time{}))

	server, err := s.ListKeys(ctx, nil)
	must(t, err)
	if len(server) != 3 || server[0].RetiredAt == nil {
		t.Errorf("ListKeys(nil) = %+v, want 3 keys, the first retired now", server)
	} else {
		keys[0].RetiredAt = server[0].RetiredAt
		same(t, "ListKeys(nil)", []index.Key{keys[0], keys[1], keys[3]}, server)
	}
	ofRepo, err := s.ListKeys(ctx, &repo.ID)
	must(t, err)
	same(t, "ListKeys(repo)", []index.Key{keys[2]}, ofRepo)

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
