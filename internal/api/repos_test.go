package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/lajstrom/lajstrom/internal/auth"
	"github.com/lajstrom/lajstrom/internal/config"
	"github.com/lajstrom/lajstrom/internal/format"
	"github.com/lajstrom/lajstrom/internal/index"
)

// TestBootstrap is the bootstrap criterion of #14: two repos in the configuration, one of
// them in the control plane already, gives one created repo, and a warning for each field of
// the existing one that differs. A second run creates nothing.
func TestBootstrap(t *testing.T) {
	f := newFixture(t, Options{})
	ctx := context.Background()
	if _, err := f.repos.Create(ctx, CreateRequest{Format: "raw", Name: "scratch", Registry: "other"}, Origin{Source: SourceAPI}); err != nil {
		t.Fatal(err)
	}
	cfg := []config.Repo{
		{Name: "scratch", Format: "raw", Registry: "local", AnonymousRead: true},
		{Name: "internal", Format: "pypi", Registry: "local", AnonymousRead: true},
	}
	created, err := f.repos.Bootstrap(ctx, cfg)
	if err != nil || created != 1 {
		t.Fatalf("Bootstrap = %d, %v; want 1 created", created, err)
	}
	wantLogs(t, f.logs,
		`"level":"WARN","msg":"repo of the configuration differs from the control plane, and the control plane wins;`+
			` change it through the API or lajstrom admin repo","repo":"raw/scratch","field":"registry","config":"local","controlPlane":"other"`,
		`"repo":"raw/scratch","field":"anonymousRead","config":true,"controlPlane":false`,
		`"msg":"repo of the configuration created","repo":"pypi/internal","registry":"local"`,
	)
	wantRepo(t, f.repos, "raw/scratch other false") // unchanged: the control plane wins
	wantRepo(t, f.repos, "pypi/internal local true")

	rows := auditOf(t, f.st, "repo.create")
	if len(rows) != 2 || rows[0].Repo != "pypi/internal" {
		t.Fatalf("repo.create events = %+v, want the bootstrap's on top of the first", rows)
	}
	if ev := rows[0]; ev.Principal != "anonymous" || ev.IP != "" || ev.details["source"] != "config" {
		t.Errorf("bootstrap audit event = %+v, details %v; want anonymous, no IP, source config", ev.AuditEvent, ev.details)
	}

	if created, err := f.repos.Bootstrap(ctx, cfg); err != nil || created != 0 {
		t.Errorf("second Bootstrap = %d, %v; want nothing created", created, err)
	}
	if n := len(auditOf(t, f.st, "repo.create")); n != 2 {
		t.Errorf("%d repo.create events after the second run, want 2", n)
	}
}

// wantRepo checks a repo, given as "<format>/<name> <registry> <anonymousRead>", hosted.
func wantRepo(t *testing.T, s *Repos, want string) {
	t.Helper()
	id, _, _ := strings.Cut(want, " ")
	formatName, name, _ := strings.Cut(id, "/")
	r, err := s.Get(context.Background(), formatName, name)
	if err != nil {
		t.Fatal(err)
	}
	if got := fmt.Sprintf("%s/%s %s %t", r.Format, r.Name, r.Registry, r.AnonymousRead); got != want || r.Kind != "hosted" {
		t.Errorf("repo = %s (kind %s), want %s (hosted)", got, r.Kind, want)
	}
}

// racingStore answers every repo lookup with ErrNotFound, as if each repo was created by
// another replica between Bootstrap's lookup and its create.
type racingStore struct{ index.Store }

func (racingStore) GetRepo(_ context.Context, formatName, name string) (*index.Repo, error) {
	return nil, errors.Join(index.ErrNotFound, errors.New(formatName+"/"+name))
}

