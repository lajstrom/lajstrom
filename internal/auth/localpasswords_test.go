package auth

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/lajstrom/lajstrom/internal/index"
)

// TestPasswordsOff: with auth.localPasswords false every password is refused, the right one
// included, before the user is read, without argon2id and counted by no throttle, with the
// message that passwords are off; a personal access token in Basic credentials still
// authenticates (docs/spec/rbac.md §1, Local passwords).
func TestPasswordsOff(t *testing.T) {
	ctx := context.Background()
	f := newAuthFixture(t, Options{PasswordsOff: true})
	for _, tt := range []struct{ user, secret string }{
		{"alice", alicePassword}, {"alice", "a wrong password"}, {"dave", alicePassword}, {"bob", bobPassword},
		{"Alice", alicePassword}, {"alice", ""}, {"alice", PATPrefix + "in-place-of-a-password"},
	} {
		for range LoginFailures + 1 {
			_, _, err := f.a.AuthenticatePassword(ctx, tt.user, tt.secret)
			if !errors.Is(err, ErrPasswordsOff) || f.a.RefusalMessage(err, "") != MsgPasswordsOff {
				t.Fatalf("AuthenticatePassword(%s) = %v; want ErrPasswordsOff saying so", tt.user, err)
			}
			if strings.HasPrefix(tt.secret, PATPrefix) {
				continue // in Basic credentials, a token's shape is a token: below
			}
			_, _, err = f.a.Authenticate(ctx, tt.user, tt.secret)
			if !errors.Is(err, ErrPasswordsOff) || f.a.RefusalMessage(err, "") != MsgPasswordsOff {
				t.Fatalf("Authenticate(%s) = %v, message %q; want ErrPasswordsOff saying so", tt.user, err, f.a.RefusalMessage(err, ""))
			}
		}
	}
	if n := f.users.reads.Load(); n != 0 {
		t.Errorf("%d reads of a user, want none: a password is refused before", n)
	}
	if n := f.verifications(); n != 0 {
		t.Errorf("%d verifications, want none", n)
	}
	if n := f.a.logins.len() + f.a.addresses.len(); n != 0 {
		t.Errorf("the throttles count %d keys, want none", n)
	}
	if logs := f.logs.String(); !strings.Contains(logs, `"reason":"passwords off"`) || !strings.Contains(logs, `"account":"unknown"`) ||
		strings.Contains(logs, alicePassword) {
		t.Errorf("the refusal line lacks its reason or account, or has the password:\n%s", logs)
	}
}

// TestPasswordsOffTokens: while local passwords are off, a personal access token in Basic
// credentials is its user, as before.
func TestPasswordsOffTokens(t *testing.T) {
	ctx := context.Background()
	f := newAuthFixture(t, Options{PasswordsOff: true})
	pat := f.users.addToken(PATPrefix, index.Token{ID: 1, Kind: TokenPAT, UserID: ptr(int64(1)), ExpiresAt: ptr(f.clock.Now().Add(time.Hour))})
	if p, c, err := f.a.Authenticate(ctx, "alice", pat); err != nil || p != userP("alice") || c.Kind != CredPAT {
		t.Errorf("a personal access token in Basic credentials = %v %v, %v; want alice's", p, c, err)
	}
}

// TestRefusalMessage: the message of a refusal is the authenticator's when it chose one, the
// caller's otherwise; a refusal while passwords are on is the caller's.
func TestRefusalMessage(t *testing.T) {
	a := newAuthFixture(t, Options{}).a
	_, _, wrong := a.Authenticate(context.Background(), "alice", "a wrong password")
	for _, tt := range []struct {
		err  error
		want string
	}{
		{ErrPasswordsOff, MsgPasswordsOff},
		{wrong, "fallback"},
		{ErrBadCredentials, "fallback"},
		{ErrBound, "fallback"},
	} {
		if got := a.RefusalMessage(tt.err, "fallback"); got != tt.want {
			t.Errorf("RefusalMessage(%v) = %q, want %q", tt.err, got, tt.want)
		}
	}
}

// TestUsersPasswordsOff: while local passwords are off no password is set, a new user's or a
// user's new one, and the refusal names the setting; a service account is made as before.
func TestUsersPasswordsOff(t *testing.T) {
	ctx := context.Background()
	users, st, _ := newUsers(t)
	createAdminAndAlice(t, users)
	users.PasswordsOff = true
	for what, err := range map[string]error{
		"Create":           func() error { _, err := users.Create(ctx, "bob", "bob-password", false, cli); return err }(),
		"Create an admin":  func() error { _, err := users.Create(ctx, "root", "root-password", true, cli); return err }(),
		"SetPassword":      users.SetPassword(ctx, "alice", "a new password", cli),
		"CheckPasswordSet": users.CheckPasswordSet(),
	} {
		if !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), "auth.localPasswords: false") {
			t.Errorf("%s = %v, want ErrInvalid naming auth.localPasswords", what, err)
		}
	}
	if _, err := st.GetUser(ctx, "bob"); !errors.Is(err, index.ErrNotFound) {
		t.Errorf("bob = %v, want no such user", err)
	}
	if _, err := users.CreateService(ctx, "robot", "user:alice", cli); err != nil {
		t.Errorf("CreateService = %v, want a service account", err)
	}
	users.PasswordsOff = false
	if err := users.CheckPasswordSet(); err != nil {
		t.Errorf("CheckPasswordSet with passwords on = %v", err)
	}
}

// TestBootstrapPasswordsOff: while local passwords are off, or the admin mode is on, and no admin
// exists, Arm makes no token valid and says how to make the first admin through the IdP; the
// exchange is closed, the right token included (docs/spec/rbac.md §6, §12).
func TestBootstrapPasswordsOff(t *testing.T) {
	for setting, closeIt := range map[string]func(*Bootstrap){
		"auth.localPasswords: false":  func(b *Bootstrap) { b.PasswordsOff = true },
		"auth.admin.requireMFA: true": func(b *Bootstrap) { b.AdminMFA = true },
	} {
		ctx := context.Background()
		replicas, logs, users, _ := newBootstrap(t, 1)
		b := replicas[0]
		closeIt(b)
		const token = "0123456789abcdef0123456789abcdef"
		if err := b.Arm(ctx, writeToken(t, token)); err != nil {
			t.Fatal(err)
		}
		if _, err := users.Index.GetBootstrapToken(ctx); !errors.Is(err, index.ErrNotFound) {
			t.Errorf("%s: the bootstrap token = %v, want none made", setting, err)
		}
		if l := logs[0].String(); !strings.Contains(l, setting) ||
			!strings.Contains(l, "lajstrom admin binding create --principal user:<name> --role admin --mask '*/*'") {
			t.Errorf("%s: Arm does not say how to make the first admin:\n%s", setting, l)
		}
		if open, err := b.Open(ctx); err != nil || open {
			t.Errorf("%s: Open = %v, %v; want closed", setting, open, err)
		}
		if _, err := b.Exchange(ctx, token, "admin", "admin-password", cli); !errors.Is(err, ErrBootstrapClosed) {
			t.Errorf("%s: Exchange = %v, want ErrBootstrapClosed", setting, err)
		}
	}
}
