package main

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/lajstrom/lajstrom/internal/auth"
)

// bindingCLI migrates a new configuration with the users alice and root (admin), and returns a
// function that runs admin binding with args against it.
func bindingCLI(t *testing.T) func(args ...string) (string, error) {
	t.Helper()
	cfg, _ := tokenCLI(t)
	env := map[string]string{bootstrapEnv: "root-password"}
	if _, _, err := runIn(t, env, strings.NewReader(""), "admin", "user", "create", "--name", "root", "--admin", "--config", cfg); err != nil {
		t.Fatal(err)
	}
	return func(args ...string) (string, error) {
		t.Helper()
		out, _, err := run(t, nil, append(append([]string{"admin", "binding"}, args...), "--config", cfg)...)
		return out, err
	}
}

func TestAdminBindingCreate(t *testing.T) {
	binding := bindingCLI(t)
	out, err := binding("create", "--principal", "user:alice", "--role", "writer", "--mask", "raw/*")
	if err != nil {
		t.Fatal(err)
	}
	var b auth.Binding
	if err := json.Unmarshal([]byte(out), &b); err != nil || strings.Count(out, "\n") != 1 {
		t.Fatalf("create printed %q: want one JSON line (%v)", out, err)
	}
	if b.ID == 0 || b.Principal != "user:alice" || b.Role != "writer" || b.RepoMask != "raw/*" {
		t.Errorf("create = %+v", b)
	}
	for _, tt := range []struct {
		args    []string
		wantErr string
	}{
		{[]string{"create", "--principal", "user:alice", "--role", "writer", "--mask", "raw/*"}, "exists already"},
		{[]string{"create", "--principal", "anonymous", "--role", "reader", "--mask", "raw/*"}, "principal: anonymous cannot be bound"},
		{[]string{"create", "--principal", "user:bob", "--role", "reader", "--mask", "raw/*"}, "principal: user bob does not exist"},
		{[]string{"create", "--principal", "user:alice", "--role", "reader", "--mask", "rpm/*"}, `"rpm" is not a format of this server`},
		{[]string{"create", "--principal", "user:alice", "--role", "reader", "--mask", "oci/*"}, ""},
		{[]string{"create", "--principal", "user:alice", "--role", "reader"}, `required flag(s) "mask" not set`},
		{[]string{"delete", "x"}, `binding id "x"`},
		{[]string{"delete", "999"}, "binding 999 does not exist"},
		{[]string{"delete", "1"}, "the last admin binding of an enabled user"},
	} {
		if _, err := binding(tt.args...); (tt.wantErr == "") != (err == nil) || (err != nil && !strings.Contains(err.Error(), tt.wantErr)) {
			t.Errorf("binding %v: %v, want %q", tt.args, err, tt.wantErr)
		}
	}
}

func TestAdminBindingListDelete(t *testing.T) {
	binding := bindingCLI(t)
	for _, mask := range []string{"raw/*", "oci/*"} {
		if _, err := binding("create", "--principal", "user:alice", "--role", "writer", "--mask", mask); err != nil {
			t.Fatal(err)
		}
	}
	out, err := binding("list")
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != 4 || !strings.HasPrefix(lines[0], "ID  PRINCIPAL") ||
		!strings.Contains(lines[1], "user:root   admin   */*") || !strings.Contains(lines[2], "user:alice  writer  raw/*") {
		t.Errorf("list:\n%s", out)
	}
	if out, err := binding("list", "--principal", "user:root"); err != nil || strings.Count(out, "\n") != 2 {
		t.Errorf("list --principal user:root = %q, %v; want the header and one binding", out, err)
	}
	if _, err := binding("delete", "2"); err != nil {
		t.Errorf("delete 2: %v", err)
	}
	if out, _ := binding("list", "--principal", "user:alice"); strings.Contains(out, "raw/*") {
		t.Errorf("list after delete:\n%s", out)
	}
}
