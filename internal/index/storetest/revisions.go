package storetest

import (
	"encoding/json"
	"testing"

	"github.com/lajstrom/lajstrom/internal/index"
)

// testRevisions: a repo, a user and a webhook are inserted at revision 1, and every update of
// the row moves it by one, which the write fills and every read returns (#796). A group's
// members change with its row; a deleted user's row moves too.
func testRevisions(t *testing.T, s index.Store) {
	ctx := t.Context()
	revision := func(what string, got, want int64) {
		t.Helper()
		if got != want {
			t.Errorf("%s: revision %d, want %d", what, got, want)
		}
	}

	proxy := index.Repo{Format: "npm", Name: "npmjs", Registry: "local", Prefix: "npm/npmjs", Kind: index.KindProxy,
		Upstream: json.RawMessage(`{"name": "npmjs"}`)}
	must(t, s.CreateRepo(ctx, &proxy))
	revision("CreateRepo", proxy.Revision, 1)
	proxy.AnonymousRead = true
	must(t, s.UpdateRepo(ctx, &proxy))
	revision("UpdateRepo", proxy.Revision, 2)
	proxy.Revision = 7 // not stored
	must(t, s.UpdateRepo(ctx, &proxy))
	revision("UpdateRepo again", proxy.Revision, 3)
	must(t, s.ConvertRepo(ctx, proxy.ID, at(0)))
	got, err := s.GetRepo(ctx, "npm", "npmjs")
	must(t, err)
	revision("ConvertRepo", got.Revision, 4)
	byID, err := s.GetRepoByID(ctx, proxy.ID)
	must(t, err)
	revision("GetRepoByID", byID.Revision, 4)
	group := index.Repo{Format: "npm", Name: "all", Prefix: "npm/all", Kind: index.KindGroup}
	must(t, s.CreateRepo(ctx, &group))
	err = s.Tx(ctx, func(w index.Writer) error {
		if err := w.UpdateRepo(ctx, &group); err != nil {
			return err
		}
		return w.SetGroupMembers(ctx, group.ID, []int64{proxy.ID})
	})
	must(t, err)
	members, err := s.ListGroupsOf(ctx, proxy.ID)
	must(t, err)
	if len(members) != 1 {
		t.Fatalf("ListGroupsOf = %+v, want the group", members)
	}
	revision("a group whose members changed", members[0].Revision, 2)

	u := index.User{Name: "alice", Kind: "local"}
	must(t, s.CreateUser(ctx, &u))
	revision("CreateUser", u.Revision, 1)
	u.PasswordHash, u.Revision = passwordHash("alice"), 0
	must(t, s.UpdateUser(ctx, &u))
	revision("UpdateUser", u.Revision, 2)
	u.Name, u.DeletedName, u.DeletedAt = "~"+u.UID, "alice", ptr(at(1))
	must(t, s.UpdateUser(ctx, &u))
	byUID, err := s.GetUserByUID(ctx, u.UID)
	must(t, err)
	revision("a deleted user", byUID.Revision, 3)

	h := newWebhook(t, s, "npm/*")
	revision("CreateWebhook", h.Revision, 1)
	h.Active = false
	must(t, s.UpdateWebhook(ctx, &h))
	revision("UpdateWebhook", h.Revision, 2)
	hooks, err := s.ListWebhooks(ctx)
	must(t, err)
	revision("ListWebhooks", hooks[0].Revision, 2)
}
