package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"

	"github.com/lajstrom/lajstrom/internal/auth"
	"github.com/lajstrom/lajstrom/internal/index"
	"github.com/lajstrom/lajstrom/internal/server"
)

const scratch = `{"format":"raw","name":"scratch","registry":"local"}`

func TestCreate(t *testing.T) {
	f := newFixture(t, Options{})
	rr := f.do(http.MethodPost, "/api/v1/repos", scratch, auth.Principal{})
	var got Repo
	decodeJSON(t, rr, http.StatusCreated, &got)
	if loc := rr.Header().Get("Location"); loc != "/api/v1/repos/raw/scratch" {
		t.Errorf("Location = %q, want /api/v1/repos/raw/scratch", loc)
	}
	want := Repo{Format: "raw", Name: "scratch", Registry: "local", Kind: "hosted", CreatedAt: testNow, UpdatedAt: testNow}
	wantJSON(t, "created repo", got, mustJSON(t, want))
	if !strings.Contains(rr.Body.String(), `"policy":null`) {
		t.Errorf("body %s: want \"policy\":null", rr.Body)
	}

	var fetched Repo
	decodeJSON(t, f.do(http.MethodGet, "/api/v1/repos/raw/scratch", "", auth.Principal{}), http.StatusOK, &fetched)
	wantJSON(t, "fetched repo", fetched, mustJSON(t, want))

	ev := oneAudit(t, f.st, "repo.create")
	if ev.Principal != "anonymous" || ev.Repo != "raw/scratch" || ev.IP != "192.0.2.1" || ev.Subject != "" {
		t.Errorf("audit event = %+v, want anonymous on raw/scratch from 192.0.2.1 without subject", ev.AuditEvent)
	}
	wantJSON(t, "audit details", ev.details, `{"source":"api","repo":`+mustJSON(t, want)+`}`)

	row, err := f.st.GetRepo(context.Background(), "raw", "scratch")
	if err != nil {
		t.Fatal(err)
	}
	if row.Prefix != "raw/scratch" {
		t.Errorf("prefix = %q, want raw/scratch", row.Prefix)
	}
}

func TestCreateWithPolicy(t *testing.T) {
	f := newFixture(t, Options{})
	body := `{"format":"pypi","name":"internal.v2","registry":"other","kind":"hosted","anonymousRead":true,` +
		`"policy": { "maxFileSize" : 1048576 }}`
	var got Repo
	decodeJSON(t, f.do(http.MethodPost, "/api/v1/repos", body, auth.Principal{}), http.StatusCreated, &got)
	if !got.AnonymousRead || got.Registry != "other" || string(got.Policy) != `{"maxFileSize":1048576}` {
		t.Errorf("repo = %+v (policy %s), want anonymous read in other with the compacted policy", got, got.Policy)
	}
}

func TestCreateDuplicate(t *testing.T) {
	f := newFixture(t, Options{})
	f.create(t, "raw", "scratch")
	wantError(t, f.do(http.MethodPost, "/api/v1/repos", scratch, auth.Principal{}), http.StatusConflict, codeConflict,
		"repo raw/scratch already exists")
	if n := len(auditOf(t, f.st, "repo.create")); n != 1 {
		t.Errorf("%d repo.create events, want the first one only", n)
	}
}

