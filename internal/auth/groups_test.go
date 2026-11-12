package auth

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lajstrom/lajstrom/internal/index"
)

func TestCheckGroupName(t *testing.T) {
	for _, tt := range []struct {
		name string
		ok   bool
	}{
		{"dev", true},
		{"org:team", true},
		{"/realm/platform team", true},
		{"0f6b1a3e-9d1c-4e7e-8a52-2f3f0c6d7b11", true},
		{"équipe", true},
		{strings.Repeat("g", MaxGroupNameLen), true},
		{"", false},
		{strings.Repeat("g", MaxGroupNameLen+1), false},
		{" dev", false},
		{"dev ", false},
		{"a\tb", false},
		{"a\nb", false},
		{"\xff", false},
	} {
		err := CheckGroupName(tt.name)
		if (err == nil) != tt.ok || (err != nil && !errors.Is(err, ErrInvalidPrincipal)) {
			t.Errorf("CheckGroupName(%q) = %v, want ok %v", tt.name, err, tt.ok)
		}
	}
}

// groupFixture is an index with the repos raw/private and pypi/internal, the users alice
// (identity groups dev and admins of a login that lapses at the given time) and bob (no
// identity), and the bindings of the groups dev (reader on raw/*) and admins (admin).
func groupFixture(t *testing.T, lapse time.Time) (*index.Identity, *index.User, index.Store) {
	t.Helper()
	ctx := context.Background()
	st := newIndex(t)
	addRepo(t, st, "raw", "private", false)
	addRepo(t, st, "pypi", "internal", false)
	alice := index.User{Name: "alice", Kind: "oidc"}
	if err := st.CreateUser(ctx, &alice); err != nil {
		t.Fatal(err)
	}
	bob := index.User{Name: "bob", Kind: "local"}
	if err := st.CreateUser(ctx, &bob); err != nil {
		t.Fatal(err)
	}
	id := index.Identity{UserID: alice.ID, Issuer: "https://idp", Subject: "a"}
	if err := st.CreateIdentity(ctx, &id); err != nil {
		t.Fatal(err)
	}
	if err := st.SetIdentityGroups(ctx, id.ID, []string{"dev"}, lapse.Add(-time.Hour), lapse); err != nil {
		t.Fatal(err)
	}
	bind(t, st, "group:dev", RoleReader, "raw/*")
	bind(t, st, "group:admins", RoleAdmin, "*/*")
	return &id, &alice, st
}

