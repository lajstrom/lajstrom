package auth

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
)

// TestWorkloadPrincipals: the grammar of the cert: and k8s: principals and patterns of
// docs/spec/rbac.md §9.
func TestWorkloadPrincipals(t *testing.T) {
	valid := []struct {
		s       string
		pattern bool
	}{
		{"cert:spiffe://cluster-a/node/w1", false},
		{"cert:spiffe://cluster-a.example_1/ns/ci/sa/builder", false},
		{"cert:spiffe://cluster-a/*", true},
		{"cert:spiffe://cluster-a/node/*", true},
		{"cert:corp/node-1.example.com", false},
		{"cert:corp/a/b", false},
		{"cert:corp/*", true},
		{"cert:corp/a/*", true},
		{"k8s:prod/ci/builder", false},
		{"k8s:prod/ci/builder.v2", false},
		{"k8s:prod/*", true},
		{"k8s:prod/ci/*", true},
	}
	for _, tt := range valid {
		p, err := ParsePrincipal(tt.s)
		if err != nil {
			t.Errorf("ParsePrincipal(%q): %v", tt.s, err)
			continue
		}
		if p.String() != tt.s || p.IsPattern() != tt.pattern {
			t.Errorf("ParsePrincipal(%q) = %#v, pattern %v; want the same string, pattern %v", tt.s, p, p.IsPattern(), tt.pattern)
		}
	}
	for _, s := range []string{
		"cert:",
		"cert:spiffe://",
		"cert:spiffe://cluster-a",
		"cert:spiffe://cluster-a/",
		"cert:spiffe://Cluster-A/node",
		"cert:spiffe://cluster-a/node/",
		"cert:spiffe://cluster-a/node//w1",
		"cert:spiffe://cluster-a/node/../w1",
		"cert:spiffe://cluster-a/no*de",
		"cert:spiffe://cluster-a/*/w1",
		"cert:spiffe://*",
		"cert:spiffe://cluster-a/" + strings.Repeat("a", maxSPIFFEID),
		"cert:corp",
		"cert:corp/",
		"cert:corp/Node 1",
		"cert:corp/a*",
		"cert:corp/*/b",
		"cert:corp//*",
		"cert:Corp/a",
		"cert:spiffe/a",
		"cert:corp/" + strings.Repeat("a", MaxCertificateName+1),
		"k8s:prod",
		"k8s:prod/ci",
		"k8s:prod/ci/",
		"k8s:prod/CI/builder",
		"k8s:prod/ci/Builder",
		"k8s:prod/*/builder",
		"k8s:prod/ci/builder/x",
		"k8s:spiffe/ci/builder",
		"k8s:*",
	} {
		if p, err := ParsePrincipal(s); !errors.Is(err, ErrInvalidPrincipal) {
			t.Errorf("ParsePrincipal(%q) = %#v, %v; want ErrInvalidPrincipal", s, p, err)
		}
	}
}

// TestPatternMatches: a pattern matches the principals of its kind below it, at any depth, and
// nothing else.
func TestPatternMatches(t *testing.T) {
	p := func(s string) Principal {
		t.Helper()
		q, err := ParsePrincipal(s)
		if err != nil {
			t.Fatal(err)
		}
		return q
	}
	for _, tt := range []struct {
		pattern, principal string
		want               bool
	}{
		{"cert:spiffe://cluster-a/*", "cert:spiffe://cluster-a/node/w1", true},
		{"cert:spiffe://cluster-a/*", "cert:spiffe://cluster-a/w1", true},
		{"cert:spiffe://cluster-a/node/*", "cert:spiffe://cluster-a/node/w1", true},
		{"cert:spiffe://cluster-a/node/*", "cert:spiffe://cluster-a/nodes/w1", false},
		{"cert:spiffe://cluster-a/*", "cert:spiffe://cluster-ab/node/w1", false},
		{"cert:corp/*", "cert:corp/node-1", true},
		{"cert:corp/*", "cert:corp2/node-1", false},
		{"cert:corp/a/*", "cert:corp/a/b", true},
		{"cert:corp/a/*", "cert:corp/a", false},
		{"k8s:prod/*", "k8s:prod/ci/builder", true},
		{"k8s:prod/ci/*", "k8s:prod/ci/builder", true},
		{"k8s:prod/ci/*", "k8s:prod/cd/builder", false},
		{"k8s:prod/*", "k8s:staging/ci/builder", false},
		{"k8s:prod/*", "k8s:prod/*", false},                   // a pattern is never a request's principal
		{"cert:corp/*", "k8s:corp/ci/builder", false},         // another kind
		{"k8s:prod/ci/builder", "k8s:prod/ci/builder", false}, // not a pattern
	} {
		if got := p(tt.pattern).Matches(p(tt.principal)); got != tt.want {
			t.Errorf("%s matches %s: %v, want %v", tt.pattern, tt.principal, got, tt.want)
		}
	}
	if (Principal{Kind: KindUser, Name: "a/*"}).IsPattern() {
		t.Error("a user is a pattern")
	}
}

