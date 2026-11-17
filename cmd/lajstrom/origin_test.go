package main

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lajstrom/lajstrom/internal/auth"
	"github.com/lajstrom/lajstrom/internal/index"
)

// TestAuditedCommands: every command of auditedCommands exists (newRootCmd panics otherwise) and
// takes --actor, and --reason as the reason of the change but on admin policy rule create, whose
// --reason is the rule's; a command that only reads or goes over HTTP takes neither.
func TestAuditedCommands(t *testing.T) {
	root := newRootCmd(func(string) (string, bool) { return "", false })
	for path, reason := range auditedCommands {
		cmd, _, err := root.Find(strings.Fields(path))
		if err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		if cmd.Flags().Lookup("actor") == nil || cmd.PreRunE == nil {
			t.Errorf("%s: no --actor, or no PreRunE to check it", path)
		}
		f := cmd.Flags().Lookup("reason")
		if f == nil || strings.Contains(f.Usage, "changeReason") != reason {
			t.Errorf("%s: --reason %+v, want the reason of the change: %t", path, f, reason)
		}
	}
	for _, path := range []string{"admin user list", "admin token list", "admin key export", "admin bootstrap",
		"admin retention", "admin repo prefetch", "admin webhook test", "admin access-report", "migrate", "serve"} {
		cmd, _, err := root.Find(strings.Fields(path))
		if err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		if cmd.Flags().Lookup("actor") != nil {
			t.Errorf("%s takes --actor, and writes no row against the database", path)
		}
	}
}

// TestOriginOfTheCommandLine is #1284's 1A: every row of the command line has source cli, the
// host and the operating-system user, and the actor and the reason of --actor and --reason, or
// of LAJSTROM_ACTOR and LAJSTROM_REASON, the flag first; the row stays by anonymous. A value out
// of bounds, or an actor in the form of a principal, is exit status 2 and writes nothing.
func TestOriginOfTheCommandLine(t *testing.T) {
	c := newUserCLI(t)
	steps := []struct {
		name, user string
		env        map[string]string
		flags      []string
		actor      string // of the user.create row; "" none
		reason     string
		wantErr    string // "" for success; a failure is exit status 2
	}{
		{name: "flags", user: "u1", flags: []string{"--actor", "alice@example.com", "--reason", "INC-42: the recovery"},
			actor: "alice@example.com", reason: "INC-42: the recovery"},
		{name: "variables", user: "u2", env: map[string]string{envActor: "bob", envReason: "CHG-7"}, actor: "bob", reason: "CHG-7"},
		{name: "the flag first", user: "u3", env: map[string]string{envActor: "bob", envReason: "CHG-7"},
			flags: []string{"--actor", "carol"}, actor: "carol", reason: "CHG-7"},
		{name: "empty variables are unset", user: "u4", env: map[string]string{envActor: "", envReason: ""}},
		{name: "neither", user: "u5"},
		{name: "a principal's form", user: "x1", flags: []string{"--actor", "user:alice"}, wantErr: "has the form of a principal"},
		{name: "anonymous", user: "x2", flags: []string{"--actor", "anonymous"}, wantErr: "has the form of a principal"},
		{name: "a token's form, from the variable", user: "x3", env: map[string]string{envActor: "token:3"},
			wantErr: "has the form of a principal"},
		{name: "a long actor", user: "x4", flags: []string{"--actor", strings.Repeat("a", maxActor+1)}, wantErr: "129 bytes, at most 128"},
		{name: "a long reason", user: "x5", flags: []string{"--reason", strings.Repeat("r", maxReason+1)},
			wantErr: "1025 bytes, at most 1024"},
		{name: "a line break", user: "x6", flags: []string{"--reason", "two\nlines"}, wantErr: "a control character"},
		{name: "not UTF-8", user: "x7", flags: []string{"--actor", "\xff"}, wantErr: "not UTF-8"},
	}
	for _, s := range steps {
		t.Run(s.name, func(t *testing.T) {
			_, err := c.create(t, s.env, strings.NewReader("a-password\n"), s.user, append([]string{"--password-stdin"}, s.flags...)...)
			if s.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), s.wantErr) || exitStatus(err) != 2 {
					t.Fatalf("error = %v (exit status %d), want %q, exit status 2", err, exitStatus(err), s.wantErr)
				}
				if _, err := openTestStore(t, c.dsn).GetUser(t.Context(), s.user); err == nil {
					t.Errorf("the user %s was created", s.user)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			d := userCreateDetails(t, c.dsn, s.user)
			if d["source"] != "cli" || d["host"] != hostName() || d["osUser"] != osUser() || d["actor"] != nilIfEmpty(s.actor) ||
				d["changeReason"] != nilIfEmpty(s.reason) || d["reason"] != nil {
				t.Errorf("details %v; want source cli, the host, osUser %s, actor %q, changeReason %q", d, osUser(), s.actor, s.reason)
			}
		})
	}
}

