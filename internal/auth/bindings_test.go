package auth

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/lajstrom/lajstrom/internal/index"
	"github.com/lajstrom/lajstrom/internal/index/sqlite"
)

// newBindings returns the bindings service over the index of users, which has the users admin
// (with the admin binding) and alice; the formats are raw, pypi and oci.
func newBindings(t *testing.T) (*Bindings, *Users, *sqlite.Store) {
	t.Helper()
	users, st, _ := newUsers(t)
	createAdminAndAlice(t, users)
	formats := []string{"raw", "pypi", "oci"}
	b := &Bindings{Index: st, Now: users.Now, Log: users.Log, Formats: func(n string) bool { return slices.Contains(formats, n) }}
	return b, users, st
}

func TestBindingsCreate(t *testing.T) {
	ctx := context.Background()
	s, _, st := newBindings(t)
	before, err := st.AuthVersion(ctx)
	if err != nil {
		t.Fatal(err)
	}
	b, err := s.Create(ctx, "user:alice", RoleWriter, "raw/assets", cli)
	if err != nil {
		t.Fatal(err)
	}
	if b.ID == 0 || named(t, st, b.Principal) != "user:alice" || b.Name != "alice" || b.Role != RoleWriter || b.RepoMask != "raw/assets" || !b.CreatedAt.Equal(usersNow) {
		t.Errorf("Create = %+v", b)
	}
	for _, mask := range []string{"raw/*", "*/assets", "*/*", "oci/mirror"} {
		if _, err := s.Create(ctx, "user:alice", RoleReader, mask, cli); err != nil {
			t.Errorf("Create on %s: %v", mask, err)
		}
	}
	if after, _ := st.AuthVersion(ctx); after != before+5 {
		t.Errorf("auth_version %d after five bindings, want %d", after, before+5)
	}
	_, err = s.Create(ctx, "user:alice", RoleWriter, "raw/assets", cli)
	if !errors.Is(err, index.ErrConflict) || !strings.Contains(err.Error(), "user:alice writer on raw/assets exists already") {
		t.Errorf("Create of the same binding: %v, want a conflict that names it", err)
	}
	wantBindingEvents(t, st, ActionBindingCreate, 5, `{"id":2,"repoMask":"raw/assets","role":"writer","source":"cli"}`)
}

// wantBindingEvents checks that the index has n audit events of action, by the anonymous
// principal of the CLI about user:alice, the first with the details first.
func wantBindingEvents(t *testing.T, st index.Reader, action string, n int, first string) {
	t.Helper()
	evs, err := st.ListAudit(context.Background(), index.AuditFilter{Action: action})
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != n {
		t.Fatalf("%d %s events, want %d", len(evs), action, n)
	}
	if ev := evs[n-1]; ev.Principal != "anonymous" || named(t, st, ev.Subject) != "user:alice" || string(ev.Details) != first {
		t.Errorf("the first %s event: %+v, want details %s", action, ev, first)
	}
}

// TestBindingsRules: what docs/spec/rbac.md §3 refuses, each with ErrInvalid and the field in
// the message, and nothing written.
func TestBindingsRules(t *testing.T) {
	ctx := context.Background()
	s, _, st := newBindings(t)
	for _, tt := range []struct {
		principal, role, mask, msg string
	}{
		{"anonymous", RoleReader, "raw/*", "principal: anonymous cannot be bound"},
		{"token:5", RoleWriter, "raw/assets", "principal: token:5 cannot be bound"},
		{"group:dev", RoleReader, "*/*", "principal: group:dev cannot be bound"},
		{"alice", RoleReader, "*/*", "principal: "},
		{"user:bob", RoleReader, "raw/*", "principal: user bob does not exist"},
		{"user:alice", "owner", "raw/*", `role: "owner" is not reader, writer, maintainer or admin`},
		{"user:alice", RoleReader, "raw", "repoMask: mask \"raw\""},
		{"user:alice", RoleReader, "raw/a/b", "repoMask: mask \"raw/a/b\""},
		{"user:alice", RoleReader, "maven/*", `repoMask: "maven" is not a format of this server`},
		{"user:alice", RoleReader, "raw/Assets", `repoMask: "Assets" is not a repo name`},
		{"user:alice", RoleAdmin, "raw/*", "repoMask: an admin binding is on */*"},
	} {
		_, err := s.Create(ctx, tt.principal, tt.role, tt.mask, cli)
		if !errors.Is(err, ErrInvalid) || !strings.HasPrefix(err.Error(), tt.msg) {
			t.Errorf("Create(%s, %s, %s) = %v, want ErrInvalid starting %q", tt.principal, tt.role, tt.mask, err, tt.msg)
		}
	}
	if all, _ := st.ListBindings(ctx, ""); len(all) != 1 {
		t.Errorf("%d bindings after refusals, want admin's alone: %+v", len(all), all)
	}
}