func TestBootstrapConcurrentCreate(t *testing.T) {
	f := newFixture(t, Options{})
	ctx := context.Background()
	f.create(t, "raw", "scratch")
	f.repos.Index = racingStore{f.st}
	created, err := f.repos.Bootstrap(ctx, []config.Repo{{Name: "scratch", Format: "raw", Registry: "local"}})
	if err != nil || created != 0 {
		t.Fatalf("Bootstrap = %d, %v; want the conflict counted as existing", created, err)
	}
	if !strings.Contains(f.logs.String(), "created meanwhile, by another replica") {
		t.Errorf("log = %s, want the conflict", f.logs)
	}
	if n := len(auditOf(t, f.st, "repo.create")); n != 1 {
		t.Errorf("%d repo.create events, want the first create's only", n)
	}
}

// Replicas that start together create every repo once.
func TestBootstrapReplicas(t *testing.T) {
	f := newFixture(t, Options{})
	cfg := []config.Repo{{Name: "a", Format: "raw", Registry: "local"}, {Name: "b", Format: "pypi", Registry: "local"}}
	var (
		wg    sync.WaitGroup
		mu    sync.Mutex
		total int
	)
	for range 4 {
		wg.Go(func() {
			created, err := f.repos.Bootstrap(context.Background(), cfg)
			if err != nil {
				t.Error(err)
			}
			mu.Lock()
			total += created
			mu.Unlock()
		})
	}
	wg.Wait()
	if total != 2 || len(auditOf(t, f.st, "repo.create")) != 2 {
		t.Errorf("replicas created %d repos with %d events, want 2 and 2", total, len(auditOf(t, f.st, "repo.create")))
	}
}

func TestBootstrapErrors(t *testing.T) {
	f := newFixture(t, Options{})
	created, err := f.repos.Bootstrap(context.Background(), []config.Repo{
		{Name: "scratch", Format: "raw", Registry: "local"},
		{Name: "ubuntu", Format: "apt", Registry: "local"},
	})
	if created != 1 || !errors.Is(err, format.ErrInvalid) || err == nil || err.Error() != `repos[1]: format: "apt" is not a format this server has` {
		t.Errorf("Bootstrap = %d, %v; want 1 created and the error of repos[1]", created, err)
	}

	f.repos.Index = failingStore{f.st}
	created, err = f.repos.Bootstrap(context.Background(), []config.Repo{{Name: "scratch", Format: "raw", Registry: "local"}})
	if created != 0 || !errors.Is(err, errDatabase) || !strings.Contains(err.Error(), "repos[0] raw/scratch") {
		t.Errorf("Bootstrap on a failing index = %d, %v", created, err)
	}
}

// recordingAudit is an audit.Writer that keeps the events, or fails with err.
type recordingAudit struct {
	events []index.AuditEvent
	err    error
}

func (a *recordingAudit) Write(_ context.Context, ev index.AuditEvent) error {
	a.events = append(a.events, ev)
	return a.err
}

// With an audit.Writer the events go to it, after the commit, and not to the index.
func TestAuditWriter(t *testing.T) {
	f := newFixture(t, Options{})
	w := &recordingAudit{}
	f.repos.Audit = w
	wantStatus(t, f.do(http.MethodPost, "/api/v1/repos", scratch, auth.Principal{}), http.StatusCreated)
	wantStatus(t, f.do(http.MethodPatch, "/api/v1/repos/raw/scratch", `{"anonymousRead":true}`, auth.Principal{}), http.StatusOK)
	wantStatus(t, f.do(http.MethodDelete, "/api/v1/repos/raw/scratch", "", auth.Principal{}), http.StatusNoContent)
	var actions []string
	for _, ev := range w.events {
		actions = append(actions, ev.Action+" "+ev.Repo+" "+ev.Principal)
	}
	if got := strings.Join(actions, ", "); got != "repo.create raw/scratch anonymous, repo.update raw/scratch anonymous, repo.delete raw/scratch anonymous" {
		t.Errorf("audit.Writer got %s", got)
	}
	if rows := auditOf(t, f.st, ""); len(rows) != 0 {
		t.Errorf("index has %d audit rows, want none", len(rows))
	}

	// A failing writer does not undo the change; the failure is logged.
	w.err = errors.New("audit: disk full")
	body := `{"format":"raw","name":"other","registry":"local"}`
	wantStatus(t, f.do(http.MethodPost, "/api/v1/repos", body, auth.Principal{}), http.StatusCreated)
	if !strings.Contains(f.logs.String(), `"msg":"api: audit event of a repo change not written","action":"repo.create","repo":"raw/other"`) {
		t.Errorf("log = %s, want the audit failure", f.logs)
	}
}

