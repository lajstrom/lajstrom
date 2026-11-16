package auth

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/lajstrom/lajstrom/internal/index"
)

// newServiceFixture is the users of createAdminAndAlice, the services over them, and robot, a
// service account owned by alice.
func newServiceFixture(t *testing.T) (*renameDeleteFixture, *User) {
	t.Helper()
	f := newRenameDeleteFixture(t)
	robot, err := f.users.CreateService(context.Background(), "robot", "user:alice", cli)
	if err != nil {
		t.Fatal(err)
	}
	return f, robot
}

// TestServiceAccountCreate covers docs/spec/rbac.md §1: a user of kind service without a
// password, its owner kept as user:<uid> or group:<name> and answered with a user owner's name,
// one user.create event with its kind and owner.
func TestServiceAccountCreate(t *testing.T) {
	ctx := context.Background()
	f, robot := newServiceFixture(t)
	b, _ := json.Marshal(robot)
	want := `{"uid":"` + robot.UID + `","name":"robot","kind":"service","disabled":false,"createdAt":"2026-11-07T12:00:00Z","lastSeenAt":null,` +
		`"owner":"user:` + f.alice.UID + `","ownerName":"alice"}`
	if string(b) != want {
		t.Errorf("service account = %s, want %s", b, want)
	}
	row, err := f.st.GetUser(ctx, "robot")
	if err != nil || row.Kind != KindService || row.PasswordHash != "" || row.Owner != "user:"+f.alice.UID {
		t.Errorf("row = %+v, %v; want a service account without a password, owned by alice's uid", row, err)
	}
	if _, err := f.users.CreateService(ctx, "deployer", "group:ops", cli); err != nil {
		t.Fatal(err)
	}
	evs, _ := f.st.ListAudit(ctx, index.AuditFilter{Action: ActionUserCreate})
	if len(evs) != 4 || named(t, f.st, string(evs[1].Details)) != `{"kind":"service","name":"robot","owner":"user:alice","source":"cli"}` ||
		string(evs[0].Details) != `{"kind":"service","name":"deployer","owner":"group:ops","source":"cli"}` {
		t.Errorf("user.create events %+v", evs)
	}
}

// TestServiceAccountList: List selects the users of a kind, a service account with its owner;
// an unknown kind is ErrInvalid.
func TestServiceAccountList(t *testing.T) {
	ctx := context.Background()
	f, _ := newServiceFixture(t)
	if _, err := f.users.CreateService(ctx, "deployer", "group:ops", cli); err != nil {
		t.Fatal(err)
	}
	for kind, names := range map[string]string{"": "admin alice deployer robot", KindService: "deployer robot", KindLocal: "admin alice", KindOIDC: ""} {
		list, err := f.users.List(ctx, kind)
		got := make([]string, len(list))
		for i, u := range list {
			got[i] = u.Name
		}
		if err != nil || strings.Join(got, " ") != names {
			t.Errorf("List(%q) = %v, %v; want %s", kind, got, err, names)
		}
	}
	if list, _ := f.users.List(ctx, KindService); len(list) != 2 || list[0].Owner != "group:ops" || list[0].OwnerName != "" {
		t.Errorf("List(service) = %+v; want deployer owned by group:ops, without an owner name", list)
	}
	if _, err := f.users.List(ctx, "robot"); !errors.Is(err, ErrInvalid) {
		t.Errorf("List of an unknown kind = %v, want ErrInvalid", err)
	}
}

