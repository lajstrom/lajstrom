package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/lajstrom/lajstrom/internal/api"
	"github.com/lajstrom/lajstrom/internal/auth"
	"github.com/lajstrom/lajstrom/internal/config"
	"github.com/lajstrom/lajstrom/internal/index"
)

// accessCLI writes a configuration with the settings that the access report reads, migrates its
// index and fills it through the services with real secrets: root, a local admin with a password
// and a personal access token; robot, its service account; a deploy token over */*; and a
// webhook whose URL has a path and a query. It returns the path of the configuration, the
// configuration as serve loads it, the index, and the secrets, hashes and parts of the URL that
// no report may hold.
func accessCLI(t *testing.T) (string, *config.Config, index.Store, []string) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "lajstrom.yaml")
	body := "server:\n  listen: \":8080\"\n  externalURL: https://art.example.com\ndatabase:\n  driver: sqlite\n  dsn: \"file:" +
		filepath.Join(dir, "lajstrom.db") + "\"\nauth:\n  anonymousRead: true\n  disableInactiveUsersAfter: 2160h\n" +
		"  deployTokens:\n    maxTTL: 720h\n  serviceAccounts:\n    maxTokenTTL: 2160h\n"
	must(t, os.WriteFile(path, []byte(body), 0o600))
	cfg, err := config.Load(path, func(string) (string, bool) { return "", false })
	must(t, err)
	st := openIndex(t, dir)
	ctx := context.Background()
	const password = "Corr3ct-Horse-Battery-Staple"
	users := &auth.Users{Index: st}
	_, err = users.Create(ctx, "root", password, true, auth.Origin{})
	must(t, err)
	_, err = users.CreateService(ctx, "robot", "user:root", auth.Origin{})
	must(t, err)
	tokens := &auth.Tokens{Index: st}
	_, pat, err := tokens.CreatePAT(ctx, "root", "laptop", nil, auth.Expiry{}, auth.Origin{})
	must(t, err)
	_, robot, err := tokens.CreatePAT(ctx, "robot", "ci", []string{"read"}, auth.Expiry{}, auth.Origin{})
	must(t, err)
	_, mask, err := tokens.CreateMaskDeploy(ctx, "*/*", "fleet", []string{"read"}, auth.Expiry{}, auth.Origin{})
	must(t, err)
	must(t, st.CreateWebhook(ctx, &index.Webhook{UID: index.NewUID(time.Now()), Mask: "*/*", Active: true, CreatedBy: "user:x",
		URL: "https://hooks.example.com/hook-path-x7/deliver?key=query-secret-q9", SecretEnc: []byte("webhook-secret-w3")}))
	row, err := st.GetUser(ctx, "root")
	must(t, err)
	secrets := []string{password, row.PasswordHash, "$argon2id$", "hook-path-x7", "query-secret-q9", "webhook-secret-w3"}
	for _, s := range []string{pat, robot, mask} {
		secrets = append(secrets, s, auth.HashSecret(s), auth.HashSecret(s)[auth.HashPrefixLen:])
	}
	return path, cfg, st, secrets
}

// canonical returns doc decoded and encoded again in the canonical form of the access report: a
// document in it is its own canonical form.
func canonical(t *testing.T, doc string) string {
	t.Helper()
	dec := json.NewDecoder(strings.NewReader(doc))
	dec.UseNumber()
	var v any
	must(t, dec.Decode(&v))
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	must(t, enc.Encode(v))
	return buf.String()
}

// auditRows returns how many audit rows st has.
func auditRows(t *testing.T, st index.Store) int {
	t.Helper()
	rows, err := st.ListAudit(t.Context(), index.AuditFilter{Feed: true, Limit: 1 << 20})
	must(t, err)
	return len(rows)
}

// TestAdminAccessReport: admin access-report writes the document on standard output in its
// canonical form, and its sha256 on standard error and nothing else there; the document holds
// the settings of the configuration and none of the secrets, hashes and webhook URLs of the
// index; and the command writes no audit row.
func TestAdminAccessReport(t *testing.T) {
	path, _, st, secrets := accessCLI(t)
	before := auditRows(t, st)
	out, errOut, err := run(t, nil, "admin", "access-report", "--config", path)
	must(t, err)
	if errOut != api.AccessDigest([]byte(out))+"\n" {
		t.Errorf("standard error %q, want the sha256 of standard output alone", errOut)
	}
	var rep api.AccessReport
	must(t, json.Unmarshal([]byte(out), &rep))
	if canonical(t, out) != out {
		t.Errorf("the document is not in its canonical form:\n%s", out)
	}
	for _, s := range secrets {
		if strings.Contains(out, s) {
			t.Errorf("the report holds %q:\n%s", s, out)
		}
	}
	wantSettings := api.AccessSettings{AnonymousRead: true, DisableInactiveUsersAfter: 7776000, DeployTokensMaxTTL: 2592000,
		ServiceAccountsMaxTokenTTL: 7776000}
	if rep.Settings != wantSettings || rep.Scope != auth.AccessAll || rep.UnusedDays != 90 || len(rep.Users) != 2 ||
		len(*rep.DeployTokens) != 1 || rep.Anonymous == nil {
		t.Errorf("the report of the configuration and its rows:\n%s", out)
	}
	if after := auditRows(t, st); after != before {
		t.Errorf("admin access-report wrote %d audit rows", after-before)
	}
}

