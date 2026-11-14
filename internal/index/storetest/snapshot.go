package storetest

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/lajstrom/lajstrom/internal/index"
)

// The snapshots of the control plane (#76; docs/spec/index-schema.md, Snapshots of the control
// plane): index.Snapshotter, which both engines implement beside Store.

// snapshotter returns s as an index.Snapshotter, or fails the test.
func snapshotter(t *testing.T, s index.Store) index.Snapshotter {
	t.Helper()
	sn, ok := s.(index.Snapshotter)
	if !ok {
		t.Fatalf("%T does not implement index.Snapshotter", s)
	}
	return sn
}

// fullControlPlane fills s with a row of every table a snapshot carries, of every kind: a hosted, a
// proxy and a group repo with its members; a live user, a deleted one and an OIDC one with its
// identity and groups; a PAT, a deploy token and a revoked one; bindings; a repo's key and a
// retired server key; audit rows. Besides, rows a snapshot leaves out: a session, derived rows,
// a bootstrap token.
func fullControlPlane(t *testing.T, s index.Store) fixture {
	t.Helper()
	ctx := t.Context()
	f := populate(t, s, "assets")
	proxy := index.Repo{Format: "npm", Name: "npmjs", Registry: "local", Prefix: "npm/npmjs", Kind: index.KindProxy,
		Policy: json.RawMessage(`{"maxFileSize": 1024}`), Upstream: json.RawMessage(`{"name": "npmjs", "metadataTTL": "5m"}`),
		CreatedAt: at(1)}
	must(t, s.CreateRepo(ctx, &proxy))
	hosted := newRepo(t, s, "npm", "internal")
	group := index.Repo{Format: "npm", Name: "all", Prefix: "npm/all", Kind: index.KindGroup, Group: json.RawMessage(`{"merge": []}`)}
	must(t, s.CreateRepo(ctx, &group))
	must(t, s.SetGroupMembers(ctx, group.ID, []int64{hosted.ID, proxy.ID}))

	alice := newUser(t, s, "alice")
	gone := newUser(t, s, "gone")
	gone.Name, gone.DeletedName, gone.DeletedAt = "~"+gone.UID, "gone", ptr(at(50))
	must(t, s.UpdateUser(ctx, &gone))
	dora := index.User{Name: "dora", Kind: "oidc", CreatedAt: at(2)}
	must(t, s.CreateUser(ctx, &dora))
	id := index.Identity{UserID: dora.ID, Issuer: "https://idp.example.com", Subject: "dora-sub", CreatedAt: at(3)}
	must(t, s.CreateIdentity(ctx, &id))
	must(t, s.SetIdentityGroups(ctx, id.ID, []string{"ops", "dev"}, at(4), at(4000)))

	for _, tok := range []index.Token{
		{Kind: "pat", UserID: &alice.ID, Name: "laptop", Hash: "hash-pat", ExpiresAt: ptr(at(9000)), LastUsedAt: ptr(at(60))},
		{Kind: "deploy", RepoID: &hosted.ID, Name: "ci", Hash: "hash-deploy", Scopes: []string{"read", "write"}},
		{Kind: "pat", UserID: &alice.ID, Name: "old", Hash: "hash-revoked", RevokedAt: ptr(at(70))},
	} {
		must(t, s.CreateToken(ctx, &tok))
	}
	for _, b := range []index.Binding{
		{Principal: "user:" + alice.UID, Role: "admin", RepoMask: "*/*"},
		{Principal: "group:ops", Role: "writer", RepoMask: "npm/*"},
		{Principal: "token:1", Role: "reader", RepoMask: "raw/assets"},
	} {
		must(t, s.CreateBinding(ctx, &b))
	}
	server := index.Key{Kind: "jwt", Public: []byte("server-pub"), PrivateEnc: []byte("server-enc"), Fingerprint: "jwt-1", RetiredAt: ptr(at(80))}
	must(t, s.CreateKey(ctx, &server))
	for i, ev := range []index.AuditEvent{
		{Principal: "user:" + alice.UID, Action: "repo.create", Repo: "npm/internal", Subject: "npm/internal", IP: "10.0.0.1",
			Details: json.RawMessage(`{"source": "api"}`)},
		{Principal: "token:2", Action: "push", Repo: "npm/internal", Subject: "left-pad 1.0.0", Digest: dg("left-pad"), IP: "10.0.0.2",
			State: index.AuditFailed, Error: "the registry refused"},
		{Principal: "anonymous", Action: "token.create", Subject: "user:" + alice.UID},
	} {
		ev.At = at(100 * (i + 1))
		must(t, s.AppendAudit(ctx, &ev))
	}
	must(t, s.PutBootstrapToken(ctx, &index.BootstrapToken{Hash: "bootstrap", Replica: "host 1", ExpiresAt: at(86400)}, ""))
	return f
}

