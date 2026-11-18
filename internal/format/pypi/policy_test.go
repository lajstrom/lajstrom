package pypi

import (
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/lajstrom/lajstrom/internal/attest"
	"github.com/lajstrom/lajstrom/internal/auth"
	"github.com/lajstrom/lajstrom/internal/format"
	"github.com/lajstrom/lajstrom/internal/index"
	"github.com/lajstrom/lajstrom/internal/ingest"
	"github.com/lajstrom/lajstrom/internal/policy"
	"github.com/lajstrom/lajstrom/internal/provider"
	"github.com/lajstrom/lajstrom/internal/testutil"
)

// withPolicy rebuilds the pipeline and the routes of e with a Decider over its index.
func (e *env) withPolicy() *policy.Decider { return e.withPolicyTrust(nil) }

// withPolicyTrust is withPolicy with trust, the trust of attachments, in the Decider.
func (e *env) withPolicyTrust(trust *attest.Trust) *policy.Decider {
	d := policy.New(policy.Options{Index: e.st, Trust: trust})
	log := slog.New(slog.DiscardHandler)
	e.pipe = ingest.New(ingest.Options{Providers: provider.Map{"local": e.prov}, Index: e.st, Log: log, Policy: d,
		Plugins: func(name string) (format.Plugin, bool) { return Plugin{}, name == Name }})
	deps := format.Deps{Index: e.st, Ingest: e.pipe, Providers: provider.Map{"local": e.prov}, Log: log, Policy: d}
	mux := http.NewServeMux()
	Plugin{}.Routes(format.NewRouter(mux, Name, deps), deps)
	e.h = mux
	return d
}

// TestPolicyDeny is the first criterion of #101 on a hosted repo: a rule that denies requests
// 2.32.0 leaves its file out of the simple index while 2.32.1 stays, the file's download is 403
// with the rule's reason and an audit row, and an upload of that version is refused; the page
// rendered under a rule that expires carries its expiry, and a page past its expiry is rendered
// again.
func TestPolicyDeny(t *testing.T) {
	e := newEnv(t)
	e.withPolicy()
	ctx := t.Context()
	old, oldInfo := testutil.Wheel(testutil.WheelOptions{Name: "requests", Version: "2.32.0"})
	cur, curInfo := testutil.Wheel(testutil.WheelOptions{Name: "requests", Version: "2.32.1"})
	e.publish(oldInfo.Filename, old)
	e.publish(curInfo.Filename, cur)
	expires := time.Now().Add(time.Hour).UTC().Truncate(time.Second)
	rule := index.PolicyRule{RepoMask: "pypi/internal", Name: "requests", Version: "==2.32.0", Variant: "*", Effect: "deny",
		Reason: "CVE-2026-1234", CreatedBy: "user:u", ExpiresAt: &expires}
	must(t, e.st.CreatePolicyRule(ctx, &rule))

	for _, page := range []string{"simple/requests/", "simple/requests/?format=json"} {
		rr := e.do(http.MethodGet, page, "Accept", "application/vnd.pypi.simple.v1+json, text/html;q=0.5")
		if rr.Code != http.StatusOK || strings.Contains(rr.Body.String(), oldInfo.Filename) || !strings.Contains(rr.Body.String(), curInfo.Filename) {
			t.Errorf("%s: %d, want 2.32.1 and not 2.32.0: %s", page, rr.Code, rr.Body)
		}
	}
	c, err := e.st.GetIndexCache(ctx, e.repo.ID, "simple/requests", "simple.html")
	must(t, err)
	if c.ExpiresAt == nil || !c.ExpiresAt.Equal(expires) {
		t.Errorf("the page's expiry: %v, want the rule's %v", c.ExpiresAt, expires)
	}

	policyRefusals(t, e, oldInfo.Filename, curInfo.Filename, rule.ID)
}

// policyRefusals checks the downloads, the audit rows and the upload that the rule of
// TestPolicyDeny refuses, and that a page past its expiry is rendered again.
func policyRefusals(t *testing.T, e *env, old, cur string, ruleID int64) {
	ctx := t.Context()
	rr := e.do(http.MethodGet, "files/requests/2.32.0/"+old)
	if rr.Code != http.StatusForbidden || !strings.Contains(rr.Body.String(), "requests 2.32.0 is denied by policy: CVE-2026-1234") {
		t.Errorf("the denied file: %d %s", rr.Code, rr.Body)
	}
	if rr := e.do(http.MethodGet, "files/requests/2.32.0/"+old+metadataSuffix); rr.Code != http.StatusForbidden {
		t.Errorf("the denied file's metadata: %d", rr.Code)
	}
	if rr := e.do(http.MethodGet, "files/requests/2.32.1/"+cur); rr.Code != http.StatusOK {
		t.Errorf("the allowed file: %d", rr.Code)
	}
	evs, err := e.st.ListAudit(ctx, index.AuditFilter{Action: policy.ActionRefuse})
	must(t, err)
	if len(evs) != 2 || evs[0].Subject != "requests 2.32.0 py3-none-any" || !strings.Contains(string(evs[0].Details), `"surface":"download"`) {
		t.Errorf("the audit rows of the refusals: %+v", evs)
	}
	sdist, info := testutil.Sdist(testutil.SdistOptions{Name: "requests", Version: "2.32.0"})
	_, err = e.pipe.Upload(ctx, e.repo, upload(info.Filename, sdist), auth.Principal{Kind: auth.KindUser, Name: "alice"})
	var denied *policy.DeniedError
	if !errors.As(err, &denied) || denied.Verdict.Rule != ruleID {
		t.Errorf("the upload of 2.32.0: %v, want a refusal by rule %d", err, ruleID)
	}
	if ms, err := e.st.FindFiles(ctx, e.repo.ID, info.Filename); err != nil || len(ms) != 0 {
		t.Errorf("the refused upload is indexed: %d, %v", len(ms), err)
	}

	// A page past its expiry is rendered again.
	past := time.Now().Add(-time.Minute)
	gen, err := e.st.IndexCacheGeneration(ctx, e.repo.ID)
	must(t, err)
	_, err = e.st.PutIndexCache(ctx, &index.Cache{RepoID: e.repo.ID, Scope: "simple/requests", Kind: "simple.html",
		Content: []byte("stale"), SHA256: "stale", ExpiresAt: &past}, gen)
	must(t, err)
	if rr := e.do(http.MethodGet, "simple/requests/"); strings.Contains(rr.Body.String(), "stale") {
		t.Error("a page past its expiry is served")
	}
}
