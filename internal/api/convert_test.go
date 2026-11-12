package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/opencontainers/go-digest"

	"github.com/lajstrom/lajstrom/internal/auth"
	"github.com/lajstrom/lajstrom/internal/format"
	"github.com/lajstrom/lajstrom/internal/index"
)

// stubAdopter is the stub proxier with format.Adopter: its kept document is {"tags": {tag:
// version}}, an add op per tag whose version is held, the others skipped.
type stubAdopter struct{ stubProxier }

func (stubAdopter) AdoptKind() string { return "packument" }

func (stubAdopter) AdoptOps(_ string, kept []byte, held []string) ([]format.JournalOp, []string, error) {
	var doc struct{ Tags map[string]string }
	if err := json.Unmarshal(kept, &doc); err != nil {
		return nil, nil, format.ErrInvalid
	}
	var ops []format.JournalOp
	var skipped []string
	for _, tag := range []string{"latest", "next"} {
		v, ok := doc.Tags[tag]
		switch {
		case !ok:
		case slices.Contains(held, v):
			ops = append(ops, format.JournalOp{ArtifactType: "dist-tags", Op: "add", Tag: tag, Version: v})
		default:
			skipped = append(skipped, "dist-tag "+tag+": "+v+" is not held")
		}
	}
	return ops, skipped, nil
}

// fakeJournal records the journal writes of a conversion, or fails them with err.
type fakeJournal struct {
	calls []string // "<repo kind> <package path> <ops> <principal>"
	err   error
}

func (j *fakeJournal) Journal(_ context.Context, repo index.Repo, pkgPath string, ops []format.JournalOp, p auth.Principal) ([]int64, error) {
	if j.err != nil {
		return nil, j.err
	}
	var s []string
	for _, op := range ops {
		s = append(s, op.Op+" "+op.Tag+" "+op.Version)
	}
	j.calls = append(j.calls, repo.Kind+" "+pkgPath+" "+strings.Join(s, ",")+" "+p.String())
	return make([]int64, len(ops)), nil
}

// newConvertFixture is the API with stubAdopter for npm and j as its Journal, the proxy
// npm/<proxy> of the upstream npmjs created by the admin, holding a (1.0.0, 2.0.0) with a kept
// document whose latest is 1.0.0 and next 9.9.9, and b (1.0.0) with none.
func newConvertFixture(t *testing.T, j Journaler, proxy string) (*fixture, *index.Repo) {
	t.Helper()
	g := map[string]bool{"user:carol manage npm/" + proxy: true, "user:carol read npm/" + proxy: true}
	for k, v := range grants {
		g[k] = v
	}
	f := newFixture(t, Options{Auth: fakeAuth{grants: g}, Journal: j})
	withProxies(f)
	f.repos.Groupers = oneOf("npm")
	f.repos.Proxiers = func(name string) (format.Proxier, bool) { return stubAdopter{}, name == "npm" }
	body := `{"format": "npm", "name": "` + proxy + `", "registry": "local", "kind": "proxy", "upstream": {"name": "npmjs"}}`
	wantStatus(t, f.do(http.MethodPost, "/api/v1/repos", body, admin), http.StatusCreated)
	ctx := context.Background()
	repo, err := f.st.GetRepo(ctx, "npm", proxy)
	must(t, err)
	for name, versions := range map[string][]string{"a": {"1.0.0", "2.0.0"}, "b": {"1.0.0"}} {
		pkg := index.Package{RepoID: repo.ID, Name: name, NameOCI: name}
		must(t, f.st.UpsertPackage(ctx, &pkg))
		for _, v := range versions {
			must(t, f.st.UpsertVersion(ctx, &index.Version{PackageID: pkg.ID, Version: v, Tag: v, Digest: digest.FromString(name + v),
				ArtifactType: "application/vnd.lajstrom.npm.v1"}))
		}
	}
	must(t, f.st.PutProxyMeta(ctx, &index.ProxyMeta{RepoID: repo.ID, Name: "a", Kind: "packument", State: index.ProxyMetaPresent,
		Content: []byte(`{"tags": {"latest": "1.0.0", "next": "9.9.9"}}`), CheckedAt: testNow}))
	return f, repo
}

// TestConvertProxy: the admin turns a proxy into a hosted repo with {"kind": "hosted"} alone
// (docs/spec/proxy-and-group.md §2.6): refused to a maintainer, beside another key, for
// another kind, on a group, while a fetch runs and while a group of it has a hosted member;
// then the row is hosted without its upstream, the ops of the kept documents go to the journal
// by the admin on the hosted repo, the cache is dropped, and one audit row repo.convert says
// what was planned and left out. A hosted repo without a cache changes nothing.
func TestConvertProxy(t *testing.T) {
	j := &fakeJournal{}
	f, repo := newConvertFixture(t, j, "legacy")
	const target = "/api/v1/repos/npm/legacy"
	wantConvertRefused(t, f, repo, j)

	var got Repo
	decodeJSON(t, f.do(http.MethodPatch, target, `{"kind": "hosted"}`, admin), http.StatusOK, &got)
	if got.Kind != index.KindHosted || got.Upstream != nil {
		t.Errorf("the resource %+v, want hosted without upstream", got)
	}
	if want := []string{"hosted a add latest 1.0.0 " + admin.String()}; !slices.Equal(j.calls, want) {
		t.Errorf("journal %q, want %q", j.calls, want)
	}
	if meta, _ := cached(t, f.st, repo.ID, "a"); meta {
		t.Error("the cached document of a after the conversion, want it dropped")
	}
	ev := oneAudit(t, f.st, actionConvert)
	if ev.details["from"] != "proxy" || ev.details["to"] != "hosted" || ev.details["upstream"] != "npmjs" ||
		ev.details["packages"] != float64(2) || ev.details["ops"] != float64(1) {
		t.Errorf("audit details %v", ev.details)
	}
	skipped, _ := json.Marshal(ev.details["skipped"])
	for _, want := range []string{"a: dist-tag next: 9.9.9 is not held", "b: no cached document"} {
		if !strings.Contains(string(skipped), want) {
			t.Errorf("skipped %s, want %q", skipped, want)
		}
	}

	decodeJSON(t, f.do(http.MethodPatch, target, `{"kind": "hosted"}`, admin), http.StatusOK, &got)
	if len(j.calls) != 1 || len(auditOf(t, f.st, actionConvert)) != 1 {
		t.Errorf("again on the hosted repo: journal %q, %d audit rows; want nothing new", j.calls, len(auditOf(t, f.st, actionConvert)))
	}
}

