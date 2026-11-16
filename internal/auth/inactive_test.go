package auth

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/lajstrom/lajstrom/internal/index"
	"github.com/lajstrom/lajstrom/internal/index/sqlite"
)

// ninetyDays is auth.disableInactiveUsersAfter of PCI DSS 8.2.6.
const ninetyDays = 2160 * time.Hour

// bobJKT is the thumbprint of the DPoP key of bob's web UI: 43 characters of base64url.
const bobJKT = "0ZcOCORZNYy-DWpqq30jZyJGHTN0d2HglBV3uiguA4I"

// TestInactive: a live, enabled user is inactive when the setting has passed since its last
// activity, or since its creation when it has none; a setting of zero makes nobody inactive.
func TestInactive(t *testing.T) {
	created := usersNow
	seen := created.Add(10 * 24 * time.Hour)
	for _, tt := range []struct {
		name  string
		u     index.User
		after time.Duration
		now   time.Time
		want  bool
	}{
		{"never seen, within", index.User{CreatedAt: created}, ninetyDays, created.Add(ninetyDays), false},
		{"never seen, beyond", index.User{CreatedAt: created}, ninetyDays, created.Add(ninetyDays + time.Second), true},
		{"seen, within", index.User{CreatedAt: created, LastSeenAt: &seen}, ninetyDays, created.Add(ninetyDays + time.Hour), false},
		{"seen, beyond", index.User{CreatedAt: created, LastSeenAt: &seen}, ninetyDays, seen.Add(ninetyDays + time.Second), true},
		{"off", index.User{CreatedAt: created}, 0, created.Add(10 * ninetyDays), false},
		{"disabled", index.User{CreatedAt: created, Disabled: true}, ninetyDays, created.Add(10 * ninetyDays), false},
		{"deleted", index.User{CreatedAt: created, DeletedAt: &seen}, ninetyDays, created.Add(10 * ninetyDays), false},
	} {
		if got := Inactive(&tt.u, tt.after, tt.now); got != tt.want {
			t.Errorf("%s: Inactive = %v, want %v", tt.name, got, tt.want)
		}
	}
	if s := shortDuration(ninetyDays); s != "2160h" {
		t.Errorf("shortDuration(2160h) = %q", s)
	}
	if s := shortDuration(36*time.Hour + 30*time.Minute); s != "36h30m" {
		t.Errorf("shortDuration(36h30m) = %q", s)
	}
}

// inactiveFixture is the users service and an Authenticator over one SQLite index and one clock,
// with auth.disableInactiveUsersAfter 90 days, and the users admin, with the admin binding, and
// alice, created at usersNow.
type inactiveFixture struct {
	users *Users
	st    *sqlite.Store
	logs  *bytes.Buffer
	a     *Authenticator
	now   time.Time
}

func newInactiveFixture(t *testing.T) *inactiveFixture {
	t.Helper()
	users, st, logs := newUsers(t)
	f := &inactiveFixture{users: users, st: st, logs: logs, now: usersNow}
	users.Now = func() time.Time { return f.now }
	users.InactiveAfter = ninetyDays
	f.a = NewAuthenticator(Options{Index: st, Now: users.Now, Users: users, Log: users.Log})
	createAdminAndAlice(t, users)
	return f
}

// user returns the row of the user called name.
func (f *inactiveFixture) user(t *testing.T, name string) *index.User {
	t.Helper()
	u, err := f.st.GetUser(t.Context(), name)
	if err != nil {
		t.Fatal(err)
	}
	return u
}

// wantSeen checks the last activity of the user called name.
func (f *inactiveFixture) wantSeen(t *testing.T, name string, want time.Time) {
	t.Helper()
	if u := f.user(t, name); u.LastSeenAt == nil || !u.LastSeenAt.Equal(want) {
		t.Errorf("%s: LastSeenAt = %v, want %v", name, u.LastSeenAt, want)
	}
}