// TestServiceAccountCreateRefused: an owner that is no live user, a service account, a principal
// that is neither a user nor a group, or a group name outside the rule; a name that is taken or
// breaks its rule. Each writes nothing.
func TestServiceAccountCreateRefused(t *testing.T) {
	ctx := context.Background()
	f, _ := newServiceFixture(t)
	before, _ := f.st.ListAudit(ctx, index.AuditFilter{})
	for _, tt := range []struct {
		name, owner, msg string
		want             error
	}{
		{"bot", "user:nobody", "owner: user nobody does not exist", ErrInvalid},
		{"bot", "user:robot", "owner: robot is a service account", ErrInvalid},
		{"bot", "token:1", `owner: "token:1" is neither user:<name> nor group:<name>`, ErrInvalid},
		{"bot", "anonymous", "is neither", ErrInvalid},
		{"bot", "", "owner: ", ErrInvalid},
		{"bot", "group: ops", "owner: ", ErrInvalid},
		{"alice", "user:admin", "user alice already exists", index.ErrConflict},
		{"Bot", "user:admin", "name: ", ErrInvalid},
	} {
		_, err := f.users.CreateService(ctx, tt.name, tt.owner, cli)
		if !errors.Is(err, tt.want) || !strings.Contains(err.Error(), tt.msg) {
			t.Errorf("CreateService(%q, %q) = %v, want %v with %q", tt.name, tt.owner, err, tt.want, tt.msg)
		}
	}
	if after, _ := f.st.ListAudit(ctx, index.AuditFilter{}); len(after) != len(before) {
		t.Errorf("the refusals wrote %d audit events", len(after)-len(before))
	}
}

// TestServiceAccountOwner: SetOwner changes a service account's owner with one user.owner event,
// none for the owner it has; a user of another kind has no owner.
func TestServiceAccountOwner(t *testing.T) {
	ctx := context.Background()
	f, _ := newServiceFixture(t)
	for range 2 {
		if err := f.users.SetOwner(ctx, "robot", "group:ops", cli); err != nil {
			t.Fatal(err)
		}
	}
	if err := f.users.SetOwner(ctx, "robot", "user:"+f.alice.UID, cli); err != nil {
		t.Fatal(err)
	}
	evs, _ := f.st.ListAudit(ctx, index.AuditFilter{Action: ActionUserOwner})
	if len(evs) != 2 || named(t, f.st, string(evs[1].Details)) != `{"from":"user:alice","name":"robot","owner":"group:ops","source":"cli"}` ||
		named(t, f.st, string(evs[0].Details)) != `{"from":"group:ops","name":"robot","owner":"user:alice","source":"cli"}` {
		t.Errorf("user.owner events %+v", evs)
	}
	for _, tt := range []struct {
		name, owner string
		want        error
	}{
		{"alice", "user:admin", ErrInvalid},
		{"robot", "user:robot", ErrInvalid},
		{"nobody", "user:admin", index.ErrNotFound},
	} {
		if err := f.users.SetOwner(ctx, tt.name, tt.owner, cli); !errors.Is(err, tt.want) {
			t.Errorf("SetOwner(%s, %s) = %v, want %v", tt.name, tt.owner, err, tt.want)
		}
	}
}

// TestServiceAccountOwnerGone: a disabled owner changes nothing but ownerDisabled; deleting the
// owner of a live service account is a conflict naming it, until the account has another owner.
func TestServiceAccountOwnerGone(t *testing.T) {
	ctx := context.Background()
	f, _ := newServiceFixture(t)
	if err := f.users.Disable(ctx, "alice", cli); err != nil {
		t.Fatal(err)
	}
	if u, err := f.users.Get(ctx, "robot"); err != nil || !u.OwnerDisabled || u.Disabled || u.OwnerName != "alice" {
		t.Errorf("robot with alice disabled: %+v, %v; want ownerDisabled, robot enabled", u, err)
	}
	err := f.users.Delete(ctx, "alice", cli)
	if !errors.Is(err, index.ErrConflict) || !strings.Contains(err.Error(), "owns the service accounts robot") {
		t.Errorf("deleting alice = %v, want a conflict naming robot", err)
	}
	if err := f.users.SetOwner(ctx, "robot", "user:admin", cli); err != nil {
		t.Fatal(err)
	}
	if err := f.users.Delete(ctx, "alice", cli); err != nil {
		t.Errorf("deleting alice once robot is admin's = %v", err)
	}
	if err := f.users.Delete(ctx, "robot", cli); err != nil {
		t.Errorf("deleting robot = %v", err)
	}
}

