package auth

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/lajstrom/lajstrom/internal/index"
	"github.com/lajstrom/lajstrom/internal/index/sqlite"
)

// newIndex returns a migrated in-memory SQLite index.
func newIndex(t testing.TB) *sqlite.Store {
	t.Helper()
	st, err := sqlite.Open(context.Background(), fmt.Sprintf("file:rbac-%d?mode=memory&cache=shared", memoryDBs.Add(1)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := st.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	return st
}

// addRepo creates the repo format/name, readable anonymously when anon is set.
func addRepo(t testing.TB, st index.Writer, format, name string, anon bool) index.Repo {
	t.Helper()
	r := index.Repo{Format: format, Name: name, Registry: "local", Prefix: format + "/" + name, Kind: "hosted", AnonymousRead: anon}
	if err := st.CreateRepo(context.Background(), &r); err != nil {
		t.Fatal(err)
	}
	return r
}

// bind creates the binding (principal, role, mask) without checking it, as rows written before
// the bindings service checked them would be.
func bind(t testing.TB, st index.Writer, principal, role, mask string) index.Binding {
	t.Helper()
	b := index.Binding{Principal: principal, Role: role, RepoMask: mask}
	if err := st.CreateBinding(context.Background(), &b); err != nil {
		t.Fatal(err)
	}
	return b
}

func user(name string) Principal { return Principal{Kind: KindUser, Name: name} }

// longGroup is a group name one byte too long: its binding grants nothing.
var longGroup = strings.Repeat("g", MaxGroupNameLen+1)

// The credentials of the table: deploy tokens 7 (raw/private, read and write) and 8
// (raw/public, read), 9 over the mask pypi/* (read) and 10 over */payments (read and write),
// and personal access tokens.
var (
	deploy7    = Credential{Kind: CredDeploy, TokenID: 7, Repo: "raw/private", Scopes: []string{"read", "write"}}
	deploy8    = Credential{Kind: CredDeploy, TokenID: 8, Repo: "raw/public", Scopes: []string{"read"}}
	mask9      = Credential{Kind: CredDeploy, TokenID: 9, Mask: "pypi/*", Scopes: []string{"read"}}
	mask10     = Credential{Kind: CredDeploy, TokenID: 10, Mask: "*/payments", Scopes: []string{"read", "write"}}
	patRead    = Credential{Kind: CredPAT, TokenID: 20, Scopes: []string{"read"}}
	patWrite   = Credential{Kind: CredPAT, TokenID: 21, Scopes: []string{"write"}}
	patAll     = Credential{Kind: CredPAT, TokenID: 22}
	password   = Credential{Kind: CredPassword}
	token7     = Principal{Kind: KindToken, Name: "7"}
	token8     = Principal{Kind: KindToken, Name: "8"}
	token9     = Principal{Kind: KindToken, Name: "9"}
	token10    = Principal{Kind: KindToken, Name: "10"}
	allActions = []Action{ActionRead, ActionWrite, ActionDelete, ActionManage}
)

// rbacFixture is the index of TestRBACRules: repos, valid bindings, and bindings that grant
// nothing.
func rbacFixture(t *testing.T) *sqlite.Store {
	t.Helper()
	st := newIndex(t)
	addRepo(t, st, "raw", "private", false)
	addRepo(t, st, "raw", "public", true)
	addRepo(t, st, "pypi", "internal", false)
	addRepo(t, st, "pypi", "payments", false)
	addRepo(t, st, "raw", "payments", false)
	addRepo(t, st, "apt", "ubuntu", false)
	for _, b := range [][3]string{
		{"user:root", "admin", "*/*"},
		{"user:alice", "reader", "raw/private"},
		{"user:alice", "writer", "pypi/*"},
		{"user:bob", "reader", "pypi/*"},
		{"user:carol", "maintainer", "*/payments"},
		{"user:dave", "writer", "raw/private"},
		{"user:dave", "reader", "*/*"},
		{"user:erin", "maintainer", "*/*"},
		// Granting nothing: not user:<name>, an unknown role, a mask of one segment.
		{"anonymous", "reader", "*/*"},
		{"token:8", "writer", "*/*"},
		{"group:" + longGroup, "reader", "*/*"},
		{"user:frank", "owner", "raw/private"},
		{"user:frank", "reader", "raw"},
	} {
		bind(t, st, b[0], b[1], b[2])
	}
	return st
}

// TestRBACRules is the table of docs/spec/rbac.md §3: every step, the masks, the union of a
// principal's bindings, personal access tokens that narrow their user, deploy tokens, anonymous
// read with both switches, the system resources, and bindings that grant nothing.
func TestRBACRules(t *testing.T) {
	st := rbacFixture(t)
	var logs bytes.Buffer
	log := slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	open := NewRBAC(RBACOptions{Index: st, AnonymousRead: true, Log: log})
	closed := NewRBAC(RBACOptions{Index: st, AnonymousRead: false, Log: log})

	tests := []struct {
		name    string
		p       Principal
		c       Credential
		actions []Action
		repo    string // format/name
		want    bool
		closed  bool // evaluate with auth.anonymousRead off
	}{
		// 1. admin.
		{"admin: every action on a repo", user("root"), password, allActions, "raw/private", true, false},
		{"admin: a repo that does not exist", user("root"), password, allActions, "maven/missing", true, false},
		{"admin: the system", user("root"), password, []Action{ActionManage}, "*/*", true, false},
		{"admin with a read PAT: read", user("root"), patRead, []Action{ActionRead}, "raw/private", true, false},
		{"admin with a read PAT: no write, delete, manage", user("root"), patRead, allActions[1:], "raw/private", false, false},
		{"admin with a read PAT: not the system", user("root"), patRead, []Action{ActionManage}, "*/*", false, false},
		{"admin with a PAT without scopes: the system", user("root"), patAll, []Action{ActionManage}, "*/*", true, false},

		// 2. deploy tokens.
		{"deploy token: read and write on its repo", token7, deploy7, allActions[:2], "raw/private", true, false},
		{"deploy token: never delete or manage", token7, deploy7, allActions[2:], "raw/private", false, false},
		{"deploy token: nothing on another repo", token7, deploy7, allActions, "pypi/internal", false, false},
		{"deploy token: not another repo readable by anonymous", token7, deploy7, []Action{ActionRead}, "raw/public", false, false},
		{"deploy token with read: no write", token8, deploy8, []Action{ActionWrite}, "raw/public", false, false},
		{"deploy token with read: read", token8, deploy8, []Action{ActionRead}, "raw/public", true, false},
		{"deploy token: a binding of it grants nothing", token8, deploy8, []Action{ActionWrite}, "raw/public", false, false},
		{"deploy token: another token's credential", token8, deploy7, allActions, "raw/private", false, false},
		{"deploy token: without its credential", token7, Credential{}, allActions, "raw/private", false, false},
		{"deploy token: not the system", token7, deploy7, []Action{ActionManage}, "*/*", false, false},
		{"mask token: read on a repo of its mask", token9, mask9, []Action{ActionRead}, "pypi/internal", true, false},
		{"mask token: read on another repo of its mask", token9, mask9, []Action{ActionRead}, "pypi/payments", true, false},
		{"mask token: read on a repo created later", token9, mask9, []Action{ActionRead}, "pypi/later", true, false},
		{"mask token with read: no write, delete, manage", token9, mask9, allActions[1:], "pypi/internal", false, false},
		{"mask token: nothing outside its mask", token9, mask9, allActions, "raw/private", false, false},
		{"mask token: not a repo outside its mask readable by anonymous", token9, mask9, []Action{ActionRead}, "raw/public", false, false},
		{"mask token over a name: read and write in every format", token10, mask10, allActions[:2], "raw/payments", true, false},
		{"mask token over a name: the other format", token10, mask10, allActions[:2], "pypi/payments", true, false},
		{"mask token over a name: never delete or manage", token10, mask10, allActions[2:], "pypi/payments", false, false},
		{"mask token over a name: not another name", token10, mask10, allActions, "pypi/internal", false, false},
		{"mask token: another token's credential", token9, mask10, allActions, "pypi/payments", false, false},
		{"mask token: without its credential", token9, Credential{}, allActions, "pypi/internal", false, false},
		{"mask token: not the system", token10, Credential{Kind: CredDeploy, TokenID: 10, Mask: "*/*", Scopes: []string{"read", "write"}},
			allActions, "*/*", false, false},

		// 3. personal access tokens.
		{"PAT without scopes: its user", user("alice"), patAll, []Action{ActionWrite}, "pypi/internal", true, false},
		{"PAT with read: read as its user", user("alice"), patRead, []Action{ActionRead}, "pypi/internal", true, false},
		{"PAT with read: no write its user has", user("alice"), patRead, []Action{ActionWrite}, "pypi/internal", false, false},
		{"PAT with write: write its user has", user("alice"), patWrite, []Action{ActionWrite}, "pypi/internal", true, false},
		{"PAT with write: no read", user("alice"), patWrite, []Action{ActionRead}, "pypi/internal", false, false},
		{"PAT with write: nothing its user lacks", user("bob"), patWrite, []Action{ActionWrite}, "pypi/internal", false, false},

		// 4. bindings and masks.
		{"exact mask: reader reads", user("alice"), password, []Action{ActionRead}, "raw/private", true, false},
		{"exact mask: reader does not write", user("alice"), password, allActions[1:], "raw/private", false, false},
		{"exact mask: not another repo of the format", user("alice"), password, allActions, "raw/payments", false, false},
		{"format-wide mask: writer", user("alice"), password, allActions[:2], "pypi/payments", true, false},
		{"format-wide mask: not delete", user("alice"), password, allActions[2:], "pypi/payments", false, false},
		{"format-wide mask: a repo that does not exist yet", user("bob"), password, []Action{ActionRead}, "pypi/later", true, false},
		{"format-wide mask: not another format", user("bob"), password, allActions, "raw/private", false, false},
		{"name across formats: maintainer", user("carol"), password, allActions, "raw/payments", true, false},
		{"name across formats: the other format", user("carol"), password, allActions, "pypi/payments", true, false},
		{"name across formats: not another name", user("carol"), password, allActions, "pypi/internal", false, false},
		{"union: writer on one repo, reader everywhere", user("dave"), password, allActions[:2], "raw/private", true, false},
		{"union: reader on the others", user("dave"), password, []Action{ActionRead}, "apt/ubuntu", true, false},
		{"union: no write on the others", user("dave"), password, []Action{ActionWrite}, "apt/ubuntu", false, false},
		{"maintainer everywhere: every action", user("erin"), password, allActions, "apt/ubuntu", true, false},
		{"maintainer everywhere: not the system", user("erin"), password, []Action{ActionManage}, "*/*", false, false},
		{"reader everywhere: not the system", user("dave"), password, []Action{ActionRead}, "*/*", false, false},
		{"no binding: nothing", user("zoe"), password, allActions, "raw/private", false, false},
		{"unknown role and one-segment mask: nothing", user("frank"), password, allActions, "raw/private", false, false},
		{"a group is no principal of a request", Principal{Kind: KindGroup, Name: "dev"}, Credential{}, allActions, "raw/private", false, false},
		{"unknown action", user("root"), password, []Action{"approve"}, "raw/private", false, false},

		// 5. anonymous read.
		{"anonymous: read where both switches allow", Anonymous, Credential{}, []Action{ActionRead}, "raw/public", true, false},
		{"anonymous: nothing else there", Anonymous, Credential{}, allActions[1:], "raw/public", false, false},
		{"anonymous: not a private repo, its binding grants nothing", Anonymous, Credential{}, allActions, "raw/private", false, false},
		{"anonymous: not a repo that does not exist", Anonymous, Credential{}, allActions, "raw/missing", false, false},
		{"anonymous: not with auth.anonymousRead off", Anonymous, Credential{}, []Action{ActionRead}, "raw/public", false, true},
		{"anonymous: not the system", Anonymous, Credential{}, []Action{ActionRead}, "*/*", false, false},
		{"a user without bindings reads a public repo", user("zoe"), password, []Action{ActionRead}, "raw/public", true, false},
		{"a user without bindings: not with the switch off", user("zoe"), password, []Action{ActionRead}, "raw/public", false, true},
		{"a PAT with write: no anonymous read either", user("zoe"), patWrite, []Action{ActionRead}, "raw/public", false, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := open
			if tt.closed {
				r = closed
			}
			ctx := WithCredential(context.Background(), tt.c)
			format, name, _ := strings.Cut(tt.repo, "/")
			for _, a := range tt.actions {
				got, err := r.Allowed(ctx, tt.p, a, format, name)
				if err != nil {
					t.Fatal(err)
				}
				if got != tt.want {
					t.Errorf("Allowed(%s, %s, %s) = %v, want %v", tt.p, a, tt.repo, got, tt.want)
				}
			}
		})
	}
	for _, principal := range []string{"anonymous", "token:8", "group:" + longGroup, "user:frank"} {
		if !strings.Contains(logs.String(), "binding grants nothing") || !strings.Contains(logs.String(), "principal="+principal) {
			t.Errorf("no warning about the binding of %s:\n%s", principal, logs.String())
		}
	}
}

// countingIndex counts the reads of auth_version and of the bindings, and fails them on demand.
type countingIndex struct {
	index.Reader
	versions, loads atomic.Int64
	fail            atomic.Bool
}

var errIndexDown = errors.New("index down")

func (c *countingIndex) AuthVersion(ctx context.Context) (int64, error) {
	c.versions.Add(1)
	if c.fail.Load() {
		return 0, errIndexDown
	}
	return c.Reader.AuthVersion(ctx)
}

func (c *countingIndex) ListBindings(ctx context.Context, principal string) ([]index.Binding, error) {
	c.loads.Add(1)
	return c.Reader.ListBindings(ctx, principal)
}

// TestRBACNextRequest: a binding created or deleted and a repo's anonymousRead changed count
// from the next evaluation on, and the snapshot is read again only when auth_version moved.
func TestRBACNextRequest(t *testing.T) {
	ctx := context.Background()
	st := newIndex(t)
	repo := addRepo(t, st, "raw", "assets", false)
	idx := &countingIndex{Reader: st}
	r := NewRBAC(RBACOptions{Index: idx, AnonymousRead: true})
	check := func(what string, p Principal, a Action, want bool) {
		t.Helper()
		got, err := r.Allowed(ctx, p, a, "raw", "assets")
		if err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Errorf("%s: Allowed(%s, %s) = %v, want %v", what, p, a, got, want)
		}
	}
	check("before the binding", user("alice"), ActionWrite, false)
	b := bind(t, st, "user:alice", "writer", "raw/assets")
	check("the next request after the binding", user("alice"), ActionWrite, true)
	check("again", user("alice"), ActionWrite, true)
	if n := idx.loads.Load(); n != 2 {
		t.Errorf("%d loads for two versions, want 2", n)
	}
	if err := st.DeleteBinding(ctx, b.ID); err != nil {
		t.Fatal(err)
	}
	check("the next request after the delete", user("alice"), ActionWrite, false)

	check("anonymous before anonymousRead", Anonymous, ActionRead, false)
	repo.AnonymousRead = true
	if err := st.UpdateRepo(ctx, &repo); err != nil {
		t.Fatal(err)
	}
	check("anonymous after anonymousRead", Anonymous, ActionRead, true)
	if err := st.DeleteRepo(ctx, repo.ID); err != nil {
		t.Fatal(err)
	}
	check("anonymous after the repo is deleted", Anonymous, ActionRead, false)
	if v, l := idx.versions.Load(), idx.loads.Load(); v != 7+5 || l != 5 {
		t.Errorf("%d reads of auth_version and %d loads, want 12 (one per evaluation, one per load) and 5", v, l)
	}
}