// lastAudit returns the newest audit row, its uids replaced by the users' names.
func (f *inactiveFixture) lastAudit(t *testing.T) (index.AuditEvent, string) {
	t.Helper()
	evs, err := f.st.ListAudit(t.Context(), index.AuditFilter{Limit: 1})
	if err != nil || len(evs) == 0 {
		t.Fatalf("ListAudit = %v, %v", evs, err)
	}
	ev := evs[0]
	return ev, named(t, f.st, ev.Principal+" "+ev.Action+" "+ev.Subject+" "+string(ev.Details))
}

// TestAuthenticateRecordsActivity: a credential that verifies records its user's activity, at
// most once a minute, without moving the user's revision (docs/spec/rbac.md §1).
func TestAuthenticateRecordsActivity(t *testing.T) {
	ctx := t.Context()
	f := newInactiveFixture(t)
	revision := f.user(t, "alice").Revision
	if _, _, err := f.a.Authenticate(ctx, "alice", "alice-password"); err != nil {
		t.Fatal(err)
	}
	f.wantSeen(t, "alice", usersNow)
	f.now = usersNow.Add(30 * time.Second)
	if _, _, err := f.a.Authenticate(ctx, "alice", "alice-password"); err != nil {
		t.Fatal(err)
	}
	f.wantSeen(t, "alice", usersNow) // within the minute: not written
	f.now = usersNow.Add(2 * time.Minute)
	if _, _, err := f.a.AuthenticatePassword(ctx, "alice", "alice-password"); err != nil {
		t.Fatal(err)
	}
	f.wantSeen(t, "alice", f.now)
	if r := f.user(t, "alice").Revision; r != revision {
		t.Errorf("alice's revision moved from %d to %d: an activity is no change of the user", revision, r)
	}
	if _, _, err := f.a.Authenticate(ctx, "alice", "a wrong password"); !errors.Is(err, ErrBadCredentials) {
		t.Fatalf("a wrong password = %v", err)
	}
	f.wantSeen(t, "alice", f.now)
}

// TestAuthenticateInactive: past the setting, a wrong password disables nobody; the right one is
// refused and disables the user, with one user.disable row by anonymous with the origin of the
// request; the last enabled admin is taken, with a warning, and recorded.
func TestAuthenticateInactive(t *testing.T) {
	f := newInactiveFixture(t)
	ctx := WithRequest(t.Context(), "192.0.2.7", "req-1")
	f.now = usersNow.Add(ninetyDays + time.Hour)
	if _, _, err := f.a.Authenticate(ctx, "alice", "a wrong password"); !errors.Is(err, ErrBadCredentials) {
		t.Fatalf("a wrong password = %v", err)
	}
	if f.user(t, "alice").Disabled {
		t.Fatal("a wrong password disabled alice")
	}
	if _, _, err := f.a.Authenticate(ctx, "alice", "alice-password"); !errors.Is(err, ErrBadCredentials) {
		t.Fatalf("an inactive user's password = %v, want ErrBadCredentials", err)
	}
	if !f.user(t, "alice").Disabled {
		t.Fatal("alice, inactive, is not disabled by her credential")
	}
	ev, row := f.lastAudit(t)
	want := `anonymous user.disable user:alice {"after":"2160h","name":"alice","reason":"inactive","requestId":"req-1","source":"inactivity"}`
	if row != want || ev.IP != "192.0.2.7" {
		t.Errorf("the row of the disabling:\n%s from %q\nwant:\n%s from 192.0.2.7", row, ev.IP, want)
	}
	if _, _, err := f.a.Authenticate(ctx, "alice", "alice-password"); !errors.Is(err, ErrBadCredentials) {
		t.Errorf("a disabled user's password = %v, want ErrBadCredentials", err)
	}
	if _, last := f.lastAudit(t); last != want {
		t.Errorf("a second refusal wrote a row: %s", last)
	}

	// The admin, inactive too, is the last enabled admin: taken, recorded, not disabled.
	if p, _, err := f.a.Authenticate(ctx, "admin", "admin-password"); err != nil || p.Kind != KindUser {
		t.Fatalf("the last enabled admin, inactive: %v, %v; want it taken", p, err)
	}
	if f.user(t, "admin").Disabled {
		t.Error("the last enabled admin is disabled")
	}
	f.wantSeen(t, "admin", f.now)
	if !strings.Contains(f.logs.String(), "the last enabled user with an admin binding is inactive") {
		t.Errorf("no warning about the last enabled admin in the log:\n%s", f.logs)
	}
	if !strings.Contains(f.logs.String(), `"msg":"user disabled: inactive beyond auth.disableInactiveUsersAfter`) {
		t.Errorf("no info line about alice's disabling in the log:\n%s", f.logs)
	}
}

