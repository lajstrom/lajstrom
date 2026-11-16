package proxy_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/lajstrom/lajstrom/internal/format"
	"github.com/lajstrom/lajstrom/internal/index"
	"github.com/lajstrom/lajstrom/internal/policy"
	"github.com/lajstrom/lajstrom/internal/proxy"
)

// Trim leaves the versions drop names out of the kept document (format.Trimmer).
func (*pkgPlugin) Trim(_, _ string, kept []byte, drop func(version, variant string) bool) ([]byte, error) {
	var d upstreamDoc
	if err := json.Unmarshal(kept, &d); err != nil {
		return nil, err
	}
	var vs []upstreamVersion
	for _, v := range d.Versions {
		if !drop(v.Version, "") {
			vs = append(vs, v)
		}
	}
	d.Versions = vs
	return json.Marshal(d)
}

var _ format.Trimmer = (*pkgPlugin)(nil)

// withQuarantine gives the env's repo the policy quarantineNewVersions of d.
func (e *env) withQuarantine(t *testing.T, d string) {
	t.Helper()
	e.repo.Policy = json.RawMessage(`{"quarantineNewVersions": "` + d + `"}`)
	must(t, e.st.UpdateRepo(t.Context(), &e.repo))
}

// listed returns the versions of name that the document of the proxy repo lists.
func listed(t *testing.T, c *proxy.Core, e *env, name string) map[string]string {
	t.Helper()
	doc, err := c.Document(asAlice(t), e.repo, name, docKind)
	must(t, err)
	var out map[string]string
	must(t, json.Unmarshal(doc.Content, &out))
	return out
}

// TestQuarantine is the second criterion of #101: with a quarantine of 72 hours, a version
// published upstream an hour ago is neither listed nor fetched, and is 403 with when it lifts; 72
// hours later (the clock injected) it is listed, fetched and served, and Lifted gets it. A
// version published long ago is served at once.
func TestQuarantine(t *testing.T) {
	e := newEnv(t, `{"name": "up"}`)
	e.withQuarantine(t, "72h")
	hour := e.clock().Add(-time.Hour).Format(time.RFC3339)
	e.up.publish("tool", "1.0.0", "old", at("2025-01-01T00:00:00Z"))
	e.up.publish("tool", "2.0.0", "new", at(hour))
	lifted := make(chan string, 2)
	d := policy.New(policy.Options{Index: e.st, Now: e.clock})
	c := e.core(t, "r1", proxy.Options{Policy: d, Lifted: func(_ context.Context, _ index.Repo, m index.FileMatch) {
		lifted <- m.Version.Version
	}})

	if got := listed(t, c, e, "tool"); len(got) != 1 || got["1.0.0"] == "" {
		t.Errorf("the document during the quarantine: %v, want 1.0.0 alone", got)
	}
	_, err := c.File(asAlice(t), e.repo, fileOf("tool", "2.0.0"))
	var denied *policy.DeniedError
	if !errors.As(err, &denied) || format.StatusOf(err) != 403 || !strings.Contains(err.Error(), "tool 2.0.0 is quarantined until 2026-11-13T11:00:00Z") {
		t.Fatalf("2.0.0 an hour after its publish: %v", err)
	}
	if n := e.up.fileCalls.Load(); n != 0 {
		t.Errorf("the upstream was asked for %d files during the quarantine", n)
	}
	if _, err := c.File(asAlice(t), e.repo, fileOf("tool", "1.0.0")); err != nil {
		t.Errorf("1.0.0, published long ago: %v", err)
	}
	if v := <-lifted; v != "1.0.0" {
		t.Errorf("Lifted got %q, want 1.0.0, whose quarantine was over when it was fetched", v)
	}
	wantRefusal(t, e, `"surface":"fetch"`, `"effect":"quarantine"`)

	e.advance(72 * time.Hour)
	if got := listed(t, c, e, "tool"); got["2.0.0"] == "" {
		t.Errorf("the document after the quarantine: %v, want 2.0.0", got)
	}
	m, err := c.File(asAlice(t), e.repo, fileOf("tool", "2.0.0"))
	if err != nil || m.Version.Version != "2.0.0" {
		t.Fatalf("2.0.0 after 72 hours: %v", err)
	}
	if v := <-lifted; v != "2.0.0" {
		t.Errorf("Lifted got %q, want 2.0.0", v)
	}
}