// TestRBACEvaluationScope: the evaluations of one request share one read of auth_version and
// one snapshot; the next request sees what changed in between.
func TestRBACEvaluationScope(t *testing.T) {
	st := newIndex(t)
	addRepo(t, st, "raw", "assets", false)
	idx := &countingIndex{Reader: st}
	r := NewRBAC(RBACOptions{Index: idx})
	allowed := func(ctx context.Context) bool {
		t.Helper()
		ok, err := r.Allowed(ctx, user("alice"), ActionRead, "raw", "assets")
		if err != nil {
			t.Fatal(err)
		}
		return ok
	}

	req := WithEvaluationScope(context.Background())
	for range 5 {
		if allowed(req) {
			t.Fatal("alice reads without a binding")
		}
	}
	bind(t, st, "user:alice", "reader", "raw/assets")
	if allowed(req) {
		t.Error("the binding counted within the request that was being evaluated")
	}
	if v, l := idx.versions.Load(), idx.loads.Load(); v != 2 || l != 1 {
		t.Errorf("one request of six evaluations: %d reads of auth_version, %d loads; want 2 (one, and one by the load) and 1", v, l)
	}

	if !allowed(WithEvaluationScope(context.Background())) {
		t.Error("the next request does not see the binding")
	}
	before := idx.loads.Load()
	allowed(WithEvaluationScope(context.Background()))
	if idx.loads.Load() != before {
		t.Error("a request with auth_version unchanged reloaded the snapshot")
	}
}

