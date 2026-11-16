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

// testCIPolicies: a CI trust policy is created, read back as written, listed by id, changed in
// its issuer, claims and grants but never its name or creator, and deleted; a name is unique;
// every write moves auth_version (docs/spec/rbac.md §11).
func testCIPolicies(t *testing.T, s index.Store) {
	ctx := t.Context()
	v0, err := s.AuthVersion(ctx)
	must(t, err)
	p := newCIPolicy(t, s, "app-release")
	if p.ID == 0 || !p.UpdatedAt.Equal(at(0)) {
		t.Fatalf("CreateCIPolicy filled %+v, want an ID and UpdatedAt = CreatedAt", p)
	}
	got, err := s.GetCIPolicy(ctx, "app-release")
	must(t, err)
	same(t, "GetCIPolicy", p, *got)
	other := index.CIPolicy{Name: "acme-read", Issuer: "gitlab", CreatedBy: "user:bob"}
	must(t, s.CreateCIPolicy(ctx, &other))
	got, err = s.GetCIPolicy(ctx, "acme-read")
	must(t, err)
	if got.Claims == nil || len(got.Claims) != 0 || got.Grants == nil || len(got.Grants) != 0 {
		t.Errorf("a policy without claims or grants reads back %+v, want them empty, not nil", got)
	}
	list, err := s.ListCIPolicies(ctx)
	must(t, err)
	same(t, "ListCIPolicies", []int64{p.ID, other.ID}, ids(list, func(c index.CIPolicy) int64 { return c.ID }))
	wantErr(t, "CreateCIPolicy of a name taken", s.CreateCIPolicy(ctx, &index.CIPolicy{Name: "app-release", Issuer: "x",
		CreatedBy: "user:mallory"}), index.ErrConflict)
	_, err = s.GetCIPolicy(ctx, "none")
	wantErr(t, "GetCIPolicy of no row", err, index.ErrNotFound)

	changed := index.CIPolicy{ID: p.ID, Name: "renamed", CreatedBy: "user:mallory", Issuer: "gitlab",
		Claims: map[string][]string{"project_id": {"7"}}, Grants: []index.CIGrant{{Role: "writer", Mask: "oci/app"}}}
	must(t, s.UpdateCIPolicy(ctx, &changed))
	got, err = s.GetCIPolicy(ctx, "app-release")
	must(t, err)
	if got.Name != p.Name || got.CreatedBy != p.CreatedBy || !got.CreatedAt.Equal(p.CreatedAt) {
		t.Errorf("UpdateCIPolicy changed what never changes: %+v", got)
	}
	same(t, "UpdateCIPolicy stores and fills", changed, *got)
	wantErr(t, "UpdateCIPolicy of no row", s.UpdateCIPolicy(ctx, &index.CIPolicy{ID: 1 << 40}), index.ErrNotFound)

	must(t, s.DeleteCIPolicy(ctx, other.ID))
	wantErr(t, "DeleteCIPolicy of no row", s.DeleteCIPolicy(ctx, other.ID), index.ErrNotFound)
	if v, err := s.AuthVersion(ctx); err != nil || v != v0+4 {
		t.Errorf("auth_version after two creates, an update and a delete: %d, %v; want %d", v, err, v0+4)
	}
}