// wantRefusal fails the test unless the env has one audit row policy.refuse whose details have
// each of parts.
func wantRefusal(t *testing.T, e *env, parts ...string) {
	t.Helper()
	evs, err := e.st.ListAudit(t.Context(), index.AuditFilter{Action: policy.ActionRefuse})
	must(t, err)
	if len(evs) != 1 {
		t.Fatalf("%d audit rows of refusals, want 1", len(evs))
	}
	for _, part := range parts {
		if !strings.Contains(string(evs[0].Details), part) {
			t.Errorf("the audit row of the refusal %s has no %s", evs[0].Details, part)
		}
	}
}

// TestQuarantineFirstSeen: a version whose upstream gives no publish time is quarantined from the
// first time the repo saw it, which a policy_seen row keeps, and served once that is 72 hours
// ago, held too (policy's TestDecideQuarantine: a created_at later than the row does not start
// it again).
func TestQuarantineFirstSeen(t *testing.T) {
	e := newEnv(t, `{"name": "up"}`)
	e.withQuarantine(t, "72h")
	e.up.publish("tool", "1.0.0", "one", nil)
	d := policy.New(policy.Options{Index: e.st, Now: e.clock})
	c := e.core(t, "r1", proxy.Options{Policy: d})
	first := e.clock()
	if got := listed(t, c, e, "tool"); len(got) != 0 {
		t.Errorf("a version seen now: %v, want none listed", got)
	}
	seen, err := e.st.GetPolicySeen(t.Context(), e.repo.ID, "tool", "1.0.0", "")
	if err != nil || !seen.SeenAt.Equal(first) {
		t.Fatalf("the policy_seen row: %+v, %v", seen, err)
	}
	e.advance(71 * time.Hour)
	if _, err := c.File(asAlice(t), e.repo, fileOf("tool", "1.0.0")); !errors.Is(err, policy.ErrDenied) {
		t.Errorf("71 hours after it was first seen: %v", err)
	}
	e.advance(time.Hour)
	if _, err := c.File(asAlice(t), e.repo, fileOf("tool", "1.0.0")); err != nil {
		t.Fatalf("72 hours after it was first seen: %v", err)
	}
	if _, err := c.File(asAlice(t), e.repo, fileOf("tool", "1.0.0")); err != nil {
		t.Errorf("the held version, fetched after its quarantine: %v", err)
	}
}

// TestProxyRule: a rule refuses a version of a proxy repo before anything is asked of the
// upstream's files, held or not, and leaves it out of the document.
func TestProxyRule(t *testing.T) {
	e := newEnv(t, `{"name": "up"}`)
	e.up.publish("tool", "1.0.0", "one", nil)
	e.up.publish("tool", "2.0.0", "two", nil)
	d := policy.New(policy.Options{Index: e.st, Now: e.clock})
	c := e.core(t, "r1", proxy.Options{Policy: d})
	if _, err := c.File(asAlice(t), e.repo, fileOf("tool", "1.0.0")); err != nil {
		t.Fatal(err)
	}
	must(t, e.st.CreatePolicyRule(t.Context(), &index.PolicyRule{RepoMask: "raw/up", Name: "TOOL", Version: "*", Variant: "*",
		Effect: index.PolicyDeny, Reason: "malware", CreatedBy: "user:u"}))
	must(t, e.st.InvalidateIndexCache(t.Context(), e.repo.ID, ""))
	calls := e.up.fileCalls.Load()
	for _, version := range []string{"1.0.0", "2.0.0"} {
		if _, err := c.File(asAlice(t), e.repo, fileOf("tool", version)); !errors.Is(err, policy.ErrDenied) ||
			!strings.Contains(err.Error(), "is denied by policy: malware") {
			t.Errorf("%s under the rule: %v", version, err)
		}
	}
	if n := e.up.fileCalls.Load(); n != calls {
		t.Errorf("the upstream was asked for %d files under the rule", n-calls)
	}
	if got := listed(t, c, e, "tool"); len(got) != 0 {
		t.Errorf("the document under the rule: %v", got)
	}
}
