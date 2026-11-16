package api

import (
	"net/http"
	"testing"
	"time"

	"github.com/lajstrom/lajstrom/internal/auth"
	"github.com/lajstrom/lajstrom/internal/index"
)

// TestUsersAPIInactive: a user is answered with lastSeenAt, null before any record, and, with
// auth.disableInactiveUsersAfter, inactive: true when it is inactive beyond it; recording an
// activity moves neither the user's revision nor its ETag (docs/spec/rbac.md §1; #1283).
func TestUsersAPIInactive(t *testing.T) {
	ctx := t.Context()
	f := newFixture(t, Options{InactiveUsersAfter: 2160 * time.Hour})
	old := index.User{Name: "olga", Kind: auth.KindLocal, CreatedAt: testNow.Add(-100 * 24 * time.Hour)}
	recent := index.User{Name: "rita", Kind: auth.KindLocal, CreatedAt: testNow.Add(-100 * 24 * time.Hour)}
	for _, u := range []*index.User{&old, &recent} {
		if err := f.st.CreateUser(ctx, u); err != nil {
			t.Fatal(err)
		}
	}
	seen := testNow.Add(-time.Hour)
	if err := f.st.TouchUser(ctx, recent.ID, seen); err != nil {
		t.Fatal(err)
	}
	var list UserList
	decodeJSON(t, f.do(http.MethodGet, "/api/v1/users", "", admin), http.StatusOK, &list)
	byName := map[string]auth.User{}
	for _, u := range list.Users {
		byName[u.Name] = u
	}
	if u := byName["olga"]; !u.Inactive || u.LastSeenAt != nil {
		t.Errorf("olga, never seen, created 100 days ago: %+v; want inactive, lastSeenAt null", u)
	}
	if u := byName["rita"]; u.Inactive || u.LastSeenAt == nil || !u.LastSeenAt.Equal(seen) {
		t.Errorf("rita, seen an hour ago: %+v; want lastSeenAt %v, not inactive", u, seen)
	}
	wantJSON(t, "rita", byName["rita"], `{"uid": "`+recent.UID+`", "name": "rita", "kind": "local", "disabled": false,
		"createdAt": "2025-09-24T12:00:00Z", "lastSeenAt": "2026-01-02T11:00:00Z"}`)

	etag := f.etagOf(t, "/api/v1/users/rita")
	if err := f.st.TouchUser(ctx, recent.ID, testNow); err != nil {
		t.Fatal(err)
	}
	rr := f.do(http.MethodGet, "/api/v1/users/rita", "", admin)
	wantETag(t, "rita after an activity", rr, etag)
	var rita auth.User
	decodeJSON(t, rr, http.StatusOK, &rita)
	if rita.LastSeenAt == nil || !rita.LastSeenAt.Equal(testNow) {
		t.Errorf("rita's lastSeenAt after an activity = %v, want %v", rita.LastSeenAt, testNow)
	}

	off := newFixture(t, Options{})
	if err := off.st.CreateUser(ctx, &index.User{Name: "olga", Kind: auth.KindLocal, CreatedAt: old.CreatedAt}); err != nil {
		t.Fatal(err)
	}
	var u auth.User
	decodeJSON(t, off.do(http.MethodGet, "/api/v1/users/olga", "", admin), http.StatusOK, &u)
	if u.Inactive {
		t.Errorf("olga without the setting: %+v, want no inactive", u)
	}
}
