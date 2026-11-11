package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/lajstrom/lajstrom/internal/auth"
)

// tokenCLI migrates the database of a new configuration, creates the repo raw/scratch and the
// user alice in it, and returns the configuration and the DSN.
func tokenCLI(t *testing.T) (cfg, dsn string) {
	t.Helper()
	cfg, dsn = writeRepoConfig(t, t.TempDir())
	runSteps(t, cfg, []step{
		{args: []string{"migrate"}},
		{args: []string{"admin", "repo", "create", "raw/scratch", "--registry", "local"}},
	})
	if _, _, err := runIn(t, nil, strings.NewReader("alice-password"), "admin", "user", "create", "--name", "alice", "--password-stdin",
		"--config", cfg); err != nil {
		t.Fatal(err)
	}
	return cfg, dsn
}

// cliCreate runs admin token create with args and returns the token it prints, after checking
// that it printed one line and nothing else.
func cliCreate(t *testing.T, cfg string, args ...string) auth.CreatedToken {
	t.Helper()
	out, _, err := run(t, nil, append(append([]string{"admin", "token", "create"}, args...), "--config", cfg)...)
	if err != nil {
		t.Fatalf("create %v: %v", args, err)
	}
	if strings.Count(out, "\n") != 1 || !strings.HasSuffix(out, "\n") {
		t.Errorf("create %v printed %d lines, want one", args, strings.Count(out, "\n"))
	}
	var c auth.CreatedToken
	if err := json.Unmarshal([]byte(out), &c); err != nil {
		t.Fatalf("create %v: the output is not one JSON object: %v", args, err)
	}
	return c
}

// summary is what the CLI tests compare of a created token: kind, owner, name, scopes, expiry,
// and whether the secret has the prefix of the kind and the hash prefix is the secret's.
func summary(c auth.CreatedToken) string {
	expires := "never"
	if c.ExpiresAt != nil {
		expires = "expires"
		if c.ExpiresAt.Year() == 2027 && c.ExpiresAt.Month() == time.January {
			expires = c.ExpiresAt.Format(time.RFC3339)
		}
	}
	prefix := map[string]string{auth.TokenPAT: auth.PATPrefix, auth.TokenDeploy: auth.DeployPrefix}[c.Kind]
	ok := strings.HasPrefix(c.Secret, prefix) && len(c.Secret) == auth.SecretLen && c.HashPrefix == auth.HashSecret(c.Secret)[:8]
	return fmt.Sprintf("%s %s%s %s %v %s secret:%t", c.Kind, c.User, c.Repo, c.Name, c.Scopes, expires, ok)
}

