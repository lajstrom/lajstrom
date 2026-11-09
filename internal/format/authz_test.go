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

// TestAuthorizeCredential checks the credential's step before the Authorizer: without an
// Authorizer a deploy token reaches its own repo only (404 elsewhere, the message of a repo
// that does not exist), and gets neither delete nor manage there (403); a personal access token
// with read does not write (403); with an Authorizer a denial of the credential asks it
// nothing, and an allowance still asks it.
func TestAuthorizeCredential(t *testing.T) {
	scratch := index.Repo{ID: 10, Format: "raw", Name: "scratch"}
	other := index.Repo{ID: 11, Format: "raw", Name: "other"}
	deploy := auth.Credential{Kind: auth.CredDeploy, TokenID: 20, RepoID: 10, Repo: "raw/scratch", Scopes: []string{"write"}}
	readPAT := auth.Credential{Kind: auth.CredPAT, TokenID: 1, UserID: 1, Scopes: []string{"read"}}
	tests := []struct {
		name   string
		cred   auth.Credential
		action auth.Action
		repo   index.Repo
		authz  *authorizer
		want   error
		msg    string
		asked  int
	}{
		{name: "deploy writes its repo", cred: deploy, action: auth.ActionWrite, repo: scratch},
		{name: "deploy writes another repo", cred: deploy, action: auth.ActionWrite, repo: other, want: index.ErrNotFound,
			msg: "index: not found: repository raw/other"},
		{name: "deploy deletes", cred: deploy, action: auth.ActionDelete, repo: scratch, want: format.ErrForbidden,
			msg: "format: forbidden: delete on raw/scratch: auth: outside the credential's scopes: the deploy token has write"},
		{name: "deploy manages", cred: deploy, action: auth.ActionManage, repo: scratch, want: format.ErrForbidden},
		{name: "deploy reads without read", cred: deploy, action: auth.ActionRead, repo: scratch, want: format.ErrForbidden},
		{name: "pat read writes", cred: readPAT, action: auth.ActionWrite, repo: other, want: format.ErrForbidden},
		{name: "pat read reads", cred: readPAT, action: auth.ActionRead, repo: other},
		{name: "a denial asks no Authorizer", cred: readPAT, action: auth.ActionWrite, repo: other, authz: &authorizer{allow: []auth.Action{auth.ActionWrite}},
			want: format.ErrForbidden},
		{name: "an allowance asks the Authorizer", cred: readPAT, action: auth.ActionRead, repo: other, authz: &authorizer{}, want: index.ErrNotFound, asked: 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := auth.WithCredential(auth.WithPrincipal(context.Background(), auth.Principal{Kind: auth.KindToken, Name: "20"}), tt.cred)
			var deps format.Deps
			if tt.authz != nil {
				deps.Auth = tt.authz
			}
			err := format.Authorize(ctx, deps, tt.action, tt.repo)
			if !errors.Is(err, tt.want) || (tt.want != nil && errors.Is(err, auth.ErrScope) != errors.Is(tt.want, format.ErrForbidden)) {
				t.Fatalf("Authorize = %v, want %v", err, tt.want)
			}
			if tt.msg != "" && err.Error() != tt.msg {
				t.Errorf("message = %q, want %q", err, tt.msg)
			}
			if tt.authz != nil && len(tt.authz.asked) != tt.asked {
				t.Errorf("the Authorizer was asked %d times, want %d", len(tt.authz.asked), tt.asked)
			}
		})
	}
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

// TestAuthorizeScopeOfAUser: a personal access token whose scopes refuse the action tells that
// the repo exists (403) only when its user may read the repo by the bindings; otherwise the
// answer is the 404 of a repo that does not exist (docs/spec/rbac.md §3; #63, R-4 of the
// threat model).
func TestAuthorizeScopeOfAUser(t *testing.T) {
	repo := index.Repo{ID: 10, Format: "raw", Name: "scratch"}
	readPAT := auth.Credential{Kind: auth.CredPAT, TokenID: 1, UserID: 1, Scopes: []string{"read"}}
	alice := auth.Principal{Kind: auth.KindUser, Name: "alice"}
	tests := []struct {
		name  string
		authz *authorizer
		want  error
	}{
		{"the user may read the repo", &authorizer{allow: []auth.Action{auth.ActionRead}}, format.ErrForbidden},
		{"the user may not read the repo", &authorizer{}, index.ErrNotFound},
		{"the bindings cannot be read", &authorizer{err: errors.New("index: down")}, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := auth.WithCredential(auth.WithPrincipal(context.Background(), alice), readPAT)
			err := format.Authorize(ctx, format.Deps{Auth: tt.authz}, auth.ActionWrite, repo)
			switch {
			case tt.want == nil && (err == nil || errors.Is(err, format.ErrForbidden) || errors.Is(err, index.ErrNotFound)):
				t.Fatalf("Authorize = %v, want the Authorizer's error", err)
			case tt.want != nil && !errors.Is(err, tt.want):
				t.Fatalf("Authorize = %v, want %v", err, tt.want)
			}
			if want := []question{{alice, auth.ActionRead, "raw", "scratch"}}; !slices.Equal(tt.authz.asked, want) {
				t.Errorf("Allowed asked %v, want %v", tt.authz.asked, want)
			}
		})
	}
}
