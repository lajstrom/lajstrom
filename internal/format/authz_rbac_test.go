package format_test

import (
	"context"
	"fmt"
	"net/http"
	"sync/atomic"
	"testing"

	"github.com/lajstrom/lajstrom/internal/auth"
	"github.com/lajstrom/lajstrom/internal/format"
	"github.com/lajstrom/lajstrom/internal/index"
	"github.com/lajstrom/lajstrom/internal/index/sqlite"
)

var rbacDBs atomic.Int64

// TestAuthorizeRBAC runs format.Authorize with the RBAC evaluator over a real index: the RBAC
// matrix of docs/acceptance.md §3.8 (S-02) and S-04, S-07 and S-08, as the statuses a frontend
// answers. A repo that does not exist is answered by the router before Authorize (401 for
// anonymous, 404 for everyone else: router.notFound); Authorize gives the same answers, except
// to the admin, whom it allows, and whom the router has answered 404 already.
func TestAuthorizeRBAC(t *testing.T) {
	ctx := context.Background()
	st, err := sqlite.Open(ctx, fmt.Sprintf("file:authz-rbac-%d?mode=memory&cache=shared", rbacDBs.Add(1)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	repos := map[string]index.Repo{}
	for _, r := range []struct {
		format, name string
		anon         bool
	}{{"raw", "assets", false}, {"raw", "public", true}, {"pypi", "internal", false}} {
		repo := index.Repo{Format: r.format, Name: r.name, Registry: "local", Prefix: r.format + "/" + r.name, Kind: "hosted", AnonymousRead: r.anon}
		if err := st.CreateRepo(ctx, &repo); err != nil {
			t.Fatal(err)
		}
		repos[r.format+"/"+r.name] = repo
	}
	repos["raw/missing"] = index.Repo{Format: "raw", Name: "missing"}
	for _, b := range [][3]string{
		{"user:root", "admin", "*/*"},
		{"user:reader", "reader", "raw/assets"},
		{"user:writer", "writer", "raw/assets"},
		{"user:maintainer", "maintainer", "raw/assets"},
		{"user:bob", "reader", "pypi/*"},
	} {
		if err := st.CreateBinding(ctx, &index.Binding{Principal: b[0], Role: b[1], RepoMask: b[2]}); err != nil {
			t.Fatal(err)
		}
	}
	deps := format.Deps{Auth: auth.NewRBAC(auth.RBACOptions{Index: st, AnonymousRead: true})}
	u := func(name string) auth.Principal { return auth.Principal{Kind: auth.KindUser, Name: name} }
	password := auth.Credential{Kind: auth.CredPassword}
	deploy := auth.Credential{Kind: auth.CredDeploy, TokenID: 9, RepoID: repos["raw/assets"].ID, Repo: "raw/assets", Scopes: []string{"read", "write"}}
	get, put, del := auth.ActionRead, auth.ActionWrite, auth.ActionDelete

	type ask struct {
		a    auth.Action
		repo string
		want int
	}
	tests := []struct {
		name string
		p    auth.Principal
		c    auth.Credential
		asks []ask
	}{
		// S-02: GET private, PUT private, DELETE private, GET missing repo.
		{"anonymous", auth.Anonymous, auth.Credential{}, []ask{{get, "raw/assets", 401}, {put, "raw/assets", 401}, {del, "raw/assets", 401}, {get, "raw/missing", 401}}},
		{"reader", u("reader"), password, []ask{{get, "raw/assets", 200}, {put, "raw/assets", 403}, {del, "raw/assets", 403}, {get, "raw/missing", 404}}},
		{"writer", u("writer"), password, []ask{{get, "raw/assets", 200}, {put, "raw/assets", 200}, {del, "raw/assets", 403}, {get, "raw/missing", 404}}},
		{"maintainer", u("maintainer"), password, []ask{{get, "raw/assets", 200}, {put, "raw/assets", 200}, {del, "raw/assets", 200}, {get, "raw/missing", 404}}},
		{"admin", u("root"), password, []ask{{get, "raw/assets", 200}, {put, "raw/assets", 200}, {del, "raw/assets", 200}, {get, "raw/missing", 200}}},
		{"user with no binding on the repo", u("nobody"), password, []ask{{get, "raw/assets", 404}, {put, "raw/assets", 404}, {del, "raw/assets", 404}, {get, "raw/missing", 404}}},
		// S-04: a deploy token with write on raw/assets.
		{"deploy token", auth.Principal{Kind: auth.KindToken, Name: "9"}, deploy, []ask{{put, "raw/assets", 200}, {put, "pypi/internal", 404}, {del, "raw/assets", 403}}},
		// S-07: anonymousRead on raw/public.
		{"anonymous and a public repo", auth.Anonymous, auth.Credential{}, []ask{{get, "raw/public", 200}, {put, "raw/public", 401}, {get, "raw/assets", 401}, {get, "raw/missing", 401}}},
		// S-08: bob, reader on pypi/*.
		{"bob", u("bob"), password, []ask{{get, "raw/assets", 404}, {get, "raw/missing", 404}, {get, "pypi/internal", 200}, {put, "pypi/internal", 403}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := auth.WithCredential(auth.WithPrincipal(context.Background(), tt.p), tt.c)
			for _, q := range tt.asks {
				err := format.Authorize(ctx, deps, q.a, repos[q.repo])
				got := http.StatusOK
				if err != nil {
					got = format.StatusOf(err)
				}
				if got != q.want {
					t.Errorf("%s %s: %d (%v), want %d", q.a, q.repo, got, err, q.want)
				}
			}
		})
	}
}
