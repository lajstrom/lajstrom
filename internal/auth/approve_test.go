package auth

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/lajstrom/lajstrom/internal/index"
)

// TestApproveBinding: the requester does not approve its request (ErrScope); another admin does,
// which makes the binding with its binding.create row, the request's reason and the approval in
// its details, and the approval.approve row with the approver's reason and credential. A request
// is decided once; no request is ErrNotFound.
func TestApproveBinding(t *testing.T) {
	f := newApprovals(t)
	ctx, rootCtx := f.as(t, "admin", password), f.as(t, "root", password)
	a, err := f.s.RequestBinding(ctx, "user:alice", RoleAdmin, MaskAll, Origin{Source: "api", ChangeReason: "CHG-1"})
	must(t, err)
	alice, admin, root := f.uid(t, "alice"), f.uid(t, "admin"), f.uid(t, "root")
	_, err = f.s.Approve(ctx, a.ID, cli)
	wantErr(t, "an approval by the requester", err, ErrScope, "another admin does")
	f.now = f.now.Add(time.Hour)
	done, err := f.s.Approve(rootCtx, a.ID, Origin{Source: "api", ChangeReason: "CHG-1 approved"})
	must(t, err)
	bs, err := f.st.ListBindings(context.Background(), alice)
	if err != nil || len(bs) != 1 || bs[0].Role != RoleAdmin {
		t.Fatalf("after the approval: %+v, %v", bs, err)
	}
	decidedAt := f.now
	want := *a
	want.State, want.DecidedBy, want.DecidedAt, want.DecisionReason = index.ApprovalApproved, root, &decidedAt, "CHG-1 approved"
	want.Result = "binding:" + strconv.FormatInt(bs[0].ID, 10)
	if !sameJSON(t, *done, want) {
		t.Errorf("Approve = %+v, want %+v", *done, want)
	}
	d, by := f.detailsOf(t, ActionBindingCreate)
	approval, _ := d["approval"].(map[string]any)
	if by != root || d["changeReason"] != "CHG-1" || approval["id"] != float64(a.ID) || approval["requestedBy"] != admin {
		t.Errorf("binding.create by %s: %v", by, d)
	}
	if d, by = f.detailsOf(t, ActionApprovalApprove); by != root || d["changeReason"] != "CHG-1 approved" || d["requestedBy"] != admin ||
		d["credential"] != "password" {
		t.Errorf("approval.approve by %s: %v", by, d)
	}
	_, err = f.s.Approve(rootCtx, a.ID, cli)
	wantErr(t, "an approval again", err, index.ErrConflict, "it was decided")
	_, err = f.s.Reject(ctx, a.ID, cli)
	wantErr(t, "a rejection after", err, index.ErrConflict, "it was decided")
	_, err = f.s.Approve(rootCtx, 1<<40, cli)
	wantErr(t, "an approval of no request", err, index.ErrNotFound, "does not exist")
	_, err = f.s.Approve(context.Background(), a.ID, cli)
	wantErr(t, "an approval by anonymous", err, ErrScope, "is not one")
}

// TestApproveMaskDeploy: the approval of a deploy token over a mask makes the token with the
// request's hash, expiry and createdBy, and its secret authenticates from then on.
func TestApproveMaskDeploy(t *testing.T) {
	f := newApprovals(t)
	a, secret, err := f.s.RequestMaskDeploy(f.as(t, "admin", password), "raw/*", "fleet", []string{"read", "write"},
		Expiry{In: 30 * 24 * time.Hour}, Origin{Source: "api", ChangeReason: "CHG-2"})
	must(t, err)
	done, err := f.s.Approve(f.as(t, "root", password), a.ID, cli)
	must(t, err)
	tok, err := f.st.GetTokenByHash(context.Background(), HashSecret(secret))
	must(t, err)
	exp := usersNow.Add(30 * 24 * time.Hour)
	want := index.Token{ID: tok.ID, Kind: TokenDeploy, Name: "fleet", Hash: HashSecret(secret), Scopes: []string{"read", "write"},
		ExpiresAt: &exp, CreatedAt: usersNow, RepoMask: "raw/*"}
	if !sameJSON(t, *tok, want) || done.Result != "token:"+strconv.FormatInt(tok.ID, 10) {
		t.Fatalf("the token of the approval: %+v, want %+v; the request %+v", tok, want, done)
	}
	if !f.works(secret) {
		t.Error("the secret of an approved request does not authenticate")
	}
	d, by := f.detailsOf(t, ActionTokenCreate)
	if by != f.uid(t, "root") || d["createdBy"] != "password" || d["changeReason"] != "CHG-2" || d["approval"] == nil {
		t.Errorf("token.create by %s: %v", by, d)
	}
}