// TestServiceAccountDisableWithOwner covers auth.serviceAccounts.onOwnerDisabled disable
// (docs/spec/rbac.md §1): disabling a user disables the enabled service accounts it owns in the
// same transaction, a user.disable event each with the owner; another owner's and one disabled
// already are left as they are, and enabling the owner again enables none of them.
func TestServiceAccountDisableWithOwner(t *testing.T) {
	ctx := context.Background()
	f, _ := newServiceFixture(t)
	f.users.DisableWithOwner = true
	for name, owner := range map[string]string{"deployer": "group:ops", "spare": "user:alice"} {
		if _, err := f.users.CreateService(ctx, name, owner, cli); err != nil {
			t.Fatal(err)
		}
	}
	if err := f.users.Disable(ctx, "spare", cli); err != nil {
		t.Fatal(err)
	}
	if err := f.users.Disable(ctx, "alice", cli); err != nil {
		t.Fatal(err)
	}
	if err := f.users.Enable(ctx, "alice", cli); err != nil {
		t.Fatal(err)
	}
	for name, disabled := range map[string]bool{"alice": false, "robot": true, "spare": true, "deployer": false} {
		if u, err := f.users.Get(ctx, name); err != nil || u.Disabled != disabled {
			t.Errorf("%s after alice disabled and enabled: %+v, %v; want disabled %v", name, u, err, disabled)
		}
	}
	evs, _ := f.st.ListAudit(ctx, index.AuditFilter{Action: ActionUserDisable})
	if len(evs) != 3 || named(t, f.st, evs[0].Subject+" "+string(evs[0].Details)) != `user:robot {"name":"robot","owner":"user:alice","source":"cli"}` ||
		named(t, f.st, evs[1].Subject+" "+string(evs[1].Details)) != `user:alice {"name":"alice","source":"cli"}` {
		t.Errorf("user.disable events %+v", evs)
	}
}

// TestServiceAccountCredentials: a service account has no password, and setting one is refused;
// it authenticates with its personal access token, as user:<uid>.
func TestServiceAccountCredentials(t *testing.T) {
	ctx := context.Background()
	f, robot := newServiceFixture(t)
	if err := f.users.SetPassword(ctx, "robot", "a-password-1", cli); !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), "service account") {
		t.Errorf("SetPassword of robot = %v, want ErrInvalid", err)
	}
	_, secret, err := f.tokens.CreatePAT(ctx, "robot", "ci", []string{ScopeRead}, Expiry{}, cli)
	if err != nil {
		t.Fatal(err)
	}
	a := NewAuthenticator(Options{Index: f.st, Now: f.users.Now})
	p, c, err := a.Authenticate(ctx, "robot", secret)
	if err != nil || p.String() != "user:"+robot.UID || c.Kind != CredPAT || len(c.Scopes) != 1 {
		t.Errorf("robot's token: %v, %+v, %v; want user:<uid> with a scoped PAT", p, c, err)
	}
	if _, _, err := a.Authenticate(ctx, "robot", "a-password-1"); !errors.Is(err, ErrBadCredentials) {
		t.Errorf("robot with a password = %v, want ErrBadCredentials", err)
	}
	withPAT := WithCredential(ctx, Credential{Kind: CredPAT, TokenID: 1})
	if _, _, err := f.tokens.CreatePAT(withPAT, "robot", "other", nil, Expiry{}, cli); !errors.Is(err, ErrScope) {
		t.Errorf("a token of robot created with a token = %v, want ErrScope", err)
	}
}

