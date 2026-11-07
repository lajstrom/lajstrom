package auth

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lajstrom/lajstrom/internal/index"
	"github.com/lajstrom/lajstrom/internal/index/sqlite"
)

var memoryDBs atomic.Int64

var usersNow = time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)

// newUsers returns the users service over a migrated in-memory SQLite index, logging JSON into
// the returned buffer.
func newUsers(t *testing.T) (*Users, *sqlite.Store, *bytes.Buffer) {
	t.Helper()
	st, err := sqlite.Open(t.Context(), fmt.Sprintf("file:auth-%d?mode=memory&cache=shared", memoryDBs.Add(1)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := st.Migrate(t.Context()); err != nil {
		t.Fatal(err)
	}
	var logs bytes.Buffer
	log := slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	return &Users{Index: st, Now: func() time.Time { return usersNow }, Log: log}, st, &logs
}

var cli = Origin{Source: "cli"}

// wantAudit checks the audit events of the index, newest first: each "<principal> <action>
// <subject> <details>".
func wantAudit(t *testing.T, st index.Reader, want ...string) {
	t.Helper()
	evs, err := st.ListAudit(context.Background(), index.AuditFilter{})
	if err != nil {
		t.Fatal(err)
	}
	got := make([]string, len(evs))
	for i, ev := range evs {
		got[i] = fmt.Sprintf("%s %s %s %s", ev.Principal, ev.Action, ev.Subject, ev.Details)
		if ev.Repo != "" || !ev.At.Equal(usersNow) {
			t.Errorf("event %d: repo %q at %v, want no repo at %v", i, ev.Repo, ev.At, usersNow)
		}
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("audit events:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

// createAdminAndAlice creates admin with the admin binding and alice without, from the CLI.
func createAdminAndAlice(t *testing.T, users *Users) *User {
	t.Helper()
	u, err := users.Create(context.Background(), "admin", "admin-password", true, cli)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := users.Create(context.Background(), "alice", "alice-password", false, cli); err != nil {
		t.Fatal(err)
	}
	return u
}

// wantAdminBinding checks that the only binding is (user:admin, admin, */*).
func wantAdminBinding(t *testing.T, st index.Reader) {
	t.Helper()
	bindings, err := st.ListBindings(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	if len(bindings) != 1 || bindings[0].Principal != "user:admin" || bindings[0].Role != RoleAdmin || bindings[0].RepoMask != MaskAll {
		t.Errorf("bindings = %+v; want the one (user:admin, admin, */*)", bindings)
	}
}

// TestUsersCreate covers the bootstrap: a local user whose stored hash verifies its password,
// the admin binding with admin only, and one user.create event each.
func TestUsersCreate(t *testing.T) {
	ctx := context.Background()
	users, st, _ := newUsers(t)
	u := createAdminAndAlice(t, users)
	b, _ := json.Marshal(u)
	if want := `{"name":"admin","kind":"local","disabled":false,"createdAt":"2026-11-07T12:00:00Z"}`; string(b) != want {
		t.Errorf("user = %s, want %s", b, want)
	}
	row, err := st.GetUser(ctx, "admin")
	if err != nil {
		t.Fatal(err)
	}
	if !phc.MatchString(row.PasswordHash) || row.Kind != KindLocal || row.Disabled {
		t.Errorf("row = %+v; want a local user with a hash of the parameters of new hashes", row)
	}
	if ok, err := VerifyPassword(row.PasswordHash, "admin-password"); !ok || err != nil {
		t.Errorf("the stored hash does not verify the password: %v, %v", ok, err)
	}
	wantAdminBinding(t, st)
	list, err := users.List(ctx)
	if err != nil || len(list) != 2 || list[0].Name != "admin" || list[1].Name != "alice" {
		t.Errorf("List = %+v, %v; want admin and alice", list, err)
	}
	wantAudit(t, st,
		`anonymous user.create alice {"admin":false,"source":"cli"}`,
		`anonymous user.create admin {"admin":true,"source":"cli"}`,
	)
}

// TestUsersCreateRefused checks the refusals of Create: a name that is taken, invalid or
// reserved, a password out of bounds; each names the user or the field and writes nothing.
func TestUsersCreateRefused(t *testing.T) {
	users, st, _ := newUsers(t)
	createAdminAndAlice(t, users)
	for _, tt := range []struct {
		name, user, password string
		admin                bool
		sentinel             error
		msg                  string
	}{
		{"name taken", "alice", "other-password", false, index.ErrConflict, "user alice already exists"},
		{"admin name taken", "admin", "other-password", true, index.ErrConflict, "user admin already exists"},
		{"invalid name", "Bob", "bob-password", true, ErrInvalid, `name: "Bob" must be lowercase`},
		{"reserved name", "deploy", "deploy-password", false, ErrInvalid, `name: "deploy" is reserved`},
		{"short password", "bob", "short", false, ErrInvalid, "password: must be at least 8 bytes"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			_, err := users.Create(context.Background(), tt.user, tt.password, tt.admin, cli)
			if !errors.Is(err, tt.sentinel) || !strings.HasPrefix(err.Error(), tt.msg) {
				t.Errorf("Create = %v, want %v saying %q", err, tt.sentinel, tt.msg)
			}
		})
	}
	wantAdminBinding(t, st)
	wantAudit(t, st,
		`anonymous user.create alice {"admin":false,"source":"cli"}`,
		`anonymous user.create admin {"admin":true,"source":"cli"}`,
	)
}

// TestUsersChanges covers set-password, disable and enable: each changes the row with one
// audit event by the principal of the context, with the origin; a change that changes nothing
// writes nothing.
func TestUsersChanges(t *testing.T) {
	ctx := context.Background()
	users, st, _ := newUsers(t)
	if _, err := users.Create(ctx, "alice", "alice-password", false, cli); err != nil {
		t.Fatal(err)
	}
	ctx = WithPrincipal(ctx, Principal{Kind: KindUser, Name: "admin"})
	if err := users.SetPassword(ctx, "alice", "new-alice-password", Origin{Source: "api", IP: "192.0.2.7", RequestID: "req-1"}); err != nil {
		t.Fatal(err)
	}
	row, _ := st.GetUser(ctx, "alice")
	if ok, _ := VerifyPassword(row.PasswordHash, "new-alice-password"); !ok {
		t.Error("the new password does not verify")
	}
	if ok, _ := VerifyPassword(row.PasswordHash, "alice-password"); ok {
		t.Error("the old password still verifies")
	}
	for _, s := range []struct {
		name string
		do   func(context.Context, string, Origin) error
		want bool // disabled after the step
	}{
		{"disable", users.Disable, true},
		{"disable again", users.Disable, true},
		{"enable", users.Enable, false},
		{"enable again", users.Enable, false},
	} {
		if err := s.do(ctx, "alice", cli); err != nil {
			t.Fatalf("%s: %v", s.name, err)
		}
		if u, err := users.Get(ctx, "alice"); err != nil || u.Disabled != s.want {
			t.Fatalf("after %s: %+v, %v; want disabled %v", s.name, u, err, s.want)
		}
	}
	wantAudit(t, st,
		`user:admin user.enable alice {"source":"cli"}`,
		`user:admin user.disable alice {"source":"cli"}`,
		`user:admin user.password alice {"requestId":"req-1","source":"api"}`,
		`anonymous user.create alice {"admin":false,"source":"cli"}`,
	)
	evs, _ := st.ListAudit(ctx, index.AuditFilter{Action: ActionUserPassword})
	if len(evs) != 1 || evs[0].IP != "192.0.2.7" {
		t.Errorf("user.password events = %+v, want one from 192.0.2.7", evs)
	}
}

// TestUsersChangesRefused checks that a change of an unknown user is index.ErrNotFound naming
// it, and that a password out of bounds is ErrInvalid.
func TestUsersChangesRefused(t *testing.T) {
	ctx := context.Background()
	users, st, _ := newUsers(t)
	if _, err := users.Create(ctx, "alice", "alice-password", false, cli); err != nil {
		t.Fatal(err)
	}
	for name, err := range map[string]error{
		"set-password": users.SetPassword(ctx, "nobody", "nobody-password", cli),
		"disable":      users.Disable(ctx, "nobody", cli),
		"enable":       users.Enable(ctx, "nobody", cli),
		"get":          func() error { _, err := users.Get(ctx, "nobody"); return err }(),
	} {
		if !errors.Is(err, index.ErrNotFound) || err.Error() != "user nobody does not exist" {
			t.Errorf("%s of an unknown user = %v, want ErrNotFound naming it", name, err)
		}
	}
	if err := users.SetPassword(ctx, "alice", "short", cli); !errors.Is(err, ErrInvalid) {
		t.Errorf("SetPassword(short) = %v, want ErrInvalid", err)
	}
	wantAudit(t, st, `anonymous user.create alice {"admin":false,"source":"cli"}`)
}

// recordingAudit is an audit.Writer that keeps the events, or fails.
type recordingAudit struct {
	mu   sync.Mutex
	evs  []index.AuditEvent
	fail error
}

func (r *recordingAudit) Write(_ context.Context, ev index.AuditEvent) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.fail != nil {
		return r.fail
	}
	r.evs = append(r.evs, ev)
	return nil
}

// TestUsersAuditWriter checks that with an audit.Writer the events go to it after the commit,
// not to the index, and that a failing writer is logged without undoing the change.
func TestUsersAuditWriter(t *testing.T) {
	ctx := context.Background()
	users, st, logs := newUsers(t)
	rec := &recordingAudit{}
	users.Audit = rec
	if _, err := users.Create(ctx, "alice", "alice-password", true, cli); err != nil {
		t.Fatal(err)
	}
	if err := users.Disable(ctx, "alice", cli); err != nil {
		t.Fatal(err)
	}
	if len(rec.evs) != 2 || rec.evs[0].Action != ActionUserCreate || rec.evs[1].Action != ActionUserDisable {
		t.Errorf("audit writer got %+v, want user.create and user.disable", rec.evs)
	}
	wantAudit(t, st)

	rec.fail = errors.New("audit: gone")
	if err := users.Enable(ctx, "alice", cli); err != nil {
		t.Fatalf("Enable with a failing audit writer: %v", err)
	}
	if u, _ := users.Get(ctx, "alice"); u.Disabled {
		t.Error("the change was undone for its event")
	}
	if !strings.Contains(logs.String(), `"msg":"auth: audit event of a user change not written","action":"user.enable","user":"alice","error":"audit: gone"`) {
		t.Errorf("log %s does not have the failure", logs)
	}
}

// failingStore is an index whose transactions and listings fail.
type failingStore struct{ index.Store }

var errDatabase = errors.New("index: database is gone")

func (failingStore) Tx(context.Context, func(index.Writer) error) error { return errDatabase }
func (failingStore) ListUsers(context.Context) ([]index.User, error)    { return nil, errDatabase }

// TestUsersFailures checks that a failure of the index is wrapped with the operation, for the
// log, and is none of the sentinels for the person.
func TestUsersFailures(t *testing.T) {
	ctx := context.Background()
	users := &Users{Index: failingStore{}}
	for name, err := range map[string]error{
		"create":  func() error { _, err := users.Create(ctx, "alice", "alice-password", false, cli); return err }(),
		"disable": users.Disable(ctx, "alice", cli),
		"list":    func() error { _, err := users.List(ctx); return err }(),
	} {
		if !errors.Is(err, errDatabase) || errors.Is(err, index.ErrNotFound) || !strings.HasPrefix(err.Error(), "auth: ") {
			t.Errorf("%s = %v, want the index error wrapped", name, err)
		}
	}
	if users.now().IsZero() || users.log() == nil {
		t.Error("the defaults of a zero Users are not set")
	}
}
