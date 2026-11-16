package api

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/lajstrom/lajstrom/internal/attest"
	"github.com/lajstrom/lajstrom/internal/auth"
	"github.com/lajstrom/lajstrom/internal/index"
	"github.com/lajstrom/lajstrom/internal/ingest"
	"github.com/lajstrom/lajstrom/internal/policy"
)

// testPolicyKey is an ECDSA P-256 key as the API takes the policy key.
type testPolicyKey struct {
	priv *ecdsa.PrivateKey
	der  []byte
}

func newTestPolicyKey(t *testing.T) *testPolicyKey {
	t.Helper()
	priv, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	der, err := x509.MarshalPKIXPublicKey(&priv.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	return &testPolicyKey{priv: priv, der: der}
}

func (k *testPolicyKey) Sign(_ context.Context, msg []byte) ([]byte, error) {
	sum := sha256.Sum256(msg)
	return ecdsa.SignASN1(rand.Reader, k.priv, sum[:])
}
func (k *testPolicyKey) PublicDER() []byte { return k.der }
func (k *testPolicyKey) PEM() []byte {
	return pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: k.der})
}

// fakeVerdicter records the verdicts a deny attaches and writes their rows as ingest would.
type fakeVerdicter struct {
	st       index.Store
	attached []attest.Verdict
}

func (v *fakeVerdicter) AttachVerdict(ctx context.Context, repo index.Repo, path string, subject digest.Digest, verdict attest.Verdict,
	_ ingest.VerdictSigner, _ auth.Principal) (digest.Digest, error) {
	v.attached = append(v.attached, verdict)
	ref := digest.FromString(fmt.Sprintf("verdict %d of %s", len(v.attached), subject))
	err := v.st.ReplaceOCIVerdicts(ctx, repo.ID, ref, []index.Verdict{{Path: path, Subject: subject, Verdict: verdict.Verdict,
		Reason: verdict.Reason, Source: verdict.Source, Scope: verdict.Scope, ValidUntil: verdict.ValidUntil, SignedBy: "lajstrom"}})
	return ref, err
}

func (v *fakeVerdicter) ResolveManifest(context.Context, index.Repo, string, string) (ocispec.Descriptor, error) {
	return ocispec.Descriptor{Digest: digest.FromString("image")}, nil
}

// policyGrants: carol manages raw/open, alice reads it, bob nothing, the admin everything.
var policyGrants = map[string]bool{
	"user:admin manage */*":      true,
	"user:admin read */*":        true,
	"user:alice read raw/open":   true,
	"user:carol read raw/open":   true,
	"user:carol manage raw/open": true,
}

// policyFixture is a fixture with the rules service and the repos raw/open and raw/secret.
func policyFixture(t *testing.T, key PolicyKey, verdicter Verdicter) *fixture {
	t.Helper()
	rules := &Rules{Formats: oneOf("raw", "pypi", "npm"), Now: func() time.Time { return testNow }}
	f := newFixture(t, Options{Auth: fakeAuth{grants: policyGrants}, Rules: rules, PolicyKey: key, Verdicter: verdicter,
		Deleter: nil})
	rules.Index = f.st
	f.create(t, "raw", "open")
	f.create(t, "raw", "secret")
	return f
}

// TestPolicyRulesAuthorization: a rule of one repo needs manage on it, any other mask the admin
// role; the list of every rule is the admin's, a repo's list its readers'; a delete by who may not
// is 404.
func TestPolicyRulesAuthorization(t *testing.T) {
	f := policyFixture(t, nil, nil)
	own := `{"repoMask": "raw/open", "name": "evil", "version": "==1.0", "reason": "CVE-2026-1234"}`
	for _, tt := range []struct {
		name           string
		p              auth.Principal
		method, target string
		body           string
		status         int
	}{
		{"anonymous creates", auth.Anonymous, "POST", "/api/v1/policy/rules", own, 401},
		{"a reader creates", alice, "POST", "/api/v1/policy/rules", own, 403},
		{"the maintainer creates", carol, "POST", "/api/v1/policy/rules", own, 201},
		{"the maintainer, a wildcard", carol, "POST", "/api/v1/policy/rules", `{"repoMask": "raw/*", "name": "x", "reason": "r"}`, 403},
		{"the admin, a wildcard", admin, "POST", "/api/v1/policy/rules", `{"repoMask": "*/*", "name": "x", "reason": "r"}`, 201},
		{"a range without an order", admin, "POST", "/api/v1/policy/rules", `{"repoMask": "raw/open", "name": "x", "version": ">=1", "reason": "r"}`, 400},
		{"no reason", admin, "POST", "/api/v1/policy/rules", `{"repoMask": "raw/open", "name": "x"}`, 400},
		{"the maintainer lists every rule", carol, "GET", "/api/v1/policy/rules", "", 403},
		{"the admin lists every rule", admin, "GET", "/api/v1/policy/rules", "", 200},
		{"a reader lists the repo's", alice, "GET", "/api/v1/policy/rules?repo=raw/open", "", 200},
		{"a stranger lists the repo's", bob, "GET", "/api/v1/policy/rules?repo=raw/open", "", 404},
		{"the maintainer deletes the admin's wildcard", carol, "DELETE", "/api/v1/policy/rules/2", "", 404},
		{"the maintainer deletes hers", carol, "DELETE", "/api/v1/policy/rules/1", "", 204},
		{"again", carol, "DELETE", "/api/v1/policy/rules/1", "", 404},
	} {
		t.Run(tt.name, func(t *testing.T) {
			rr := f.do(tt.method, tt.target, tt.body, tt.p)
			if rr.Code != tt.status {
				t.Errorf("%d, want %d: %s", rr.Code, tt.status, rr.Body)
			}
		})
	}
}