// TestServiceAccountAdmin: a service account's admin binding counts neither for the last-admin
// rule nor for the bootstrap, so that rule never refuses disabling or deleting it, nor deleting
// its binding.
func TestServiceAccountAdmin(t *testing.T) {
	ctx := context.Background()
	f, _ := newServiceFixture(t)
	b, err := f.bindings.Create(ctx, "user:robot", RoleAdmin, MaskAll, cli)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.users.Disable(ctx, "admin", cli); !errors.Is(err, index.ErrConflict) {
		t.Errorf("disabling the last admin beside robot = %v, want a conflict", err)
	}
	admin, err := f.st.GetUser(ctx, "admin")
	if err != nil {
		t.Fatal(err)
	}
	admin.Disabled = true // as lajstrom admin could leave it: robot is then the only enabled admin
	if err := f.st.UpdateUser(ctx, admin); err != nil {
		t.Fatal(err)
	}
	if ok, err := anyAdmin(ctx, f.st); ok || err != nil {
		t.Errorf("anyAdmin with robot's binding = %v, %v; want false", ok, err)
	}
	if err := f.users.Disable(ctx, "robot", cli); err != nil {
		t.Errorf("disabling robot = %v", err)
	}
	if err := f.users.Enable(ctx, "robot", cli); err != nil {
		t.Fatal(err)
	}
	if err := f.bindings.Delete(ctx, b.ID, cli); err != nil {
		t.Errorf("deleting robot's admin binding = %v", err)
	}
	if _, err := f.bindings.Create(ctx, "user:robot", RoleAdmin, MaskAll, cli); err != nil {
		t.Fatal(err)
	}
	if err := f.users.Delete(ctx, "robot", cli); err != nil {
		t.Errorf("deleting robot = %v", err)
	}
}

// TestServiceAccountTokenLifetime: a service account's token is bounded by ServiceMaxLifetime,
// not by the year of a person's; zero bounds nothing, and a token without an expiry then never
// expires. A person's token keeps its bounds.
func TestServiceAccountTokenLifetime(t *testing.T) {
	ctx := context.Background()
	f, _ := newServiceFixture(t)
	decade := Expiry{In: 3650 * 24 * time.Hour}
	for i, tt := range []struct {
		max  time.Duration
		user string
		exp  Expiry
		want time.Duration // the lifetime given; 0 for none
		err  string
	}{
		{0, "robot", Expiry{}, 0, ""},
		{0, "robot", decade, decade.In, ""},
		{30 * 24 * time.Hour, "robot", Expiry{}, 30 * 24 * time.Hour, ""},
		{30 * 24 * time.Hour, "robot", Expiry{In: 7 * 24 * time.Hour}, 7 * 24 * time.Hour, ""},
		{30 * 24 * time.Hour, "robot", Expiry{In: 31 * 24 * time.Hour}, 0,
			"expiresIn: a service account's token expires at most 720h0m0s ahead (auth.serviceAccounts.maxTokenTTL), at 2026-12-07T12:00:00Z at the latest"},
		{0, "alice", Expiry{}, DefaultPATLifetime, ""},
		{0, "alice", decade, 0, "expiresIn: a personal access token expires at most 365 days ahead"},
		{0, "robot", Expiry{At: usersNow.Add(-time.Hour)}, 0, "expiresAt: must be in the future"},
	} {
		tokens := &Tokens{Index: f.st, Now: f.users.Now, Log: f.users.Log, ServiceMaxLifetime: tt.max}
		tok, _, err := tokens.CreatePAT(ctx, tt.user, "token "+string(rune('a'+i)), nil, tt.exp, cli)
		switch {
		case tt.err != "":
			if !errors.Is(err, ErrInvalid) || !strings.HasPrefix(err.Error(), tt.err) {
				t.Errorf("%d: %v, want %q", i, err, tt.err)
			}
		case err != nil:
			t.Errorf("%d: %v", i, err)
		case tt.want == 0 && tok.ExpiresAt != nil, tt.want != 0 && (tok.ExpiresAt == nil || !tok.ExpiresAt.Equal(usersNow.Add(tt.want))):
			t.Errorf("%d: expiresAt %v, want %v from now", i, tok.ExpiresAt, tt.want)
		}
	}
}
