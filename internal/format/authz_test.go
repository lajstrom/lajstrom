package format_test

import (
	"context"
	"errors"
	"testing"

	"github.com/lajstrom/lajstrom/internal/auth"
	"github.com/lajstrom/lajstrom/internal/format"
	"github.com/lajstrom/lajstrom/internal/index"
)

// authorizer answers allow and err and records the question.
type authorizer struct {
	allow bool
	err   error

	p            auth.Principal
	a            auth.Action
	format, repo string
	calls        int
}

func (z *authorizer) Allowed(_ context.Context, p auth.Principal, a auth.Action, f, r string) (bool, error) {
	z.calls++
	z.p, z.a, z.format, z.repo = p, a, f, r
	return z.allow, z.err
}

func TestAuthorize(t *testing.T) {
	alice := auth.Principal{Kind: auth.KindUser, Name: "alice"}
	deploy := auth.Principal{Kind: auth.KindToken, Name: "42"}
	dbDown := errors.New("index: database is unreachable")
	tests := []struct {
		name      string
		authz     *authorizer // nil: Deps.Auth is nil
		principal *auth.Principal
		want      error // nil means allowed
		unchanged bool  // err is want itself, not wrapped
	}{
		{name: "nil Auth allows everything"},
		{name: "nil Auth allows a user", principal: &alice},
		{name: "allowed anonymous", authz: &authorizer{allow: true}},
		{name: "allowed user", authz: &authorizer{allow: true}, principal: &alice},
		{name: "denied anonymous", authz: &authorizer{}, want: format.ErrUnauthenticated},
		{name: "denied user", authz: &authorizer{}, principal: &alice, want: format.ErrForbidden},
		{name: "denied deploy token", authz: &authorizer{}, principal: &deploy, want: format.ErrForbidden},
		{name: "authorizer error", authz: &authorizer{allow: true, err: dbDown}, principal: &alice, want: dbDown, unchanged: true},
	}
	repo := index.Repo{ID: 1, Format: "fake", Name: "scratch"}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			if tt.principal != nil {
				ctx = auth.WithPrincipal(ctx, *tt.principal)
			}
			var deps format.Deps
			if tt.authz != nil {
				deps.Auth = tt.authz
			}
			err := format.Authorize(ctx, deps, auth.ActionWrite, repo)
			if !errors.Is(err, tt.want) { // errors.Is(nil, nil) is true
				t.Fatalf("Authorize = %v, want %v", err, tt.want)
			}
			if tt.unchanged && err.Error() != tt.want.Error() {
				t.Errorf("Authorize = %q, want the authorizer's error unchanged", err)
			}
			if tt.authz == nil {
				return
			}
			wantP := auth.Anonymous
			if tt.principal != nil {
				wantP = *tt.principal
			}
			z := tt.authz
			if z.calls != 1 || z.p != wantP || z.a != auth.ActionWrite || z.format != "fake" || z.repo != "scratch" {
				t.Errorf("Allowed called %d times with %v %s %s/%s, want once with %v write fake/scratch", z.calls, z.p, z.a, z.format, z.repo, wantP)
			}
		})
	}
}