// TestPolicyRulesLifecycle: a rule created reads back with its defaults, is listed for the repos
// its mask matches, invalidates their documents, writes its audit rows, and goes.
func TestPolicyRulesLifecycle(t *testing.T) {
	f := policyFixture(t, nil, nil)
	ctx := t.Context()
	open, err := f.st.GetRepo(ctx, "raw", "open")
	must(t, err)
	gen0, err := f.st.IndexCacheGeneration(ctx, open.ID)
	must(t, err)
	var rule Rule
	decodeJSON(t, f.do("POST", "/api/v1/policy/rules", `{"repoMask": "raw/*", "name": "Evil-*", "reason": "malware"}`, admin), 201, &rule)
	if rule.Version != "*" || rule.Variant != "*" || rule.Effect != "deny" || rule.CreatedBy != "user:"+uidOf(t, f, admin) {
		t.Errorf("the rule: %+v", rule)
	}
	if gen, err := f.st.IndexCacheGeneration(ctx, open.ID); err != nil || gen <= gen0 {
		t.Errorf("the documents of raw/open were not invalidated: %d, %v", gen, err)
	}
	var list RuleList
	decodeJSON(t, f.do("GET", "/api/v1/policy/rules?repo=raw/secret", "", admin), 200, &list)
	if len(list.Rules) != 1 || list.Rules[0].ID != rule.ID {
		t.Errorf("the rules of raw/secret: %+v", list)
	}
	wantStatus(t, f.do("DELETE", fmt.Sprintf("/api/v1/policy/rules/%d", rule.ID), "", admin), 204)
	evs, err := f.st.ListAudit(ctx, index.AuditFilter{Repo: "raw/*"})
	must(t, err)
	if len(evs) != 2 || evs[1].Action != policy.ActionRuleCreate || evs[0].Action != policy.ActionRuleDelete ||
		evs[0].Subject != fmt.Sprintf("rule:%d", rule.ID) || !strings.Contains(string(evs[1].Details), `"reason":"malware"`) {
		t.Errorf("the audit rows: %+v", evs)
	}
}

// uidOf is the uid of p as the fixture stores it.
func uidOf(t *testing.T, f *fixture, p auth.Principal) string {
	t.Helper()
	return strings.TrimPrefix(f.asStored(p).String(), "user:")
}

