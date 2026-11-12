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
		_, err := f.st.PutIndexCache(ctx, &index.Cache{RepoID: repo.ID, Scope: name, Kind: "packument", Content: []byte("{}"), SHA256: "s"}, 0)
		must(t, err)
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

	wantError(t, f.do(http.MethodPatch, "/api/v1/repos/npm/npmjs", `{"upstream": {"name": "npmjs2"}}`, carol),
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

// TestPatchUpstreamName: the admin role is asked for a change of upstream.name, not for the
// key: a maintainer who sends back the name the repo has, as a read-modify-write of the
// resource of GET does, changes the other fields; a maintainer who sends another name is 403
// with nothing of the body applied, before the name is checked against the configuration
// (docs/spec/proxy-and-group.md §2.2, rbac.md §7, #778).
func TestPatchUpstreamName(t *testing.T) {
	const stored = `{"name":"npmjs","metadataTTL":"10m","negativeTTL":"2m","offline":false,"allow":[],"deny":["@acme/*"]}`
	tests := []struct {
		name     string
		p        auth.Principal
		body     string
		status   int
		upstream string // the settings after the PATCH
		policy   string // the policy after the PATCH
		events   int    // repo.update events
	}{
		{"the resource of GET sent back with changes", carol,
			`{"anonymousRead": false, "policy": {"maxFileSize": 1048576}, "upstream": {"name": "npmjs", "metadataTTL": "1h",
			  "negativeTTL": "2m", "offline": false, "allow": [], "deny": ["@acme/*"]}}`,
			http.StatusOK,
			`{"name":"npmjs","metadataTTL":"1h","negativeTTL":"2m","offline":false,"allow":[],"deny":["@acme/*"]}`,
			`{"maxFileSize":1048576}`, 1},
		{"the name the repo has, escaped", carol, `{"upstream": {"name": "npmjs"}}`, http.StatusOK, stored, "null", 0},
		{"another name", carol, `{"upstream": {"name": "npmjs2"}}`, http.StatusForbidden, stored, "null", 0},
		{"another name with other changes", carol, `{"policy": {"maxFileSize": 1048576}, "upstream": {"name": "npmjs2", "offline": true}}`,
			http.StatusForbidden, stored, "null", 0},
		{"a name not configured", carol, `{"upstream": {"name": "mirror"}}`, http.StatusForbidden, stored, "null", 0},
		{"the name with a space", carol, `{"upstream": {"name": "npmjs "}}`, http.StatusForbidden, stored, "null", 0},
		{"another name by the admin", admin, `{"upstream": {"name": "npmjs2"}}`, http.StatusOK,
			`{"name":"npmjs2","metadataTTL":"10m","negativeTTL":"2m","offline":false,"allow":[],"deny":["@acme/*"]}`, "null", 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f, repo := newUpstreamFixture(t)
			rr := f.do(http.MethodPatch, "/api/v1/repos/npm/npmjs", tt.body, tt.p)
			if tt.status == http.StatusForbidden {
				wantError(t, rr, tt.status, codeForbidden, "upstream.name: only the admin role chooses the upstream")
			} else {
				wantStatus(t, rr, tt.status)
			}
			var got Repo
			decodeJSON(t, f.do(http.MethodGet, "/api/v1/repos/npm/npmjs", "", admin), http.StatusOK, &got)
			wantJSON(t, "upstream", got.Upstream, tt.upstream)
			wantJSON(t, "policy", got.Policy, tt.policy)
			if n := len(auditOf(t, f.st, "repo.update")); n != tt.events {
				t.Errorf("%d repo.update events, want %d", n, tt.events)
			}
			renamed := tt.p == admin && tt.status == http.StatusOK
			if meta, doc := cached(t, f.st, repo.ID, "left-pad"); meta == renamed || doc == renamed {
				t.Errorf("the cache after the PATCH: row %t, document %t; want both %t", meta, doc, !renamed)
			}
		})
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
