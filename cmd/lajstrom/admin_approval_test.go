package main

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/lajstrom/lajstrom/internal/auth"
	"github.com/lajstrom/lajstrom/internal/index"
)

// approvalCLI is a database with the admin alice and two requests of hers, a binding's and then a
// deploy token's, whose change keeps a hash.
func approvalCLI(t *testing.T) (userCLI, *index.User) {
	t.Helper()
	c := newUserCLI(t)
	if _, err := c.create(t, nil, strings.NewReader("alice-password\n"), "alice", "--password-stdin", "--admin"); err != nil {
		t.Fatal(err)
	}
	st := openTestStore(t, c.dsn)
	alice, err := st.GetUser(t.Context(), "alice")
	if err != nil {
		t.Fatal(err)
	}
	by := auth.UserPrincipal(alice).String()
	now := time.Now().UTC()
	for _, a := range []index.Approval{
		{Kind: index.ApprovalBinding, Subject: by + " admin */*", RequestedBy: by,
			Change: json.RawMessage(`{"principal":"` + by + `","role":"admin","repoMask":"*/*"}`)},
		{Kind: index.ApprovalDeployToken, Subject: "raw/* fleet", RequestedBy: by,
			Change: json.RawMessage(`{"name":"fleet","repoMask":"raw/*","scopes":["read","write"],"expiresAt":null,"hash":"secret-hash"}`)},
	} {
		a.RequestedAt, a.ExpiresAt = now, now.Add(auth.ApprovalLifetime)
		if err := st.CreateApproval(t.Context(), &a); err != nil {
			t.Fatal(err)
		}
	}
	return c, alice
}

// TestAdminApprovalList: admin approval list prints the requests for a second admin, newest
// first, of a state or of every state, a user by its name and a deploy token's change without its
// hash.
func TestAdminApprovalList(t *testing.T) {
	c, alice := approvalCLI(t)
	out, _, err := run(t, nil, "admin", "approval", "list", "--config", c.cfg)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != 3 {
		t.Fatalf("admin approval list:\n%s", out)
	}
	for i, want := range []string{"ID  KIND", "raw/* fleet read,write", "user:alice admin */*"} {
		if !strings.Contains(lines[i], want) || i > 0 && !strings.Contains(lines[i], "pending") {
			t.Errorf("line %d: %q, want %q, pending", i, lines[i], want)
		}
	}
	if strings.Count(out, "user:alice") != 3 || strings.Contains(out, "secret-hash") || strings.Contains(out, alice.UID) {
		t.Errorf("admin approval list, by name, without the hash:\n%s", out)
	}
	if out, _, err := run(t, nil, "admin", "approval", "list", "--state", "rejected", "--config", c.cfg); err != nil ||
		strings.Count(out, "\n") != 1 {
		t.Errorf("the rejected requests: %q, %v; want the header alone", out, err)
	}
}

// TestAdminApprovalListRefused: another state and a limit below 1 are refused.
func TestAdminApprovalListRefused(t *testing.T) {
	c, _ := approvalCLI(t)
	for _, args := range [][]string{{"--state", "waiting"}, {"--limit", "0"}} {
		if _, _, err := run(t, nil, append([]string{"admin", "approval", "list", "--config", c.cfg}, args...)...); err == nil {
			t.Errorf("admin approval list %v: no error", args)
		}
	}
}
