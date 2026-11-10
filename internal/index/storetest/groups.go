package storetest

import (
	"encoding/json"
	"testing"

	"github.com/lajstrom/lajstrom/internal/index"
)

// testGroups: a group's settings and its members (docs/spec/index-schema.md, group_members),
// in order and replaced whole; a member restricts its own delete, and a group's rows go with it.
func testGroups(t *testing.T, s index.Store) {
	ctx := t.Context()
	internal, npmjs, mirror := newRepo(t, s, "npm", "internal"), newRepo(t, s, "npm", "npmjs"), newRepo(t, s, "npm", "mirror")
	all := index.Repo{Format: "npm", Name: "all", Kind: index.KindGroup, Prefix: "npm/all",
		Group: json.RawMessage(`{"merge": ["@types/*"]}`)}
	must(t, s.CreateRepo(ctx, &all))
	got, err := s.GetRepoByID(ctx, all.ID)
	must(t, err)
	same(t, "group after CreateRepo", all, *got)
	other := index.Repo{Format: "npm", Name: "aa", Kind: index.KindGroup, Prefix: "npm/aa"}
	must(t, s.CreateRepo(ctx, &other))

	repoIDs := func(rs []index.Repo) []int64 { return ids(rs, func(r index.Repo) int64 { return r.ID }) }
	members := func(what string, group index.Repo, want ...index.Repo) {
		t.Helper()
		got, err := s.ListGroupMembers(ctx, group.ID)
		must(t, err)
		same(t, what, repoIDs(want), repoIDs(got))
	}
	must(t, s.SetGroupMembers(ctx, all.ID, []int64{internal.ID, npmjs.ID}))
	members("members", all, internal, npmjs)
	must(t, s.SetGroupMembers(ctx, other.ID, []int64{npmjs.ID}))
	groups, err := s.ListGroupsOf(ctx, npmjs.ID)
	must(t, err)
	same(t, "ListGroupsOf, by format and name", []int64{other.ID, all.ID}, repoIDs(groups))
	if len(groups) == 2 {
		same(t, "a group of ListGroupsOf", all, groups[1])
	}

	must(t, s.SetGroupMembers(ctx, all.ID, []int64{mirror.ID, internal.ID}))
	members("members replaced, in the new order", all, mirror, internal)
	for _, tt := range []struct {
		name string
		err  error
		want error
	}{
		{"a member twice", s.SetGroupMembers(ctx, all.ID, []int64{npmjs.ID, npmjs.ID}), index.ErrConflict},
		{"a member that is no repo", s.SetGroupMembers(ctx, all.ID, []int64{npmjs.ID, 999999}), index.ErrNotFound},
		{"a group that is no repo", s.SetGroupMembers(ctx, 999999, []int64{npmjs.ID}), index.ErrNotFound},
		{"DeleteRepo of a member", s.DeleteRepo(ctx, internal.ID), index.ErrConflict},
	} {
		wantErr(t, tt.name, tt.err, tt.want)
	}
	members("members after the refused changes", all, mirror, internal)
	if _, err := s.GetRepoByID(ctx, internal.ID); err != nil {
		t.Errorf("a refused DeleteRepo deleted the member: %v", err)
	}

	err = s.Tx(ctx, func(w index.Writer) error {
		all.Group = nil
		if err := w.UpdateRepo(ctx, &all); err != nil {
			return err
		}
		return w.SetGroupMembers(ctx, all.ID, nil)
	})
	must(t, err)
	members("no members", all)
	got, err = s.GetRepoByID(ctx, all.ID)
	must(t, err)
	same(t, "group after UpdateRepo", all, *got)
	must(t, s.DeleteRepo(ctx, internal.ID))

	must(t, s.DeleteRepo(ctx, other.ID))
	groups, err = s.ListGroupsOf(ctx, npmjs.ID)
	must(t, err)
	same(t, "groups of a member after its group was deleted", []int64{}, repoIDs(groups))
	must(t, s.DeleteRepo(ctx, npmjs.ID))
	members("members of a hosted repo", mirror)
}
