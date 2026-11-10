package api

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/lajstrom/lajstrom/internal/auth"
	"github.com/lajstrom/lajstrom/internal/index"
)

// carol maintains the proxy npm/npmjs: manage on it, not the admin role.
var carol = auth.Principal{Kind: auth.KindUser, Name: "carol"}

// newUpstreamFixture is newProxyFixture with the authorization of the tests and carol's
// grants, and the proxy npm/npmjs of upstream npmjs created, with a cached document of the
// names left-pad and react each and their rendered documents.
func newUpstreamFixture(t *testing.T) (*fixture, *index.Repo) {
	t.Helper()
	g := map[string]bool{"user:carol manage npm/npmjs": true, "user:carol read npm/npmjs": true}
	for k, v := range grants {
		g[k] = v
	}
	f := newFixture(t, Options{Auth: fakeAuth{grants: g}})
	withProxies(f)
	ctx := context.Background()
	body := `{"format": "npm", "name": "npmjs", "registry": "local", "kind": "proxy", "upstream": {"name": "npmjs", "deny": ["@acme/*"]}}`
	wantStatus(t, f.do(http.MethodPost, "/api/v1/repos", body, admin), http.StatusCreated)
	repo, err := f.st.GetRepo(ctx, "npm", "npmjs")
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"left-pad", "react"} {
		must(t, f.st.PutProxyMeta(ctx, &index.ProxyMeta{RepoID: repo.ID, Name: name, Kind: "packument", State: index.ProxyMetaPresent,
			Content: []byte("{}")}))
		must(t, f.st.PutIndexCache(ctx, &index.Cache{RepoID: repo.ID, Scope: name, Kind: "packument", Content: []byte("{}"), SHA256: "s"}))
	}
	return f, repo
}

// cached reports whether the proxy_meta row and the rendered document of name are there.
func cached(t *testing.T, st index.Store, repoID int64, name string) (meta, doc bool) {
	t.Helper()
	ctx := context.Background()
	_, merr := st.GetProxyMeta(ctx, repoID, name, "packument")
	_, derr := st.GetIndexCache(ctx, repoID, name, "packument")
	for _, err := range []error{merr, derr} {
		if err != nil && !errors.Is(err, index.ErrNotFound) {
			t.Fatal(err)
		}
	}
	return merr == nil, derr == nil
}

// TestPatchUpstream: manage changes a proxy's TTLs and lists, merged as a policy is; only the
// admin role changes its upstream's name, which drops the cache of the old upstream
// (docs/spec/proxy-and-group.md §2.2, rbac.md §7).
func TestPatchUpstream(t *testing.T) {
	f, repo := newUpstreamFixture(t)
	var got Repo
	decodeJSON(t, f.do(http.MethodPatch, "/api/v1/repos/npm/npmjs", `{"upstream": {"metadataTTL": "1h", "allow": ["left-*"]}}`, carol),
		http.StatusOK, &got)
	wantJSON(t, "upstream after carol's PATCH", got.Upstream,
		`{"name":"npmjs","metadataTTL":"1h","negativeTTL":"2m","offline":false,"allow":["left-*"],"deny":["@acme/*"]}`)
	if meta, doc := cached(t, f.st, repo.ID, "left-pad"); !meta || !doc {
		t.Errorf("the cache after a change of the TTL: row %t, document %t; want both kept", meta, doc)
	}
	ev := auditOf(t, f.st, "repo.update")[0]
	if up, _ := ev.details["changed"].(map[string]any)["upstream"].(map[string]any); up["metadataTTL"] != "1h" {
		t.Errorf("audit details %v, want the settings as the merge left them", ev.details)
	}

	wantError(t, f.do(http.MethodPatch, "/api/v1/repos/npm/npmjs", `{"upstream": {"name": "npmjs"}}`, carol),
		http.StatusForbidden, codeForbidden, "upstream.name: only the admin role")
	wantError(t, f.do(http.MethodPatch, "/api/v1/repos/npm/npmjs", `{"upstream": {"name": "mirror"}}`, admin),
		http.StatusBadRequest, codeInvalid, `upstream.name: "mirror" is not a configured upstream`)
	decodeJSON(t, f.do(http.MethodPatch, "/api/v1/repos/npm/npmjs", `{"upstream": {"name": "npmjs2", "allow": null}}`, admin),
		http.StatusOK, &got)
	wantJSON(t, "upstream after the admin's PATCH", got.Upstream,
		`{"name":"npmjs2","metadataTTL":"1h","negativeTTL":"2m","offline":false,"allow":[],"deny":["@acme/*"]}`)
	for _, name := range []string{"left-pad", "react"} {
		if meta, doc := cached(t, f.st, repo.ID, name); meta || doc {
			t.Errorf("%s after a change of the upstream: row %t, document %t; want neither", name, meta, doc)
		}
	}
}

