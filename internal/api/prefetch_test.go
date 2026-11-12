package api

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/lajstrom/lajstrom/internal/auth"
	"github.com/lajstrom/lajstrom/internal/index"
	"github.com/lajstrom/lajstrom/internal/proxy"
)

// fakePrefetcher answers a held result per name, or err, and records its calls.
type fakePrefetcher struct {
	calls []prefetchCall
	err   error
}

type prefetchCall struct {
	repo  string
	names []string
	all   bool
	by    string
}

func (p *fakePrefetcher) Prefetch(ctx context.Context, repo index.Repo, names []string, all bool) ([]proxy.PrefetchResult, error) {
	p.calls = append(p.calls, prefetchCall{repo: repo.Format + "/" + repo.Name, names: names, all: all, by: auth.PrincipalFrom(ctx).String()})
	if p.err != nil {
		return nil, p.err
	}
	out := make([]proxy.PrefetchResult, len(names))
	for i, n := range names {
		out[i] = proxy.PrefetchResult{Name: n, Version: "1.0.0", Status: "held"}
	}
	return out, nil
}

// TestPrefetchAPI: POST …/prefetch needs manage on the repo, checked before the body is read,
// takes 1 to 100 names of a proxy repo, and answers the core's results; the core's refusals
// keep their statuses, and a server without a prefetcher answers 501
// (docs/spec/proxy-and-group.md §5.1, http-routing.md §6).
func TestPrefetchAPI(t *testing.T) {
	g := map[string]bool{"user:carol manage npm/npmjs": true, "user:carol read npm/npmjs": true, "user:alice read npm/npmjs": true}
	for k, v := range grants {
		g[k] = v
	}
	pf := &fakePrefetcher{}
	f := newFixture(t, Options{Auth: fakeAuth{grants: g}, Prefetcher: pf})
	withProxies(f)
	body := `{"format": "npm", "name": "npmjs", "registry": "local", "kind": "proxy", "upstream": {"name": "npmjs"}}`
	wantStatus(t, f.do(http.MethodPost, "/api/v1/repos", body, admin), http.StatusCreated)
	f.create(t, "npm", "internal")
	const target = "/api/v1/repos/npm/npmjs/prefetch"

	var got PrefetchAnswer
	decodeJSON(t, f.do(http.MethodPost, target, `{"names": ["left-pad", "@acme/w"], "allVersions": true}`, carol), http.StatusOK, &got)
	wantJSON(t, "the answer", got, `{"results": [{"name": "left-pad", "version": "1.0.0", "status": "held"},
		{"name": "@acme/w", "version": "1.0.0", "status": "held"}]}`)
	if len(pf.calls) != 1 || pf.calls[0].repo != "npm/npmjs" || !pf.calls[0].all || pf.calls[0].by != carol.String() ||
		strings.Join(pf.calls[0].names, ",") != "left-pad,@acme/w" {
		t.Errorf("calls %+v, want one by carol of npm/npmjs with every version", pf.calls)
	}

	many := make([]string, proxy.MaxPrefetchNames+1)
	for i := range many {
		many[i] = fmt.Sprintf("%q", fmt.Sprint("n", i))
	}
	for _, tc := range []struct {
		name, target, body string
		p                  auth.Principal
		status             int
		code, msg          string
	}{
		{"a reader", target, `{"names": ["a"]}`, alice, http.StatusForbidden, codeForbidden, ""},
		{"a reader, a body that does not parse", target, `{`, alice, http.StatusForbidden, codeForbidden, ""},
		{"a stranger", target, `{"names": ["a"]}`, bob, http.StatusNotFound, codeNotFound, ""},
		{"no names", target, `{"names": []}`, carol, http.StatusBadRequest, codeInvalid, "names: 0 of them"},
		{"too many", target, `{"names": [` + strings.Join(many, ",") + `]}`, carol, http.StatusBadRequest, codeInvalid, "names: 101 of them"},
		{"an empty name", target, `{"names": ["a", ""]}`, carol, http.StatusBadRequest, codeInvalid, "names[1]: empty"},
		{"an unknown key", target, `{"names": ["a"], "all": true}`, carol, http.StatusBadRequest, codeInvalid, ""},
		{"a hosted repo", "/api/v1/repos/npm/internal/prefetch", `{"names": ["a"]}`, admin, http.StatusBadRequest, codeInvalid,
			"is a hosted repo"},
		{"no repo", "/api/v1/repos/npm/none/prefetch", `{"names": ["a"]}`, admin, http.StatusNotFound, codeNotFound, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			wantError(t, f.do(http.MethodPost, tc.target, tc.body, tc.p), tc.status, tc.code, tc.msg)
		})
	}
	if len(pf.calls) != 1 {
		t.Errorf("%d calls to the core, want the first only", len(pf.calls))
	}

	pf.err = fmt.Errorf("%w: proxy: npm/npmjs is offline", index.ErrConflict)
	wantError(t, f.do(http.MethodPost, target, `{"names": ["a"]}`, carol), http.StatusConflict, codeConflict, "offline")

	off := newFixture(t, Options{})
	withProxies(off)
	wantStatus(t, off.do(http.MethodPost, "/api/v1/repos", body, admin), http.StatusCreated)
	wantError(t, off.do(http.MethodPost, target, `{"names": ["a"]}`, admin), http.StatusNotImplemented, codeUnavailable,
		"prefetch is not enabled")
}
