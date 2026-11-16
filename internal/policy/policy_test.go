package policy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/opencontainers/go-digest"

	"github.com/lajstrom/lajstrom/internal/auth"
	"github.com/lajstrom/lajstrom/internal/index"
	"github.com/lajstrom/lajstrom/internal/index/sqlite"
)

var memoryDBs atomic.Int64

// env is a migrated in-memory index, a clock and a Decider over them.
type env struct {
	st  *sqlite.Store
	now time.Time
	d   *Decider
}

func newEnv(t *testing.T) *env {
	t.Helper()
	st, err := sqlite.Open(context.Background(), fmt.Sprintf("file:policy-%d?mode=memory&cache=shared", memoryDBs.Add(1)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := st.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	e := &env{st: st, now: time.Date(2026, 11, 15, 10, 0, 0, 0, time.UTC)}
	e.d = New(Options{Index: st, Order: orders, Now: func() time.Time { return e.now }})
	return e
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func (e *env) repo(t *testing.T, format, name, kind, policy string) index.Repo {
	t.Helper()
	r := index.Repo{Format: format, Name: name, Registry: "default", Kind: kind}
	if policy != "" {
		r.Policy = json.RawMessage(policy)
	}
	must(t, e.st.CreateRepo(t.Context(), &r))
	return r
}

func (e *env) rule(t *testing.T, mask, name, version, effect, reason string) index.PolicyRule {
	t.Helper()
	r := index.PolicyRule{RepoMask: mask, Name: name, Version: version, Variant: "*", Effect: effect, Reason: reason,
		CreatedBy: "user:u"}
	must(t, e.st.CreatePolicyRule(t.Context(), &r))
	return r
}

func (e *env) verdict(t *testing.T, repo index.Repo, subject digest.Digest, verdict, scope string, validUntil *time.Time) digest.Digest {
	t.Helper()
	ref := digest.FromString(fmt.Sprintf("%s %s %s %s", repo.Name, subject, verdict, scope))
	must(t, e.st.ReplaceOCIVerdicts(t.Context(), repo.ID, ref, []index.Verdict{{Path: "p", Subject: subject, Verdict: verdict,
		Reason: verdict + " by a scanner", Source: "trivy", Scope: scope, ValidUntil: validUntil, SignedBy: "scanner"}}))
	return ref
}

func (e *env) decide(t *testing.T, s Subject) Verdict {
	t.Helper()
	v, err := e.d.Decide(t.Context(), s)
	must(t, err)
	return v
}

// TestDecideRules: the rule of the issue denies its version and nothing else; an allow rule
// makes an exception; an expired rule holds nothing; Until is the earliest expiry read.
func TestDecideRules(t *testing.T) {
	e := newEnv(t)
	proxy := e.repo(t, "pypi", "proxy-pypi", index.KindProxy, "")
	other := e.repo(t, "pypi", "internal", index.KindHosted, "")
	deny := e.rule(t, "pypi/proxy-pypi", "requests", "==2.32.0", "deny", "CVE-2026-1234")

	v := e.decide(t, Subject{Repo: proxy, Name: "requests", Version: "2.32.0"})
	if v.Effect != EffectDeny || v.Rule != deny.ID || v.Reason != "CVE-2026-1234" {
		t.Errorf("2.32.0: %+v, want denied by rule %d", v, deny.ID)
	}
	err := Refusal(Subject{Repo: proxy, Name: "requests", Version: "2.32.0"}, v)
	if err == nil || err.Error() != "requests 2.32.0 is denied by policy: CVE-2026-1234" {
		t.Errorf("the refusal: %v", err)
	}
	for _, s := range []Subject{{Repo: proxy, Name: "requests", Version: "2.32.1"}, {Repo: other, Name: "requests", Version: "2.32.0"},
		{Repo: proxy, Name: "Requests-Toolbelt", Version: "2.32.0"}} {
		if v := e.decide(t, s); !v.Allowed() {
			t.Errorf("%s in %s: %+v, want allowed", s, s.Repo.Name, v)
		}
	}
	if v := e.decide(t, Subject{Repo: proxy, Name: "REQUESTS", Version: "2.32.0"}); v.Allowed() {
		t.Error("the name is matched ignoring ASCII case")
	}

	// An allow rule is an exception to the deny rules.
	e.rule(t, "pypi/*", "requests", "*", "allow", "pinned by the platform team")
	if v := e.decide(t, Subject{Repo: proxy, Name: "requests", Version: "2.32.0"}); !v.Allowed() || v.Rule == 0 {
		t.Errorf("with the allow rule: %+v", v)
	}

}

// TestDecideExpiry: an expired rule holds nothing, one that expires bounds Until; a range
// compares in the format's order.
func TestDecideExpiry(t *testing.T) {
	e := newEnv(t)
	expiring := index.PolicyRule{RepoMask: "npm/*", Name: "left-pad", Version: "*", Variant: "*", Effect: "deny", Reason: "r",
		CreatedBy: "user:u", ExpiresAt: ptr(e.now.Add(time.Hour))}
	must(t, e.st.CreatePolicyRule(t.Context(), &expiring))
	npm := e.repo(t, "npm", "npmjs", index.KindProxy, "")
	v := e.decide(t, Subject{Repo: npm, Name: "left-pad", Version: "1.3.0"})
	if v.Allowed() || !v.Until.Equal(e.now.Add(time.Hour)) {
		t.Errorf("an expiring rule: %+v, want denied until its expiry", v)
	}
	e.now = e.now.Add(time.Hour)
	if v := e.decide(t, Subject{Repo: npm, Name: "left-pad", Version: "1.3.0"}); !v.Allowed() || !v.Until.IsZero() {
		t.Errorf("the expired rule: %+v, want allowed", v)
	}
	// A range in the format's order.
	e.rule(t, "npm/npmjs", "lodash", ">=4.0, <4.17.21", "deny", "prototype pollution")
	for version, allowed := range map[string]bool{"4.17.20": false, "4.17.21": true, "3.10.1": true, "4.0": false} {
		if v := e.decide(t, Subject{Repo: npm, Name: "lodash", Version: version}); v.Allowed() != allowed {
			t.Errorf("lodash %s: %+v, want allowed %v", version, v, allowed)
		}
	}
}

// TestDecideVerdicts: a deny verdict wins over an allow rule; a verdict holds in its own repo
// unless its scope says otherwise; an expired one holds nothing; a quarantine verdict refuses
// until an allow verdict releases it.
func TestDecideVerdicts(t *testing.T) {
	e := newEnv(t)
	images := e.repo(t, "oci", "images", index.KindHosted, "")
	mirror := e.repo(t, "oci", "mirror", index.KindHosted, "")
	d := digest.FromString("image")
	s := Subject{Repo: images, Name: "app", Version: "1.0", Digest: d}
	allow := e.rule(t, "oci/*", "*", "*", "allow", "everything")

	ref := e.verdict(t, images, d, "deny", "", nil)
	v := e.decide(t, s)
	if v.Effect != EffectDeny || v.Attestation != ref || v.Source != "trivy" || v.Reason != "deny by a scanner" {
		t.Errorf("a deny verdict under an allow rule: %+v", v)
	}
	if v := e.decide(t, Subject{Repo: mirror, Name: "app", Digest: d}); !v.Allowed() {
		t.Errorf("the verdict of another repo without a scope: %+v, want allowed", v)
	}
	e.verdict(t, images, d, "deny", "oci/mirror", nil)
	if v := e.decide(t, Subject{Repo: mirror, Name: "app", Digest: d}); v.Allowed() {
		t.Error("a verdict whose scope is the other repo holds there")
	}

	// A quarantine verdict until a time; the allow rule releases it, and so does an allow
	// verdict.
	q := digest.FromString("quarantined")
	until := e.now.Add(2 * time.Hour)
	e.verdict(t, images, q, "quarantine", "", &until)
	qs := Subject{Repo: images, Name: "app", Version: "2.0", Digest: q}
	if v := e.decide(t, qs); !v.Allowed() {
		t.Errorf("an allow rule releases a quarantine verdict: %+v", v)
	}
	must(t, e.st.DeletePolicyRule(t.Context(), allow.ID))
	v = e.decide(t, qs)
	if v.Effect != EffectQuarantine || !v.Lifts.Equal(until) || !v.Until.Equal(until) || v.Reason != "quarantine by a scanner" {
		t.Errorf("a quarantine verdict: %+v", v)
	}
	expired := e.now.Add(-time.Minute)
	e.verdict(t, images, q, "allow", "", &expired)
	if v := e.decide(t, qs); v.Allowed() {
		t.Error("an expired allow verdict releases the quarantine")
	}
	e.verdict(t, images, q, "allow", "oci/*", nil)
	if v := e.decide(t, qs); !v.Allowed() {
		t.Errorf("an allow verdict: %+v", v)
	}
	if v := e.decide(t, s); v.Allowed() {
		t.Error("an allow verdict of another digest releases a deny verdict")
	}
}

// TestDecideQuarantine: a proxy repo's quarantine refuses a version published an hour ago and
// serves it after the quarantine; the policy_seen row is the clock when it is earlier; an allow
// verdict and an allow rule release it; a hosted repo has none.
func TestDecideQuarantine(t *testing.T) {
	e := newEnv(t)
	proxy := e.repo(t, "npm", "npmjs", index.KindProxy, `{"quarantineNewVersions": "72h"}`)
	hosted := e.repo(t, "npm", "internal", index.KindHosted, `{"quarantineNewVersions": "72h"}`)
	published := e.now.Add(-time.Hour)
	s := Subject{Repo: proxy, Name: "left-pad", Version: "1.3.1", Published: published}
	v := e.decide(t, s)
	if v.Effect != EffectQuarantine || !v.Lifts.Equal(published.Add(72*time.Hour)) || !v.Until.Equal(v.Lifts) {
		t.Fatalf("an hour after its publish: %+v", v)
	}
	if err := Refusal(s, v); err == nil || err.Error() != "left-pad 1.3.1 is quarantined until 2026-11-18T09:00:00Z" {
		t.Errorf("the refusal: %v", err)
	}
	if v := e.decide(t, Subject{Repo: hosted, Name: "left-pad", Version: "1.3.1", Published: published}); !v.Allowed() {
		t.Errorf("a hosted repo: %+v", v)
	}
	if v := e.decide(t, Subject{Repo: proxy, Name: "left-pad", Version: "1.3.0", Published: e.now.Add(-73 * time.Hour)}); !v.Allowed() {
		t.Errorf("73 hours after its publish: %+v", v)
	}
	if v := e.decide(t, Subject{Repo: proxy, Name: "fresh", Version: "1.0"}); v.Effect != EffectQuarantine ||
		!v.Lifts.Equal(e.now.Add(72*time.Hour)) {
		t.Errorf("no clock: %+v, want quarantined from now", v)
	}

	// A version fetched once its quarantine was over: its created_at is the fetch, the
	// policy_seen row the clock.
	seenAt := e.now.Add(-80 * time.Hour)
	must(t, e.st.PutPolicySeen(t.Context(), &index.PolicySeen{RepoID: proxy.ID, Name: "maven-like", Version: "1.0", SeenAt: seenAt}))
	if v := e.decide(t, Subject{Repo: proxy, Name: "maven-like", Version: "1.0", Published: e.now.Add(-time.Minute)}); !v.Allowed() {
		t.Errorf("a version seen 80 hours ago, fetched a minute ago: %+v", v)
	}

	// After 72 hours (the clock injected), the same version is served.
	e.now = e.now.Add(71 * time.Hour)
	if v := e.decide(t, s); !v.Allowed() {
		t.Errorf("72 hours after its publish: %+v", v)
	}

	// An allow verdict releases a quarantine, and so does an allow rule.
	e.now = published.Add(time.Hour)
	d := digest.FromString("left-pad 1.3.1")
	held := Subject{Repo: proxy, Name: "left-pad", Version: "1.3.1", Digest: d, Published: published}
	e.verdict(t, proxy, d, "allow", "", nil)
	if v := e.decide(t, held); !v.Allowed() {
		t.Errorf("with an allow verdict: %+v", v)
	}
	e.rule(t, "npm/npmjs", "@acme/*", "*", "allow", "ours")
	if v := e.decide(t, Subject{Repo: proxy, Name: "@acme/ui", Version: "2.0", Published: e.now}); !v.Allowed() {
		t.Errorf("with an allow rule: %+v", v)
	}
}

// TestDecideGroups: a rule whose mask names a group exactly holds for its members.
func TestDecideGroups(t *testing.T) {
	e := newEnv(t)
	member := e.repo(t, "npm", "npmjs", index.KindProxy, "")
	group := index.Repo{Format: "npm", Name: "all", Kind: index.KindGroup}
	must(t, e.st.CreateRepo(t.Context(), &group))
	must(t, e.st.SetGroupMembers(t.Context(), group.ID, []int64{member.ID}))
	e.rule(t, "npm/all", "evil", "*", "deny", "malware")
	if v := e.decide(t, Subject{Repo: member, Name: "evil", Version: "1.0"}); v.Allowed() {
		t.Error("a rule on the group does not hold for its member")
	}
	alone := e.repo(t, "npm", "alone", index.KindProxy, "")
	if v := e.decide(t, Subject{Repo: alone, Name: "evil", Version: "1.0"}); !v.Allowed() {
		t.Error("a rule on a group holds for a repo that is not its member")
	}
}

// TestRulesCache: the rules are read again when the generation moves, and only then.
func TestRulesCache(t *testing.T) {
	e := newEnv(t)
	repo := e.repo(t, "raw", "files", index.KindHosted, "")
	s := Subject{Repo: repo, Name: "a", Version: "1"}
	if v := e.decide(t, s); !v.Allowed() {
		t.Fatal("no rule")
	}
	r := e.rule(t, "raw/files", "a", "*", "deny", "no")
	if v := e.decide(t, s); v.Allowed() {
		t.Error("a rule created after the first decision is not read")
	}
	must(t, e.st.DeletePolicyRule(t.Context(), r.ID))
	if v := e.decide(t, s); !v.Allowed() {
		t.Error("a deleted rule still holds")
	}
}

// TestRefuse: the audit row of a refusal names the surface, the rule and the reason.
func TestRefuse(t *testing.T) {
	e := newEnv(t)
	e.d.o.Origin = func(context.Context) (string, string) { return "192.0.2.7", "req-1" }
	repo := e.repo(t, "pypi", "proxy-pypi", index.KindProxy, "")
	rule := e.rule(t, "pypi/proxy-pypi", "requests", "==2.32.0", "deny", "CVE-2026-1234")
	s := Subject{Repo: repo, Name: "requests", Version: "2.32.0"}
	err := e.d.Check(t.Context(), s)
	var de *DeniedError
	if !errors.As(err, &de) {
		t.Fatalf("Check: %v", err)
	}
	e.d.Refuse(t.Context(), SurfaceDownload, de, auth.Anonymous)
	evs, err := e.st.ListAudit(t.Context(), index.AuditFilter{Action: ActionRefuse})
	must(t, err)
	if len(evs) != 1 {
		t.Fatalf("%d rows", len(evs))
	}
	ev := evs[0]
	var details map[string]any
	must(t, json.Unmarshal(ev.Details, &details))
	if ev.Repo != "pypi/proxy-pypi" || ev.Subject != "requests 2.32.0" || ev.IP != "192.0.2.7" || details["surface"] != "download" ||
		details["rule"] != float64(rule.ID) || details["reason"] != "CVE-2026-1234" || details["requestId"] != "req-1" {
		t.Errorf("the row: %+v %v", ev, details)
	}
}
