package format_test

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/lajstrom/lajstrom/internal/auth"
	"github.com/lajstrom/lajstrom/internal/format"
	"github.com/lajstrom/lajstrom/internal/index"
)

// authorizer allows the actions in allow, answers err to the question about errOn (every
// question when errOn is empty), and records the questions it was asked.
type authorizer struct {
	allow []auth.Action
	err   error
	errOn auth.Action

	asked []question
}

// question is one call of Allowed.
type question struct {
	p            auth.Principal
	a            auth.Action
	format, repo string
}

func (z *authorizer) Allowed(_ context.Context, p auth.Principal, a auth.Action, f, r string) (bool, error) {
	z.asked = append(z.asked, question{p, a, f, r})
	if z.err != nil && (z.errOn == "" || z.errOn == a) {
		return false, z.err
	}
	return slices.Contains(z.allow, a), nil
}

func TestAuthorize(t *testing.T) {
	alice := auth.Principal{Kind: auth.KindUser, Name: "alice"}
	deploy := auth.Principal{Kind: auth.KindToken, Name: "42"}
	dbDown := errors.New("index: database is unreachable")
	read, write, del := auth.ActionRead, auth.ActionWrite, auth.ActionDelete
	actions := func(as ...auth.Action) []auth.Action { return as }
	tests := []struct {
		name      string
		authz     *authorizer // nil: Deps.Auth is nil
		principal *auth.Principal
		action    auth.Action
		want      error         // nil means allowed
		unchanged bool          // err is want itself, not wrapped
		asked     []auth.Action // the questions, in order
	}{
		{name: "nil Auth allows everything", action: write},
		{name: "nil Auth allows a user", principal: &alice, action: write},
		{name: "allowed anonymous read", authz: &authorizer{allow: actions(read)}, action: read, asked: actions(read)},
		{name: "allowed user write", authz: &authorizer{allow: actions(read, write)}, principal: &alice, action: write, asked: actions(write)},
		{name: "denied anonymous read", authz: &authorizer{}, action: read, want: format.ErrUnauthenticated, asked: actions(read)},
		{name: "denied anonymous write", authz: &authorizer{}, action: write, want: format.ErrUnauthenticated, asked: actions(write)},
		{name: "user with read lacks write", authz: &authorizer{allow: actions(read)}, principal: &alice, action: write, want: format.ErrForbidden, asked: actions(write, read)},
		{name: "deploy token with read lacks delete", authz: &authorizer{allow: actions(read)}, principal: &deploy, action: del, want: format.ErrForbidden, asked: actions(del, read)},
		{name: "user without access asks write", authz: &authorizer{}, principal: &alice, action: write, want: index.ErrNotFound, asked: actions(write, read)},
		{name: "user without access asks read", authz: &authorizer{}, principal: &alice, action: read, want: index.ErrNotFound, asked: actions(read)},
		{name: "authorizer error", authz: &authorizer{allow: actions(write), err: dbDown}, principal: &alice, action: write, want: dbDown, unchanged: true, asked: actions(write)},
		{name: "authorizer error on the read question", authz: &authorizer{err: dbDown, errOn: read}, principal: &alice, action: write, want: dbDown, unchanged: true, asked: actions(write, read)},
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
			err := format.Authorize(ctx, deps, tt.action, repo)
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
			var got []auth.Action
			for _, q := range tt.authz.asked {
				got = append(got, q.a)
				if q.p != wantP || q.format != "fake" || q.repo != "scratch" {
					t.Errorf("Allowed asked about %v %s %s/%s, want %v on fake/scratch", q.p, q.a, q.format, q.repo, wantP)
				}
			}
			if !slices.Equal(got, tt.asked) {
				t.Errorf("Allowed asked %v, want %v", got, tt.asked)
			}
		})
	}
}
