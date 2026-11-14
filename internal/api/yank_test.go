package api

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/opencontainers/go-digest"

	"github.com/lajstrom/lajstrom/internal/auth"
	"github.com/lajstrom/lajstrom/internal/format"
	"github.com/lajstrom/lajstrom/internal/index"
	"github.com/lajstrom/lajstrom/internal/oci/profile"
)

// yankJournal is a Journaler that folds the ops into the store's journal_state as ingest does,
// and records them.
type yankJournal struct {
	st   index.Store
	seq  int64
	ops  []format.JournalOp
	repo []string // the repo and package path of each op
}

func (j *yankJournal) Journal(ctx context.Context, repo index.Repo, pkgPath string, ops []format.JournalOp, _ auth.Principal) ([]int64, error) {
	pkg, err := j.st.GetPackageByNameOCI(ctx, repo.ID, pkgPath)
	if err != nil {
		return nil, err
	}
	rows, err := j.st.ListJournalState(ctx, pkg.ID)
	if err != nil {
		return nil, err
	}
	s := profile.JournalState{}
	for _, r := range rows {
		s[profile.JournalKey{Kind: r.Kind, Key: r.Key}] = profile.JournalValue{Value: r.Value, Seq: r.Seq}
	}
	var seqs []int64
	for _, op := range ops {
		j.seq++
		s.Apply(j.seq, profile.JournalChange(op))
		j.ops, j.repo, seqs = append(j.ops, op), append(j.repo, repo.Format+"/"+repo.Name+" "+pkgPath), append(seqs, j.seq)
	}
	state := []index.JournalState{}
	for _, k := range s.Keys() {
		state = append(state, index.JournalState{Kind: k.Kind, Key: k.Key, Value: s[k].Value, Seq: s[k].Seq})
	}
	return seqs, j.st.ReplaceJournalState(ctx, pkg.ID, state)
}

// newYankFixture is the API with o and a yankJournal, the repo pypi/libs with requests 1.0 (a
// wheel and an sdist, publishDetailed) and 1.1 (an sdist), and the repo raw/assets.
func newYankFixture(t *testing.T, o Options) (*fixture, *yankJournal) {
	t.Helper()
	ctx := context.Background()
	j := &yankJournal{}
	o.Journal = j
	f := newFixture(t, o)
	j.st = f.st
	repo := f.create(t, "pypi", "libs")
	publishDetailed(t, f.st, repo.ID)
	pkg, err := f.st.GetPackage(ctx, repo.ID, "requests")
	must(t, err)
	must(t, f.st.UpsertVersion(ctx, &index.Version{PackageID: pkg.ID, Version: "1.1", Variant: "sdist", Tag: "1.1__sdist",
		Digest: digest.FromString("1.1"), ArtifactType: "application/vnd.lajstrom.pypi.v1", CreatedAt: testNow}))
	f.create(t, "raw", "assets")
	return f, j
}

// yankTarget is the path of the versions of requests in pypi/libs.
const yankTarget = "/api/v1/repos/pypi/libs/packages/requests/versions/"

// TestYankVersion: PUT …/versions/{version}/yank yanks a version of a hosted pypi repo through
// the journal, with the body's reason or none, and DELETE unyanks it (#81); both are 204 and
// append nothing when nothing changes; the version list and the version show the yank.
func TestYankVersion(t *testing.T) {
	f, j := newYankFixture(t, Options{})
	target := yankTarget
	yanked := func(version string) any {
		var v struct {
			Yanked any `json:"yanked"`
		}
		decodeJSON(t, f.do(http.MethodGet, target+version, "", admin), http.StatusOK, &v)
		return v.Yanked
	}

	steps := []struct {
		name, method, version, body string
		want                        any // the yanked of the version after the step
		appended                    bool
	}{
		{"yank with a reason", http.MethodPut, "1.0", `{"reason": "breaks proxies"}`, map[string]any{"reason": "breaks proxies"}, true},
		{"the same yank again", http.MethodPut, "1.0", `{"reason": "breaks proxies"}`, map[string]any{"reason": "breaks proxies"}, false},
		{"another reason", http.MethodPut, "1.0", `{"reason": "use 1.1"}`, map[string]any{"reason": "use 1.1"}, true},
		{"a yank without a body", http.MethodPut, "1.1", "", map[string]any{"reason": ""}, true},
		{"a reason of null is none", http.MethodPut, "1.1", `{"reason": null}`, map[string]any{"reason": ""}, false},
		{"unyank", http.MethodDelete, "1.1", "", nil, true},
		{"unyank again", http.MethodDelete, "1.1", "", nil, false},
	}
	for _, s := range steps {
		before := len(j.ops)
		wantStatus(t, f.do(s.method, target+s.version+"/yank", s.body, admin), http.StatusNoContent)
		if appended := len(j.ops) > before; appended != s.appended {
			t.Errorf("%s: appended %v, want %v (ops %+v)", s.name, appended, s.appended, j.ops)
		}
		if got := yanked(s.version); !equalJSON(got, s.want) {
			t.Errorf("%s: yanked %#v, want %#v", s.name, got, s.want)
		}
	}
	wantOps := []format.JournalOp{
		{ArtifactType: profile.ArtifactTypePypiYank, Op: profile.JournalYank, Version: "1.0", Message: "breaks proxies"},
		{ArtifactType: profile.ArtifactTypePypiYank, Op: profile.JournalYank, Version: "1.0", Message: "use 1.1"},
		{ArtifactType: profile.ArtifactTypePypiYank, Op: profile.JournalYank, Version: "1.1"},
		{ArtifactType: profile.ArtifactTypePypiYank, Op: profile.JournalUnyank, Version: "1.1"},
	}
	if len(j.ops) != len(wantOps) {
		t.Fatalf("ops %+v, want %+v", j.ops, wantOps)
	}
	for i := range wantOps {
		if j.ops[i] != wantOps[i] || j.repo[i] != "pypi/libs requests" {
			t.Errorf("op %d: %+v on %s, want %+v on pypi/libs requests", i, j.ops[i], j.repo[i], wantOps[i])
		}
	}
	var list VersionList
	decodeJSON(t, f.do(http.MethodGet, strings.TrimSuffix(target, "/"), "", admin), http.StatusOK, &list)
	for _, v := range list.Versions {
		if want := map[string]*Yank{"1.0": {Reason: "use 1.1"}, "1.1": nil}[v.Version]; (v.Yanked == nil) != (want == nil) ||
			(want != nil && *v.Yanked != *want) {
			t.Errorf("the list: %s yanked %+v, want %+v", v.Version, v.Yanked, want)
		}
	}
}

