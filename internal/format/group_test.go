package format_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/lajstrom/lajstrom/internal/auth"
	"github.com/lajstrom/lajstrom/internal/format"
	"github.com/lajstrom/lajstrom/internal/index"
	"github.com/lajstrom/lajstrom/internal/index/sqlite"
)

// TestWriteTarget: a write sent to a hosted repo stays there; one sent to a group goes to its
// hosted member once the principal may read the group, marked WithVia, and the member decides
// the write, a principal that cannot read the member getting 403 naming it rather than 404; a
// proxy, and a group without a hosted member, are 405 to a reader and denied to anyone else
// (docs/spec/proxy-and-group.md §8.4, rbac.md §7).
func TestWriteTarget(t *testing.T) {
	ctx := context.Background()
	st, err := sqlite.Open(ctx, fmt.Sprintf("file:write-target-%d?mode=memory&cache=shared", rbacDBs.Add(1)))
	must(t, err)
	t.Cleanup(func() { _ = st.Close() })
	must(t, st.Migrate(ctx))
	repos := map[string]index.Repo{}
	for _, r := range []index.Repo{
		{Name: "internal", Registry: "local", Kind: index.KindHosted},
		{Name: "npmjs", Registry: "local", Kind: index.KindProxy},
		{Name: "all", Kind: index.KindGroup},
		{Name: "proxies", Kind: index.KindGroup},
	} {
		r.Format, r.Prefix = "npm", "npm/"+r.Name
		must(t, st.CreateRepo(ctx, &r))
		repos[r.Name] = r
	}
	must(t, st.SetGroupMembers(ctx, repos["all"].ID, []int64{repos["npmjs"].ID, repos["internal"].ID}))
	must(t, st.SetGroupMembers(ctx, repos["proxies"].ID, []int64{repos["npmjs"].ID}))
	for _, b := range [][3]string{
		{"user:writer", "reader", "npm/all"}, {"user:writer", "writer", "npm/internal"},
		{"user:reader", "reader", "npm/all"}, {"user:reader", "reader", "npm/proxies"}, {"user:reader", "reader", "npm/npmjs"},
		{"user:peer", "reader", "npm/all"}, {"user:peer", "reader", "npm/internal"},
	} {
		must(t, st.CreateBinding(ctx, &index.Binding{Principal: b[0], Role: b[1], RepoMask: b[2]}))
	}
	deps := format.Deps{Index: st, Auth: auth.NewRBAC(auth.RBACOptions{Index: st})}

	tests := []struct {
		user, repo string
		target     string // the repo WriteTarget answers
		want       error  // of WriteTarget
		write      error  // of Authorize(write) on the target
		msg        string // in the error of the write
	}{
		{user: "writer", repo: "internal", target: "internal"},
		{user: "writer", repo: "all", target: "internal"},
		{user: "reader", repo: "all", target: "internal", write: format.ErrForbidden, msg: "the hosted member of the group npm/all"},
		{user: "peer", repo: "all", target: "internal", write: format.ErrForbidden},
		{user: "", repo: "all", want: format.ErrUnauthenticated},
		{user: "nobody", repo: "all", want: index.ErrNotFound},
		{user: "reader", repo: "npmjs", want: format.ErrMethodNotAllowed},
		{user: "writer", repo: "npmjs", want: index.ErrNotFound},
		{user: "reader", repo: "proxies", want: format.ErrMethodNotAllowed},
	}
	for _, tt := range tests {
		t.Run(tt.user+" "+tt.repo, func(t *testing.T) {
			p := auth.Anonymous
			if tt.user != "" {
				p = auth.Principal{Kind: auth.KindUser, Name: tt.user}
			}
			ctx := auth.WithCredential(auth.WithPrincipal(ctx, p), auth.Credential{Kind: auth.CredPassword})
			wctx, target, err := format.WriteTarget(ctx, deps, repos[tt.repo])
			if !errors.Is(err, tt.want) {
				t.Fatalf("WriteTarget = %v, want %v", err, tt.want)
			}
			if err != nil {
				return
			}
			group, via := format.ViaFrom(wctx)
			if target.ID != repos[tt.target].ID || via != (repos[tt.repo].Kind == index.KindGroup) || (via && group.Name != tt.repo) {
				t.Errorf("WriteTarget = %s, via %v %s; want %s", target.Name, via, group.Name, tt.target)
			}
			err = format.Authorize(wctx, deps, auth.ActionWrite, target)
			if !errors.Is(err, tt.write) || (err != nil && !strings.Contains(err.Error(), tt.msg)) {
				t.Errorf("Authorize(write) on %s = %v, want %v %q", target.Name, err, tt.write, tt.msg)
			}
		})
	}
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