// TestAdminAccessReportScope: --scope and --unused-days are the document's scope and unusedDays.
func TestAdminAccessReportScope(t *testing.T) {
	path, _, _, _ := accessCLI(t)
	people, _, err := run(t, nil, "admin", "access-report", "--config", path, "--scope", "people", "--unused-days", "1")
	must(t, err)
	var p api.AccessReport
	must(t, json.Unmarshal([]byte(people), &p))
	if p.Scope != auth.AccessPeople || p.UnusedDays != 1 || len(p.Users) != 1 || p.Users[0].Name != "root" || p.DeployTokens != nil {
		t.Errorf("--scope people --unused-days 1:\n%s", people)
	}
}

// fixedAccess is an index whose ReadAccess reads at a fixed time, as the tests fix readAccess.
type fixedAccess struct {
	index.Store
	at time.Time
}

func (f fixedAccess) ReadAccess(ctx context.Context) (*index.Access, error) {
	a, err := f.Store.ReadAccess(ctx)
	if a != nil {
		a.ReadAt = f.at
	}
	return a, err
}

// TestAdminAccessReportSameBytes: the same rows at the same time are the same bytes from admin
// access-report and from GET /api/v1/access-report of a server of the same configuration, with
// the same sha256, and generatedAt is the time of the read, not the host's clock; --format csv
// writes the table and, on standard error, the sha256 of the JSON document of the same read.
func TestAdminAccessReportSameBytes(t *testing.T) {
	path, cfg, st, _ := accessCLI(t)
	at := time.Date(2026, 11, 18, 10, 0, 0, 0, time.UTC)
	read := readAccess
	readAccess = func(ctx context.Context, r index.Reader) (*index.Access, error) { // the command's own read, at
		a, err := r.ReadAccess(ctx)
		if a != nil {
			a.ReadAt = at
		}
		return a, err
	}
	t.Cleanup(func() { readAccess = read })

	out, digest, err := run(t, nil, "admin", "access-report", "--config", path, "--scope", "systems")
	must(t, err)
	h := api.New(api.Options{Repos: &api.Repos{Index: fixedAccess{st, at}}, Access: accessConfigOf(cfg)})
	r := httptest.NewRequest(http.MethodGet, "/api/v1/access-report?scope=systems", nil)
	r = r.WithContext(auth.WithPrincipal(r.Context(), auth.Principal{Kind: auth.KindUser, Name: "root"}))
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, r)
	if rr.Code != http.StatusOK {
		t.Fatalf("GET /api/v1/access-report: %d %s", rr.Code, rr.Body)
	}
	if !bytes.Equal(rr.Body.Bytes(), []byte(out)) {
		t.Errorf("the API answers\n%s\nthe command line writes\n%s", rr.Body, out)
	}
	if want := api.AccessDigest(rr.Body.Bytes()) + "\n"; digest != want {
		t.Errorf("the command line's sha256 %q, the API's document's %q", digest, want)
	}
	if !strings.Contains(out, `"generatedAt":"2026-11-18T10:00:00Z"`) {
		t.Errorf("generatedAt is not the time of the read:\n%s", out)
	}

	table, csvDigest, err := run(t, nil, "admin", "access-report", "--config", path, "--scope", "systems", "--format", "csv")
	must(t, err)
	if csvDigest != digest {
		t.Errorf("--format csv: the sha256 %q, want that of the JSON document of the same read, %q", csvDigest, digest)
	}
	if lines := strings.Split(strings.TrimSuffix(table, "\n"), "\n"); lines[0] != "principal,kind,name,item,role,mask,scopes,"+
		"expiresAt,lastUsedAt,via,flags" || len(lines) != 3 {
		t.Errorf("--format csv:\n%s", table)
	}
}

// TestAdminAccessReportUsage: a scope, an unused-days or a format out of its bounds is a usage
// error, exit status 2, that names the flag, before the database is read.
func TestAdminAccessReportUsage(t *testing.T) {
	path := writeConfig(t, t.TempDir()) // no database: a usage error comes first
	for _, tt := range []struct {
		args []string
		want string
	}{
		{[]string{"--scope", "everyone"}, `--scope: "everyone" is not all, people or systems`},
		{[]string{"--unused-days", "0"}, "--unused-days: 0 is not a number of days from 1 to 3650"},
		{[]string{"--unused-days", "3651"}, "--unused-days: 3651 is not a number of days from 1 to 3650"},
		{[]string{"--format", "xml"}, `--format "xml": json or csv`},
	} {
		_, _, err := run(t, nil, append([]string{"admin", "access-report", "--config", path}, tt.args...)...)
		if err == nil || !strings.Contains(err.Error(), tt.want) || exitStatus(err) != 2 {
			t.Errorf("%v: %v (exit %d), want %q, exit 2", tt.args, err, exitStatus(err), tt.want)
		}
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(path), "lajstrom.db")); err == nil {
		t.Errorf("a refused command made the database")
	}
}