// TestRejectAndExpire: a request withdrawn by its requester, or expired, leaves a secret that
// never authenticates. An approval and a rejection at the expiry are ErrConflict; Expire marks the
// request once, with its approval.expire row by anonymous.
func TestRejectAndExpire(t *testing.T) {
	f := newApprovals(t)
	ctx, rootCtx := f.as(t, "admin", password), f.as(t, "root", password)
	withdrawn, gone, err := f.s.RequestMaskDeploy(ctx, "pypi/*", "publish", []string{"write"}, Expiry{}, cli)
	must(t, err)
	r, err := f.s.Reject(ctx, withdrawn.ID, Origin{Source: "api", ChangeReason: "withdrawn"})
	must(t, err)
	if r.State != index.ApprovalRejected || r.DecidedBy != f.uid(t, "admin") || r.DecisionReason != "withdrawn" {
		t.Errorf("a withdrawal: %+v", r)
	}
	late, lost, err := f.s.RequestMaskDeploy(ctx, "oci/*", "mirror", []string{"write"}, Expiry{}, cli)
	must(t, err)
	f.now = f.now.Add(ApprovalLifetime)
	_, err = f.s.Approve(rootCtx, late.ID, cli)
	wantErr(t, "an approval at the expiry", err, index.ErrConflict, "expired at")
	_, err = f.s.Reject(rootCtx, late.ID, cli)
	wantErr(t, "a rejection at the expiry", err, index.ErrConflict, "expired at")
	for i, want := range []int{1, 0} {
		if n, err := f.s.Expire(context.Background(), Origin{Source: "expiry"}); n != want || err != nil {
			t.Errorf("Expire, pass %d = %d, %v; want %d", i, n, err, want)
		}
	}
	if d, by := f.detailsOf(t, ActionApprovalExpire); by != "anonymous" || d["source"] != "expiry" || d["kind"] != "deploy-token" {
		t.Errorf("approval.expire by %s: %v", by, d)
	}
	got, err := f.s.Get(context.Background(), late.ID)
	must(t, err)
	if got.State != index.ApprovalExpired || got.DecidedBy != "anonymous" {
		t.Errorf("the expired request: %+v", got)
	}
	if f.works(gone) || f.works(lost) {
		t.Error("the secret of a request withdrawn or expired authenticates")
	}
}

// works reports whether secret authenticates as a bearer at the fixture's now.
func (f *approvalsFixture) works(secret string) bool {
	_, _, err := NewAuthenticator(Options{Index: f.st, Now: func() time.Time { return f.now }}).AuthenticateBearer(context.Background(), secret)
	return err == nil
}

