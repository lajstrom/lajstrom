package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/opencontainers/go-digest"

	"github.com/lajstrom/lajstrom/internal/auth"
	"github.com/lajstrom/lajstrom/internal/index"
	"github.com/lajstrom/lajstrom/internal/index/sqlite"
)

// memoryDBs numbers the in-memory databases, so that every test gets its own.
var memoryDBs atomic.Int64

// testNow is the clock of the tests: createdAt of every repo they create, in the past so that
// an update moves updatedAt.
var testNow = time.Date(2026, 1, 2, 12, 0, 0, 0, time.UTC)

// newStore returns a migrated in-memory SQLite index that is closed when the test ends.
func newStore(t *testing.T) *sqlite.Store {
	t.Helper()
	st, err := sqlite.Open(t.Context(), fmt.Sprintf("file:api-%d?mode=memory&cache=shared", memoryDBs.Add(1)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := st.Migrate(t.Context()); err != nil {
		t.Fatal(err)
	}
	return st
}

// oneOf returns a lookup that knows exactly names.
func oneOf(names ...string) func(string) bool {
	return func(n string) bool { return slices.Contains(names, n) }
}

// fixture is the API over a fresh index, with the formats raw and pypi and the registries local
// and other, logging JSON into logs.
type fixture struct {
	st    *sqlite.Store
	repos *Repos
	h     http.Handler
	logs  *bytes.Buffer
}

// newFixture returns a fixture whose handler is New(o) with the fixture's Repos and logger.
func newFixture(t *testing.T, o Options) *fixture {
	t.Helper()
	f := &fixture{st: newStore(t), logs: &bytes.Buffer{}}
	log := slog.New(slog.NewJSONHandler(f.logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	f.repos = &Repos{
		Index: f.st, Formats: oneOf("raw", "pypi"), Registries: oneOf("local", "other"),
		Now: func() time.Time { return testNow }, Log: log,
	}
	o.Repos, o.Log = f.repos, log
	f.h = recordStatuses(New(o))
	return f
}

// do sends a request with body ("" for none) through the handler as the principal p (the
// zero Principal is anonymous) and returns the answer.
func (f *fixture) do(method, target, body string, p auth.Principal) *httptest.ResponseRecorder {
	var r *http.Request
	if body == "" {
		r = httptest.NewRequest(method, target, nil)
	} else {
		r = httptest.NewRequest(method, target, strings.NewReader(body))
	}
	if p.Kind != "" {
		r = r.WithContext(auth.WithPrincipal(r.Context(), f.asStored(p)))
	}
	rr := httptest.NewRecorder()
	f.h.ServeHTTP(rr, r)
	return rr
}

// create creates format/name in the registry local through the service and returns it.
func (f *fixture) create(t *testing.T, formatName, name string) *index.Repo {
	t.Helper()
	ctx := context.Background()
	if _, err := f.repos.Create(ctx, CreateRequest{Format: formatName, Name: name, Registry: "local"}, Origin{}); err != nil {
		t.Fatalf("create %s/%s: %v", formatName, name, err)
	}
	row, err := f.st.GetRepo(ctx, formatName, name)
	if err != nil {
		t.Fatal(err)
	}
	return row
}

// addVersion adds a package with one version to a repo and returns the package.
func addVersion(t *testing.T, st index.Store, repoID int64, pkgName string) index.Package {
	t.Helper()
	ctx := context.Background()
	pkg := index.Package{RepoID: repoID, Name: pkgName, NameOCI: pkgName}
	if err := st.UpsertPackage(ctx, &pkg); err != nil {
		t.Fatal(err)
	}
	v := index.Version{PackageID: pkg.ID, Version: "0123456789abcdef", Tag: "0123456789abcdef",
		Digest: digest.FromString(pkgName), ArtifactType: "application/vnd.lajstrom.raw.v1"}
	if err := st.UpsertVersion(ctx, &v); err != nil {
		t.Fatal(err)
	}
	return pkg
}

// wantStatus fails the test when rr has another status.
func wantStatus(t *testing.T, rr *httptest.ResponseRecorder, status int) {
	t.Helper()
	if rr.Code != status {
		t.Fatalf("status = %d, want %d; body %s", rr.Code, status, rr.Body)
	}
}

// decodeJSON checks that rr is JSON with status and decodes its body into v.
func decodeJSON(t *testing.T, rr *httptest.ResponseRecorder, status int, v any) {
	t.Helper()
	wantStatus(t, rr, status)
	if ct := rr.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
	if err := json.Unmarshal(rr.Body.Bytes(), v); err != nil {
		t.Fatalf("body %q: %v", rr.Body, err)
	}
}

// wantError checks that rr is the error envelope with status and code and that its message
// contains each of substrs.
func wantError(t *testing.T, rr *httptest.ResponseRecorder, status int, code string, substrs ...string) {
	t.Helper()
	var env struct {
		Error envelopeError `json:"error"`
	}
	decodeJSON(t, rr, status, &env)
	if env.Error.Code != code {
		t.Errorf("code = %q, want %q (message %q)", env.Error.Code, code, env.Error.Message)
	}
	for _, s := range substrs {
		if !strings.Contains(env.Error.Message, s) {
			t.Errorf("message = %q, want it to contain %q", env.Error.Message, s)
		}
	}
}

// auditRow is an audit event with its details decoded.
type auditRow struct {
	index.AuditEvent
	details map[string]any
}

// auditOf returns the audit events of action, newest first.
func auditOf(t *testing.T, st index.Store, action string) []auditRow {
	t.Helper()
	evs, err := st.ListAudit(context.Background(), index.AuditFilter{Action: action})
	if err != nil {
		t.Fatal(err)
	}
	rows := make([]auditRow, len(evs))
	for i, ev := range evs {
		rows[i].AuditEvent = ev
		if err := json.Unmarshal(ev.Details, &rows[i].details); err != nil {
			t.Fatalf("details %s: %v", ev.Details, err)
		}
	}
	return rows
}

// oneAudit returns the one audit event of action and fails the test when there is not exactly
// one.
func oneAudit(t *testing.T, st index.Store, action string) auditRow {
	t.Helper()
	rows := auditOf(t, st, action)
	if len(rows) != 1 {
		t.Fatalf("%d %s audit events, want 1: %+v", len(rows), action, rows)
	}
	return rows[0]
}

// wantJSON checks that v marshals to the JSON of want, compared as values.
func wantJSON(t *testing.T, what string, v any, want string) {
	t.Helper()
	got, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	if !samePolicy(got, json.RawMessage(want)) {
		t.Errorf("%s = %s, want %s", what, got, want)
	}
}

// wantLogs checks that the log in buf contains each of wants.
func wantLogs(t *testing.T, buf *bytes.Buffer, wants ...string) {
	t.Helper()
	for _, want := range wants {
		if !strings.Contains(buf.String(), want) {
			t.Errorf("log does not contain %s:\n%s", want, buf)
		}
	}
}
