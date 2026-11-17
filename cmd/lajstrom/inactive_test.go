package main

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/lajstrom/lajstrom/internal/auth"
	"github.com/lajstrom/lajstrom/internal/config"
	"github.com/lajstrom/lajstrom/internal/index/sqlite"
)

// TestWarnInactiveUsers: with auth.disableInactiveUsersAfter set, serve warns at start how many
// users are inactive and names the last enabled admin among them; with none, or without the
// setting, it says nothing (docs/spec/rbac.md §1, What is shown; #1283).
func TestWarnInactiveUsers(t *testing.T) {
	ctx := t.Context()
	st, err := sqlite.Open(ctx, "file:"+t.TempDir()+"/lajstrom.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	c := config.Default()
	c.Auth.DisableInactiveUsersAfter = 2160 * time.Hour
	users := usersOf(c, st, slog.New(slog.DiscardHandler))
	created := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	users.Now = func() time.Time { return created }
	for _, u := range []struct {
		name  string
		admin bool
	}{{"admin", true}, {"alice", false}} {
		if _, err := users.Create(ctx, u.name, u.name+"-password", u.admin, auth.Origin{Source: "cli"}); err != nil {
			t.Fatal(err)
		}
	}
	warned := func(now time.Time, after time.Duration) string {
		users.Now, users.InactiveAfter = func() time.Time { return now }, after
		var logs bytes.Buffer
		warnInactiveUsers(ctx, users, slog.New(slog.NewTextHandler(&logs, nil)))
		return logs.String()
	}
	if logs := warned(created.Add(24*time.Hour), 2160*time.Hour); logs != "" {
		t.Errorf("no user is inactive, and serve warns: %s", logs)
	}
	if logs := warned(created.Add(3000*time.Hour), 0); logs != "" {
		t.Errorf("without the setting, serve warns: %s", logs)
	}
	logs := warned(created.Add(3000*time.Hour), 2160*time.Hour)
	for _, want := range []string{"level=WARN", "users=2", "auth.disableInactiveUsersAfter=2160h0m0s", "lastEnabledAdmin=admin"} {
		if !strings.Contains(logs, want) {
			t.Errorf("the warning %q lacks %q", logs, want)
		}
	}
	if !usersOf(&config.Config{Auth: config.Auth{ServiceAccounts: config.ServiceAccounts{OnOwnerDisabled: config.OwnerDisable}}},
		st, nil).DisableWithOwner {
		t.Error("usersOf does not carry auth.serviceAccounts.onOwnerDisabled disable")
	}
}

// TestWriteUserTableLastSeen: lajstrom admin user list writes each user's last activity, - before
// any record, and (inactive) after it for an inactive user.
func TestWriteUserTableLastSeen(t *testing.T) {
	seen := time.Date(2026, 11, 16, 9, 12, 30, 0, time.UTC)
	var out bytes.Buffer
	err := writeUserTable(&out, []auth.User{
		{Name: "alice", UID: "u1", Kind: auth.KindLocal, CreatedAt: seen, LastSeenAt: &seen},
		{Name: "olga", UID: "u2", Kind: auth.KindOIDC, CreatedAt: seen, Inactive: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"LAST SEEN", "alice  u1   local  false     2026-11-16T09:12:30Z  2026-11-16T09:12:30Z  -",
		"olga   u2   oidc   false     2026-11-16T09:12:30Z  - (inactive)"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("the table lacks %q:\n%s", want, out.String())
		}
	}
}
