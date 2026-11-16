package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lajstrom/lajstrom/internal/api"
	"github.com/lajstrom/lajstrom/internal/auth/ci"
	"github.com/lajstrom/lajstrom/internal/index"
	"github.com/lajstrom/lajstrom/internal/index/sqlite"
)

// ciPolicyCLI migrates a configuration with a GitHub issuer and a policy of the configuration,
// and returns a function that runs admin ci-policy with args against it, and the database's DSN.
func ciPolicyCLI(t *testing.T) (func(args ...string) (string, error), string) {
	t.Helper()
	dir := t.TempDir()
	cfg, dsn := filepath.Join(dir, "lajstrom.yaml"), "file:"+filepath.Join(dir, "lajstrom.db")
	body := "server:\n  listen: \":8080\"\n  externalURL: https://art.example.com\ndatabase:\n  driver: sqlite\n  dsn: \"" + dsn + "\"\n" +
		"auth:\n  ci:\n    apiWriteRepos: [\"oci/*\"]\n    issuers: [{name: github, platform: github, issuer: \"https://token.actions.githubusercontent.com\"}]\n" +
		"    policies:\n      - {name: from-config, issuer: github, claims: {repository_owner_id: \"9\"}, grants: [{role: reader, mask: \"*/*\"}]}\n"
	if err := os.WriteFile(cfg, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	runSteps(t, cfg, []step{{args: []string{"migrate"}}})
	return func(args ...string) (string, error) {
		t.Helper()
		out, _, err := run(t, nil, append(append([]string{"admin", "ci-policy"}, args...), "--config", cfg)...)
		return out, err
	}, dsn
}

// TestAdminCIPolicy: admin ci-policy creates a policy of the API by the rules of the
// configuration's, lists it with the configuration's, deletes it, refuses to delete one of the
// configuration, and writes a ci.policy.* row with the source cli for each change.
func TestAdminCIPolicy(t *testing.T) {
	cli, dsn := ciPolicyCLI(t)
	out, err := cli("create", "--name", "acme-read", "--issuer", "github", "--claim", "repository_id=123", "--claim", "ref=refs/tags/v*",
		"--grant", "reader:oci/*")
	if err != nil {
		t.Fatal(err)
	}
	var p ci.Policy
	if err := json.Unmarshal([]byte(out), &p); err != nil || p.Source != "api" || p.Claims["ref"][0] != "refs/tags/v*" {
		t.Fatalf("create printed %q (%v)", out, err)
	}
	for _, tt := range []struct {
		args    []string
		wantErr string
	}{
		{[]string{"create", "--name", "w", "--issuer", "github", "--claim", "repository_id=1", "--claim", "ref=main", "--grant", "writer:oci/app"},
			"a policy of the API that grants write: this needs a sign-in with a second factor"},
		{[]string{"create", "--name", "x", "--issuer", "github", "--claim", "repository=acme/app", "--grant", "reader:*/*"}, "must name the project by an id"},
		{[]string{"create", "--name", "x", "--issuer", "github", "--claim", "repository_id", "--grant", "reader:*/*"}, "want <claim>=<value>"},
		{[]string{"delete", "from-config"}, "declared in the configuration"},
		{[]string{"delete", "none"}, "policy none does not exist"},
	} {
		if _, err := cli(tt.args...); err == nil || !strings.Contains(err.Error(), tt.wantErr) {
			t.Errorf("ci-policy %v: %v, want %q", tt.args, err, tt.wantErr)
		}
	}
	out, err = cli("list")
	if err != nil || !strings.Contains(out, "from-config  config  active  -        github") ||
		!strings.Contains(out, "acme-read    api     active  -        github  ref=refs/tags/v* repository_id=123  reader:oci/*") {
		t.Errorf("list = %q, %v", out, err)
	}
	stale := api.ETag(api.TableCIPolicies, 99, 1)
	if _, err := cli("delete", "acme-read", "--if-match", stale); err == nil || !strings.Contains(err.Error(), "CI trust policy acme-read changed") {
		t.Errorf("delete --if-match of another revision: %v", err)
	}
	if _, err := cli("delete", "acme-read", "--if-match", "*"); err != nil {
		t.Fatal(err)
	}
	if got := cliPolicyRows(t, dsn); got != "ci.policy.delete ci.policy.create" {
		t.Errorf("rows %v", got)
	}
}

// cliPolicyRows returns the actions of the ci.policy.* rows of the source cli in the database of
// dsn, newest first.
func cliPolicyRows(t *testing.T, dsn string) string {
	t.Helper()
	st, err := sqlite.Open(t.Context(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	rows, err := st.ListAudit(t.Context(), index.AuditFilter{})
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, r := range rows {
		if strings.HasPrefix(r.Action, "ci.policy.") && strings.Contains(string(r.Details), `"source":"cli"`) {
			got = append(got, r.Action)
		}
	}
	return strings.Join(got, " ")
}
