package auth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/lajstrom/lajstrom/internal/index"
	"github.com/lajstrom/lajstrom/internal/index/sqlite"
)

// approvalsFixture is the approvals service over the index of the users admin and root, each with
// the admin binding, and alice; the formats raw, pypi and oci; its clock, which a test moves.
type approvalsFixture struct {
	s   *Approvals
	st  *sqlite.Store
	now time.Time
}

func newApprovals(t *testing.T) *approvalsFixture {
	t.Helper()
	users, st, _ := newUsers(t)
	createAdminAndAlice(t, users)
	if _, err := users.Create(context.Background(), "root", "root-password", true, cli); err != nil {
		t.Fatal(err)
	}
	f := &approvalsFixture{st: st, now: usersNow}
	clock := func() time.Time { return f.now }
	formats := func(n string) bool { return slices.Contains([]string{"raw", "pypi", "oci"}, n) }
	f.s = &Approvals{Index: st, Now: clock, Log: users.Log,
		Bindings: &Bindings{Index: st, Formats: formats, Now: clock, Log: users.Log},
		Tokens:   &Tokens{Index: st, Formats: formats, Now: clock, Log: users.Log}}
	return f
}

// as returns a context of the user called name with the credential c.
func (f *approvalsFixture) as(t *testing.T, name string, c Credential) context.Context {
	t.Helper()
	u, err := f.st.GetUser(context.Background(), name)
	if err != nil {
		t.Fatal(err)
	}
	return WithCredential(WithPrincipal(context.Background(), UserPrincipal(u)), c)
}

// uid returns the principal of the user called name, user:<uid>.
func (f *approvalsFixture) uid(t *testing.T, name string) string {
	t.Helper()
	u, err := f.st.GetUser(context.Background(), name)
	if err != nil {
		t.Fatal(err)
	}
	return UserPrincipal(u).String()
}

// detailsOf returns the details of the newest audit row of action, and its principal.
func (f *approvalsFixture) detailsOf(t *testing.T, action string) (map[string]any, string) {
	t.Helper()
	evs, err := f.st.ListAudit(context.Background(), index.AuditFilter{Action: action})
	if err != nil || len(evs) == 0 {
		t.Fatalf("%s rows: %v, %v", action, evs, err)
	}
	var d map[string]any
	if err := json.Unmarshal(evs[0].Details, &d); err != nil {
		t.Fatal(err)
	}
	return d, evs[0].Principal
}

// TestApprovalRequestBinding: a binding of the role admin, or on */*, is requested in place of
// being made, pending for ApprovalLifetime, its principal as it would be bound, with its
// approval.request row, which has its credential, and grants nothing. The same change twice and a
// binding that exists are ErrConflict, a user that does not exist ErrInvalid, a request by
// anonymous ErrScope.
func TestApprovalRequestBinding(t *testing.T) {
	f := newApprovals(t)
	ctx := f.as(t, "admin", Credential{Kind: CredPassword})
	a, err := f.s.RequestBinding(ctx, "user:alice", RoleAdmin, MaskAll, Origin{Source: "api", ChangeReason: "CHG-1"})
	if err != nil {
		t.Fatal(err)
	}
	alice, admin := f.uid(t, "alice"), f.uid(t, "admin")
	want := Approval{ID: a.ID, Kind: index.ApprovalBinding, Change: json.RawMessage(`{"principal":"` + alice + `","role":"admin","repoMask":"*/*"}`),
		ChangeReason: "CHG-1", State: index.ApprovalPending, RequestedBy: admin, RequestedAt: usersNow, ExpiresAt: usersNow.Add(ApprovalLifetime)}
	if !reflect.DeepEqual(*a, want) {
		t.Errorf("RequestBinding = %+v, want %+v", *a, want)
	}
	if bs, _ := f.st.ListBindings(context.Background(), alice); len(bs) != 0 {
		t.Errorf("a request made the binding: %+v", bs)
	}
	d, by := f.detailsOf(t, ActionApprovalRequest)
	if by != admin || d["kind"] != "binding" || d["changeReason"] != "CHG-1" || d["expiresAt"] == nil || d["credential"] != "password" {
		t.Errorf("approval.request by %s: %v", by, d)
	}
	_, err = f.s.RequestBinding(ctx, "user:alice", RoleAdmin, MaskAll, cli)
	wantErr(t, "the same request twice", err, index.ErrConflict, "waits for a second admin already")
	_, err = f.s.RequestBinding(ctx, "user:admin", RoleAdmin, MaskAll, cli)
	wantErr(t, "a binding that exists", err, index.ErrConflict, "exists already")
	_, err = f.s.RequestBinding(ctx, "user:nobody", RoleReader, MaskAll, cli)
	wantErr(t, "a user that does not exist", err, ErrInvalid, "does not exist")
	_, err = f.s.RequestBinding(context.Background(), "user:alice", RoleReader, MaskAll, cli)
	wantErr(t, "a request by anonymous", err, ErrScope, "is not one")
}

