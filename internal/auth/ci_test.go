package auth

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"github.com/lajstrom/lajstrom/internal/index"
)

// TestCIPrincipal: ci:<issuer>/<project> parses and prints, and the grammar of docs/spec/rbac.md
// §11 refuses the rest; a ci: principal is never a pattern.
func TestCIPrincipal(t *testing.T) {
	for _, s := range []string{"ci:github/acme/app", "ci:gitlab/acme/platform/sub/app", "ci:gh-enterprise/Acme.Inc/my_app-2"} {
		p, err := ParsePrincipal(s)
		if err != nil || p.Kind != KindCI || p.String() != s || p.IsPattern() {
			t.Errorf("ParsePrincipal(%q) = %+v, %v", s, p, err)
		}
	}
	for _, s := range []string{"ci:github", "ci:GitHub/acme/app", "ci:spiffe/acme/app", "ci:github/", "ci:github/acme//app",
		"ci:github/acme/../app", "ci:github/acme/*", "ci:github/acme app", "ci:github/" + strings.Repeat("a", 256)} {
		if _, err := ParsePrincipal(s); !errors.Is(err, ErrInvalidPrincipal) {
			t.Errorf("ParsePrincipal(%q) = %v, want ErrInvalidPrincipal", s, err)
		}
	}
	if p, err := CIPrincipal("gitlab", "acme/app"); err != nil || p != (Principal{Kind: KindCI, Name: "gitlab/acme/app"}) {
		t.Errorf("CIPrincipal = %+v, %v", p, err)
	}
	if _, err := CIPrincipal("gitlab", "acme/./app"); !errors.Is(err, ErrInvalidPrincipal) {
		t.Errorf("CIPrincipal of a dot segment = %v", err)
	}
}

// TestCILimit: a CI token gets the actions of the roles of its grants whose mask matches the repo;
// 404 where none matches, 403 for an action they lack, and an admin grant is nothing.
func TestCILimit(t *testing.T) {
	c := Credential{Kind: CredCI, Grants: []Grant{
		{Role: RoleWriter, Mask: "pypi/internal"},
		{Role: RoleReader, Mask: "*/*"},
		{Role: RoleMaintainer, Mask: "npm/app"},
		{Role: RoleAdmin, Mask: "raw/*"},
	}}
	for _, tt := range []struct {
		a     Action
		repo  string
		scope bool // ErrScope, else index.ErrNotFound; neither when allow
		allow bool
	}{
		{ActionWrite, "pypi/internal", false, true},
		{ActionRead, "pypi/internal", false, true},
		{ActionDelete, "pypi/internal", true, false},
		{ActionRead, "apt/ubuntu", false, true},
		{ActionWrite, "apt/ubuntu", true, false},
		{ActionDelete, "npm/app", false, true},
		{ActionManage, "npm/app", false, true},
		{ActionWrite, "raw/x", true, false}, // the reader of */* matches; admin grants nothing
	} {
		format, name, _ := strings.Cut(tt.repo, "/")
		err := c.Limit(tt.a, index.Repo{Format: format, Name: name})
		switch {
		case tt.allow && err != nil, !tt.allow && tt.scope && !errors.Is(err, ErrScope):
			t.Errorf("Limit(%s, %s) = %v", tt.a, tt.repo, err)
		}
	}
	narrow := Credential{Kind: CredCI, Grants: []Grant{{Role: RoleWriter, Mask: "pypi/internal"}}}
	if err := narrow.Limit(ActionRead, index.Repo{Format: "pypi", Name: "other"}); !errors.Is(err, index.ErrNotFound) {
		t.Errorf("a repo outside every mask: %v, want index.ErrNotFound", err)
	}
	if err := (Credential{Kind: CredCI}).Limit(ActionRead, index.Repo{Format: "raw", Name: "x"}); !errors.Is(err, index.ErrNotFound) {
		t.Errorf("no grant: %v, want index.ErrNotFound", err)
	}
}

