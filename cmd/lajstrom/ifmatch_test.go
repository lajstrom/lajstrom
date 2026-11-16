package main

import (
	"context"
	"strings"
	"testing"

	"github.com/lajstrom/lajstrom/internal/api"
	"github.com/lajstrom/lajstrom/internal/index/sqlite"
)

// TestAdminIfMatch: --if-match makes a change of a user or a repo conditional, as If-Match makes
// the API's: with the ETag of the revision the resource has, or *, the change is made; with
// another, it is refused with what changed and the ETag now, alone on standard error, exit
// status 1, and nothing changes; a value that is no entity tag is refused naming the flag.
func TestAdminIfMatch(t *testing.T) {
	c := newUserCLI(t)
	if _, err := c.create(t, nil, strings.NewReader("alice-password"), "alice", "--password-stdin"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := run(t, nil, "admin", "repo", "create", "raw/scratch", "--registry", "local", "--config", c.cfg); err != nil {
		t.Fatal(err)
	}
	st := openTestStore(t, c.dsn)
	etag := func(table string) string { t.Helper(); return storedETag(t, st, table) }
	try := func(wantErr string, args ...string) { t.Helper(); runIfMatch(t, c.cfg, wantErr, args...) }
	stale := etag(api.TableUsers)
	try("", "admin", "user", "disable", "alice", "--if-match", stale)
	fresh := etag(api.TableUsers)
	try("user alice changed since it was read: its ETag is now "+fresh+"; read it again and send the change again",
		"admin", "user", "enable", "alice", "--if-match", stale)
	try("user alice changed since it was read", "admin", "user", "enable", "alice", "--if-match", "W/"+fresh)
	try("user alice changed since it was read", "admin", "user", "rename", "alice", "alicia", "--if-match", stale)
	try("user alice changed since it was read", "admin", "user", "delete", "alice", "--if-match", stale)
	try(`--if-match: "stale" is not * or a list of entity tags`, "admin", "user", "disable", "alice", "--if-match", "stale")
	try("", "admin", "user", "enable", "alice", "--if-match", stale+", "+fresh)
	try("", "admin", "user", "disable", "alice", "--if-match", "*")
	try("repo raw/scratch changed since it was read", "admin", "repo", "delete", "raw/scratch", "--if-match", stale)
	try("", "admin", "repo", "delete", "raw/scratch", "--if-match", etag(api.TableRepos))
	u, err := st.GetUser(context.Background(), "alice")
	if err != nil || !u.Disabled {
		t.Errorf("alice = %+v, %v: want her disabled, under her name", u, err)
	}
	if _, err := st.GetRepo(context.Background(), "raw", "scratch"); err == nil {
		t.Error("raw/scratch is still there after its delete with its ETag")
	}
}

// storedETag returns the ETag of alice, or of raw/scratch for TableRepos, as st has them.
func storedETag(t *testing.T, st *sqlite.Store, table string) string {
	t.Helper()
	ctx := context.Background()
	if table == api.TableRepos {
		r, err := st.GetRepo(ctx, "raw", "scratch")
		if err != nil {
			t.Fatal(err)
		}
		return api.ETag(table, r.ID, r.Revision)
	}
	u, err := st.GetUser(ctx, "alice")
	if err != nil {
		t.Fatal(err)
	}
	return api.ETag(table, u.ID, u.Revision)
}

// runIfMatch runs args with --config cfg: it must succeed when wantErr is "", else fail with an
// error that starts with wantErr, exit status 1.
func runIfMatch(t *testing.T, cfg, wantErr string, args ...string) {
	t.Helper()
	_, _, err := run(t, nil, append(args, "--config", cfg)...)
	switch {
	case wantErr == "" && err != nil:
		t.Fatalf("%v: %v", args, err)
	case wantErr != "" && (err == nil || !strings.HasPrefix(err.Error(), wantErr)):
		t.Fatalf("%v: error = %v, want it to start with %q", args, err, wantErr)
	case wantErr != "" && exitStatus(err) != 1:
		t.Errorf("%v: exit status %d, want 1", args, exitStatus(err))
	}
}