func TestBindingsListDelete(t *testing.T) {
	ctx := context.Background()
	s, _, st := newBindings(t)
	b, err := s.Create(ctx, "user:alice", RoleReader, "pypi/*", cli)
	if err != nil {
		t.Fatal(err)
	}
	for principal, want := range map[string]int{"": 2, "user:alice": 1, "user:nobody": 0} {
		list, err := s.List(ctx, principal)
		if err != nil || len(list) != want || (want > 0 && list[want-1] != *b) {
			t.Errorf("List(%q) = %+v, %v; want %d, the last %+v", principal, list, err, want, *b)
		}
	}
	if _, err := s.List(ctx, "alice"); !errors.Is(err, ErrInvalid) {
		t.Errorf("List(alice) = %v, want ErrInvalid", err)
	}
	if err := s.Delete(ctx, b.ID, cli); err != nil {
		t.Fatal(err)
	}
	if err := s.Delete(ctx, b.ID, cli); !errors.Is(err, index.ErrNotFound) || err.Error() != "binding 2 does not exist" {
		t.Errorf("Delete again = %v, want ErrNotFound naming it", err)
	}
	wantBindingEvents(t, st, ActionBindingDelete, 1, `{"id":2,"repoMask":"pypi/*","role":"reader","source":"cli"}`)
}

// TestBindingsLastAdmin: the last admin binding of an enabled user stays; another admin, or one
// whose user is disabled, lets it go.
func TestBindingsLastAdmin(t *testing.T) {
	ctx := context.Background()
	s, users, _ := newBindings(t)
	adminID := func(name string) int64 {
		t.Helper()
		list, err := s.List(ctx, "user:"+name)
		if err != nil {
			t.Fatal(err)
		}
		for _, b := range list {
			if b.Role == RoleAdmin {
				return b.ID
			}
		}
		t.Fatalf("user %s has no admin binding", name)
		return 0
	}
	err := s.Delete(ctx, adminID("admin"), cli)
	if !errors.Is(err, index.ErrConflict) || !strings.Contains(err.Error(), "the last admin binding of an enabled user") {
		t.Fatalf("Delete of the only admin binding = %v, want a conflict", err)
	}
	if _, err := users.Create(ctx, "root", "root-password", true, cli); err != nil {
		t.Fatal(err)
	}
	if err := users.Disable(ctx, "root", cli); err != nil {
		t.Fatal(err)
	}
	if err := s.Delete(ctx, adminID("admin"), cli); !errors.Is(err, index.ErrConflict) {
		t.Errorf("Delete while the other admin is disabled = %v, want a conflict", err)
	}
	if err := s.Delete(ctx, adminID("root"), cli); err != nil {
		t.Errorf("Delete of a disabled user's admin binding: %v", err)
	}
	if _, err := s.Create(ctx, "user:alice", RoleAdmin, MaskAll, cli); err != nil {
		t.Fatal(err)
	}
	if err := s.Delete(ctx, adminID("admin"), cli); err != nil {
		t.Errorf("Delete with alice an admin: %v", err)
	}
}
