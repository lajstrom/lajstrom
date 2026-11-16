package ci

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/lajstrom/lajstrom/internal/auth"
	"github.com/lajstrom/lajstrom/internal/config"
	"github.com/lajstrom/lajstrom/internal/index"
	"github.com/lajstrom/lajstrom/internal/webhook"
)

// testNow is the clock of the tests of the policies that write.
var testNow = time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC)

// The admins of the tests: alice makes the policies, bob approves them.
var (
	alice = auth.Principal{Kind: auth.KindUser, Name: "019a6c3e-7b1e-7c3d-8e2f-0000000000a1"}
	bob   = auth.Principal{Kind: auth.KindUser, Name: "019a6c3e-7b1e-7c3d-8e2f-0000000000b0"}
)

// writePolicy is a policy of the API of acme/app that publishes to oci/app from its tags.
func writePolicy(name string) Policy {
	return Policy{Name: name, Issuer: "github", Claims: map[string][]string{"repository_id": {"123"}, "ref": {"refs/tags/v*"}},
		Grants: []index.CIGrant{{Role: "writer", Mask: "oci/app"}, {Role: "reader", Mask: "*/*"}}}
}

// signedIn is a context of p with the token of a web UI's sign-in that authenticated ago before
// testNow with amr.
func signedIn(p auth.Principal, ago time.Duration, amr ...string) context.Context {
	return auth.WithCredential(auth.WithPrincipal(context.Background(), p),
		auth.Credential{Kind: auth.CredPAT, TokenID: 7, UserID: 1, SignIn: &auth.SignIn{At: testNow.Add(-ago), AMR: amr}})
}

// at is ctx with the If-Match of the revision of p.
func at(ctx context.Context, p *Policy) context.Context {
	want := p.Revision
	return index.WithCondition(ctx, func(_, revision int64) error {
		if revision != want {
			return errStale
		}
		return nil
	})
}

var errStale = errors.New("stale")

// writeFixture is a Service of policies that write over a new index, with a webhook of the admin
// that gets every event, and a Live over the index.
type writeFixture struct {
	st   index.Store
	s    *Service
	live *Live
	hook index.Webhook
	now  time.Time
}

func newWriteFixture(t *testing.T, approval string) *writeFixture {
	t.Helper()
	st := newStore(t)
	c := serviceCI()
	c.APIWriteApproval = approval
	f := &writeFixture{st: st, now: testNow}
	clock := func() time.Time { return f.now }
	f.s = &Service{Index: st, Config: c, Webhooks: &webhook.Outbox{Source: "https://art.example.com"}, Now: clock}
	var err error
	if f.live, err = NewLive(c, st, nil); err != nil {
		t.Fatal(err)
	}
	f.live.now = clock
	f.hook = index.Webhook{Mask: "oci/*", URL: "https://hooks.example.com/", SecretEnc: []byte("s"), Active: true, CreatedBy: "user:x"}
	if err := st.CreateWebhook(context.Background(), &f.hook); err != nil {
		t.Fatal(err)
	}
	return f
}