// The principal of a change is the request's.
func TestPrincipalInAudit(t *testing.T) {
	f := newFixture(t, Options{})
	ctx := auth.WithPrincipal(context.Background(), alice)
	if _, err := f.repos.Create(ctx, CreateRequest{Format: "raw", Name: "scratch", Registry: "local"}, Origin{Source: SourceCLI}); err != nil {
		t.Fatal(err)
	}
	ev := oneAudit(t, f.st, "repo.create")
	if ev.Principal != "user:alice" || ev.details["source"] != "cli" || ev.IP != "" {
		t.Errorf("audit event = %+v, details %v", ev.AuditEvent, ev.details)
	}
	if !ev.At.Equal(testNow) {
		t.Errorf("audit at %v, want %v", ev.At, testNow)
	}
}

// The zero Repos looks formats up in the format registry and knows no registry.
func TestReposDefaults(t *testing.T) {
	s := &Repos{Index: newStore(t)}
	_, err := s.Create(context.Background(), CreateRequest{Format: "raw", Name: "scratch", Registry: "local"}, Origin{})
	if !errors.Is(err, format.ErrInvalid) || !strings.Contains(err.Error(), "format:") {
		t.Errorf("Create without plugins = %v, want the format refused", err)
	}
	s.Formats = oneOf("raw")
	_, err = s.Create(context.Background(), CreateRequest{Format: "raw", Name: "scratch", Registry: "local"}, Origin{})
	if !errors.Is(err, format.ErrInvalid) || !strings.Contains(err.Error(), "registry:") {
		t.Errorf("Create without registries = %v, want the registry refused", err)
	}
	s.Registries = oneOf("local")
	repo, err := s.Create(context.Background(), CreateRequest{Format: "raw", Name: "scratch", Registry: "local"}, Origin{})
	if err != nil || repo.CreatedAt.IsZero() {
		t.Errorf("Create = %+v, %v; want a repo created now", repo, err)
	}
}

// oci needs no plugin: the facade serves its repos.
func TestCreateOCIRepo(t *testing.T) {
	s := &Repos{Index: newStore(t), Formats: oneOf("raw"), Registries: oneOf("local")}
	repo, err := s.Create(context.Background(), CreateRequest{Format: "oci", Name: "images", Registry: "local"}, Origin{})
	if err != nil || repo.Format != "oci" || repo.Name != "images" {
		t.Fatalf("Create oci/images = %+v, %v", repo, err)
	}
	if repos, err := s.List(context.Background(), "oci"); err != nil || len(repos) != 1 {
		t.Errorf("List(oci) = %+v, %v", repos, err)
	}
}

func TestSamePolicy(t *testing.T) {
	for _, tt := range []struct {
		a, b string
		want bool
	}{
		{"", "", true},
		{`{"a":1}`, "", false},
		{"", `{"a":1}`, false},
		{`{"a":1,"b":[1,2]}`, `{"b": [1, 2], "a": 1}`, true},
		{`{"a":1}`, `{"a":2}`, false},
		{`{"a":`, `{"a":1}`, false},
	} {
		if got := samePolicy(json.RawMessage(tt.a), json.RawMessage(tt.b)); got != tt.want {
			t.Errorf("samePolicy(%s, %s) = %v, want %v", tt.a, tt.b, got, tt.want)
		}
	}
}