func TestCreateInvalid(t *testing.T) {
	long := strings.Repeat("a", 65)
	tests := []struct {
		name  string
		body  string
		field string // substring of the message
	}{
		{"no format", `{"name":"scratch","registry":"local"}`, "format: must not be empty"},
		{"unknown format", `{"format":"apt","name":"scratch","registry":"local"}`, `format: "apt"`},
		{"no name", `{"format":"raw","registry":"local"}`, "name: must not be empty"},
		{"upper-case name", `{"format":"raw","name":"Scratch","registry":"local"}`, `name: "Scratch"`},
		{"name with a slash", `{"format":"raw","name":"a/b","registry":"local"}`, `name: "a/b"`},
		{"name with a double separator", `{"format":"raw","name":"a--b","registry":"local"}`, `name: "a--b"`},
		{"name too long", `{"format":"raw","name":"` + long + `","registry":"local"}`, "name: "},
		{"no registry", `{"format":"raw","name":"scratch"}`, "registry: must not be empty"},
		{"unknown registry", `{"format":"raw","name":"scratch","registry":"zot"}`, `registry: "zot" is not a configured registry`},
		{"kind proxy of a format without", `{"format":"raw","name":"scratch","registry":"local","kind":"proxy"}`, "kind: format raw has no proxy repos"},
		{"kind group", `{"format":"raw","name":"scratch","kind":"group","group":{"members":["a"]}}`, `kind: format raw has no group repos`},
		{"kind other", `{"format":"raw","name":"scratch","registry":"local","kind":"mirror"}`, `kind: "mirror" is not hosted, proxy or group`},
		{"upstream on hosted", `{"format":"raw","name":"scratch","registry":"local","upstream":{"name":"npmjs"}}`,
			"upstream: only a proxy repo has one, not a hosted repo"},
		{"policy string", `{"format":"raw","name":"scratch","registry":"local","policy":"x"}`, "policy: must be a JSON object or null"},
		{"policy array", `{"format":"raw","name":"scratch","registry":"local","policy":[1]}`, "policy: "},
		{"policy unknown key", `{"format":"raw","name":"scratch","registry":"local","policy":{"maxSize":1}}`, "policy.maxSize: unknown key"},
		{"policy maven on raw", `{"format":"raw","name":"scratch","registry":"local","policy":{"maven":{"sessionTimeout":"1h"}}}`,
			"policy.maven: only a maven repo has it"},
		{"anonymousRead string", `{"format":"raw","name":"scratch","registry":"local","anonymousRead":"yes"}`, "anonymousRead: "},
		{"unknown field", `{"format":"raw","name":"scratch","registry":"local","prefix":"x/"}`, `body: unknown field "prefix"`},
		{"not an object", `[]`, "body: must be a JSON object"},
		{"empty body", ``, "body: must be a JSON object"},
		{"two objects", scratch + `{}`, "body: must be one JSON object"},
		{"trailing garbage", scratch + ` x`, "body: invalid character"},
		{"truncated", `{"format":`, "body: unexpected EOF"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFixture(t, Options{})
			wantError(t, f.do(http.MethodPost, "/api/v1/repos", tt.body, auth.Principal{}), http.StatusBadRequest, codeInvalid, tt.field)
			if repos, _ := f.st.ListRepos(context.Background(), ""); len(repos) != 0 {
				t.Errorf("repos created: %+v", repos)
			}
		})
	}
}

func TestCreateLongestName(t *testing.T) {
	f := newFixture(t, Options{})
	name := strings.Repeat("a", 64)
	body := `{"format":"raw","name":"` + name + `","registry":"local"}`
	wantStatus(t, f.do(http.MethodPost, "/api/v1/repos", body, auth.Principal{}), http.StatusCreated)
}

func TestOversizedBody(t *testing.T) {
	f := newFixture(t, Options{})
	f.create(t, "raw", "scratch")
	big := `{"format":"raw","name":"big","registry":"local","policy":{"x":"` + strings.Repeat("a", maxBody) + `"}}`
	for _, tt := range []struct{ method, target string }{
		{http.MethodPost, "/api/v1/repos"},
		{http.MethodPatch, "/api/v1/repos/raw/scratch"},
	} {
		wantError(t, f.do(tt.method, tt.target, big, auth.Principal{}), http.StatusRequestEntityTooLarge, codeInvalid,
			"body: larger than 65536 bytes")
	}
}