// TestAuthenticateInactiveTokens: the personal access token of an inactive user, in Basic or as a
// bearer, is refused and disables the user; a service account's activity is its tokens' use; a
// user seen within the setting is taken.
func TestAuthenticateInactiveTokens(t *testing.T) {
	ctx := t.Context()
	f := newInactiveFixture(t)
	tokens := &Tokens{Index: f.st, Now: f.users.Now, Log: f.users.Log}
	if _, err := f.users.CreateService(ctx, "robot", "user:alice", cli); err != nil {
		t.Fatal(err)
	}
	_, robotSecret, err := tokens.CreatePAT(ctx, "robot", "ci", []string{ScopeRead}, Expiry{}, cli)
	if err != nil {
		t.Fatal(err)
	}
	_, aliceSecret, err := tokens.CreatePAT(ctx, "alice", "laptop", nil, Expiry{In: 365 * 24 * time.Hour}, cli)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.users.Create(ctx, "bob", "bob-password", false, cli); err != nil {
		t.Fatal(err)
	}
	_, bobSecret, err := tokens.CreateBoundPAT(ctx, "bob", "login", bobJKT, Expiry{In: 365 * 24 * time.Hour}, cli)
	if err != nil {
		t.Fatal(err)
	}
	f.now = usersNow.Add(80 * 24 * time.Hour)
	if _, _, err := f.a.AuthenticateBearer(ctx, robotSecret); err != nil {
		t.Fatalf("robot's token within the setting: %v", err)
	}
	f.wantSeen(t, "robot", f.now)

	f.now = usersNow.Add(ninetyDays + time.Hour)
	if _, _, err := f.a.AuthenticateBearer(ctx, robotSecret); err != nil {
		t.Errorf("robot's token, used 10 days ago: %v, want taken", err)
	}
	if _, _, err := f.a.Authenticate(ctx, "alice", aliceSecret); !errors.Is(err, ErrBadCredentials) {
		t.Errorf("an inactive user's token in Basic = %v, want ErrBadCredentials", err)
	}
	if !f.user(t, "alice").Disabled {
		t.Error("alice, inactive, is not disabled by her token")
	}
	if _, _, err := f.a.AuthenticateDPoP(ctx, bobSecret, bobJKT); !errors.Is(err, ErrBadCredentials) {
		t.Errorf("an inactive user's web UI token = %v, want ErrBadCredentials", err)
	}
	if !f.user(t, "bob").Disabled {
		t.Error("bob, inactive, is not disabled by his web UI token")
	}
	f.now = f.now.Add(ninetyDays + time.Second)
	if _, _, err := f.a.AuthenticateBearer(ctx, robotSecret); !errors.Is(err, ErrBadCredentials) {
		t.Errorf("an inactive service account's bearer = %v, want ErrBadCredentials", err)
	}
	if !f.user(t, "robot").Disabled {
		t.Error("robot, inactive, is not disabled by its token")
	}
}

// TestInactiveOwner: with DisableWithOwner, the service accounts of a user owner that inactivity
// disables are disabled with it, each with a row naming the owner.
func TestInactiveOwner(t *testing.T) {
	ctx := t.Context()
	f := newInactiveFixture(t)
	f.users.DisableWithOwner = true
	if _, err := f.users.CreateService(ctx, "robot", "user:alice", cli); err != nil {
		t.Fatal(err)
	}
	f.now = usersNow.Add(ninetyDays + time.Hour)
	if _, _, err := f.a.Authenticate(ctx, "alice", "alice-password"); !errors.Is(err, ErrBadCredentials) {
		t.Fatalf("alice inactive = %v", err)
	}
	if !f.user(t, "robot").Disabled {
		t.Fatal("robot is not disabled with its owner")
	}
	_, row := f.lastAudit(t)
	if want := `anonymous user.disable user:robot {"name":"robot","owner":"user:alice","source":"inactivity"}`; row != want {
		t.Errorf("robot's row = %s, want %s", row, want)
	}
}