// TestOriginOfARule: on admin policy rule create, --reason is the rule's, which clients see, and
// the reason of the change comes from LAJSTROM_REASON.
func TestOriginOfARule(t *testing.T) {
	cfg, dsn := writeKeyConfig(t, t.TempDir())
	env := map[string]string{envReason: "INC-9"}
	runEnv(t, cfg, env, []step{
		{args: []string{"migrate"}},
		{args: []string{"admin", "policy", "rule", "create", "--mask", "raw/*", "--name", "evil", "--reason", "CVE-2026-1234",
			"--actor", "dana"}},
	})
	evs, err := openTestStore(t, dsn).ListAudit(t.Context(), index.AuditFilter{Action: "policy.rule.create"})
	if err != nil || len(evs) != 1 {
		t.Fatalf("policy.rule.create rows %v, %v; want 1", evs, err)
	}
	var d map[string]any
	if err := json.Unmarshal(evs[0].Details, &d); err != nil || d["reason"] != "CVE-2026-1234" || d["changeReason"] != "INC-9" ||
		d["actor"] != "dana" || d["osUser"] != osUser() {
		t.Errorf("details %s, %v; want the rule's reason, changeReason INC-9, actor dana", evs[0].Details, err)
	}
}

// TestRequireReasonOnTheCommandLine: with auth.requireReason, a command that changes the control
// plane without --reason or LAJSTROM_REASON is exit status 2 and writes nothing; admin policy rule
// create, whose --reason is the rule's, needs LAJSTROM_REASON; a command that reads, or that is no
// change of §13 (admin key rotate), needs none (docs/spec/rbac.md §6, §13).
func TestRequireReasonOnTheCommandLine(t *testing.T) {
	dir := t.TempDir()
	cfg := writeAuthConfig(t, dir, "keys:\n  masterKeyEnv: LAJSTROM_MASTER_KEY\nauth:\n  requireReason: true\n")
	runEnv(t, cfg, nil, []step{{args: []string{"migrate"}}})
	const required = "required (auth.requireReason)"
	binding := []string{"admin", "binding", "create", "--principal", "group:ops", "--role", "reader", "--mask", "raw/*"}
	rule := []string{"admin", "policy", "rule", "create", "--mask", "raw/*", "--name", "evil", "--reason", "CVE-2026-1"}
	for _, args := range [][]string{binding, rule, {"admin", "user", "unlock", "nobody"}, {"admin", "binding", "delete", "1"}} {
		_, _, err := run(t, nil, append(args, "--config", cfg)...)
		if err == nil || !strings.Contains(err.Error(), required) || exitStatus(err) != 2 {
			t.Errorf("%v without a reason: %v (exit status %d), want %q, exit status 2", args, err, exitStatus(err), required)
		}
	}
	if _, _, err := run(t, nil, append(rule, "--config", cfg)...); err == nil || !strings.Contains(err.Error(), envReason+": required") {
		t.Errorf("a rule without %s: %v", envReason, err)
	}
	if bs, err := openTestStore(t, "file:"+filepath.Join(dir, "lajstrom.db")).ListBindings(t.Context(), ""); err != nil || len(bs) != 0 {
		t.Fatalf("bindings made without a reason: %v, %v", bs, err)
	}
	runEnv(t, cfg, map[string]string{"LAJSTROM_MASTER_KEY": testMasterKey}, []step{
		{args: append(binding, "--reason", "CHG-1")},
		{args: []string{"admin", "binding", "list"}, wantOut: "group:ops"},
		{args: []string{"admin", "key", "rotate", "--token"}},
	})
	runEnv(t, cfg, map[string]string{envReason: "CHG-2"}, []step{{args: rule, wantOut: `"name":"evil"`}})
}

// TestOSUser: the name of the uid, never $USER; uid:<n> only where the system has no name.
func TestOSUser(t *testing.T) {
	t.Setenv("USER", "someone-else")
	if u := osUser(); u == "" || u == "someone-else" {
		t.Errorf("osUser() = %q", u)
	}
}

// userCreateDetails returns the details of the user.create row of the user name.
func userCreateDetails(t *testing.T, dsn, name string) map[string]any {
	t.Helper()
	st := openTestStore(t, dsn)
	u, err := st.GetUser(t.Context(), name)
	if err != nil {
		t.Fatal(err)
	}
	evs, err := st.ListAudit(t.Context(), index.AuditFilter{Action: auth.ActionUserCreate})
	if err != nil {
		t.Fatal(err)
	}
	for _, ev := range evs {
		if ev.Subject == "user:"+u.UID {
			if ev.Principal != "anonymous" {
				t.Errorf("principal %s, want anonymous", ev.Principal)
			}
			var d map[string]any
			if err := json.Unmarshal(ev.Details, &d); err != nil {
				t.Fatal(err)
			}
			return d
		}
	}
	t.Fatalf("no user.create row of %s", name)
	return nil
}

// nilIfEmpty is s as a JSON value of the details: nil for "", a key that is not there.
func nilIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}