// counts reports whether the Live counts the policy name now.
func (f *writeFixture) counts(t *testing.T, name string) bool {
	t.Helper()
	ps, err := f.live.Current(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	_, ok := ps.policies[name]
	return ok
}

// events returns the types of the events of the fixture's webhook, in order.
func (f *writeFixture) events(t *testing.T) []string {
	t.Helper()
	ds, err := f.st.ListWebhookDeliveries(context.Background(), f.hook.ID, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, d := range ds {
		out = append(out, d.EventType)
	}
	slices.Reverse(out) // the log is newest first
	return out
}

// TestWritePolicy: a policy of the API that writes, made with a fresh sign-in with a second
// factor, waits pending and grants nothing until another admin, signed in so too, approves the
// revision they read; a renewal waits beside the policy, which keeps its expiry until it is
// approved; a change that writes waits again; every step is an audit row and an event of the
// admin's webhooks (docs/spec/rbac.md §11).
func TestWritePolicy(t *testing.T) {
	f := newWriteFixture(t, config.CIApprovalSecondAdmin)
	approved(t, f)
	renewed(t, f)
	changed(t, f)
	want := []string{webhook.TypeCIPolicyCreated, webhook.TypeCIPolicyApproved, webhook.TypeCIPolicyRenewed, webhook.TypeCIPolicyApproved,
		webhook.TypeCIPolicyUpdated, webhook.TypeCIPolicyUpdated, webhook.TypeCIPolicyDeleted}
	if got := f.events(t); !slices.Equal(got, want) {
		t.Errorf("the events: %v, want %v", got, want)
	}
	checkWriteRows(t, f.st)
}

// approved makes app-publish, pending, refuses the approvals that lack a step, and has bob approve
// it.
func approved(t *testing.T, f *writeFixture) {
	t.Helper()
	o := auth.Origin{Source: "api"}
	p, err := f.s.Create(signedIn(alice, time.Minute, "pwd", "otp"), writePolicy("app-publish"), o)
	if err != nil || p.State != index.CIPolicyPending || p.ExpiresAt == nil || !p.ExpiresAt.Equal(testNow.Add(WriteLifetime)) ||
		p.UpdatedBy != alice.String() {
		t.Fatalf("Create = %+v, %v", p, err)
	}
	if f.counts(t, "app-publish") {
		t.Error("a pending policy counts")
	}
	for name, tc := range map[string]struct {
		ctx context.Context
		err error
	}{
		"by its maker":           {at(signedIn(alice, time.Minute, "mfa"), p), auth.ErrScope},
		"without a sign-in":      {at(auth.WithPrincipal(context.Background(), bob), p), auth.ErrScope},
		"a stale sign-in":        {at(signedIn(bob, 11*time.Minute, "mfa"), p), auth.ErrScope},
		"one factor":             {at(signedIn(bob, time.Minute, "pwd"), p), auth.ErrScope},
		"without If-Match":       {signedIn(bob, time.Minute, "mfa"), auth.ErrInvalid},
		"of a revision not read": {at(signedIn(bob, time.Minute, "mfa"), &Policy{Revision: 9}), errStale},
	} {
		if _, err := f.s.Approve(tc.ctx, "app-publish", o); !errors.Is(err, tc.err) {
			t.Errorf("an approval %s: %v, want %v", name, err, tc.err)
		}
	}
	p, err = f.s.Approve(at(signedIn(bob, time.Minute, "hwk", "pin"), p), "app-publish", o)
	if err != nil || p.State != index.CIPolicyActive || p.ApprovedBy != bob.String() || p.ApprovedAt == nil {
		t.Fatalf("Approve = %+v, %v", p, err)
	}
	if !f.counts(t, "app-publish") {
		t.Error("an approved policy does not count")
	}
	if _, err := f.s.Approve(at(signedIn(bob, time.Minute, "mfa"), p), "app-publish", o); !errors.Is(err, index.ErrConflict) {
		t.Errorf("an approval of nothing that waits: %v", err)
	}
}

// renewed has bob renew app-publish a day after testNow and alice approve the renewal.
func renewed(t *testing.T, f *writeFixture) {
	t.Helper()
	o := auth.Origin{Source: "api"}
	f.now = testNow.Add(24 * time.Hour)
	expiry := testNow.Add(100 * 24 * time.Hour)
	p, err := f.s.Renew(signedIn(bob, -24*time.Hour+time.Minute, "mfa"), "app-publish", &expiry, o)
	if err != nil || p.State != index.CIPolicyActive || p.Renewal == nil || !p.Renewal.ExpiresAt.Equal(expiry) ||
		!p.ExpiresAt.Equal(testNow.Add(WriteLifetime)) {
		t.Fatalf("Renew = %+v, %v", p, err)
	}
	if !f.counts(t, "app-publish") {
		t.Error("a policy whose renewal waits does not count")
	}
	p, err = f.s.Approve(at(signedIn(alice, -24*time.Hour+time.Minute, "mfa"), p), "app-publish", o)
	if err != nil || p.Renewal != nil || !p.ExpiresAt.Equal(expiry) || p.ApprovedBy != alice.String() {
		t.Fatalf("Approve of the renewal = %+v, %v", p, err)
	}
}

// changed has alice change app-publish to write more, which waits again, then to read only,
// without a sign-in, and delete it.
func changed(t *testing.T, f *writeFixture) {
	t.Helper()
	o := auth.Origin{Source: "api"}
	more := writePolicy("")
	more.Grants = append(more.Grants, index.CIGrant{Role: "maintainer", Mask: "pypi/app"})
	if p, err := f.s.Update(signedIn(alice, -24*time.Hour+time.Minute, "mfa"), "app-publish", more, o); err != nil ||
		p.State != index.CIPolicyPending || p.ApprovedBy != "" {
		t.Fatalf("Update = %+v, %v", p, err)
	}
	if f.counts(t, "app-publish") {
		t.Error("a policy changed to write more counts before its approval")
	}
	if p, err := f.s.Update(auth.WithPrincipal(context.Background(), alice), "app-publish", readPolicy(""), o); err != nil ||
		p.State != index.CIPolicyActive || p.ExpiresAt != nil {
		t.Fatalf("Update to read only, without a sign-in = %+v, %v", p, err)
	}
	if err := f.s.Delete(auth.WithPrincipal(context.Background(), alice), "app-publish", o); err != nil {
		t.Fatal(err)
	}
}

// checkWriteRows checks the audit rows of TestWritePolicy: one per step, with the state, the
// expiry and the sign-in of the request.
func checkWriteRows(t *testing.T, st index.Store) {
	t.Helper()
	rows, err := st.ListAudit(context.Background(), index.AuditFilter{})
	if err != nil {
		t.Fatal(err)
	}
	var actions []string
	for _, r := range slices.Backward(rows) {
		actions = append(actions, r.Action)
		var d map[string]any
		_ = json.Unmarshal(r.Details, &d)
		if r.Action == ActionPolicyCreate {
			in, _ := d["signIn"].(map[string]any)
			if d["state"] != index.CIPolicyPending || d["expiresAt"] != "2027-05-15T12:00:00Z" || in["token"] != float64(7) ||
				in["authTime"] != "2026-11-16T11:59:00Z" {
				t.Errorf("the create row: %s", r.Details)
			}
		}
	}
	want := "ci.policy.create ci.policy.approve ci.policy.renew ci.policy.approve ci.policy.update ci.policy.update ci.policy.delete"
	if got := strings.Join(actions, " "); got != want {
		t.Errorf("the audit rows: %s, want %s", got, want)
	}
}

// TestWritePolicyRefusals: the steps of a policy that writes refuse each with what is missing.
func TestWritePolicyRefusals(t *testing.T) {
	f := newWriteFixture(t, config.CIApprovalSecondAdmin)
	o := auth.Origin{Source: "api"}
	fresh := signedIn(alice, time.Minute, "mfa")
	outside, far, past := writePolicy("p"), writePolicy("p"), writePolicy("p")
	outside.Grants[0].Mask = "raw/app"
	farAt, pastAt := testNow.Add(WriteLifetime+time.Second), testNow
	far.ExpiresAt, past.ExpiresAt = &farAt, &pastAt
	for name, tc := range map[string]struct {
		ctx context.Context
		p   Policy
		err error
		why string
	}{
		"a token of the person":    {auth.WithCredential(auth.WithPrincipal(context.Background(), alice), auth.Credential{Kind: auth.CredPAT}), writePolicy("p"), auth.ErrScope, "credential is a personal access token"},
		"the command line":         {context.Background(), writePolicy("p"), auth.ErrScope, "credential is none"},
		"a stale sign-in":          {signedIn(alice, time.Hour, "mfa"), writePolicy("p"), auth.ErrScope, "1h0m0s ago"},
		"one factor":               {signedIn(alice, time.Minute, "pwd"), writePolicy("p"), auth.ErrScope, "amr (pwd) shows none"},
		"outside apiWriteRepos":    {fresh, outside, auth.ErrScope, "grants[0]: writer on raw/app is outside auth.ci.apiWriteRepos, oci/*, pypi/app"},
		"an expiry beyond 180d":    {fresh, far, auth.ErrInvalid, "at most 180 days ahead"},
		"an expiry that is past":   {fresh, past, auth.ErrInvalid, "must be in the future"},
		"a rule of the policy too": {fresh, Policy{Name: "p", Issuer: "github"}, auth.ErrInvalid, "claims: must name the project"},
	} {
		_, err := f.s.Create(tc.ctx, tc.p, o)
		if !errors.Is(err, tc.err) || !strings.Contains(err.Error(), tc.why) {
			t.Errorf("%s: %v, want %v saying %q", name, err, tc.err, tc.why)
		}
	}
	if _, err := f.s.Create(fresh, writePolicy("app-publish"), o); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.Renew(signedIn(bob, time.Minute, "mfa"), "app-publish", nil, o); !errors.Is(err, index.ErrConflict) {
		t.Errorf("a renewal of a pending policy: %v", err)
	}
	p, _ := f.s.Get(context.Background(), "app-publish")
	f.now = testNow.Add(WriteLifetime)
	if _, err := f.s.Approve(at(signedIn(bob, -WriteLifetime+time.Minute, "mfa"), p), "app-publish", o); !errors.Is(err, index.ErrConflict) ||
		!strings.Contains(err.Error(), "its expiry passed before the approval") {
		t.Errorf("an approval after the expiry: %v", err)
	}
	if got := f.events(t); !slices.Equal(got, []string{webhook.TypeCIPolicyCreated}) {
		t.Errorf("events of refusals: %v", got)
	}
}

// TestApprovalNone: with auth.ci.apiWriteApproval none a policy that writes counts once made with a
// fresh sign-in, and a renewal at once; the other steps hold.
func TestApprovalNone(t *testing.T) {
	f := newWriteFixture(t, config.CIApprovalNone)
	o := auth.Origin{Source: "api"}
	p, err := f.s.Create(signedIn(alice, time.Minute, "mfa"), writePolicy("app-publish"), o)
	if err != nil || p.State != index.CIPolicyActive || p.ApprovedBy != "" || !f.counts(t, "app-publish") {
		t.Fatalf("Create = %+v, %v", p, err)
	}
	soon := testNow.Add(time.Hour)
	if p, err = f.s.Renew(signedIn(alice, time.Minute, "mfa"), "app-publish", &soon, o); err != nil || p.Renewal != nil ||
		!p.ExpiresAt.Equal(soon) {
		t.Fatalf("Renew = %+v, %v", p, err)
	}
	if _, err := f.s.Create(context.Background(), writePolicy("other"), o); !errors.Is(err, auth.ErrScope) {
		t.Errorf("a policy that writes without a sign-in: %v", err)
	}
	if _, err := f.s.Approve(at(signedIn(bob, time.Minute, "mfa"), p), "app-publish", o); !errors.Is(err, index.ErrConflict) {
		t.Errorf("an approval with nothing that waits: %v", err)
	}
}

// TestExpire: a policy past its expiry grants nothing at once; the passes of two replicas mark it
// expired once, with one audit row and one event; a renewal of it waits pending.
func TestExpire(t *testing.T) {
	f := newWriteFixture(t, config.CIApprovalSecondAdmin)
	o := auth.Origin{Source: "api"}
	soon := testNow.Add(time.Hour)
	p := writePolicy("app-publish")
	p.ExpiresAt = &soon
	made, err := f.s.Create(signedIn(alice, time.Minute, "mfa"), p, o)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.Approve(at(signedIn(bob, time.Minute, "mfa"), made), "app-publish", o); err != nil {
		t.Fatal(err)
	}
	if n, err := f.s.Expire(context.Background(), auth.Origin{Source: "expiry"}); n != 0 || err != nil {
		t.Errorf("Expire before the expiry = %d, %v", n, err)
	}
	f.now = soon
	if f.counts(t, "app-publish") {
		t.Error("a policy at its expiry counts")
	}
	if got, _ := f.s.Get(context.Background(), "app-publish"); got.State != index.CIPolicyExpired {
		t.Errorf("Get at the expiry: state %s", got.State)
	}
	if marked := expireOnReplicas(t, f); marked != 1 {
		t.Errorf("the passes marked %d, want 1", marked)
	}
	checkExpired(t, f)
	renewed, err := f.s.Renew(signedIn(alice, time.Minute-time.Hour, "mfa"), "app-publish", nil, o)
	if err != nil || renewed.State != index.CIPolicyPending || renewed.Renewal != nil || !renewed.ExpiresAt.Equal(soon.Add(WriteLifetime)) {
		t.Errorf("Renew of an expired policy = %+v, %v", renewed, err)
	}
}

// expireOnReplicas runs a pass of the expiry on the Service of f, on another over the same index,
// and on the first again, and returns how many policies they marked in all.
func expireOnReplicas(t *testing.T, f *writeFixture) int {
	t.Helper()
	other := &Service{Index: f.st, Config: f.s.Config, Webhooks: f.s.Webhooks, Now: f.s.Now}
	var marked int
	for _, s := range []*Service{f.s, other, f.s} {
		n, err := s.Expire(context.Background(), auth.Origin{Source: "expiry"})
		if err != nil {
			t.Fatal(err)
		}
		marked += n
	}
	return marked
}

// checkExpired checks the expiry of TestExpire: one ci.policy.expire row, anonymous with the
// source expiry, and one event.
func checkExpired(t *testing.T, f *writeFixture) {
	t.Helper()
	rows, _ := f.st.ListAudit(context.Background(), index.AuditFilter{Action: ActionPolicyExpire})
	if len(rows) != 1 || rows[0].Principal != "anonymous" || !strings.Contains(string(rows[0].Details), `"source":"expiry"`) {
		t.Errorf("the ci.policy.expire rows: %+v", rows)
	}
	want := []string{webhook.TypeCIPolicyCreated, webhook.TypeCIPolicyApproved, webhook.TypeCIPolicyExpired}
	if got := f.events(t); !slices.Equal(got, want) {
		t.Errorf("the events: %v, want %v", got, want)
	}
}
