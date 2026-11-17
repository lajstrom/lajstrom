package main

import (
	"bytes"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/lajstrom/lajstrom/internal/auth"
	"github.com/lajstrom/lajstrom/internal/config"
	"github.com/lajstrom/lajstrom/internal/index"
)

// writeAuthConfig writes the configuration of writeConfig, whose index database is a SQLite
// file in dir, with the section auth, and returns its path.
func writeAuthConfig(t *testing.T, dir, auth string) string {
	t.Helper()
	cfg := filepath.Join(dir, "lajstrom.yaml")
	body := "server:\n  listen: \":8080\"\n  externalURL: https://art.example.com\ndatabase:\n  driver: sqlite\n  dsn: \"file:" +
		filepath.Join(dir, "lajstrom.db") + "\"\n" + auth
	if err := os.WriteFile(cfg, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return cfg
}

// TestCheckLocalPasswords: serve refuses auth.localPasswords false without auth.oidc.issuer,
// since no person could log in, and says at info that passwords are off when they are.
func TestCheckLocalPasswords(t *testing.T) {
	for _, tc := range []struct {
		auth    config.Auth
		wantErr bool
		wantLog string
	}{
		{config.Auth{LocalPasswords: true}, false, ""},
		{config.Auth{LocalPasswords: true, Lockout: config.Lockout{Duration: 30 * time.Minute}}, false, "the lockout is on"},
		{config.Auth{LocalPasswords: true, OIDC: config.OIDC{Issuer: "https://idp.example.com"}}, false, ""},
		{config.Auth{}, true, ""},
		{config.Auth{OIDC: config.OIDC{Issuer: "https://idp.example.com"}}, false, "local passwords are off"},
	} {
		var logs bytes.Buffer
		err := checkLocalPasswords(tc.auth, slog.New(slog.NewTextHandler(&logs, nil)))
		if (err != nil) != tc.wantErr || tc.wantErr && !errors.Is(err, errPasswordsOffWithoutOIDC) {
			t.Errorf("localPasswords %v, issuer %q: %v, want an error %v", tc.auth.LocalPasswords, tc.auth.OIDC.Issuer, err, tc.wantErr)
		}
		if got := logs.String(); tc.wantLog == "" && got != "" || !strings.Contains(got, tc.wantLog) {
			t.Errorf("localPasswords %v, issuer %q: logs %q, want %q", tc.auth.LocalPasswords, tc.auth.OIDC.Issuer, got, tc.wantLog)
		}
	}
}

// TestAdminUserPasswordsOff: with auth.localPasswords false, admin user create and set-password
// fail naming the setting, before they read a password; a service account is made as before
// (docs/spec/rbac.md §1, Local passwords).
func TestAdminUserPasswordsOff(t *testing.T) {
	cfg := writeAuthConfig(t, t.TempDir(), "auth:\n  localPasswords: false\n")
	if _, _, err := run(t, nil, "migrate", "--config", cfg); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"admin", "user", "create", "--name", "bob", "--password-stdin"},
		{"admin", "user", "create", "--name", "root", "--admin", "--password-stdin"},
		{"admin", "user", "set-password", "bob", "--password-stdin"},
	} {
		stdin := strings.NewReader("a password long enough")
		_, _, err := runIn(t, nil, stdin, append(args, "--config", cfg)...)
		if err == nil || !strings.Contains(err.Error(), "auth.localPasswords: false") || exitStatus(err) != 1 {
			t.Errorf("%s: %v (exit %d), want exit 1 naming auth.localPasswords", strings.Join(args, " "), err, exitStatus(err))
		}
		if stdin.Len() == 0 {
			t.Errorf("%s read the password", strings.Join(args, " "))
		}
	}
	out, _, err := run(t, nil, "admin", "user", "create", "--name", "robot", "--service", "--owner", "group:ops", "--config", cfg)
	if err != nil || !strings.Contains(out, `"kind":"service"`) {
		t.Errorf("a service account: %q, %v", out, err)
	}
	if _, _, err := run(t, nil, "serve", "--config", cfg); !errors.Is(err, errPasswordsOffWithoutOIDC) {
		t.Errorf("serve without auth.oidc.issuer = %v, want %v", err, errPasswordsOffWithoutOIDC)
	}
}