// TestApprovalChecksAgain: an approval checks again what the request checked, and its requester:
// a requester disabled or no longer an admin, a principal gone, a binding made meanwhile, a name
// taken meanwhile and an expiry beyond a bound lowered meanwhile are each ErrConflict naming it,
// and the request stays pending; once the cause is gone, it is approved. A requester's admin role
// of a group counts while its membership has not lapsed.
func TestApprovalChecksAgain(t *testing.T) {
	f := newApprovals(t)
	ctx, rootCtx := f.as(t, "admin", password), f.as(t, "root", password)
	users := &Users{Index: f.st, Now: f.s.Now}
	refused := func(id int64, msg string) {
		t.Helper()
		_, err := f.s.Approve(rootCtx, id, cli)
		wantErr(t, "an approval", err, index.ErrConflict, msg)
		if a, err := f.s.Get(context.Background(), id); err != nil || a.State != index.ApprovalPending {
			t.Errorf("request %d after a refused approval: %+v, %v; want it pending", id, a, err)
		}
	}
	a, err := f.s.RequestBinding(ctx, "user:alice", RoleMaintainer, MaskAll, cli)
	must(t, err)
	must(t, users.Disable(context.Background(), "admin", cli))
	refused(a.ID, "its requester admin is disabled")
	must(t, users.Enable(context.Background(), "admin", cli))
	own, err := f.st.ListBindings(context.Background(), f.uid(t, "admin"))
	must(t, err)
	must(t, f.st.DeleteBinding(context.Background(), own[0].ID))
	refused(a.ID, "its requester admin no longer has the admin role")
	f.groupAdmin(t, "admin", "ops", f.now.Add(time.Minute))
	b, err := f.s.Bindings.Create(context.Background(), "user:alice", RoleMaintainer, MaskAll, cli)
	must(t, err)
	refused(a.ID, "exists already")
	must(t, f.s.Bindings.Delete(context.Background(), b.ID, cli))
	var c bindingChange
	must(t, json.Unmarshal(a.Change, &c))
	must(t, users.Delete(context.Background(), "alice", cli))
	refused(a.ID, "its principal "+c.Principal+" no longer exists")

	tok, _, err := f.s.RequestMaskDeploy(ctx, "raw/*", "fleet", []string{"write"}, Expiry{In: 48 * time.Hour}, cli)
	must(t, err)
	_, _, err = f.s.Tokens.CreateMaskDeploy(context.Background(), "raw/*", "fleet", []string{"read"}, Expiry{}, cli)
	must(t, err)
	refused(tok.ID, "is taken by a live token")
	other, _, err := f.s.RequestMaskDeploy(ctx, "raw/*", "other", []string{"write"}, Expiry{In: 48 * time.Hour}, cli)
	must(t, err)
	f.s.Tokens.DeployMaxLifetime = 24 * time.Hour
	refused(other.ID, "the deploy token no longer holds: expiresAt")
	f.s.Tokens.DeployMaxLifetime = 0
	if _, err := f.s.Approve(rootCtx, other.ID, cli); err != nil {
		t.Errorf("the approval once the bound is gone: %v", err)
	}
	third, _, err := f.s.RequestMaskDeploy(ctx, "raw/*", "third", []string{"write"}, Expiry{}, cli)
	must(t, err)
	f.now = f.now.Add(2 * time.Minute) // the requester's group lapses
	refused(third.ID, "no longer has the admin role")
}

// groupAdmin makes the user called name a member of the group, whose membership lapses at until,
// and gives the group the admin role.
func (f *approvalsFixture) groupAdmin(t *testing.T, name, group string, until time.Time) {
	t.Helper()
	ctx := context.Background()
	must(t, f.st.CreateBinding(ctx, &index.Binding{Principal: "group:" + group, Role: RoleAdmin, RepoMask: MaskAll}))
	u, err := f.st.GetUser(ctx, name)
	must(t, err)
	id := index.Identity{UserID: u.ID, Issuer: "https://idp.example", Subject: name}
	must(t, f.st.CreateIdentity(ctx, &id))
	must(t, f.st.SetIdentityGroups(ctx, id.ID, []string{group}, f.now, until))
}

// TestApprovalRace: of approvals and rejections of one request at once, one decides, and the
// binding is made only when an approval did.
func TestApprovalRace(t *testing.T) {
	f := newApprovals(t)
	a, err := f.s.RequestBinding(f.as(t, "admin", password), "user:alice", RoleAdmin, MaskAll, cli)
	must(t, err)
	const n = 8
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Go(func() {
			if i%2 == 0 {
				_, errs[i] = f.s.Approve(f.as(t, "root", password), a.ID, cli)
			} else {
				_, errs[i] = f.s.Reject(f.as(t, "admin", password), a.ID, cli)
			}
		})
	}
	wg.Wait()
	decided := 0
	for _, err := range errs {
		switch {
		case err == nil:
			decided++
		case !errors.Is(err, index.ErrConflict):
			t.Errorf("a decision that lost: %v, want ErrConflict", err)
		}
	}
	bs, err := f.st.ListBindings(context.Background(), f.uid(t, "alice"))
	must(t, err)
	got, err := f.s.Get(context.Background(), a.ID)
	must(t, err)
	if decided != 1 || (got.State == index.ApprovalApproved) != (len(bs) == 1) || len(bs) > 1 {
		t.Errorf("%d decisions at once: %d made, the request %s, bindings %+v", n, decided, got.State, bs)
	}
}