func TestPatchUpstreamInvalid(t *testing.T) {
	tests := []struct{ name, repo, body, want string }{
		{"on a hosted repo", "raw/scratch", `{"upstream": {"offline": true}}`, "upstream: only a proxy repo has one, not a hosted repo"},
		{"null", "npm/npmjs", `{"upstream": null}`, "upstream: a proxy repo must have one; it cannot be removed"},
		{"not an object", "npm/npmjs", `{"upstream": ["@acme/*"]}`, "upstream: must be a JSON object"},
		{"a URL", "npm/npmjs", `{"upstream": {"url": "http://10.0.0.1/"}}`, `upstream: "url" is not a key`},
		{"a TTL", "npm/npmjs", `{"upstream": {"metadataTTL": "1y"}}`, `upstream.metadataTTL: "1y" is not a duration`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f, _ := newUpstreamFixture(t)
			f.create(t, "raw", "scratch")
			wantError(t, f.do(http.MethodPatch, "/api/v1/repos/"+tt.repo, tt.body, admin), http.StatusBadRequest, codeInvalid, tt.want)
			if rows := auditOf(t, f.st, "repo.update"); len(rows) != 0 {
				t.Errorf("repo.update events after a refused PATCH: %+v", rows)
			}
		})
	}
}

// TestPurgeUpstreamCache: DELETE …/upstream-cache drops one name's cached documents with
// ?name=, or the repo's; manage on the proxy is enough; a repo that is not a proxy is 400
// (docs/spec/http-routing.md §6).
func TestPurgeUpstreamCache(t *testing.T) {
	f, repo := newUpstreamFixture(t)
	f.create(t, "raw", "scratch")
	wantStatus(t, f.do(http.MethodDelete, "/api/v1/repos/npm/npmjs/upstream-cache?name=left-pad", "", carol), http.StatusNoContent)
	if meta, doc := cached(t, f.st, repo.ID, "left-pad"); meta || doc {
		t.Errorf("left-pad after its purge: row %t, document %t; want neither", meta, doc)
	}
	if meta, doc := cached(t, f.st, repo.ID, "react"); !meta || !doc {
		t.Errorf("react after the purge of left-pad: row %t, document %t; want both", meta, doc)
	}
	ev := oneAudit(t, f.st, "repo.cache.purge")
	if ev.Principal != f.asStored(carol).String() || ev.Repo != "npm/npmjs" || ev.details["name"] != "left-pad" {
		t.Errorf("audit event %+v, details %v; want carol's purge of left-pad", ev.AuditEvent, ev.details)
	}
	wantStatus(t, f.do(http.MethodDelete, "/api/v1/repos/npm/npmjs/upstream-cache", "", carol), http.StatusNoContent)
	if meta, doc := cached(t, f.st, repo.ID, "react"); meta || doc {
		t.Errorf("react after the purge of the repo: row %t, document %t; want neither", meta, doc)
	}
	wantError(t, f.do(http.MethodDelete, "/api/v1/repos/raw/scratch/upstream-cache", "", admin), http.StatusBadRequest, codeInvalid,
		"repo raw/scratch is a hosted repo: only a proxy repo has an upstream cache")
	wantStatus(t, f.do(http.MethodDelete, "/api/v1/repos/npm/npmjs/upstream-cache", "", bob), http.StatusNotFound)
	if n := len(auditOf(t, f.st, "repo.cache.purge")); n != 2 {
		t.Errorf("%d repo.cache.purge events, want 2", n)
	}
}
