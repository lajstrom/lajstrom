package main

import (
	"bytes"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"github.com/lajstrom/lajstrom/internal/config"
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