// TestPolicyDeny: a deny signs a verdict for each variant of the version, answers them, and lists
// them under the verdicts of the repo; it needs manage, and the policy key (501); the key's PEM
// is answered to anyone.
func TestPolicyDeny(t *testing.T) {
	key := newTestPolicyKey(t)
	f0 := policyFixture(t, nil, nil)
	addVersion(t, f0.st, mustRepo(t, f0, "raw", "open").ID, "tool")
	rr := f0.do("POST", "/api/v1/repos/raw/open/packages/tool/versions/0123456789abcdef/deny", `{"reason": "r"}`, admin)
	if rr.Code != http.StatusNotImplemented {
		t.Errorf("a deny without the policy key: %d %s", rr.Code, rr.Body)
	}
	wantStatus(t, f0.do("GET", "/api/v1/policy/key", "", auth.Anonymous), 404)

	v := &fakeVerdicter{}
	f := policyFixture(t, key, v)
	v.st = f.st
	open := mustRepo(t, f, "raw", "open")
	addVersion(t, f.st, open.ID, "tool")
	wantStatus(t, f.do("POST", "/api/v1/repos/raw/open/packages/tool/versions/0123456789abcdef/deny", `{"reason": "r"}`, alice), 403)
	var made VerdictList
	decodeJSON(t, f.do("POST", "/api/v1/repos/raw/open/packages/tool/versions/0123456789abcdef/deny",
		`{"reason": "CVE-2026-1234"}`, carol), 201, &made)
	if len(made.Items) != 1 || made.Items[0].Verdict != "deny" || made.Items[0].Source != policy.SourceManual ||
		made.Items[0].Scope != "raw/open" || made.Items[0].Repo != "raw/open" || !made.Items[0].InForce {
		t.Errorf("the verdicts made: %+v", made)
	}
	wantStatus(t, f.do("POST", "/api/v1/repos/raw/open/packages/tool/versions/nope/deny", `{"reason": "r"}`, carol), 404)
	var list VerdictList
	decodeJSON(t, f.do("GET", "/api/v1/policy/verdicts?repo=raw/open", "", alice), 200, &list)
	if len(list.Items) != 1 {
		t.Errorf("the verdicts of raw/open: %+v", list)
	}
	decodeJSON(t, f.do("GET", "/api/v1/policy/verdicts?digest="+made.Items[0].Subject.String(), "", bob), 200, &list)
	if len(list.Items) != 0 {
		t.Errorf("a stranger sees the verdicts of raw/open: %+v", list)
	}
	wantStatus(t, f.do("GET", "/api/v1/policy/verdicts", "", admin), 400)
	rr = f.do("GET", "/api/v1/policy/key", "", auth.Anonymous)
	if rr.Code != 200 || rr.Header().Get("Content-Type") != "application/x-pem-file" || !strings.Contains(rr.Body.String(), "BEGIN PUBLIC KEY") {
		t.Errorf("the key: %d %q %s", rr.Code, rr.Header().Get("Content-Type"), rr.Body)
	}
}

// mustRepo returns the repo format/name of the fixture.
func mustRepo(t *testing.T, f *fixture, formatName, name string) *index.Repo {
	t.Helper()
	r, err := f.st.GetRepo(t.Context(), formatName, name)
	must(t, err)
	return r
}

// TestPolicyKeysOfRepo: the repo keys enforcePull and quarantineNewVersions are checked, the
// latter on a proxy repo only.
func TestPolicyKeysOfRepo(t *testing.T) {
	for _, tt := range []struct {
		policy string
		kind   string
		ok     bool
	}{
		{`{"enforcePull": true}`, index.KindHosted, true},
		{`{"enforcePull": "yes"}`, index.KindHosted, false},
		{`{"quarantineNewVersions": "72h"}`, index.KindProxy, true},
		{`{"quarantineNewVersions": "0s"}`, index.KindProxy, true},
		{`{"quarantineNewVersions": "30s"}`, index.KindProxy, false},
		{`{"quarantineNewVersions": "72h"}`, index.KindHosted, false},
	} {
		p, err := checkPolicy("npm", json.RawMessage(tt.policy))
		if err == nil {
			err = checkPolicyKind(tt.kind, p)
		}
		if (err == nil) != tt.ok {
			t.Errorf("%s on a %s repo: %v, want ok %v", tt.policy, tt.kind, err, tt.ok)
		}
	}
}

// TestPolicyDenylistFeed: the flat deny list holds the rules of the repos the caller may read.
func TestPolicyDenylistFeed(t *testing.T) {
	f := policyFixture(t, nil, nil)
	wantStatus(t, f.do("POST", "/api/v1/policy/rules", `{"repoMask": "raw/open", "name": "evil", "version": "==1.0", "reason": "r"}`, admin), 201)
	var doc struct {
		Rules []struct {
			Name string `json:"name"`
		} `json:"rules"`
		Generation int64 `json:"generation"`
	}
	decodeJSON(t, f.do("GET", "/api/v1/policy/denylist", "", alice), 200, &doc)
	if len(doc.Rules) != 1 || doc.Rules[0].Name != "evil" || doc.Generation == 0 {
		t.Errorf("the deny list of a reader of raw/open: %+v", doc)
	}
	decodeJSON(t, f.do("GET", "/api/v1/policy/denylist", "", bob), 200, &doc)
	if len(doc.Rules) != 0 {
		t.Errorf("the deny list of a stranger: %+v", doc)
	}
}