// TestRBACIndexDown: when auth_version cannot be read the evaluation is an error, not a
// decision, and the scope keeps none.
func TestRBACIndexDown(t *testing.T) {
	st := newIndex(t)
	bind(t, st, "user:root", "admin", "*/*")
	idx := &countingIndex{Reader: st}
	r := NewRBAC(RBACOptions{Index: idx})
	idx.fail.Store(true)
	req := WithEvaluationScope(context.Background())
	if ok, err := r.Allowed(req, user("root"), ActionRead, "raw", "assets"); !errors.Is(err, errIndexDown) || ok {
		t.Fatalf("Allowed with the index down = %v, %v; want an error wrapping %v", ok, err, errIndexDown)
	}
	idx.fail.Store(false)
	if ok, err := r.Allowed(req, user("root"), ActionRead, "raw", "assets"); err != nil || !ok {
		t.Errorf("Allowed once the index answers = %v, %v; want true", ok, err)
	}
}

func TestParseMask(t *testing.T) {
	for _, tt := range []struct {
		mask, format, repo string
	}{
		{"apt/ubuntu-internal", "apt", "ubuntu-internal"},
		{"apt/*", "apt", "*"},
		{"*/payments", "*", "payments"},
		{"*/*", "*", "*"},
		{"raw", "", ""},
		{"raw/", "", ""},
		{"/x", "", ""},
		{"raw/a/b", "", ""},
		{"", "", ""},
	} {
		format, repo, err := ParseMask(tt.mask)
		if format != tt.format || repo != tt.repo || (err != nil) != (tt.format == "") {
			t.Errorf("ParseMask(%q) = %q, %q, %v; want %q, %q", tt.mask, format, repo, err, tt.format, tt.repo)
		}
	}
}

