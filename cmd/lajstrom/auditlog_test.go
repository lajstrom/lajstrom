package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/lajstrom/lajstrom/internal/auth"
	"github.com/lajstrom/lajstrom/internal/index"
	"github.com/lajstrom/lajstrom/internal/index/sqlite"
)

// auditLine is what the tests read of the log line of an audit row (docs/spec/rbac.md §5).
type auditLine struct {
	Level   string          `json:"level"`
	Msg     string          `json:"msg"`
	ID      int64           `json:"id"`
	Action  string          `json:"action"`
	State   string          `json:"state"`
	Details json.RawMessage `json:"details"`
}

// auditLines returns the audit lines of logs, a process's standard error in the json format.
func auditLines(logs string) []auditLine {
	var out []auditLine
	for l := range strings.Lines(logs) {
		var line auditLine
		if json.Unmarshal([]byte(l), &line) == nil && line.Msg == "audit" {
			out = append(out, line)
		}
	}
	return out
}

// sameAsFeed fails the test unless lines are the rows of the feed of the SQLite database dsn, by
// id and action.
func sameAsFeed(t *testing.T, dsn string, lines []auditLine) {
	t.Helper()
	st, err := sqlite.Open(t.Context(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	rows, err := st.ListAudit(t.Context(), index.AuditFilter{Feed: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != len(lines) {
		t.Fatalf("%d rows and %d lines", len(rows), len(lines))
	}
	for i, r := range rows {
		if l := lines[i]; l.ID != r.ID || l.Action != r.Action {
			t.Errorf("line %d is %d %s, the feed's row %d %s", i, l.ID, l.Action, r.ID, r.Action)
		}
	}
}

// withoutAuditLines is logs, in the text format, without the audit lines.
func withoutAuditLines(logs string) string {
	var b strings.Builder
	for l := range strings.Lines(logs) {
		if !strings.Contains(l, " msg=audit ") {
			b.WriteString(l)
		}
	}
	return b.String()
}

// noSecret fails the test when logs hold one of secrets, by what each is; a secret that is
// empty stops it, since the fixture did not make it.
func noSecret(t *testing.T, logs string, secrets map[string]string) {
	t.Helper()
	for what, s := range secrets {
		switch {
		case s == "":
			t.Fatalf("the fixture made no %s", what)
		case strings.Contains(logs, s):
			t.Errorf("the log holds %s", what)
		}
	}
}

// TestAuditLinesOfCommands: the commands against the database write the line of each audit row
// they commit on standard error, at info under --log-level error and in the format of
// --log-format, one per committed state, as the feed has the rows; no fixture secret reaches the
// log (#1278). With audit.log false they write none.
func TestAuditLinesOfCommands(t *testing.T) {
	cfg, dsn := writeKeyConfig(t, t.TempDir())
	env := map[string]string{"LAJSTROM_MASTER_KEY": testMasterKey}
	var logs strings.Builder
	cli := func(in string, args ...string) string {
		t.Helper()
		out, stderr, err := runIn(t, env, strings.NewReader(in), append(args, "--config", cfg, "--log-level", "error",
			"--log-format", "json")...)
		if err != nil {
			t.Fatalf("%v: %v", args, err)
		}
		logs.WriteString(stderr)
		return out
	}
	secret := func(out string) string {
		var v struct{ Secret string }
		if err := json.Unmarshal([]byte(out), &v); err != nil {
			t.Fatalf("%q: %v", out, err)
		}
		return v.Secret
	}
	cli("", "migrate")
	cli("alice-first-password", "admin", "user", "create", "--name", "alice", "--password-stdin")
	cli("alice-second-password", "admin", "user", "set-password", "alice", "--password-stdin")
	pat := secret(cli("", "admin", "token", "create", "--user", "alice", "--name", "laptop"))
	hook := secret(cli("", "admin", "webhook", "create", "--mask", "raw/*", "--events", "version.published",
		"--url", "https://ci.example.com/hooks/url-path-secret?token=url-query-secret"))
	rotated := secret(cli("", "admin", "webhook", "rotate", "1"))

	lines := auditLines(logs.String())
	var got []string
	for _, l := range lines {
		got = append(got, l.Action+" "+l.State)
		if l.Level != "INFO" || !strings.Contains(string(l.Details), `"source":"cli"`) {
			t.Errorf("line %d: level %s, details %s; want INFO under --log-level error, source cli", l.ID, l.Level, l.Details)
		}
	}
	want := []string{"user.create ok", "user.password ok", "token.create ok", "webhook.create ok", "webhook.secret ok"}
	if !slices.Equal(got, want) {
		t.Errorf("the audit lines %q, want %q", got, want)
	}
	sameAsFeed(t, dsn, lines)
	noSecret(t, logs.String(), map[string]string{"the first password": "alice-first-password",
		"the second password": "alice-second-password", "a password's hash": "$argon2id$", "the token": pat,
		"the token's hash": auth.HashSecret(pat), "the path of the webhook's URL": "url-path-secret",
		"the query of the webhook's URL": "url-query-secret", "the webhook's secret": hook, "the rotated secret": rotated,
		"the master key": testMasterKey})
}

// TestAuditLinesOff: with audit.log false a command writes its rows and no line.
func TestAuditLinesOff(t *testing.T) {
	cfg, _ := writeKeyConfig(t, t.TempDir())
	appendConfig(t, cfg, "audit:\n  log: false\n")
	runSteps(t, cfg, []step{{args: []string{"migrate"}}})
	_, stderr, err := runIn(t, nil, strings.NewReader("bob-password"), "admin", "user", "create", "--name", "bob", "--password-stdin",
		"--config", cfg, "--log-format", "json")
	if err != nil {
		t.Fatal(err)
	}
	if n := len(auditLines(stderr)); n != 0 {
		t.Errorf("audit.log false: %d audit lines in %q", n, stderr)
	}
}

// appendConfig appends text to the configuration file cfg.
func appendConfig(t *testing.T, cfg, text string) {
	t.Helper()
	f, err := os.OpenFile(cfg, os.O_APPEND|os.O_WRONLY, 0) // #nosec G304 -- the test's own file
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(text); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
}

// TestServeAuditLines: serve writes the line of each row it commits, at info under --log-level
// error; a refused bootstrap token writes none; the bootstrap token, the admin's password and a
// token's secret and hash do not reach its log (#1278).
func TestServeAuditLines(t *testing.T) {
	dir := t.TempDir()
	cfg, addr := serveConfig(t, dir)
	token := "the-bootstrap-token-" + strings.Repeat("t", auth.MinBootstrapTokenLen)
	file := filepath.Join(dir, "bootstrap")
	if err := os.WriteFile(file, []byte(token+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	appendConfig(t, cfg, fmt.Sprintf("auth:\n  bootstrap:\n    tokenFile: %q\n", file))
	var logs syncBuffer
	stop := startServeWith(t, cfg, addr, &logs, "--log-level", "error", "--log-format", "json")
	const password = "root-password-of-the-test"
	post := func(path, user, body string, status int) string {
		t.Helper()
		req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, "http://"+addr+path, strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		if user != "" {
			req.SetBasicAuth(user, password)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		b, err := io.ReadAll(resp.Body)
		if err != nil || resp.StatusCode != status {
			t.Fatalf("POST %s: %d %s (%v), want %d", path, resp.StatusCode, b, err, status)
		}
		var v struct{ Secret string }
		_ = json.Unmarshal(b, &v) // the bootstrap's answer has none
		return v.Secret
	}
	post("/api/v1/bootstrap", "", `{"token": "`+strings.Repeat("w", 40)+`", "name": "root", "password": "`+password+`"}`,
		http.StatusForbidden)
	post("/api/v1/bootstrap", "", `{"token": "`+token+`", "name": "root", "password": "`+password+`"}`, http.StatusCreated)
	pat := post("/api/v1/users/root/tokens", "root", `{"name": "laptop"}`, http.StatusCreated)
	if err := stop(); err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, l := range auditLines(logs.String()) {
		got = append(got, l.Action)
		if l.Level != "INFO" {
			t.Errorf("line %d at %s, want INFO under --log-level error", l.ID, l.Level)
		}
	}
	if !slices.Contains(got, "user.create") || !slices.Contains(got, "token.create") {
		t.Errorf("the audit lines of serve: %q, want user.create and token.create among them", got)
	}
	noSecret(t, logs.String(), map[string]string{"the bootstrap token": token, "the password": password,
		"a password's hash": "$argon2id$", "the token": pat, "the token's hash": auth.HashSecret(pat)})
}