// TestRBACCI: a ci: principal is decided by the grants of its CI token (docs/spec/rbac.md §11,
// step 2): never the system, never a binding, which is logged as granting nothing, nor anonymous
// read; without its credential it gets nothing.
func TestRBACCI(t *testing.T) {
	st := newIndex(t)
	addRepo(t, st, "pypi", "internal", false)
	addRepo(t, st, "raw", "public", true)
	bind(t, st, "ci:github/acme/app", "maintainer", "*/*")
	var logs bytes.Buffer
	r := NewRBAC(RBACOptions{Index: st, AnonymousRead: true, Log: slog.New(slog.NewTextHandler(&logs, nil))})
	p := Principal{Kind: KindCI, Name: "github/acme/app"}
	cred := Credential{Kind: CredCI, Grants: []Grant{{Role: RoleWriter, Mask: "pypi/internal"}}}
	for _, tt := range []struct {
		name string
		c    Credential
		a    Action
		repo string
		want bool
	}{
		{"its grant writes", cred, ActionWrite, "pypi/internal", true},
		{"and reads", cred, ActionRead, "pypi/internal", true},
		{"no delete, whatever the binding", cred, ActionDelete, "pypi/internal", false},
		{"no anonymous read outside its grants", cred, ActionRead, "raw/public", false},
		{"no system", Credential{Kind: CredCI, Grants: []Grant{{Role: RoleMaintainer, Mask: "*/*"}}}, ActionManage, System, false},
		{"without its credential", Credential{}, ActionRead, "pypi/internal", false},
		{"with another credential", Credential{Kind: CredPassword}, ActionRead, "pypi/internal", false},
	} {
		format, name, _ := strings.Cut(tt.repo, "/")
		if tt.repo == System {
			format, name = System, System
		}
		got, err := r.Allowed(WithCredential(context.Background(), tt.c), p, tt.a, format, name)
		if err != nil {
			t.Fatal(err)
		}
		if got != tt.want {
			t.Errorf("%s: Allowed(%s, %s) = %v, want %v", tt.name, tt.a, tt.repo, got, tt.want)
		}
	}
	if !strings.Contains(logs.String(), "principal=ci:github/acme/app") {
		t.Errorf("no warning about the binding of a ci: principal:\n%s", logs.String())
	}
}

// TestBindingsCI: a ci: principal is refused as a binding's, naming the trust policies.
func TestBindingsCI(t *testing.T) {
	s, _, _ := newBindings(t)
	_, err := s.Create(context.Background(), "ci:github/acme/app", RoleReader, "*/*", cli)
	if !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), "trust policies") {
		t.Errorf("Create = %v, want ErrInvalid naming the trust policies", err)
	}
}

// TestCICreatesNoToken: a CI token creates no personal access token and no deploy token, of a
// repo or over a mask (docs/spec/rbac.md §11); the other credentials create a repo's deploy token
// as their rights allow.
func TestCICreatesNoToken(t *testing.T) {
	ci := WithCredential(context.Background(), Credential{Kind: CredCI, Grants: []Grant{{Role: RoleMaintainer, Mask: "*/*"}}})
	for name, f := range map[string]func(context.Context) error{
		"MayCreatePAT": MayCreatePAT, "MayCreateMaskDeploy": MayCreateMaskDeploy, "MayCreateDeploy": MayCreateDeploy,
	} {
		if err := f(ci); !errors.Is(err, ErrScope) {
			t.Errorf("%s with a CI token = %v, want ErrScope", name, err)
		}
	}
	for _, k := range []CredentialKind{"", CredPassword, CredPAT, CredDeploy} {
		if err := MayCreateDeploy(WithCredential(context.Background(), Credential{Kind: k})); err != nil {
			t.Errorf("MayCreateDeploy with %q = %v", k, err)
		}
	}
	if got := createdBy(Credential{Kind: CredCI}); got != "" {
		t.Errorf("createdBy(CredCI) = %q", got)
	}
}
