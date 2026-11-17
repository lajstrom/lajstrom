package main

import (
	"bytes"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lajstrom/lajstrom/internal/config"
)

// TestCheckLocalPasswords: serve refuses auth.localPasswords false without auth.oidc.issuer,
// since no person could log in, and says at info that passwords are off when they are.
func TestCheckLocalPasswords(t *testing.T) {
	for _, tc := range []struct {
		auth    config.Auth
		wantErr bool
		wantLog string
	}{
		{config.Auth{LocalPasswords: true}, false, ""},
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
	dir := t.TempDir()
	cfg := filepath.Join(dir, "lajstrom.yaml")
	body := "server:\n  listen: \":8080\"\n  externalURL: https://art.example.com\ndatabase:\n  driver: sqlite\n  dsn: \"file:" + filepath.Join(dir, "lajstrom.db") + "\"\n" +
		"auth:\n  localPasswords: false\n"
	if err := os.WriteFile(cfg, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
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