func TestList(t *testing.T) {
	f := newFixture(t, Options{})
	var empty RepoList
	decodeJSON(t, f.do(http.MethodGet, "/api/v1/repos", "", auth.Principal{}), http.StatusOK, &empty)
	if empty.Repos == nil || len(empty.Repos) != 0 {
		t.Errorf("empty list = %#v, want an empty array", empty.Repos)
	}
	for _, r := range []string{"raw/b", "pypi/c", "raw/a"} {
		formatName, name, _ := strings.Cut(r, "/")
		f.create(t, formatName, name)
	}
	for _, tt := range []struct {
		target string
		want   []string
	}{
		{"/api/v1/repos", []string{"pypi/c", "raw/a", "raw/b"}},
		{"/api/v1/repos?format=raw", []string{"raw/a", "raw/b"}},
		{"/api/v1/repos?format=pypi", []string{"pypi/c"}},
	} {
		var got RepoList
		decodeJSON(t, f.do(http.MethodGet, tt.target, "", auth.Principal{}), http.StatusOK, &got)
		if ids := repoIDs(got.Repos); strings.Join(ids, " ") != strings.Join(tt.want, " ") {
			t.Errorf("GET %s = %v, want %v", tt.target, ids, tt.want)
		}
	}
	wantError(t, f.do(http.MethodGet, "/api/v1/repos?format=apt", "", auth.Principal{}), http.StatusBadRequest, codeInvalid, `format: "apt"`)
}

// repoIDs returns <format>/<name> of each repo.
func repoIDs(repos []Repo) []string {
	ids := make([]string, len(repos))
	for i, r := range repos {
		ids[i] = r.Format + "/" + r.Name
	}
	return ids
}

func TestAbsentRepo(t *testing.T) {
	f := newFixture(t, Options{})
	f.create(t, "pypi", "scratch") // same name, another format
	for _, tt := range []struct{ method, target, body string }{
		{http.MethodGet, "/api/v1/repos/raw/scratch", ""},
		{http.MethodPatch, "/api/v1/repos/raw/scratch", `{"anonymousRead":true}`},
		{http.MethodDelete, "/api/v1/repos/raw/scratch", ""},
		{http.MethodDelete, "/api/v1/repos/raw/scratch?force=true", ""},
		{http.MethodPost, "/api/v1/repos/raw/scratch/reindex", ""},
	} {
		t.Run(tt.method+" "+tt.target, func(t *testing.T) {
			wantError(t, f.do(tt.method, tt.target, tt.body, auth.Principal{}), http.StatusNotFound, codeNotFound,
				"repo raw/scratch does not exist")
		})
	}
}

func TestUpdate(t *testing.T) {
	f := newFixture(t, Options{})
	f.create(t, "raw", "scratch")
	var got Repo
	decodeJSON(t, f.do(http.MethodPatch, "/api/v1/repos/raw/scratch", `{"anonymousRead":true,"policy":{"maxFileSize":10}}`,
		auth.Principal{}), http.StatusOK, &got)
	if !got.AnonymousRead || string(got.Policy) != `{"maxFileSize":10}` || got.Registry != "local" {
		t.Errorf("repo = %+v (policy %s), want anonymous read and the policy", got, got.Policy)
	}
	if !got.CreatedAt.Equal(testNow) || !got.UpdatedAt.After(testNow) {
		t.Errorf("createdAt %v, updatedAt %v: want createdAt %v and a later updatedAt", got.CreatedAt, got.UpdatedAt, testNow)
	}
	ev := oneAudit(t, f.st, "repo.update")
	if ev.Principal != "anonymous" || ev.Repo != "raw/scratch" || ev.IP != "192.0.2.1" {
		t.Errorf("audit event = %+v", ev.AuditEvent)
	}
	wantJSON(t, "audit details", ev.details, `{"source":"api","changed":{"anonymousRead":true,"policy":{"maxFileSize":10}}}`)

	// Nothing changes: no event, and the repo as it is.
	for _, body := range []string{`{}`, `{"anonymousRead":true}`, `{"policy":{"maxFileSize":10}}`} {
		decodeJSON(t, f.do(http.MethodPatch, "/api/v1/repos/raw/scratch", body, auth.Principal{}), http.StatusOK, &got)
	}
	if n := len(auditOf(t, f.st, "repo.update")); n != 1 {
		t.Errorf("%d repo.update events after updates that change nothing, want 1", n)
	}

	// The JSON null clears the policy; absent leaves anonymousRead.
	var cleared Repo
	decodeJSON(t, f.do(http.MethodPatch, "/api/v1/repos/raw/scratch", `{"policy":null}`, auth.Principal{}), http.StatusOK, &cleared)
	if string(cleared.Policy) != "null" || !cleared.AnonymousRead {
		t.Errorf("repo = %+v (policy %s), want no policy and anonymous read", cleared, cleared.Policy)
	}
	if rows := auditOf(t, f.st, "repo.update"); len(rows) != 2 {
		t.Errorf("%d repo.update events, want 2", len(rows))
	} else {
		wantJSON(t, "audit details", rows[0].details, `{"source":"api","changed":{"policy":null}}`)
	}
}