// export reads the control plane of s, every audit row included, and fails the test on an error.
func export(t *testing.T, s index.Store) *index.ControlPlane {
	t.Helper()
	cp, err := snapshotter(t, s).ExportControlPlane(t.Context(), index.ExportOptions{WithAudit: true})
	must(t, err)
	return cp
}

// rows is cp without the time of its read: what two reads of one control plane have alike.
func rows(cp *index.ControlPlane) index.ControlPlane {
	c := *cp
	c.ReadAt = time.Time{}
	return c
}

// testSnapshotRoundTrip: the control plane exported from one database and imported into a new
// one is the same, row for row with its ids; reads and new rows work after it, the new ids past
// the restored ones; the rows a snapshot leaves out stay behind.
func testSnapshotRoundTrip(t *testing.T, open func(*testing.T) index.Store) {
	ctx := t.Context()
	src := open(t)
	f := fullControlPlane(t, src)
	cp := export(t, src)
	if cp.Schema < 21 || cp.ReadAt.IsZero() || !cp.WithAudit {
		t.Errorf("Schema %d, ReadAt %v, WithAudit %v: want the newest migration, the database's time and the audit rows",
			cp.Schema, cp.ReadAt, cp.WithAudit)
	}
	counts := map[string]int{"repos": len(cp.Repos), "group_members": len(cp.GroupMembers), "users": len(cp.Users),
		"identities": len(cp.Identities), "identity_groups": len(cp.IdentityGroups), "tokens": len(cp.Tokens),
		"bindings": len(cp.Bindings), "keys": len(cp.Keys), "audit": len(cp.Audit)}
	same(t, "rows exported", map[string]int{"repos": 4, "group_members": 2, "users": 3, "identities": 1, "identity_groups": 2,
		"tokens": 4, "bindings": 3, "keys": 2, "audit": 3}, counts)

	dst := open(t)
	v0, err := dst.AuthVersion(ctx)
	must(t, err)
	event := index.AuditEvent{Principal: "anonymous", Action: "snapshot.restore", Subject: "lajstrom/control-plane:x", At: at(1000)}
	must(t, snapshotter(t, dst).ImportControlPlane(ctx, cp, index.ImportOptions{Event: &event}))
	got := export(t, dst)
	if n := len(got.Audit); n == len(cp.Audit)+1 && got.Audit[n-1].ID == event.ID && event.ID > cp.Audit[len(cp.Audit)-1].ID {
		got.Audit = got.Audit[:n-1]
	} else {
		t.Errorf("the restore's audit row: id %d, audit %+v; want it last, after the restored rows", event.ID, got.Audit)
	}
	same(t, "the control plane after the import", rows(cp), rows(got))
	if v1, err := dst.AuthVersion(ctx); err != nil || v1 <= v0 {
		t.Errorf("AuthVersion after the import = %d (%v), want more than %d", v1, err, v0)
	}

	checkImported(t, dst, cp, f)
}

