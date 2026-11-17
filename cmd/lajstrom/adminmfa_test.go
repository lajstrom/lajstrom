package main

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lajstrom/lajstrom/internal/config"
	"github.com/lajstrom/lajstrom/internal/index"
	"github.com/lajstrom/lajstrom/internal/index/sqlite"
)

// TestCheckAdminMFA: serve refuses auth.admin.requireMFA without auth.oidc.issuer, since no
// request could reach the admin role, and says at info that the mode is on when it is
// (docs/spec/rbac.md §12).
func TestCheckAdminMFA(t *testing.T) {
	idp := config.OIDC{Issuer: "https://idp.example.com"}
	for _, tc := range []struct {
		auth    config.Auth
		wantErr bool
		wantLog string
	}{
		{config.Auth{}, false, ""},
		{config.Auth{OIDC: idp}, false, ""},
		{config.Auth{Admin: config.Admin{RequireMFA: true}}, true, ""},
		{config.Auth{Admin: config.Admin{RequireMFA: true}, OIDC: idp}, false, "the admin mode is on"},
	} {
		var logs bytes.Buffer
		err := checkAdminMFA(tc.auth, slog.New(slog.NewTextHandler(&logs, nil)))
		if (err != nil) != tc.wantErr || tc.wantErr && !errors.Is(err, errAdminMFAWithoutOIDC) {
			t.Errorf("requireMFA %v, issuer %q: %v, want an error %v", tc.auth.Admin.RequireMFA, tc.auth.OIDC.Issuer, err, tc.wantErr)
		}
		if got := logs.String(); tc.wantLog == "" && got != "" || !strings.Contains(got, tc.wantLog) {
			t.Errorf("requireMFA %v, issuer %q: logs %q, want %q", tc.auth.Admin.RequireMFA, tc.auth.OIDC.Issuer, got, tc.wantLog)
		}
	}
	cfg := writeAuthConfig(t, t.TempDir(), "auth:\n  admin:\n    requireMFA: true\n")
	if _, _, err := run(t, nil, "serve", "--config", cfg); !errors.Is(err, errAdminMFAWithoutOIDC) {
		t.Errorf("serve without auth.oidc.issuer = %v, want %v", err, errAdminMFAWithoutOIDC)
	}
}

// TestWarnAdminMFA: under the admin mode serve warns at start of the admin bindings of service
// accounts and local users, which count for nothing, and, while no enabled OIDC user and no
// group has an admin binding, that no request can reach the role; nothing without the mode.
func TestWarnAdminMFA(t *testing.T) {
	ctx := context.Background()
	st, err := sqlite.Open(ctx, "file:"+filepath.Join(t.TempDir(), "lajstrom.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	admin := func(principal string) {
		if err := st.CreateBinding(ctx, &index.Binding{Principal: principal, Role: "admin", RepoMask: "*/*"}); err != nil {
			t.Fatal(err)
		}
	}
	user := func(name, kind string, disabled bool) {
		u := index.User{Name: name, Kind: kind, Disabled: disabled}
		if err := st.CreateUser(ctx, &u); err != nil {
			t.Fatal(err)
		}
		admin("user:" + u.UID)
	}
	on := config.Auth{Admin: config.Admin{RequireMFA: true}}
	const idle, unreachable = "count for nothing while auth.admin.requireMFA is on", "no request can reach the admin role"
	check := func(what string, a config.Auth, want, wantNot string) {
		t.Helper()
		var logs bytes.Buffer
		warnAdminMFA(ctx, a, st, slog.New(slog.NewTextHandler(&logs, nil)))
		got := logs.String()
		if want == "" && got != "" || !strings.Contains(got, want) || wantNot != "" && strings.Contains(got, wantNot) {
			t.Errorf("%s: logged %q, want %q and not %q", what, got, want, wantNot)
		}
	}
	check("no admin binding", on, unreachable, idle)
	user("root", "local", false)
	user("robot", "service", false)
	user("gone", "oidc", true)
	check("a local user, a service account and a disabled OIDC user", on, "bindings=2", "")
	check("unreachable", on, unreachable, "")
	admin("group:admins")
	check("a group's admin binding", on, idle, unreachable)
	check("the mode off", config.Auth{}, "", "")
}