// TestUpdatePolicyMerge: PATCH merges the policy key by key (RFC 7396), the audit event has the
// policy as the merge left it, and a refused merge changes nothing.
func TestUpdatePolicyMerge(t *testing.T) {
	f := newFixture(t, Options{})
	f.create(t, "raw", "scratch")
	patch := func(body string) Repo {
		t.Helper()
		var got Repo
		decodeJSON(t, f.do(http.MethodPatch, "/api/v1/repos/raw/scratch", body, auth.Principal{}), http.StatusOK, &got)
		return got
	}
	patch(`{"policy":{"maxFileSize":10,"blobRedirect":"never"}}`)
	got := patch(`{"policy":{"maxFileSize":20}}`)
	wantJSON(t, "policy", got.Policy, `{"blobRedirect":"never","maxFileSize":20}`)
	got = patch(`{"policy":{"blobRedirect":null}}`)
	wantJSON(t, "policy", got.Policy, `{"maxFileSize":20}`)
	rows := auditOf(t, f.st, "repo.update")
	if len(rows) != 3 {
		t.Fatalf("%d repo.update events, want 3", len(rows))
	}
	wantJSON(t, "audit details", rows[0].details, `{"source":"api","changed":{"policy":{"maxFileSize":20}}}`)

	wantError(t, f.do(http.MethodPatch, "/api/v1/repos/raw/scratch", `{"anonymousRead":true,"policy":{"maxFileSize":0}}`, auth.Principal{}),
		http.StatusBadRequest, codeInvalid, "policy.maxFileSize: must be")
	if r, err := f.repos.Get(t.Context(), "raw", "scratch"); err != nil || r.AnonymousRead || string(r.Policy) != `{"maxFileSize":20}` {
		t.Errorf("after a refused merge: %+v, %v; want anonymousRead and the policy as they were", r, err)
	}
	got = patch(`{"policy":{"maxFileSize":null}}`)
	if string(got.Policy) != "null" {
		t.Errorf("policy %s after the last key went, want null", got.Policy)
	}
}