// TestConvertResume: a journal that fails after the flip leaves a hosted repo with its cached
// documents, and the same PATCH writes the ops from them and drops them; a server without a
// journal answers 501.
func TestConvertResume(t *testing.T) {
	j := &fakeJournal{err: errors.New("the registry is away")}
	f, repo := newConvertFixture(t, j, "legacy")
	const target = "/api/v1/repos/npm/legacy"
	wantError(t, f.do(http.MethodPatch, target, `{"kind": "hosted"}`, admin), http.StatusInternalServerError, codeInternal, "")
	row, err := f.st.GetRepo(context.Background(), "npm", "legacy")
	must(t, err)
	if meta, _ := cached(t, f.st, repo.ID, "a"); row.Kind != index.KindHosted || !meta {
		t.Fatalf("after a failed journal: kind %s, cached %t; want hosted with the cache", row.Kind, meta)
	}
	j.err = nil
	wantStatus(t, f.do(http.MethodPatch, target, `{"kind": "hosted"}`, admin), http.StatusOK)
	if meta, _ := cached(t, f.st, repo.ID, "a"); meta || len(j.calls) != 1 || len(auditOf(t, f.st, actionConvert)) != 1 {
		t.Errorf("the resumed conversion: cached %t, journal %q, %d audit rows; want dropped, one write, one row", meta, j.calls,
			len(auditOf(t, f.st, actionConvert)))
	}

	off, _ := newConvertFixture(t, nil, "other")
	wantError(t, off.do(http.MethodPatch, "/api/v1/repos/npm/other", `{"kind": "hosted"}`, admin), http.StatusNotImplemented,
		codeUnavailable, "not enabled")
}

// wantConvertRefused checks the refusals of the conversion of npm/legacy, the proxy repo, and
// that it is still the proxy it was after them, its journal not written.
func wantConvertRefused(t *testing.T, f *fixture, repo *index.Repo, j *fakeJournal) {
	t.Helper()
	ctx := context.Background()
	const target = "/api/v1/repos/npm/legacy"
	f.create(t, "npm", "internal")
	wantStatus(t, f.do(http.MethodPost, "/api/v1/repos", `{"format": "npm", "name": "both", "kind": "group",
		"group": {"members": ["internal", "legacy"]}}`, admin), http.StatusCreated)
	claim := index.FetchClaim{RepoID: repo.ID, Package: "a", Tag: "3.0.0", Principal: "anonymous", Replica: "r:1",
		ClaimedAt: time.Now(), HeartbeatAt: time.Now()}
	must(t, f.st.ClaimFetch(ctx, &claim, time.Now().Add(-time.Hour)))

	for _, tc := range []struct {
		name, target, body string
		p                  auth.Principal
		status             int
		code, msg          string
	}{
		{"a maintainer", target, `{"kind": "hosted"}`, carol, http.StatusForbidden, codeForbidden, ""},
		{"with another key", target, `{"kind": "hosted", "anonymousRead": true}`, admin, http.StatusBadRequest, codeInvalid,
			"kind: changes alone"},
		{"another kind", target, `{"kind": "group"}`, admin, http.StatusBadRequest, codeInvalid, `the one change of a kind`},
		{"a group", "/api/v1/repos/npm/both", `{"kind": "hosted"}`, admin, http.StatusBadRequest, codeInvalid, "is a group"},
		{"a group with a hosted member", target, `{"kind": "hosted"}`, admin, http.StatusConflict, codeConflict,
			"member of the group npm/both, which has the hosted member internal"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			wantError(t, f.do(http.MethodPatch, tc.target, tc.body, tc.p), tc.status, tc.code, tc.msg)
		})
	}
	wantStatus(t, f.do(http.MethodDelete, "/api/v1/repos/npm/both", "", admin), http.StatusNoContent)
	wantError(t, f.do(http.MethodPatch, target, `{"kind": "hosted"}`, admin), http.StatusConflict, codeConflict, "running")
	must(t, f.st.ReleaseFetchClaim(ctx, claim.ID))
	if row, err := f.st.GetRepo(ctx, "npm", "legacy"); err != nil || row.Kind != index.KindProxy || len(j.calls) != 0 {
		t.Fatalf("after the refusals: %+v, %v, journal %v; want the proxy as it was", row, err, j.calls)
	}
}