// TestAdminUserPasswordRules: admin user create and set-password check the rules of
// auth.passwords, exit status 1 naming the rule, and with changeAtFirstUse set mustChange on the
// user (docs/spec/rbac.md §1, Local passwords; #1281).
func TestAdminUserPasswordRules(t *testing.T) {
	dir := t.TempDir()
	cfg := writeAuthConfig(t, dir, "auth:\n  passwords:\n    minLength: 12\n    requireLettersAndDigits: true\n    changeAtFirstUse: true\n")
	st := openIndex(t, dir) // migrated
	refused := func(password, rule string, args ...string) {
		t.Helper()
		_, _, err := runIn(t, nil, strings.NewReader(password), append(args, "--password-stdin", "--config", cfg)...)
		if err == nil || !strings.Contains(err.Error(), rule) || exitStatus(err) != 1 {
			t.Errorf("%s with %q: %v (exit %d), want exit 1 naming %s", strings.Join(args, " "), password, err, exitStatus(err), rule)
		}
	}
	refused("short-pw-1", "auth.passwords.minLength", "admin", "user", "create", "--name", "alice")
	refused("no digits at all", "auth.passwords.requireLettersAndDigits", "admin", "user", "create", "--name", "alice")
	if _, _, err := runIn(t, nil, strings.NewReader("a first password 1"), "admin", "user", "create", "--name", "alice", "--password-stdin",
		"--config", cfg); err != nil {
		t.Fatal(err)
	}
	refused("short-pw-2", "auth.passwords.minLength", "admin", "user", "set-password", "alice")
	alice, err := st.GetUser(t.Context(), "alice")
	if err != nil || !alice.MustChange || alice.PasswordHistory != nil {
		t.Fatalf("alice = %+v, %v; want mustChange and no history", alice, err)
	}
	if _, _, err := runIn(t, nil, strings.NewReader("a second password 2"), "admin", "user", "set-password", "alice", "--password-stdin",
		"--config", cfg); err != nil {
		t.Fatal(err)
	}
	if after, err := st.GetUser(t.Context(), "alice"); err != nil || !after.MustChange || len(after.PasswordHistory) != 1 ||
		after.PasswordHistory[0] != alice.PasswordHash {
		t.Errorf("alice after set-password = %+v, %v; want mustChange and her first hash in the history", after, err)
	}
}

// TestAdminUserUnlock: with auth.lockout.duration set, admin user list marks a locked user's
// DISABLED (locked until <time>), and admin user unlock ends the lock, with a user.unlock row
// from the cli; a user that does not exist fails (docs/spec/rbac.md §1, Local passwords).
func TestAdminUserUnlock(t *testing.T) {
	dir := t.TempDir()
	cfg := writeAuthConfig(t, dir, "auth:\n  lockout:\n    duration: 30m\n")
	st := openIndex(t, dir) // migrated
	if _, _, err := runIn(t, nil, strings.NewReader("alice-password"), "admin", "user", "create", "--name", "alice", "--password-stdin",
		"--config", cfg); err != nil {
		t.Fatal(err)
	}
	alice, err := st.GetUser(t.Context(), "alice")
	if err != nil {
		t.Fatal(err)
	}
	until := time.Now().Add(time.Hour).UTC().Truncate(time.Second)
	if _, err := st.FailPassword(t.Context(), alice.ID, time.Now(), until, 1); err != nil {
		t.Fatal(err)
	}
	locked := "false (locked until " + until.Format(time.RFC3339) + ")"
	if out, _, err := run(t, nil, "admin", "user", "list", "--config", cfg); err != nil || !strings.Contains(out, locked) {
		t.Errorf("admin user list = %q, %v; want %q", out, err, locked)
	}
	if _, _, err := run(t, nil, "admin", "user", "unlock", "alice", "--config", cfg); err != nil {
		t.Fatal(err)
	}
	if out, _, err := run(t, nil, "admin", "user", "list", "--config", cfg); err != nil || strings.Contains(out, "locked until") {
		t.Errorf("admin user list after the unlock = %q, %v", out, err)
	}
	evs, err := st.ListAudit(t.Context(), index.AuditFilter{Action: auth.ActionUserUnlock})
	if err != nil || len(evs) != 1 || !strings.Contains(string(evs[0].Details), `"source":"cli"`) ||
		!strings.Contains(string(evs[0].Details), `"lockedUntil":"`+until.Format(time.RFC3339)+`"`) {
		t.Errorf("the user.unlock rows: %+v, %v", evs, err)
	}
	if _, _, err := run(t, nil, "admin", "user", "unlock", "nobody", "--config", cfg); err == nil || err.Error() != "user nobody does not exist" {
		t.Errorf("an unlock of nobody = %v", err)
	}
}