// TestApprovalRequestMaskDeploy: a deploy token over a mask with write is requested in place of
// being made, with the credential's rule of a deploy token over a mask; its secret, answered once,
// is no credential, and the request keeps its hash and its createdBy, never the secret. The same
// name over the mask twice, and a name a live token has, are ErrConflict.
func TestApprovalRequestMaskDeploy(t *testing.T) {
	f := newApprovals(t)
	ctx := f.as(t, "admin", Credential{Kind: CredPassword})
	_, _, err := f.s.RequestMaskDeploy(f.as(t, "admin", Credential{Kind: CredPAT, TokenID: 3}), "raw/*", "fleet", []string{"write"},
		Expiry{}, cli)
	wantErr(t, "a request with a personal access token", err, ErrScope, "not with a token")
	tok, secret, err := f.s.RequestMaskDeploy(ctx, "raw/*", "fleet", []string{"write", "read"}, Expiry{In: 30 * 24 * time.Hour}, cli)
	if err != nil {
		t.Fatal(err)
	}
	exp := usersNow.Add(30 * 24 * time.Hour).Format(time.RFC3339)
	if want := `{"name":"fleet","repoMask":"raw/*","scopes":["read","write"],"expiresAt":"` + exp + `"}`; string(tok.Change) != want {
		t.Errorf("RequestMaskDeploy = %s, want %s", tok.Change, want)
	}
	row, err := f.st.GetApproval(context.Background(), tok.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{HashSecret(secret), `"createdBy":"password"`} {
		if !strings.Contains(string(row.Change), s) {
			t.Errorf("the request keeps %s, without %s", row.Change, s)
		}
	}
	if !strings.HasPrefix(secret, DeployPrefix) || strings.Contains(string(row.Change), secret) {
		t.Errorf("the secret %q, kept in %s", secret, row.Change)
	}
	if _, _, err := NewAuthenticator(Options{Index: f.st}).AuthenticateBearer(context.Background(), secret); err == nil {
		t.Error("the secret of a request that waits authenticates")
	}
	_, _, err = f.s.RequestMaskDeploy(ctx, "raw/*", "fleet", []string{"write"}, Expiry{}, cli)
	wantErr(t, "the same name over the mask twice", err, index.ErrConflict, "waits for a second admin already")
	if _, _, err := f.s.Tokens.CreateMaskDeploy(ctx, "pypi/*", "taken", []string{"read"}, Expiry{}, cli); err != nil {
		t.Fatal(err)
	}
	_, _, err = f.s.RequestMaskDeploy(ctx, "pypi/*", "taken", []string{"write"}, Expiry{}, cli)
	wantErr(t, "a name a live token has", err, index.ErrConflict, "is taken by a live token")
}

// TestApprovalList: a request is read by its id, and the requests are listed newest first, of a
// state or of every state; another state is ErrInvalid, no request ErrNotFound.
func TestApprovalList(t *testing.T) {
	f := newApprovals(t)
	ctx := f.as(t, "admin", Credential{Kind: CredPassword})
	tok, _, err := f.s.RequestMaskDeploy(ctx, "raw/*", "fleet", []string{"write"}, Expiry{}, cli)
	if err != nil {
		t.Fatal(err)
	}
	a, err := f.s.RequestBinding(ctx, "group:ops", RoleReader, MaskAll, cli)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := f.s.Get(context.Background(), tok.ID); err != nil || !reflect.DeepEqual(got, tok) {
		t.Errorf("Get = %+v, %v; want %+v", got, err, tok)
	}
	_, err = f.s.Get(context.Background(), 1<<40)
	wantErr(t, "Get of no request", err, index.ErrNotFound, "does not exist")
	for _, tt := range []struct {
		state string
		want  []int64
	}{{"", []int64{a.ID, tok.ID}}, {index.ApprovalPending, []int64{a.ID, tok.ID}}, {index.ApprovalApproved, nil}} {
		list, err := f.s.List(context.Background(), tt.state, 0, 10)
		var ids []int64
		for _, a := range list {
			ids = append(ids, a.ID)
		}
		if err != nil || !slices.Equal(ids, tt.want) {
			t.Errorf("List(%q) = %v, %v; want %v", tt.state, ids, err, tt.want)
		}
	}
	_, err = f.s.List(context.Background(), "waiting", 0, 10)
	wantErr(t, "a list of another state", err, ErrInvalid, "is not pending")
}

// wantErr fails the test unless err wraps target and says msg.
func wantErr(t *testing.T, what string, err, target error, msg string) {
	t.Helper()
	if !errors.Is(err, target) || !strings.Contains(fmt.Sprint(err), msg) {
		t.Errorf("%s: %v, want %v saying %q", what, err, target, msg)
	}
}