// TestMaskMatches is the matching of a mask, a binding's or a deploy token's (docs/spec/rbac.md
// §3): whole segments, "*" for any; a malformed mask matches nothing, and no mask matches the
// system resources.
func TestMaskMatches(t *testing.T) {
	for _, tt := range []struct {
		mask, format, name string
		want               bool
	}{
		{"oci/*", "oci", "ghcr", true},
		{"oci/*", "oci", "k8s", true},
		{"oci/*", "raw", "ghcr", false},
		{"*/talos", "oci", "talos", true},
		{"*/talos", "raw", "talos", true},
		{"*/talos", "raw", "talos-boot", false},
		{"*/talos", "oci", "tal", false},
		{"oci/k8s", "oci", "k8s", true},
		{"oci/k8s", "oci", "k8s2", false},
		{"oci/k8s", "oci", "k", false},
		{"*/*", "apt", "ubuntu", true},
		{"o*/*", "oci", "ghcr", false}, // a wildcard is a whole segment, never part of one
		{"oci/g*", "oci", "ghcr", false},
		{"*/*", System, System, false},
		{"*/*", "oci", System, false},
		{"*/*", System, "ghcr", false},
		{"*/*", "", "ghcr", false},
		{"*/*", "oci", "", false},
		{"oci", "oci", "ghcr", false},
		{"oci/ghcr/x", "oci", "ghcr", false},
		{"", "oci", "ghcr", false},
	} {
		if got := MaskMatches(tt.mask, tt.format, tt.name); got != tt.want {
			t.Errorf("MaskMatches(%q, %q, %q) = %v, want %v", tt.mask, tt.format, tt.name, got, tt.want)
		}
	}
}