func TestUpdateInvalid(t *testing.T) {
	tests := []struct {
		name  string
		body  string
		field string
	}{
		{"format", `{"format":"pypi"}`, "format: cannot change"},
		{"name", `{"name":"other"}`, "name: cannot change"},
		{"registry", `{"registry":"other"}`, "registry: cannot change"},
		{"registry null", `{"registry":null}`, "registry: cannot change"},
		{"kind", `{"kind":"hosted"}`, "kind: cannot change"},
		{"policy string", `{"policy":"x"}`, "policy: must be a JSON object or null"},
		{"policy blobRedirect", `{"policy":{"blobRedirect":"sometimes"}}`, `policy.blobRedirect: must be "auto" or "never"`},
		// A manager could otherwise make every upload of the repo a 500 (threat model U-7).
		{"policy maxFileSize not a number", `{"policy":{"maxFileSize":"big"}}`, "policy.maxFileSize: must be an integer number of bytes"},
		{"policy maxFileSize zero", `{"policy":{"maxFileSize":0}}`, "policy.maxFileSize: must be an integer number of bytes, at least 1"},
		{"policy unknown key", `{"policy":{"maxFilesize":1}}`, "policy.maxFilesize: unknown key; the keys are maxFileSize, blobRedirect, maven"},
		{"policy maven on raw", `{"policy":{"maven":{"sessionTimeout":"1h"}}}`, "policy.maven: only a maven repo has it, not a raw repo"},
		{"anonymousRead string", `{"anonymousRead":"yes"}`, "anonymousRead: "},
		{"unknown field", `{"createdAt":"2026-01-01T00:00:00Z"}`, `body: unknown field "createdAt"`},
		{"empty body", ``, "body: must be a JSON object"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFixture(t, Options{})
			f.create(t, "raw", "scratch")
			wantError(t, f.do(http.MethodPatch, "/api/v1/repos/raw/scratch", tt.body, auth.Principal{}), http.StatusBadRequest, codeInvalid, tt.field)
			if n := len(auditOf(t, f.st, "repo.update")); n != 0 {
				t.Errorf("%d repo.update events, want none", n)
			}
		})
	}
}

func TestDelete(t *testing.T) {
	f := newFixture(t, Options{})
	ctx := context.Background()
	row := f.create(t, "raw", "scratch")
	pkg := addVersion(t, f.st, row.ID, "docs/readme.md")
	addVersion(t, f.st, row.ID, "docs/index.html")
	repoID := row.ID
	if err := f.st.CreateToken(ctx, &index.Token{Kind: "deploy", RepoID: &repoID, Name: "ci", Hash: "h", Scopes: []string{"read"}}); err != nil {
		t.Fatal(err)
	}

	wantError(t, f.do(http.MethodDelete, "/api/v1/repos/raw/scratch", "", auth.Principal{}), http.StatusConflict, codeConflict,
		"repo raw/scratch has 2 versions; delete them or pass force=true")
	wantError(t, f.do(http.MethodDelete, "/api/v1/repos/raw/scratch?force=yes", "", auth.Principal{}), http.StatusBadRequest, codeInvalid,
		`force: "yes"`)
	if n := len(auditOf(t, f.st, "repo.delete")); n != 0 {
		t.Fatalf("%d repo.delete events after refusals, want none", n)
	}

	rr := f.do(http.MethodDelete, "/api/v1/repos/raw/scratch?force=true", "", auth.Principal{})
	wantStatus(t, rr, http.StatusNoContent)
	if rr.Body.Len() != 0 {
		t.Errorf("body = %q, want none", rr.Body)
	}
	wantError(t, f.do(http.MethodGet, "/api/v1/repos/raw/scratch", "", auth.Principal{}), http.StatusNotFound, codeNotFound)
	if pkgs, err := f.st.ListPackages(ctx, row.ID, ""); err != nil || len(pkgs) != 0 {
		t.Errorf("packages after delete = %v, %v; want none", pkgs, err)
	}
	if vs, err := f.st.ListVersions(ctx, pkg.ID); err != nil || len(vs) != 0 {
		t.Errorf("versions after delete = %v, %v; want none", vs, err)
	}
	if toks, err := f.st.ListTokens(ctx, nil, &repoID); err != nil || len(toks) != 0 {
		t.Errorf("deploy tokens after delete = %v, %v; want none", toks, err)
	}
	ev := oneAudit(t, f.st, "repo.delete")
	if ev.Principal != "anonymous" || ev.Repo != "raw/scratch" || ev.IP != "192.0.2.1" {
		t.Errorf("audit event = %+v", ev.AuditEvent)
	}
	wantJSON(t, "audit details", ev.details, `{"source":"api","versions":2,"force":true,"registryUntouched":true}`)
}

func TestDeleteEmptyRepo(t *testing.T) {
	f := newFixture(t, Options{})
	f.create(t, "raw", "scratch")
	wantStatus(t, f.do(http.MethodDelete, "/api/v1/repos/raw/scratch", "", auth.Principal{}), http.StatusNoContent)
	ev := oneAudit(t, f.st, "repo.delete")
	wantJSON(t, "audit details", ev.details, `{"source":"api","versions":0,"force":false,"registryUntouched":true}`)
}