// checkImported checks dst after the import of cp, the snapshot of a fullControlPlane whose
// fixture is f: its rows read as they did, the rows made after the import take ids past the
// restored ones, and the rows a snapshot leaves out are not there.
func checkImported(t *testing.T, dst index.Store, cp *index.ControlPlane, f fixture) {
	t.Helper()
	ctx := t.Context()
	tok, err := dst.GetTokenByHash(ctx, "hash-deploy")
	must(t, err)
	if repo, err := dst.GetRepoByID(ctx, *tok.RepoID); err != nil || repo.Name != "internal" {
		t.Errorf("the deploy token's repo: %+v, %v", repo, err)
	}
	members, err := dst.ListGroupMembers(ctx, cp.Repos[3].ID)
	must(t, err)
	same(t, "the group's members", []string{"internal", "npmjs"}, []string{members[0].Name, members[1].Name})
	if _, err := dst.GetUser(ctx, "gone"); err == nil {
		t.Error("the deleted user is live after the import")
	}
	groups, err := dst.ListIdentityGroups(ctx)
	must(t, err)
	same(t, "identity groups", 2, len(groups))

	r := newRepo(t, dst, "raw", "after")
	u := newUser(t, dst, "after")
	tk := index.Token{Kind: "pat", UserID: &u.ID, Name: "after", Hash: "hash-after"}
	must(t, dst.CreateToken(ctx, &tk))
	b := index.Binding{Principal: "user:" + u.UID, Role: "reader", RepoMask: "*/*"}
	must(t, dst.CreateBinding(ctx, &b))
	k := index.Key{RepoID: &r.ID, Kind: "gpg", Public: []byte("p"), PrivateEnc: []byte("e"), Fingerprint: "after"}
	must(t, dst.CreateKey(ctx, &k))
	last := func(n int, id func(int) int64) int64 { return id(n - 1) }
	for _, c := range []struct {
		what     string
		got, max int64
	}{
		{"repo", r.ID, last(len(cp.Repos), func(i int) int64 { return cp.Repos[i].ID })},
		{"user", u.ID, last(len(cp.Users), func(i int) int64 { return cp.Users[i].ID })},
		{"token", tk.ID, last(len(cp.Tokens), func(i int) int64 { return cp.Tokens[i].ID })},
		{"binding", b.ID, last(len(cp.Bindings), func(i int) int64 { return cp.Bindings[i].ID })},
		{"key", k.ID, last(len(cp.Keys), func(i int) int64 { return cp.Keys[i].ID })},
	} {
		if c.got <= c.max {
			t.Errorf("the id of a %s made after the import is %d, want more than %d, the greatest restored", c.what, c.got, c.max)
		}
	}

	pkgs, err := dst.ListPackages(ctx, f.repo.ID, "")
	must(t, err)
	_, sessErr := dst.GetSessionByID(ctx, f.session.ID)
	_, bootErr := dst.GetBootstrapToken(ctx)
	if len(pkgs) != 0 || sessErr == nil || bootErr == nil {
		t.Errorf("rows a snapshot leaves out came with it: %d packages, session %v, bootstrap token %v", len(pkgs), sessErr, bootErr)
	}
}

// testSnapshotAudit: the audit rows of a snapshot are none unless asked for, every row, or those
// of a window.
func testSnapshotAudit(t *testing.T, s index.Store) {
	ctx := t.Context()
	for i := range 3 {
		must(t, s.AppendAudit(ctx, &index.AuditEvent{At: at(100 * i), Principal: "anonymous", Action: "push"}))
	}
	sn := snapshotter(t, s)
	for _, tt := range []struct {
		name string
		opts index.ExportOptions
		want int
	}{
		{"none", index.ExportOptions{}, 0},
		{"every row", index.ExportOptions{WithAudit: true}, 3},
		{"a window", index.ExportOptions{WithAudit: true, AuditSince: at(100)}, 2},
	} {
		cp, err := sn.ExportControlPlane(ctx, tt.opts)
		must(t, err)
		if len(cp.Audit) != tt.want || cp.WithAudit != tt.opts.WithAudit || !cp.AuditSince.Equal(tt.opts.AuditSince) {
			t.Errorf("%s: %d rows, WithAudit %v, AuditSince %v; want %d", tt.name, len(cp.Audit), cp.WithAudit, cp.AuditSince, tt.want)
		}
	}
}