// TestRBACWorkload: a cert: or k8s: principal has its own bindings and those of the patterns that
// match it (docs/spec/rbac.md §9, step 4), never admin, and anonymous read as everyone.
func TestRBACWorkload(t *testing.T) {
	st := newIndex(t)
	addRepo(t, st, "oci", "k8s", false)
	addRepo(t, st, "oci", "ghcr", false)
	addRepo(t, st, "oci", "talos", false)
	addRepo(t, st, "raw", "public", true)
	for _, b := range [][3]string{
		{"cert:spiffe://cluster-a/node/*", "reader", "oci/*"},
		{"cert:spiffe://cluster-a/node/w1", "writer", "oci/talos"},
		{"cert:spiffe://cluster-a/*", "reader", "oci/talos"},
		{"cert:corp/*", "reader", "oci/ghcr"},
		{"k8s:prod/ci/*", "reader", "oci/k8s"},
		{"k8s:prod/ci/builder", "maintainer", "oci/ghcr"},
		// Granting nothing: admin is not for these principals.
		{"cert:spiffe://cluster-a/admin/*", "admin", "*/*"},
		{"k8s:prod/kube-system/*", "admin", "*/*"},
	} {
		bind(t, st, b[0], b[1], b[2])
	}
	var logs bytes.Buffer
	r := NewRBAC(RBACOptions{Index: st, AnonymousRead: true, Log: slog.New(slog.NewTextHandler(&logs, nil))})
	cert := func(n string) Principal { return Principal{Kind: KindCert, Name: n} }
	k8s := func(n string) Principal { return Principal{Kind: KindKubernetes, Name: n} }
	for _, tt := range []struct {
		name string
		p    Principal
		a    Action
		repo string
		want bool
	}{
		{"a node of the pattern reads", cert("spiffe://cluster-a/node/w2"), ActionRead, "oci/k8s", true},
		{"its pattern does not write", cert("spiffe://cluster-a/node/w2"), ActionWrite, "oci/talos", false},
		{"its own binding writes", cert("spiffe://cluster-a/node/w1"), ActionWrite, "oci/talos", true},
		{"and the pattern still reads", cert("spiffe://cluster-a/node/w1"), ActionRead, "oci/ghcr", true},
		{"a wider pattern", cert("spiffe://cluster-a/ns/ci/sa/x"), ActionRead, "oci/talos", true},
		{"outside the narrower pattern", cert("spiffe://cluster-a/ns/ci/sa/x"), ActionRead, "oci/k8s", false},
		{"another trust domain", cert("spiffe://cluster-b/node/w1"), ActionRead, "oci/k8s", false},
		{"a plain bundle", cert("corp/node-1"), ActionRead, "oci/ghcr", true},
		{"a plain bundle elsewhere", cert("corp/node-1"), ActionRead, "oci/k8s", false},
		{"a namespace pattern", k8s("prod/ci/runner"), ActionRead, "oci/k8s", true},
		{"its own maintainer", k8s("prod/ci/builder"), ActionDelete, "oci/ghcr", true},
		{"another namespace", k8s("prod/cd/runner"), ActionRead, "oci/k8s", false},
		{"no admin by a pattern", cert("spiffe://cluster-a/admin/x"), ActionManage, "oci/k8s", false},
		{"no system", k8s("prod/kube-system/x"), ActionManage, System, false},
		{"anonymous read", k8s("staging/ci/x"), ActionRead, "raw/public", true},
		{"but not more", k8s("staging/ci/x"), ActionWrite, "raw/public", false},
	} {
		format, name, _ := strings.Cut(tt.repo, "/")
		if tt.repo == System {
			format, name = System, System
		}
		got, err := r.Allowed(context.Background(), tt.p, tt.a, format, name)
		if err != nil {
			t.Fatal(err)
		}
		if got != tt.want {
			t.Errorf("%s: Allowed(%s, %s, %s) = %v, want %v", tt.name, tt.p, tt.a, tt.repo, got, tt.want)
		}
	}
	for _, principal := range []string{"cert:spiffe://cluster-a/admin/*", "k8s:prod/kube-system/*"} {
		if !strings.Contains(logs.String(), "principal="+principal) {
			t.Errorf("no warning about the admin binding of %s:\n%s", principal, logs.String())
		}
	}
}

// TestBindingsWorkload: the bindings service takes cert: and k8s: principals and patterns with
// the three roles, and refuses admin and a principal that breaks the grammar.
func TestBindingsWorkload(t *testing.T) {
	ctx := context.Background()
	s, _, _ := newBindings(t)
	for _, principal := range []string{"cert:spiffe://cluster-a/node/*", "cert:corp/node-1", "k8s:prod/ci/builder", "k8s:prod/*"} {
		b, err := s.Create(ctx, principal, RoleReader, "oci/*", cli)
		if err != nil {
			t.Fatalf("Create(%s): %v", principal, err)
		}
		if b.Principal != principal || b.Name != "" {
			t.Errorf("Create(%s) = %+v", principal, b)
		}
		if list, err := s.List(ctx, principal); err != nil || len(list) != 1 {
			t.Errorf("List(%s) = %+v, %v; want the binding", principal, list, err)
		}
	}
	for _, tt := range []struct{ principal, role, msg string }{
		{"cert:spiffe://cluster-a/*", RoleAdmin, "role: the admin role is not for cert: principals"},
		{"k8s:prod/*", RoleAdmin, "role: the admin role is not for k8s: principals"},
		{"cert:spiffe://cluster-a/*/x", RoleReader, "principal: auth: invalid principal"},
		{"k8s:prod/ci", RoleReader, "principal: auth: invalid principal"},
	} {
		_, err := s.Create(ctx, tt.principal, tt.role, "*/*", cli)
		if !errors.Is(err, ErrInvalid) || !strings.HasPrefix(err.Error(), tt.msg) {
			t.Errorf("Create(%s, %s) = %v, want ErrInvalid starting %q", tt.principal, tt.role, err, tt.msg)
		}
	}
}