// TestMayApprove: an approval, and a request (#1393), take the admin's password, or a web UI's
// fresh sign-in through OIDC; a sign-in that is not fresh is asked for more, any other token and
// none are ErrScope naming the rule.
func TestMayApprove(t *testing.T) {
	f := newApprovals(t)
	fresh := Credential{Kind: CredPAT, TokenID: 4, SignIn: &SignIn{At: usersNow.Add(-time.Minute), AMR: []string{"pwd"}}}
	stale := Credential{Kind: CredPAT, TokenID: 5, SignIn: &SignIn{At: usersNow.Add(-time.Hour), AMR: []string{"pwd", "otp"}}}
	for _, may := range []struct {
		name string
		fn   func(context.Context) error
		rule string
	}{
		{"MayApprove", f.s.MayApprove, "a request is approved with"},
		{"MayRequest", f.s.MayRequest, "a request for a second admin is made with"},
	} {
		for _, tt := range []struct {
			name   string
			c      Credential
			stepUp bool
			ok     bool
		}{
			{"a password", password, false, true},
			{"a fresh sign-in", fresh, false, true},
			{"a stale sign-in", stale, true, false},
			{"a personal access token", Credential{Kind: CredPAT, TokenID: 6}, false, false},
			{"a deploy token", Credential{Kind: CredDeploy, TokenID: 7}, false, false},
			{"an OIDC login of the command line", Credential{Kind: CredOIDC}, false, false},
			{"none", Credential{}, false, false},
		} {
			err := may.fn(f.as(t, "root", tt.c))
			var stepUp *StepUpError
			if (err == nil) != tt.ok || errors.As(err, &stepUp) != tt.stepUp || (!tt.ok && !tt.stepUp && (!errors.Is(err, ErrScope) ||
				!strings.Contains(err.Error(), may.rule))) {
				t.Errorf("%s, %s: %v", may.name, tt.name, err)
			}
		}
	}
}

// TestApprovalServiceRequester: a service account requests nothing (#1393), so the approval of a
// request whose requester is one, made on a release before, is ErrConflict naming it, and the
// request stays pending, whatever the account's bindings.
func TestApprovalServiceRequester(t *testing.T) {
	f := newApprovals(t)
	ctx := context.Background()
	robot := index.User{Name: "robot", Kind: KindService, Owner: f.uid(t, "root")}
	must(t, f.st.CreateUser(ctx, &robot))
	must(t, f.st.CreateBinding(ctx, &index.Binding{Principal: UserPrincipal(&robot).String(), Role: RoleAdmin, RepoMask: MaskAll}))
	a, err := f.s.RequestBinding(f.as(t, "robot", Credential{Kind: CredPAT, TokenID: 8}), "user:alice", RoleAdmin, MaskAll, cli)
	must(t, err)
	_, err = f.s.Approve(f.as(t, "admin", password), a.ID, cli)
	wantErr(t, "the approval of a service account's request", err, index.ErrConflict, "its requester robot is a service account")
	if got, err := f.s.Get(ctx, a.ID); err != nil || got.State != index.ApprovalPending {
		t.Errorf("the request after the refusal: %+v, %v; want it pending", got, err)
	}
	if bs, _ := f.st.ListBindings(ctx, f.uid(t, "alice")); len(bs) != 0 {
		t.Errorf("alice's bindings after the refusal: %+v", bs)
	}
}

// TestApprovers: the users who could decide a request, for serve's warning: the enabled users
// with an admin binding, their own or a group's of their last login, service accounts apart;
// under the admin mode, the OIDC users only.
func TestApprovers(t *testing.T) {
	f := newApprovals(t)
	ctx := context.Background()
	count := func(mfa bool) int {
		t.Helper()
		n, err := Approvers(ctx, f.st, mfa)
		must(t, err)
		return n
	}
	if n, m := count(false), count(true); n != 2 || m != 0 {
		t.Errorf("admin and root, local users: %d, under the admin mode %d; want 2 and 0", n, m)
	}
	olga := index.User{Name: "olga", Kind: KindOIDC}
	must(t, f.st.CreateUser(ctx, &olga))
	f.groupAdmin(t, "olga", "ops", f.now.Add(-time.Minute)) // lapsed: she signs in again
	robot := index.User{Name: "robot", Kind: KindService, Owner: f.uid(t, "root")}
	must(t, f.st.CreateUser(ctx, &robot))
	must(t, f.st.CreateBinding(ctx, &index.Binding{Principal: UserPrincipal(&robot).String(), Role: RoleAdmin, RepoMask: MaskAll}))
	must(t, (&Users{Index: f.st}).Disable(ctx, "root", cli))
	if n, m := count(false), count(true); n != 2 || m != 1 {
		t.Errorf("admin, olga by a group, root disabled, a service account: %d, under the admin mode %d; want 2 and 1", n, m)
	}
}

// sameJSON reports whether a and b marshal to the same JSON.
func sameJSON(t *testing.T, a, b any) bool {
	t.Helper()
	x, err := json.Marshal(a)
	must(t, err)
	y, err := json.Marshal(b)
	must(t, err)
	return string(x) == string(y)
}

// must stops the test on an error.
func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
