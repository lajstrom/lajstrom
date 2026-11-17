package auth

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/lajstrom/lajstrom/internal/index"
)

// rowOf returns the users row of the user called name.
func rowOf(t *testing.T, st index.Reader, name string) *index.User {
	t.Helper()
	u, err := st.GetUser(context.Background(), name)
	if err != nil {
		t.Fatal(err)
	}
	return u
}

// TestUsersPasswordRules: the rules of auth.passwords are checked wherever an admin sets a
// password, at a create and a new password, before anything is written; the error names the rule
// (docs/spec/rbac.md §1, Local passwords).
func TestUsersPasswordRules(t *testing.T) {
	ctx := context.Background()
	users, st, _ := newUsers(t)
	createAdminAndAlice(t, users)
	users.PasswordPolicy = PasswordPolicy{MinLength: 12, RequireLettersAndDigits: true}
	before := rowOf(t, st, "alice")
	for _, tt := range []struct{ password, wantErr string }{
		{"alice-pass1", "password: must be at least 12 characters (auth.passwords.minLength)"},
		{"alice-password", "password: must hold at least one letter and one digit"},
	} {
		if _, err := users.Create(ctx, "bob", tt.password, false, cli); !errors.Is(err, ErrInvalid) || !strings.HasPrefix(err.Error(), tt.wantErr) {
			t.Errorf("Create with %q = %v, want %q", tt.password, err, tt.wantErr)
		}
		if err := users.SetPassword(ctx, "alice", tt.password, cli); !errors.Is(err, ErrInvalid) || !strings.HasPrefix(err.Error(), tt.wantErr) {
			t.Errorf("SetPassword with %q = %v, want %q", tt.password, err, tt.wantErr)
		}
	}
	if _, err := st.GetUser(ctx, "bob"); !errors.Is(err, index.ErrNotFound) {
		t.Errorf("bob after the refused creates: %v, want none", err)
	}
	if after := rowOf(t, st, "alice"); after.PasswordHash != before.PasswordHash || after.Revision != before.Revision {
		t.Error("a refused password changed alice")
	}
	if err := users.SetPassword(ctx, "alice", "alice-password-2", cli); err != nil {
		t.Errorf("SetPassword under the rules = %v", err)
	}
}

// TestSetPasswordHistory: every password an admin sets puts the hash it replaces at the head of
// the user's history, cut to three, without comparing (docs/spec/rbac.md §1, Local passwords); a
// deletion empties it.
func TestSetPasswordHistory(t *testing.T) {
	ctx := context.Background()
	users, st, _ := newUsers(t)
	createAdminAndAlice(t, users)
	var hashes []string // alice's, the latest first
	hashes = append(hashes, rowOf(t, st, "alice").PasswordHash)
	if h := rowOf(t, st, "alice").PasswordHistory; h != nil {
		t.Errorf("a new user's history = %q, want none", h)
	}
	for i, password := range []string{"second-password", "third-password", "fourth-password", "alice-password"} {
		if err := users.SetPassword(ctx, "alice", password, cli); err != nil {
			t.Fatal(err)
		}
		row := rowOf(t, st, "alice")
		want := hashes[:min(len(hashes), PasswordHistoryLen)]
		if strings.Join(row.PasswordHistory, " ") != strings.Join(want, " ") {
			t.Errorf("after the password %d: history %q, want %q", i+2, row.PasswordHistory, want)
		}
		hashes = append([]string{row.PasswordHash}, hashes...)
	}
	uid := rowOf(t, st, "alice").UID
	if err := users.Delete(ctx, "alice", cli); err != nil {
		t.Fatal(err)
	}
	if row, err := st.GetUserByUID(ctx, uid); err != nil || row.DeletedAt == nil || row.PasswordHistory != nil {
		t.Errorf("alice after her deletion: %+v, %v; want her deleted, with no history", row, err)
	}
}

// TestHistoryOfAPasswordless: a password set on a user that had none, an OIDC user, puts nothing
// in its history.
func TestHistoryOfAPasswordless(t *testing.T) {
	ctx := context.Background()
	users, st, _ := newUsers(t)
	createAdminAndAlice(t, users)
	dora := index.User{Name: "dora", Kind: KindOIDC, CreatedAt: usersNow}
	if err := st.CreateUser(ctx, &dora); err != nil {
		t.Fatal(err)
	}
	if err := users.SetPassword(ctx, "dora", "dora-password", cli); err != nil {
		t.Fatal(err)
	}
	if row := rowOf(t, st, "dora"); row.PasswordHash == "" || row.PasswordHistory != nil {
		t.Errorf("dora after her first password: history %q, want none", row.PasswordHistory)
	}
}