// TestEnableRestartsClock: enabling a user sets its last activity to that time, by Enable and by
// Change, so that the user is not disabled again at its next request.
func TestEnableRestartsClock(t *testing.T) {
	ctx := t.Context()
	f := newInactiveFixture(t)
	f.now = usersNow.Add(ninetyDays + time.Hour)
	for _, enable := range []func() error{
		func() error { return f.users.Enable(ctx, "alice", cli) },
		func() error { _, err := f.users.Change(ctx, "alice", UserChange{Disabled: new(bool)}, cli); return err },
	} {
		if _, _, err := f.a.Authenticate(ctx, "alice", "alice-password"); !errors.Is(err, ErrBadCredentials) {
			t.Fatalf("alice inactive = %v", err)
		}
		if err := enable(); err != nil {
			t.Fatal(err)
		}
		f.wantSeen(t, "alice", f.now)
		f.now = f.now.Add(time.Hour)
		if _, _, err := f.a.Authenticate(ctx, "alice", "alice-password"); err != nil {
			t.Fatalf("alice enabled again: %v, want taken", err)
		}
		f.now = f.now.Add(ninetyDays + time.Hour)
	}
}

// TestInactiveOff: with the setting zero, a user unseen for years is taken, and none is counted.
func TestInactiveOff(t *testing.T) {
	ctx := t.Context()
	f := newInactiveFixture(t)
	f.users.InactiveAfter = 0
	f.now = usersNow.Add(10 * ninetyDays)
	if _, _, err := f.a.Authenticate(ctx, "alice", "alice-password"); err != nil {
		t.Fatalf("alice without the setting: %v", err)
	}
	if n, admin, err := f.users.InactiveUsers(ctx); n != 0 || admin != "" || err != nil {
		t.Errorf("InactiveUsers without the setting = %d, %q, %v", n, admin, err)
	}
}

// TestInactiveUsers counts the inactive users, and names the last enabled admin among them.
func TestInactiveUsers(t *testing.T) {
	ctx := t.Context()
	f := newInactiveFixture(t)
	f.now = usersNow.Add(ninetyDays + time.Hour)
	if n, admin, err := f.users.InactiveUsers(ctx); n != 2 || admin != "admin" || err != nil {
		t.Errorf("InactiveUsers = %d, %q, %v; want 2, admin", n, admin, err)
	}
	if _, _, err := f.a.Authenticate(ctx, "admin", "admin-password"); err != nil {
		t.Fatal(err)
	}
	if n, admin, err := f.users.InactiveUsers(ctx); n != 1 || admin != "" || err != nil {
		t.Errorf("InactiveUsers after the admin's login = %d, %q, %v; want 1, none", n, admin, err)
	}
}

// oidcInactiveFixture logs olga and oscar in through OIDC at usersNow, oscar with the admin
// binding, the only one, and returns the users service, whose clock is *now, with
// auth.disableInactiveUsersAfter 90 days.
func oidcInactiveFixture(t *testing.T, now *time.Time) (*Users, *sqlite.Store, OIDCLogin, OIDCLogin) {
	t.Helper()
	ctx := t.Context()
	users, st, _ := newUsers(t)
	*now = usersNow
	users.Now = func() time.Time { return *now }
	users.InactiveAfter = ninetyDays
	olga := OIDCLogin{Issuer: "https://idp", Subject: "s1", Username: "olga", Lifetime: time.Hour}
	oscar := OIDCLogin{Issuer: "https://idp", Subject: "s2", Username: "oscar", Lifetime: time.Hour}
	for _, l := range []OIDCLogin{olga, oscar} {
		if _, err := users.LoginOIDC(ctx, l, Origin{}); err != nil {
			t.Fatal(err)
		}
	}
	o, err := st.GetUser(ctx, "oscar")
	if err != nil || o.LastSeenAt == nil || !o.LastSeenAt.Equal(usersNow) {
		t.Fatalf("oscar after his login: %+v, %v; want seen now", o, err)
	}
	if err := st.CreateBinding(ctx, &index.Binding{Principal: "user:" + o.UID, Role: RoleAdmin, RepoMask: MaskAll}); err != nil {
		t.Fatal(err)
	}
	return users, st, olga, oscar
}

