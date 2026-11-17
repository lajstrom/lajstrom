package storetest

import (
	"testing"

	"github.com/lajstrom/lajstrom/internal/index"
)

// testUsersPasswordHistory: users.must_change and users.password_history (#1281;
// docs/spec/index-schema.md, A password's history) are what CreateUser and UpdateUser are given,
// in every read of a user; a new user has neither, an empty history is none, and a change of
// them moves the revision as every update of the row does.
func testUsersPasswordHistory(t *testing.T, s index.Store) {
	ctx := t.Context()
	bob := newUser(t, s, "bob")
	checkPasswordHistory(t, s, "a new user", bob.ID, false, nil)

	carol := index.User{Name: "carol", PasswordHash: passwordHash("carol"), Kind: "local", MustChange: true,
		PasswordHistory: []string{passwordHash("before"), passwordHash("long before")}}
	must(t, s.CreateUser(ctx, &carol))
	checkPasswordHistory(t, s, "CreateUser", carol.ID, true, carol.PasswordHistory)

	// A change of the password: the new hash, the replaced one at the head of the list, and
	// must_change, in the one UpdateUser.
	revision := bob.Revision
	bob.PasswordHash, bob.PasswordHistory, bob.MustChange = passwordHash("second"), []string{bob.PasswordHash}, true
	must(t, s.UpdateUser(ctx, &bob))
	checkPasswordHistory(t, s, "UpdateUser", bob.ID, true, []string{passwordHash("bob")})
	if bob.Revision != revision+1 {
		t.Errorf("Revision after UpdateUser = %d, want %d", bob.Revision, revision+1)
	}

	history := []string{passwordHash("second"), passwordHash("bob"), passwordHash("first")}
	bob.PasswordHash, bob.PasswordHistory, bob.MustChange = passwordHash("third"), history, false
	must(t, s.UpdateUser(ctx, &bob))
	checkPasswordHistory(t, s, "UpdateUser of three", bob.ID, false, history)

	// An empty list is no history: NULL, read as nil.
	bob.PasswordHistory = []string{}
	must(t, s.UpdateUser(ctx, &bob))
	checkPasswordHistory(t, s, "UpdateUser of an empty history", bob.ID, false, nil)

	// The lockout's writes leave both.
	must(t, s.TouchUser(ctx, carol.ID, at(10)))
	if _, err := s.FailPassword(ctx, carol.ID, at(11), at(4000), 10); err != nil {
		t.Fatal(err)
	}
	must(t, s.ResetPasswordFailures(ctx, carol.ID, at(12)))
	if _, err := s.UnlockUser(ctx, carol.ID, at(13)); err != nil {
		t.Fatal(err)
	}
	checkPasswordHistory(t, s, "after TouchUser and the lockout", carol.ID, true, carol.PasswordHistory)
}

// checkPasswordHistory checks MustChange and PasswordHistory of the user id in every read of a
// user: by id, by uid, by name when it is live, and in ListUsers.
func checkPasswordHistory(t *testing.T, s index.Store, what string, id int64, mustChange bool, history []string) {
	t.Helper()
	ctx := t.Context()
	byID, err := s.GetUserByID(ctx, id)
	must(t, err)
	byUID, err := s.GetUserByUID(ctx, byID.UID)
	must(t, err)
	byName, err := s.GetUser(ctx, byID.Name)
	must(t, err)
	all, err := s.ListUsers(ctx)
	must(t, err)
	var listed *index.User
	for i := range all {
		if all[i].ID == id {
			listed = &all[i]
		}
	}
	if listed == nil {
		t.Fatalf("%s: ListUsers has no user %d", what, id)
	}
	for read, u := range map[string]*index.User{"GetUserByID": byID, "GetUserByUID": byUID, "GetUser": byName, "ListUsers": listed} {
		if u.MustChange != mustChange {
			t.Errorf("%s: %s MustChange = %v, want %v", what, read, u.MustChange, mustChange)
		}
		same(t, what+": "+read+" PasswordHistory", history, u.PasswordHistory)
	}
}