// TestYankVersionRefused: what is refused appends nothing: a repo of another format, ?variant=,
// a reason too long, a field other than reason, a version or a package or a repo that does not
// exist. (A reason that is not UTF-8 cannot arrive: the JSON decoder replaces such bytes.)
func TestYankVersionRefused(t *testing.T) {
	f, j := newYankFixture(t, Options{})
	target := yankTarget
	refused := []struct {
		name, method, target, body string
		status                     int
		code                       string
	}{
		{"a raw repo", http.MethodPut, "/api/v1/repos/raw/assets/packages/x/versions/0123456789abcdef/yank", "", http.StatusBadRequest, codeInvalid},
		{"one variant", http.MethodPut, target + "1.0/yank?variant=sdist", "", http.StatusBadRequest, codeInvalid},
		{"a reason too long", http.MethodPut, target + "1.0/yank", `{"reason": "` + strings.Repeat("x", profile.MaxJournalMessage+1) + `"}`,
			http.StatusBadRequest, codeInvalid},
		{"another field", http.MethodPut, target + "1.0/yank", `{"why": "x"}`, http.StatusBadRequest, codeInvalid},
		{"a version the package does not have", http.MethodPut, target + "2.0/yank", "", http.StatusNotFound, codeNotFound},
		{"a package the repo does not have", http.MethodDelete, "/api/v1/repos/pypi/libs/packages/flask/versions/1.0/yank", "",
			http.StatusNotFound, codeNotFound},
		{"a repo that does not exist", http.MethodPut, "/api/v1/repos/pypi/nothing/packages/requests/versions/1.0/yank", "",
			http.StatusNotFound, codeNotFound},
	}
	before := len(j.ops)
	for _, tt := range refused {
		wantError(t, f.do(tt.method, tt.target, tt.body, admin), tt.status, tt.code)
	}
	if len(j.ops) != before {
		t.Errorf("a refused yank appended %+v", j.ops[before:])
	}
}

// TestYankVersionAuthorization: a yank needs write on the repo, as an npm deprecation; a reader
// gets 403, a principal without any access 404 and anonymous 401, before the body is read.
func TestYankVersionAuthorization(t *testing.T) {
	g := map[string]bool{"user:alice read pypi/libs": true, "user:carol write pypi/libs": true, "user:carol read pypi/libs": true}
	for k, v := range grants {
		g[k] = v
	}
	f, j := newYankFixture(t, Options{Auth: fakeAuth{grants: g}})
	target := yankTarget + "1.0/yank"
	carol := auth.Principal{Kind: auth.KindUser, Name: "carol"}
	for _, tt := range []struct {
		p      auth.Principal
		status int
	}{
		{auth.Principal{}, http.StatusUnauthorized},
		{bob, http.StatusNotFound},
		{alice, http.StatusForbidden},
		{carol, http.StatusNoContent},
	} {
		for _, method := range []string{http.MethodPut, http.MethodDelete} {
			if rr := f.do(method, target, `{"reason": "x"}`, tt.p); rr.Code != tt.status {
				t.Errorf("%s by %q: %d, want %d: %s", method, tt.p.Name, rr.Code, tt.status, rr.Body)
			}
		}
	}
	if len(j.ops) != 2 {
		t.Errorf("ops %+v, want carol's yank and unyank", j.ops)
	}
}

// equalJSON compares two values decoded from JSON.
func equalJSON(a, b any) bool {
	am, aok := a.(map[string]any)
	bm, bok := b.(map[string]any)
	if aok != bok {
		return false
	}
	if !aok {
		return a == b
	}
	if len(am) != len(bm) {
		return false
	}
	for k, v := range am {
		if bm[k] != v {
			return false
		}
	}
	return true
}