func TestDeleteOneVersion(t *testing.T) {
	f := newFixture(t, Options{})
	row := f.create(t, "raw", "scratch")
	addVersion(t, f.st, row.ID, "a.txt")
	wantError(t, f.do(http.MethodDelete, "/api/v1/repos/raw/scratch", "", auth.Principal{}), http.StatusConflict, codeConflict,
		"has 1 version;")
}

func TestReindex(t *testing.T) {
	f := newFixture(t, Options{})
	f.create(t, "raw", "scratch")
	wantError(t, f.do(http.MethodPost, "/api/v1/repos/raw/scratch/reindex", "", auth.Principal{}), http.StatusNotImplemented,
		codeUnavailable, "reindex is not enabled in this server")
}

func TestNotInThisVersion(t *testing.T) {
	f := newFixture(t, Options{})
	for _, tt := range []struct{ method, target string }{
		{http.MethodPut, "/api/v1/users/alice/password"},
		{http.MethodGet, "/api/v1/bindings/1"},
		{http.MethodDelete, "/api/v1/audit"},
		{http.MethodPost, "/api/v1/whoami"},
		{http.MethodGet, "/api/v1/users/alice/tokens/1"},
		{http.MethodPut, "/api/v1/repos"},
		{http.MethodGet, "/api/v1/repos/"},
		{http.MethodGet, "/api/"},
	} {
		t.Run(tt.method+" "+tt.target, func(t *testing.T) {
			wantError(t, f.do(tt.method, tt.target, "", auth.Principal{}), http.StatusNotFound, codeNotFound,
				"is not an endpoint of this version")
		})
	}
}

// failingStore is an index whose transactions and repo listing fail.
type failingStore struct{ index.Store }

var errDatabase = errors.New("index: database is gone: host db.internal user lajstrom")

func (failingStore) Tx(context.Context, func(index.Writer) error) error { return errDatabase }
func (failingStore) ListRepos(context.Context, string) ([]index.Repo, error) {
	return nil, errDatabase
}

func (failingStore) GetRepo(context.Context, string, string) (*index.Repo, error) {
	return nil, errDatabase
}

// TestInternalError checks a 5xx: the status text as the message, the cause in the log with the
// request id, through the server's middleware, which sets it.
func TestInternalError(t *testing.T) {
	f := newFixture(t, Options{})
	f.repos.Index = failingStore{f.st}
	srv, err := server.New(server.Options{Listen: "127.0.0.1:0", API: f.h, Log: f.repos.Log})
	if err != nil {
		t.Fatal(err)
	}
	for _, tt := range []struct{ method, target, body string }{
		{http.MethodGet, "/api/v1/repos", ""},
		{http.MethodPost, "/api/v1/repos", scratch},
		{http.MethodGet, "/api/v1/repos/raw/scratch", ""},
		{http.MethodPatch, "/api/v1/repos/raw/scratch", `{"anonymousRead":true}`},
		{http.MethodDelete, "/api/v1/repos/raw/scratch", ""},
	} {
		t.Run(tt.method+" "+tt.target, func(t *testing.T) {
			f.logs.Reset()
			r := httptest.NewRequest(tt.method, tt.target, strings.NewReader(tt.body))
			r.Header.Set("X-Request-Id", "req-500")
			rr := httptest.NewRecorder()
			srv.Handler().ServeHTTP(rr, r)
			wantError(t, rr, http.StatusInternalServerError, codeInternal, "Internal Server Error")
			if strings.Contains(rr.Body.String(), "db.internal") {
				t.Errorf("body %s carries the cause", rr.Body)
			}
			logs := f.logs.String()
			for _, want := range []string{`"msg":"api: request failed"`, `"requestId":"req-500"`, "db.internal"} {
				if !strings.Contains(logs, want) {
					t.Errorf("log does not contain %s:\n%s", want, logs)
				}
			}
		})
	}
}