// rbacBench is an evaluator over 1 000 bindings: 100 users with 10 bindings each, of every kind
// of mask, over 50 repos.
func rbacBench(b *testing.B) (*RBAC, []Principal, []repoKey) {
	b.Helper()
	st := newIndex(b)
	formats := []string{"raw", "pypi", "apt", "maven", "npm"}
	var repos []repoKey
	for i := range 50 {
		f := formats[i%len(formats)]
		name := fmt.Sprintf("repo%02d", i)
		addRepo(b, st, f, name, i%10 == 0)
		repos = append(repos, repoKey{f, name})
	}
	roles := []string{RoleReader, RoleWriter, RoleMaintainer}
	var users []Principal
	err := st.Tx(context.Background(), func(w index.Writer) error {
		for u := range 100 {
			users = append(users, user(fmt.Sprintf("u%03d", u)))
			for k := range 10 {
				repo := repos[(u*7+k*3)%len(repos)]
				mask := []string{repo.format + "/" + repo.name, repo.format + "/*", "*/" + repo.name, repo.format + "/" + repo.name}[k%4]
				if err := w.CreateBinding(context.Background(), &index.Binding{
					Principal: users[u].String(), Role: roles[k%len(roles)], RepoMask: mask,
				}); err != nil {
					return err
				}
			}
		}
		return nil
	})
	if err != nil {
		b.Fatal(err)
	}
	if all, err := st.ListBindings(context.Background(), ""); err != nil || len(all) != 1000 {
		b.Fatalf("%d bindings, %v; want 1000", len(all), err)
	}
	return NewRBAC(RBACOptions{Index: st, AnonymousRead: true, Log: slog.New(slog.DiscardHandler)}), users, repos
}

