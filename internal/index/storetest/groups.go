package storetest

import (
	"encoding/json"
	"errors"
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

// testGroupIndexCache: an invalidation of a member's index_cache is its groups' too, of the
// same scope, with their generations; ResetDerived of a member drops its groups' documents, and
// SetGroupMembers the group's (docs/spec/index-schema.md, The generation of a repo's documents).
// An apt group renders its documents from its members (docs/spec/proxy-and-group.md §8.6).
func testGroupIndexCache(t *testing.T, s index.Store) {
	ctx := t.Context()
	a, b, alone := newRepo(t, s, "apt", "a"), newRepo(t, s, "apt", "b"), newRepo(t, s, "apt", "alone")
	newGroup := func(name string, members ...index.Repo) index.Repo {
		t.Helper()
		g := index.Repo{Format: "apt", Name: name, Kind: index.KindGroup, Prefix: "apt/" + name}
		must(t, s.CreateRepo(ctx, &g))
		must(t, s.SetGroupMembers(ctx, g.ID, repoIDsOf(members)))
		return g
	}
	ab, onlyB := newGroup("ab", a, b), newGroup("only-b", b)
	repos := []index.Repo{a, b, alone, ab, onlyB}
	generations := func() []int64 {
		t.Helper()
		var gens []int64
		for _, r := range repos {
			gen, err := s.IndexCacheGeneration(ctx, r.ID)
			must(t, err)
			gens = append(gens, gen)
		}
		return gens
	}
	// put stores a document of each suite in each repo, and returns the generations it read.
	put := func() []int64 {
		t.Helper()
		gens := generations()
		for i, r := range repos {
			for _, suite := range []string{"jammy", "noble"} {
				c := index.Cache{RepoID: r.ID, Scope: suite, Kind: "Release", Content: []byte(r.Name), SHA256: r.Name}
				if ok, err := s.PutIndexCache(ctx, &c, gens[i]); err != nil || !ok {
					t.Fatalf("PutIndexCache %s %s: %t, %v", r.Name, suite, ok, err)
				}
			}
		}
		return gens
	}
	// held reports, per repo, which of jammy and noble index_cache holds.
	held := func() [][2]bool {
		t.Helper()
		var out [][2]bool
		for _, r := range repos {
			var h [2]bool
			for i, suite := range []string{"jammy", "noble"} {
				_, err := s.GetIndexCache(ctx, r.ID, suite, "Release")
				if err != nil && !errors.Is(err, index.ErrNotFound) {
					t.Fatal(err)
				}
				h[i] = err == nil
			}
			out = append(out, h)
		}
		return out
	}
	moved := func(before []int64) []int64 {
		t.Helper()
		after := generations()
		for i := range after {
			after[i] -= before[i]
		}
		return after
	}
	both, none, jammyGone := [2]bool{true, true}, [2]bool{false, false}, [2]bool{false, true}

	gens := put()
	must(t, s.InvalidateIndexCache(ctx, a.ID, "jammy"))
	same(t, "documents after a member's suite is invalidated", [][2]bool{jammyGone, both, both, jammyGone, both}, held())
	same(t, "generations moved by it", []int64{1, 0, 0, 1, 0}, moved(gens))

	gens = put()
	must(t, s.InvalidateIndexCache(ctx, b.ID, ""))
	same(t, "documents after a member of two groups is invalidated", [][2]bool{both, none, both, none, none}, held())
	same(t, "generations moved by it", []int64{0, 1, 0, 1, 1}, moved(gens))

	gens = put()
	must(t, s.InvalidateIndexCache(ctx, alone.ID, ""))
	same(t, "documents after a repo of no group is invalidated", [][2]bool{both, both, none, both, both}, held())
	same(t, "generations moved by it", []int64{0, 0, 1, 0, 0}, moved(gens))

	gens = put()
	must(t, s.ResetDerived(ctx, a.ID))
	same(t, "documents after ResetDerived of a member", [][2]bool{none, both, both, none, both}, held())
	same(t, "generations moved by it", []int64{1, 0, 0, 1, 0}, moved(gens))

	gens = put()
	must(t, s.SetGroupMembers(ctx, ab.ID, repoIDsOf([]index.Repo{b})))
	same(t, "documents after a group's members change", [][2]bool{both, both, both, none, both}, held())
	same(t, "generations moved by it", []int64{0, 0, 0, 1, 0}, moved(gens))
}

// repoIDsOf returns the ids of rs, in order.
func repoIDsOf(rs []index.Repo) []int64 { return ids(rs, func(r index.Repo) int64 { return r.ID }) }