// TestRequestIDInAudit checks that a change through the server records the request id.
func TestRequestIDInAudit(t *testing.T) {
	f := newFixture(t, Options{})
	srv, err := server.New(server.Options{Listen: "127.0.0.1:0", API: f.h, Log: f.repos.Log})
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest(http.MethodPost, "/api/v1/repos", strings.NewReader(scratch))
	r.Header.Set("X-Request-Id", "req-1")
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, r)
	wantStatus(t, rr, http.StatusCreated)
	if ev := oneAudit(t, f.st, "repo.create"); ev.details["requestId"] != "req-1" {
		t.Errorf("audit details = %v, want requestId req-1", ev.details)
	}
}

// TestClientAddrInAudit checks that a change through the server behind a trusted proxy records
// the client that X-Forwarded-For names, not the proxy (server.trustedProxies, ADR 0011), and
// the peer when the peer is not trusted.
func TestClientAddrInAudit(t *testing.T) {
	for _, tt := range []struct {
		name    string
		trusted []netip.Prefix
		want    string
	}{
		{"behind a trusted proxy", []netip.Prefix{netip.MustParsePrefix("192.0.2.0/24")}, "198.51.100.7"},
		{"no trusted proxy", nil, "192.0.2.1"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			f := newFixture(t, Options{})
			srv, err := server.New(server.Options{Listen: "127.0.0.1:0", API: f.h, Log: f.repos.Log, TrustedProxies: tt.trusted})
			if err != nil {
				t.Fatal(err)
			}
			r := httptest.NewRequest(http.MethodPost, "/api/v1/repos", strings.NewReader(scratch))
			r.RemoteAddr = "192.0.2.1:41000"
			r.Header.Set("X-Forwarded-For", "198.51.100.7")
			rr := httptest.NewRecorder()
			srv.Handler().ServeHTTP(rr, r)
			wantStatus(t, rr, http.StatusCreated)
			if ev := oneAudit(t, f.st, "repo.create"); ev.IP != tt.want {
				t.Errorf("audit ip = %q, want %q", ev.IP, tt.want)
			}
		})
	}
}

func TestWriteJSONFailure(t *testing.T) {
	f := newFixture(t, Options{})
	h := &handler{repos: f.repos, log: f.repos.Log}
	rr := httptest.NewRecorder()
	h.writeJSON(rr, httptest.NewRequest(http.MethodGet, "/api/v1/repos", nil), http.StatusOK, map[string]any{"c": make(chan int)})
	wantError(t, rr, http.StatusInternalServerError, codeInternal, "Internal Server Error")
	if !strings.Contains(f.logs.String(), "api: answer not encoded") {
		t.Errorf("log = %s, want the encoding failure", f.logs)
	}
}

func TestCodeOf(t *testing.T) {
	for status, want := range map[int]string{
		400: codeInvalid, 401: codeForbidden, 403: codeForbidden, 404: codeNotFound, 405: codeInvalid,
		409: codeConflict, 412: codeConflict, 413: codeInvalid, 500: codeInternal, 501: codeUnavailable, 502: codeUnavailable,
		503: codeUnavailable, 504: codeInternal,
	} {
		if got := codeOf(status); got != want {
			t.Errorf("codeOf(%d) = %s, want %s", status, got, want)
		}
	}
}

func TestNewWithoutRepos(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("New without Repos did not panic")
		}
	}()
	New(Options{})
}

func TestClientIP(t *testing.T) {
	for addr, want := range map[string]string{"192.0.2.1:1234": "192.0.2.1", "[2001:db8::1]:443": "2001:db8::1", "pipe": "pipe"} {
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		r.RemoteAddr = addr
		if got := clientIP(r); got != want {
			t.Errorf("clientIP(%q) = %q, want %q", addr, got, want)
		}
	}
}

// mustJSON returns v as JSON.
func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