// TestAdminToken runs the life of tokens from the command line: a personal access token of a
// user and a deploy token of a repo, created with their secret on stdout as one line of JSON
// and nothing else, listed as a table without the secret, revoked; the flag rules and the
// errors; the audit events of the source cli.
func TestAdminToken(t *testing.T) {
	cfg, dsn := tokenCLI(t)
	pat := cliCreate(t, cfg, "--user", "alice", "--name", "laptop")
	dated := cliCreate(t, cfg, "--user", "alice", "--name", "dated", "--scopes", "read", "--expires", "2027-01-31T00:00:00Z")
	dep := cliCreate(t, cfg, "--repo", "raw/scratch", "--name", "ci", "--scopes", "write,read", "--expires", "30d")
	forever := cliCreate(t, cfg, "--repo", "raw/scratch", "--name", "forever", "--scopes", "read")
	got := strings.Join([]string{summary(pat), summary(dated), summary(dep), summary(forever)}, "\n")
	want := strings.Join([]string{
		"pat alice laptop [] expires secret:true",
		"pat alice dated [read] 2027-01-31T00:00:00Z secret:true",
		"deploy raw/scratch ci [read write] expires secret:true",
		"deploy raw/scratch forever [read] never secret:true",
	}, "\n")
	if got != want {
		t.Errorf("created:\n%s\nwant:\n%s", got, want)
	}

	runSteps(t, cfg, []step{
		{args: []string{"admin", "token", "revoke", fmt.Sprint(pat.ID)}},
		{args: []string{"admin", "token", "revoke", fmt.Sprint(pat.ID)}}, // again: nothing changes
		{args: []string{"admin", "token", "list", "--user", "alice"},
			wantOut: "ID  KIND  NAME    SCOPES  EXPIRES               LAST USED  REVOKED               HASH\n" +
				fmt.Sprintf("%d   pat   laptop  all     %s  never      20", pat.ID, pat.ExpiresAt.Format(time.RFC3339))},
		{args: []string{"admin", "token", "list", "--user", "alice"},
			wantOut: fmt.Sprintf("dated   read    2027-01-31T00:00:00Z  never      -                     %s\n", dated.HashPrefix)},
		{args: []string{"admin", "token", "list", "--repo", "raw/scratch"}, wantOut: "deploy  forever  read        never  "},
		{args: []string{"admin", "token", "list", "--repo", "raw/scratch"}, wantOut: "deploy  ci       read,write  20"},
		{args: []string{"admin", "token", "create", "--user", "alice", "--repo", "raw/scratch", "--name", "x"},
			wantErr: "if any flags in the group [user repo] are set none of the others can be"},
		{args: []string{"admin", "token", "create", "--name", "x"}, wantErr: "at least one of the flags in the group [user repo] is required"},
		{args: []string{"admin", "token", "list"}, wantErr: "at least one of the flags in the group [user repo] is required"},
		{args: []string{"admin", "token", "create", "--user", "alice"}, wantErr: `required flag(s) "name" not set`},
		{args: []string{"admin", "token", "create", "--user", "alice", "--name", "dated"}, wantErr: `name: "dated" is taken by a live token of user alice`},
		{args: []string{"admin", "token", "create", "--user", "alice", "--name", "x", "--expires", "400d"},
			wantErr: "expiresIn: a personal access token expires at most 365 days ahead"},
		{args: []string{"admin", "token", "create", "--user", "alice", "--name", "x", "--expires", "soon"},
			wantErr: `--expires: "soon" is not a positive number of days, like 30d, or a duration, like 12h, or an RFC 3339 time`},
		{args: []string{"admin", "token", "create", "--repo", "raw/scratch", "--name", "x", "--scopes", "read,delete"},
			wantErr: `scopes: "delete" is not a scope of a deploy token`},
		{args: []string{"admin", "token", "create", "--repo", "raw/scratch", "--name", "x"}, wantErr: "scopes: a deploy token needs read, write or both"},
		{args: []string{"admin", "token", "create", "--user", "dave", "--name", "x"}, wantErr: "user dave does not exist"},
		{args: []string{"admin", "token", "list", "--repo", "raw/absent"}, wantErr: "repo raw/absent does not exist"},
		{args: []string{"admin", "token", "revoke", "999"}, wantErr: "token 999 does not exist"},
		{args: []string{"admin", "token", "revoke", "laptop"}, wantErr: `token id "laptop": want the number of the ID column of admin token list`},
		{args: []string{"admin", "token", "revoke"}, wantErr: "accepts 1 arg(s)"},
	})
	wantNoSecret(t, cfg, pat, dated, dep, forever)
	wantCLIAudit(t, dsn, auth.ActionTokenCreate, 4)
	wantCLIAudit(t, dsn, auth.ActionTokenRevoke, 1)
	if _, err := openTestStore(t, dsn).GetTokenByHash(context.Background(), auth.HashSecret(dep.Secret)); err != nil {
		t.Errorf("the deploy token is not in the index by the hash of its secret: %v", err)
	}
}

// wantNoSecret checks that admin token list prints none of the secrets of tokens, nor their
// hashes.
func wantNoSecret(t *testing.T, cfg string, tokens ...auth.CreatedToken) {
	t.Helper()
	var all strings.Builder
	for _, args := range [][]string{{"list", "--user", "alice"}, {"list", "--repo", "raw/scratch"}} {
		out, _, err := run(t, nil, append(append([]string{"admin", "token"}, args...), "--config", cfg)...)
		if err != nil {
			t.Fatal(err)
		}
		all.WriteString(out)
	}
	for _, tok := range tokens {
		if strings.Contains(all.String(), tok.Secret) || strings.Contains(all.String(), auth.HashSecret(tok.Secret)) {
			t.Errorf("list prints the secret of %s or its hash", tok.Name)
		}
	}
}
