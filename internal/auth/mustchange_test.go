package auth

import (
	"context"
	"errors"
	"testing"
)

// TestChangeAtFirstUse: with auth.passwords.changeAtFirstUse, every password an admin sets, at a
// create and a new password, sets mustChange, which user.password's details say; without it an
// admin's new password clears it (docs/spec/rbac.md §1, Change at first use; §5).
func TestChangeAtFirstUse(t *testing.T) {
	ctx := context.Background()
	users, st, _ := newUsers(t)
	createAdminAndAlice(t, users)
	if rowOf(t, st, "alice").MustChange {
		t.Error("alice must change her password without the setting")
	}
	users.PasswordPolicy.ChangeAtFirstUse = true
	if _, err := users.Create(ctx, "bob", "bob-password", false, cli); err != nil {
		t.Fatal(err)
	}
	if err := users.SetPassword(ctx, "alice", "alice-password-2", cli); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"alice", "bob"} {
		if !rowOf(t, st, name).MustChange {
			t.Errorf("%s need not change the password an admin set with the setting", name)
		}
	}
	users.PasswordPolicy.ChangeAtFirstUse = false
	if err := users.SetPassword(ctx, "bob", "bob-password-2", cli); err != nil {
		t.Fatal(err)
	}
	if rowOf(t, st, "bob").MustChange {
		t.Error("bob must change the password an admin set without the setting")
	}
	wantAudit(t, st,
		`anonymous user.password user:bob {"name":"bob","source":"cli"}`,
		`anonymous user.password user:alice {"mustChange":true,"name":"alice","source":"cli"}`,
		`anonymous user.create user:bob {"admin":false,"mustChange":true,"name":"bob","source":"cli"}`,
		`anonymous user.create user:alice {"admin":false,"name":"alice","source":"cli"}`,
		`anonymous user.create user:admin {"admin":true,"name":"admin","source":"cli"}`)
}

// TestChangeMustChange: an admin sets and clears mustChange whatever the setting, with a
// user.mustchange row whose details have the new value; a value the user has already writes none;
// a user without a password has none to change (ErrInvalid), and nothing else of the change is
// made (docs/spec/rbac.md §1, Change at first use; §5).
func TestChangeMustChange(t *testing.T) {
	ctx := context.Background()
	users, st, _ := newUsers(t)
	createAdminAndAlice(t, users)
	if _, err := users.CreateService(ctx, "robot", "user:admin", cli); err != nil {
		t.Fatal(err)
	}
	yes, no := true, false
	revision := rowOf(t, st, "alice").Revision
	if _, err := users.Change(ctx, "alice", UserChange{MustChange: &yes}, cli); err != nil {
		t.Fatal(err)
	}
	if row := rowOf(t, st, "alice"); !row.MustChange || row.Revision != revision+1 {
		t.Errorf("after PATCH true: mustChange %v, revision %d; want true, %d", row.MustChange, row.Revision, revision+1)
	}
	if _, err := users.Change(ctx, "alice", UserChange{MustChange: &yes}, cli); err != nil {
		t.Fatal(err)
	}
	if _, err := users.Change(ctx, "alice", UserChange{MustChange: &no}, cli); err != nil {
		t.Fatal(err)
	}
	if rowOf(t, st, "alice").MustChange {
		t.Error("alice must change her password after PATCH false")
	}
	disabled := true
	_, err := users.Change(ctx, "robot", UserChange{Disabled: &disabled, MustChange: &no}, cli)
	if !errors.Is(err, ErrInvalid) || err.Error() != "mustChange: user robot has no password, and only a password can be made to change" {
		t.Errorf("mustChange of a service account = %v, want ErrInvalid", err)
	}
	if rowOf(t, st, "robot").Disabled {
		t.Error("the refused change disabled robot")
	}
	wantAudit(t, st,
		`anonymous user.mustchange user:alice {"mustChange":false,"name":"alice","source":"cli"}`,
		`anonymous user.mustchange user:alice {"mustChange":true,"name":"alice","source":"cli"}`,
		`anonymous user.create user:robot {"kind":"service","name":"robot","owner":"user:admin","source":"cli"}`,
		`anonymous user.create user:alice {"admin":false,"name":"alice","source":"cli"}`,
		`anonymous user.create user:admin {"admin":true,"name":"admin","source":"cli"}`)
}
