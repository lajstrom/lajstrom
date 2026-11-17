package main

import (
	"bytes"
	"context"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lajstrom/lajstrom/internal/auth"
	"github.com/lajstrom/lajstrom/internal/config"
	"github.com/lajstrom/lajstrom/internal/index"
	"github.com/lajstrom/lajstrom/internal/index/sqlite"
)

// TestWarnApprovers: with auth.approval second-admin, serve says at start that the setting is on,
// and warns while fewer than two users could decide a request; under the admin mode only the OIDC
// users count. Nothing with none.
func TestWarnApprovers(t *testing.T) {
	ctx := context.Background()
	st, err := sqlite.Open(ctx, "file:"+filepath.Join(t.TempDir(), "lajstrom.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	admin := func(name, kind string) {
		t.Helper()
		u := index.User{Name: name, Kind: kind}
		if err := st.CreateUser(ctx, &u); err != nil {
			t.Fatal(err)
		}
		if err := st.CreateBinding(ctx, &index.Binding{Principal: auth.UserPrincipal(&u).String(), Role: auth.RoleAdmin,
			RepoMask: auth.MaskAll}); err != nil {
			t.Fatal(err)
		}
	}
	logs := func(a config.Auth) string {
		var buf bytes.Buffer
		warnApprovers(ctx, a, st, slog.New(slog.NewTextHandler(&buf, nil)))
		return buf.String()
	}
	second := config.Auth{Approval: config.ApprovalSecondAdmin}
	mfa := config.Auth{Approval: config.ApprovalSecondAdmin, Admin: config.Admin{RequireMFA: true}}
	const few = "fewer than two enabled users have an admin binding that counts"
	admin("root", auth.KindLocal)
	if got := logs(second); !strings.Contains(got, few) || !strings.Contains(got, "approvers=1") {
		t.Errorf("one admin: %q", got)
	}
	admin("olga", auth.KindOIDC)
	if got := logs(second); strings.Contains(got, few) || !strings.Contains(got, "wait for a second admin") {
		t.Errorf("two admins: %q", got)
	}
	if got := logs(mfa); !strings.Contains(got, few) || !strings.Contains(got, "approvers=1") {
		t.Errorf("two admins, one an OIDC user, under the admin mode: %q", got)
	}
	if got := logs(config.Auth{Approval: config.ApprovalNone}); got != "" {
		t.Errorf("auth.approval none: %q", got)
	}
}
