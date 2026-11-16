package storetest

import (
	"testing"

	"github.com/lajstrom/lajstrom/internal/index"
)

// newCIPolicy creates the CI trust policy name of the issuer github, made at at(0).
func newCIPolicy(t *testing.T, s index.Writer, name string) index.CIPolicy {
	t.Helper()
	p := index.CIPolicy{Name: name, Issuer: "github", Claims: map[string][]string{"repository_id": {"123"}, "ref": {"refs/tags/v*"}},
		Grants: []index.CIGrant{{Role: "reader", Mask: "*/*"}}, CreatedBy: "user:alice", CreatedAt: at(0)}
	must(t, s.CreateCIPolicy(t.Context(), &p))
	return p
}

// testCIPolicies: a CI trust policy is created at revision 1, active by default and updated by
// its creator, read back as written, listed by id, changed in all but its name and creator while
// it has the revision the change read (else ErrConflict, and nothing changes), the revision
// moved, and deleted; a name is unique; every write moves auth_version (docs/spec/rbac.md §11).
func testCIPolicies(t *testing.T, s index.Store) {
	ctx := t.Context()
	v0, err := s.AuthVersion(ctx)
	must(t, err)
	p := newCIPolicy(t, s, "app-release")
	if p.ID == 0 || !p.UpdatedAt.Equal(at(0)) || p.Revision != 1 || p.State != index.CIPolicyActive || p.UpdatedBy != p.CreatedBy {
		t.Fatalf("CreateCIPolicy filled %+v, want an ID, UpdatedAt = CreatedAt, revision 1, active, UpdatedBy = CreatedBy", p)
	}
	got, err := s.GetCIPolicy(ctx, "app-release")
	must(t, err)
	same(t, "GetCIPolicy", p, *got)
	other := index.CIPolicy{Name: "acme-write", Issuer: "gitlab", CreatedBy: "user:bob", State: index.CIPolicyPending,
		ExpiresAt: ptr(at(9000)), Grants: []index.CIGrant{{Role: "writer", Mask: "pypi/app"}}}
	must(t, s.CreateCIPolicy(ctx, &other))
	got, err = s.GetCIPolicy(ctx, "acme-write")
	must(t, err)
	if got.Claims == nil || len(got.Claims) != 0 {
		t.Errorf("a policy without claims reads back %+v, want them empty, not nil", got)
	}
	other.Claims = map[string][]string{}
	same(t, "a pending policy that writes", other, *got)
	list, err := s.ListCIPolicies(ctx)
	must(t, err)
	same(t, "ListCIPolicies", []int64{p.ID, other.ID}, ids(list, func(c index.CIPolicy) int64 { return c.ID }))
	wantErr(t, "CreateCIPolicy of a name taken", s.CreateCIPolicy(ctx, &index.CIPolicy{Name: "app-release", Issuer: "x",
		CreatedBy: "user:mallory"}), index.ErrConflict)
	_, err = s.GetCIPolicy(ctx, "none")
	wantErr(t, "GetCIPolicy of no row", err, index.ErrNotFound)

	changed := index.CIPolicy{ID: p.ID, Name: "renamed", CreatedBy: "user:mallory", Issuer: "gitlab",
		Claims: map[string][]string{"project_id": {"7"}}, Grants: []index.CIGrant{{Role: "writer", Mask: "oci/app"}},
		State: index.CIPolicyActive, ExpiresAt: ptr(at(8000)), RenewalExpiresAt: ptr(at(9000)), UpdatedBy: "user:carol",
		ApprovedBy: "user:dave", ApprovedAt: ptr(at(5)), Revision: 1}
	must(t, s.UpdateCIPolicy(ctx, &changed))
	if changed.Revision != 2 {
		t.Errorf("UpdateCIPolicy filled revision %d, want 2", changed.Revision)
	}
	got, err = s.GetCIPolicy(ctx, "app-release")
	must(t, err)
	if got.Name != p.Name || got.CreatedBy != p.CreatedBy || !got.CreatedAt.Equal(p.CreatedAt) {
		t.Errorf("UpdateCIPolicy changed what never changes: %+v", got)
	}
	same(t, "UpdateCIPolicy stores and fills", changed, *got)
	stale := changed
	stale.Revision, stale.State = 1, index.CIPolicyExpired
	wantErr(t, "UpdateCIPolicy of another revision", s.UpdateCIPolicy(ctx, &stale), index.ErrConflict)
	got, err = s.GetCIPolicy(ctx, "app-release")
	must(t, err)
	same(t, "a refused update changes nothing", changed, *got)
	wantErr(t, "UpdateCIPolicy of no row", s.UpdateCIPolicy(ctx, &index.CIPolicy{ID: 1 << 40, Revision: 1}), index.ErrNotFound)

	must(t, s.DeleteCIPolicy(ctx, other.ID))
	wantErr(t, "DeleteCIPolicy of no row", s.DeleteCIPolicy(ctx, other.ID), index.ErrNotFound)
	if v, err := s.AuthVersion(ctx); err != nil || v != v0+4 {
		t.Errorf("auth_version after two creates, an update and a delete: %d, %v; want %d", v, err, v0+4)
	}
}