// TestRBACGroups: a user has the bindings of its groups (docs/spec/rbac.md §8) until they lapse
// by the clock, a PAT's scopes narrow them, a group's admin binding makes its members admins,
// and a login that drops a group revokes it at the next request, on every replica.
func TestRBACGroups(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 11, 12, 12, 0, 0, 0, time.UTC)
	id, alice, st := groupFixture(t, now.Add(time.Hour))
	var clock atomic.Pointer[time.Time]
	clock.Store(&now)
	r := NewRBAC(RBACOptions{Index: st, Now: func() time.Time { return *clock.Load() }})
	other := NewRBAC(RBACOptions{Index: st, Now: func() time.Time { return *clock.Load() }}) // another replica
	ua := UserPrincipal(alice)
	check := func(what string, r *RBAC, p Principal, c Credential, a Action, repo string, want bool) {
		t.Helper()
		format, name, _ := strings.Cut(repo, "/")
		got, err := r.Allowed(WithEvaluationScope(WithCredential(ctx, c)), p, a, format, name)
		if err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Errorf("%s: Allowed(%s, %s, %s) = %v, want %v", what, p, a, repo, got, want)
		}
	}

	check("a group's reader binding", r, ua, password, ActionRead, "raw/private", true)
	check("not beyond its role", r, ua, password, ActionWrite, "raw/private", false)
	check("not beyond its mask", r, ua, password, ActionRead, "pypi/internal", false)
	check("a PAT with write only narrows it", r, ua, patWrite, ActionRead, "raw/private", false)
	check("a local user is in no group", r, user("bob"), password, ActionRead, "raw/private", false)
	check("the group itself is no principal", r, Principal{Kind: KindGroup, Name: "dev"}, Credential{}, ActionRead, "raw/private", false)

	// A login with the admins group: an admin, the system too; the other replica sees it at once.
	if err := st.SetIdentityGroups(ctx, id.ID, []string{"admins", "dev"}, now, now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	check("a group's admin binding", other, ua, password, ActionManage, System+"/"+System, true)
	check("a group's admin binding, every repo", r, ua, password, ActionDelete, "pypi/internal", true)

	// The next login drops dev and admins: revoked from the next request on.
	if err := st.SetIdentityGroups(ctx, id.ID, []string{"ops"}, now, now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	check("a group the login dropped", r, ua, password, ActionRead, "raw/private", false)
	check("a group the login dropped, on another replica", other, ua, password, ActionManage, System+"/"+System, false)

	// The groups lapse with the login's lifetime, without a change of auth_version.
	if err := st.SetIdentityGroups(ctx, id.ID, []string{"dev"}, now, now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	check("before the groups lapse", r, ua, password, ActionRead, "raw/private", true)
	later := now.Add(time.Hour)
	clock.Store(&later)
	check("at the time the groups lapse", r, ua, password, ActionRead, "raw/private", false)
}

// TestBindingsGroups: a group binding needs no members, lists by its principal, and is never
// "an admin" for the last-admin rule: the user's own last admin binding stays.
func TestBindingsGroups(t *testing.T) {
	ctx := context.Background()
	s, _, st := newBindings(t)
	g, err := s.Create(ctx, "group:platform", RoleAdmin, MaskAll, cli)
	if err != nil {
		t.Fatal(err)
	}
	if g.Principal != "group:platform" || g.Name != "" {
		t.Errorf("Create(group:platform) = %+v, want the principal as given and no name", g)
	}
	r, err := s.Create(ctx, "group:org:team", RoleWriter, "raw/*", cli)
	if err != nil {
		t.Fatal(err)
	}
	list, err := s.List(ctx, "group:org:team")
	if err != nil || len(list) != 1 || list[0] != *r {
		t.Errorf("List(group:org:team) = %+v, %v; want %+v", list, err, *r)
	}
	if _, err := s.Create(ctx, "group:platform", RoleAdmin, MaskAll, cli); !errors.Is(err, index.ErrConflict) {
		t.Errorf("the same group binding twice = %v, want ErrConflict", err)
	}
	evs, err := st.ListAudit(ctx, index.AuditFilter{Action: ActionBindingCreate})
	if err != nil {
		t.Fatal(err)
	}
	subjects := map[string]bool{}
	for _, ev := range evs {
		subjects[ev.Subject] = true
	}
	if !subjects["group:platform"] || !subjects["group:org:team"] {
		t.Errorf("binding.create events of %v, want group:platform and group:org:team among them", subjects)
	}
}

// TestBindingsGroupLastAdmin: a group's admin binding is never "an admin" for the last-admin
// rule: the user's own last admin binding stays, and the group's goes.
func TestBindingsGroupLastAdmin(t *testing.T) {
	ctx := context.Background()
	s, _, st := newBindings(t)
	g, err := s.Create(ctx, "group:platform", RoleAdmin, MaskAll, cli)
	if err != nil {
		t.Fatal(err)
	}
	all, err := st.ListBindings(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	var userAdmin int64
	for _, b := range all {
		if strings.HasPrefix(b.Principal, "user:") && b.Role == RoleAdmin {
			userAdmin = b.ID
		}
	}
	if err := s.Delete(ctx, userAdmin, cli); !errors.Is(err, index.ErrConflict) {
		t.Errorf("Delete of the user's last admin binding beside a group's = %v, want ErrConflict", err)
	}
	if err := s.Delete(ctx, g.ID, cli); err != nil {
		t.Errorf("Delete of a group's admin binding = %v, want nil: a group is never the last admin", err)
	}
}

// TestUserGroups: the groups that count now, of the user's identity only.
func TestUserGroups(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 11, 12, 12, 0, 0, 0, time.UTC)
	_, alice, st := groupFixture(t, now.Add(time.Minute))
	got, err := UserGroups(ctx, st, alice, now)
	if err != nil || len(got) != 1 || got[0] != "dev" {
		t.Errorf("UserGroups before the lapse = %q, %v; want [dev]", got, err)
	}
	got, err = UserGroups(ctx, st, alice, now.Add(time.Minute))
	if err != nil || len(got) != 0 {
		t.Errorf("UserGroups at the lapse = %q, %v; want none", got, err)
	}
}