// BenchmarkRBACAllowed is the evaluation itself, within one request's scope: issue #45 asks for
// 100 000 a second on a laptop with 1 000 bindings.
func BenchmarkRBACAllowed(b *testing.B) {
	r, users, repos := rbacBench(b)
	ctx := WithEvaluationScope(context.Background())
	if _, err := r.Allowed(ctx, users[0], ActionRead, "raw", "repo00"); err != nil {
		b.Fatal(err)
	}
	for i := 0; b.Loop(); i++ {
		repo := repos[i%len(repos)]
		if _, err := r.Allowed(ctx, users[i%len(users)], allActions[i%len(allActions)], repo.format, repo.name); err != nil {
			b.Fatal(err)
		}
	}
	b.ReportMetric(float64(b.N)/b.Elapsed().Seconds(), "evaluations/s")
}

// BenchmarkRBACRequest is a request: its read of auth_version (SQLite in memory here; a round
// trip to Postgres in production) and one evaluation.
func BenchmarkRBACRequest(b *testing.B) {
	r, users, repos := rbacBench(b)
	for i := 0; b.Loop(); i++ {
		repo := repos[i%len(repos)]
		ctx := WithEvaluationScope(context.Background())
		if _, err := r.Allowed(ctx, users[i%len(users)], ActionRead, repo.format, repo.name); err != nil {
			b.Fatal(err)
		}
	}
	b.ReportMetric(float64(b.N)/b.Elapsed().Seconds(), "requests/s")
}