// testSnapshotNotEmpty: an import into a database that has a row in a table of a snapshot is
// refused, naming the table, and changes nothing; forced, it replaces what the database holds,
// the derived and coordination rows included.
func testSnapshotNotEmpty(t *testing.T, open func(*testing.T) index.Store) {
	ctx := t.Context()
	src := open(t)
	fullControlPlane(t, src)
	cp := export(t, src)
	for _, tt := range []struct {
		table string
		fill  func(s index.Store)
	}{
		{"repos", func(s index.Store) { newRepo(t, s, "raw", "config") }},
		{"users", func(s index.Store) { newUser(t, s, "early") }},
		{"keys", func(s index.Store) {
			must(t, s.CreateKey(ctx, &index.Key{Kind: "jwt", Public: []byte("p"), PrivateEnc: []byte("e"), Fingerprint: "start"}))
		}},
		{"bindings", func(s index.Store) {
			must(t, s.CreateBinding(ctx, &index.Binding{Principal: "group:x", Role: "reader", RepoMask: "*/*"}))
		}},
		{"audit", func(s index.Store) {
			must(t, s.AppendAudit(ctx, &index.AuditEvent{Principal: "anonymous", Action: "reindex"}))
		}},
	} {
		dst := open(t)
		tt.fill(dst)
		before := export(t, dst)
		err := snapshotter(t, dst).ImportControlPlane(ctx, cp, index.ImportOptions{})
		wantErr(t, tt.table, err, index.ErrNotEmpty)
		if err != nil && !strings.Contains(err.Error(), tt.table) {
			t.Errorf("%s: the error %q does not name the table", tt.table, err)
		}
		same(t, tt.table+": the control plane after the refused import", rows(before), rows(export(t, dst)))
	}

	dst := open(t)
	f := fullControlPlane(t, dst)
	newRepo(t, dst, "raw", "config")
	must(t, snapshotter(t, dst).ImportControlPlane(ctx, cp, index.ImportOptions{Force: true}))
	same(t, "the control plane after a forced import", rows(cp), rows(export(t, dst)))
	same(t, "derived rows after a forced import", derivedRows(0), derived(t, dst, f))
	if _, err := dst.GetBootstrapToken(ctx); err == nil {
		t.Error("the bootstrap token is left after a forced import")
	}
}

// testSnapshotRefused: a snapshot of a newer schema than the binary's, and one with a row that
// refers to a row it lacks, are refused, and nothing is written.
func testSnapshotRefused(t *testing.T, open func(*testing.T) index.Store) {
	ctx := t.Context()
	src := open(t)
	fullControlPlane(t, src)
	cp := export(t, src)
	dst := open(t)
	newer := *cp
	newer.Schema = 1 << 20
	wantErr(t, "a newer schema", snapshotter(t, dst).ImportControlPlane(ctx, &newer, index.ImportOptions{}), index.ErrSchemaTooNew)
	dangling := *cp
	dangling.Users = nil
	wantErr(t, "tokens of users it lacks", snapshotter(t, dst).ImportControlPlane(ctx, &dangling, index.ImportOptions{}), index.ErrNotFound)
	twice := *cp
	twice.Bindings = append(twice.Bindings, twice.Bindings[0])
	wantErr(t, "a binding twice", snapshotter(t, dst).ImportControlPlane(ctx, &twice, index.ImportOptions{}), index.ErrConflict)
	if got := export(t, dst); len(got.Repos)+len(got.Users)+len(got.Tokens) != 0 {
		t.Errorf("a refused import wrote %d repos, %d users, %d tokens", len(got.Repos), len(got.Users), len(got.Tokens))
	}
}

// testSnapshotConsistent: an export is one read. While a writer makes repos with a deploy token
// each and deletes the repo of two rounds before, the token's row going with it, every export
// has the repo of every token it has: a read of tokens later than the read of repos would find
// the token of a repo made in between.
func testSnapshotConsistent(t *testing.T, s index.Store) {
	ctx := t.Context()
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Go(func() {
		var made []int64
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			r := index.Repo{Format: "raw", Name: fmt.Sprintf("r%d", i), Registry: "local", Prefix: fmt.Sprintf("raw/r%d", i),
				Kind: index.KindHosted}
			if s.CreateRepo(ctx, &r) != nil {
				continue
			}
			_ = s.CreateToken(ctx, &index.Token{Kind: "deploy", RepoID: &r.ID, Name: "ci", Hash: "h" + r.Name})
			if made = append(made, r.ID); len(made) > 2 {
				_ = s.DeleteRepo(ctx, made[0])
				made = made[1:]
			}
		}
	})
	defer wg.Wait()
	defer close(stop)
	sn := snapshotter(t, s)
	for range 300 {
		cp, err := sn.ExportControlPlane(ctx, index.ExportOptions{})
		must(t, err)
		repos := map[int64]bool{}
		for _, r := range cp.Repos {
			repos[r.ID] = true
		}
		for _, tok := range cp.Tokens {
			if tok.RepoID != nil && !repos[*tok.RepoID] {
				t.Fatalf("an export has the token %d of the repo %d and not the repo", tok.ID, *tok.RepoID)
			}
		}
	}
}