// TestLoginOIDCInactive: an OIDC login records the activity; the login of an inactive user
// disables it and is refused, without a user.login row.
func TestLoginOIDCInactive(t *testing.T) {
	ctx := t.Context()
	var now time.Time
	users, st, olga, _ := oidcInactiveFixture(t, &now)
	now = usersNow.Add(ninetyDays + time.Hour)
	rows := len(auditRows(t, st))
	if _, err := users.LoginOIDC(ctx, olga, Origin{IP: "192.0.2.8"}); !errors.Is(err, ErrLoginRefused) ||
		!strings.Contains(err.Error(), "no activity for more than 2160h") {
		t.Errorf("an inactive user's login = %v, want ErrLoginRefused saying why", err)
	}
	if u, err := st.GetUser(ctx, "olga"); err != nil || !u.Disabled {
		t.Errorf("olga after an inactive login: %+v, %v; want disabled", u, err)
	}
	evs := auditRows(t, st)
	if len(evs) != rows+1 || evs[0].Action != ActionUserDisable || evs[0].IP != "192.0.2.8" {
		t.Errorf("rows of an inactive login: %+v; want one user.disable, no user.login", evs[:len(evs)-rows])
	}
}

// TestLoginOIDCInactiveLastAdmin: the last enabled admin logs in through OIDC, inactive, and is
// seen.
func TestLoginOIDCInactiveLastAdmin(t *testing.T) {
	var now time.Time
	users, _, _, oscar := oidcInactiveFixture(t, &now)
	now = usersNow.Add(ninetyDays + time.Hour)
	u, err := users.LoginOIDC(t.Context(), oscar, Origin{})
	if err != nil || u.Disabled || u.LastSeenAt == nil || !u.LastSeenAt.Equal(now) {
		t.Errorf("the last enabled admin's login = %+v, %v; want taken and seen now", u, err)
	}
}

// auditRows returns the audit rows of st, newest first.
func auditRows(t *testing.T, st index.Reader) []index.AuditEvent {
	t.Helper()
	evs, err := st.ListAudit(context.Background(), index.AuditFilter{})
	if err != nil {
		t.Fatal(err)
	}
	return evs
}

// TestDisableInactiveDetails: the row of a user seen before carries its lastSeenAt.
func TestDisableInactiveDetails(t *testing.T) {
	ctx := t.Context()
	f := newInactiveFixture(t)
	if _, _, err := f.a.Authenticate(ctx, "alice", "alice-password"); err != nil {
		t.Fatal(err)
	}
	f.now = usersNow.Add(ninetyDays + time.Hour)
	if refused, err := f.users.DisableInactive(ctx, f.user(t, "alice").ID, Origin{}); !refused || err != nil {
		t.Fatalf("DisableInactive = %v, %v", refused, err)
	}
	ev, _ := f.lastAudit(t)
	var details map[string]any
	if err := json.Unmarshal(ev.Details, &details); err != nil {
		t.Fatal(err)
	}
	if details["lastSeenAt"] != usersNow.Format(time.RFC3339Nano) || details["reason"] != ReasonInactive {
		t.Errorf("details = %v, want lastSeenAt %s and the reason", details, usersNow.Format(time.RFC3339Nano))
	}
	if refused, err := f.users.DisableInactive(ctx, f.user(t, "admin").ID, Origin{}); refused || err != nil {
		t.Errorf("DisableInactive of the last enabled admin = %v, %v; want kept", refused, err)
	}
}
