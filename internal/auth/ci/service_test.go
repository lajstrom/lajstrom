package ci

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/lajstrom/lajstrom/internal/auth"
	"github.com/lajstrom/lajstrom/internal/config"
	"github.com/lajstrom/lajstrom/internal/index"
	"github.com/lajstrom/lajstrom/internal/index/sqlite"
)

// serviceCI is auth.ci of the tests of the Service: a GitHub issuer and one policy of the
// configuration.
func serviceCI() config.CI {
	return config.CI{
		Issuers: []config.CIIssuer{{Name: "github", Platform: config.CIPlatformGitHub, Issuer: "https://token.actions.example"}},
		Policies: []config.CIPolicy{{Name: "from-config", Issuer: "github", Claims: map[string]config.CIValues{"repository_owner_id": {"9"}},
			Grants: []config.CIGrant{{Role: "reader", Mask: "*/*"}}}}}
}

func newStore(t *testing.T) *sqlite.Store {
	t.Helper()
	st, err := sqlite.Open(context.Background(), fmt.Sprintf("file:cisvc-%d?mode=memory&cache=shared", serviceDBs.Add(1)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := st.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	return st
}

var serviceDBs atomic.Int64

var admin = auth.Principal{Kind: auth.KindUser, Name: "019a6c3e-7b1e-7c3d-8e2f-0000000000ad"}

// readPolicy is a policy of the API of acme/app that reads.
func readPolicy(name string) Policy {
	return Policy{Name: name, Issuer: "github", Claims: map[string][]string{"repository_id": {"123"}},
		Grants: []index.CIGrant{{Role: "reader", Mask: "oci/*"}}}
}

// TestService: the policies of the API are made, changed and deleted with an audit row each that
// holds the policy, by the rules of the configuration's; the configuration's are listed and
// read-only; a policy that writes is refused (docs/spec/rbac.md §11).
func TestService(t *testing.T) {
	st := newStore(t)
	s := &Service{Index: st, Config: serviceCI()}
	ctx := auth.WithPrincipal(context.Background(), admin)
	o := auth.Origin{Source: "api", IP: "192.0.2.7", RequestID: "req-1"}
	p, err := s.Create(ctx, readPolicy("acme-read"), o)
	if err != nil || p.Source != SourceAPI || p.CreatedBy != admin.String() || p.CreatedAt == nil {
		t.Fatalf("Create = %+v, %v", p, err)
	}
	list, err := s.List(ctx)
	if err != nil || len(list) != 2 || list[0].Name != "from-config" || list[0].Source != SourceConfig || list[1].Name != "acme-read" {
		t.Fatalf("List = %+v, %v", list, err)
	}
	checkServiceRefusals(ctx, t, s, o)
	changed := readPolicy("")
	changed.Claims["ref"] = []string{"refs/heads/*"}
	if p, err := s.Update(ctx, "acme-read", changed, o); err != nil || p.Claims["ref"][0] != "refs/heads/*" {
		t.Fatalf("Update = %+v, %v", p, err)
	}
	if err := s.Delete(ctx, "acme-read", o); err != nil {
		t.Fatal(err)
	}
	checkPolicyRows(t, st)
}

// checkServiceRefusals checks the refusals of the Service s of TestService, which has acme-read.
func checkServiceRefusals(ctx context.Context, t *testing.T, s *Service, o auth.Origin) {
	t.Helper()
	for _, tt := range []struct {
		name string
		err  error
		f    func() error
	}{
		{"a policy that writes", auth.ErrScope, func() error {
			w := readPolicy("acme-write")
			w.Claims["ref"], w.Grants = []string{"refs/heads/main"}, []index.CIGrant{{Role: "writer", Mask: "oci/app"}}
			_, err := s.Create(ctx, w, o)
			return err
		}},
		{"no id", auth.ErrInvalid, func() error {
			_, err := s.Create(ctx, Policy{Name: "x", Issuer: "github", Claims: map[string][]string{"repository": {"acme/app"}},
				Grants: []index.CIGrant{{Role: "reader", Mask: "*/*"}}}, o)
			return err
		}},
		{"the name of the configuration's", index.ErrConflict, func() error { _, err := s.Create(ctx, readPolicy("from-config"), o); return err }},
		{"a name taken", index.ErrConflict, func() error { _, err := s.Create(ctx, readPolicy("acme-read"), o); return err }},
		{"changing the configuration's", index.ErrConflict, func() error {
			_, err := s.Update(ctx, "from-config", readPolicy(""), o)
			return err
		}},
		{"deleting the configuration's", index.ErrConflict, func() error { return s.Delete(ctx, "from-config", o) }},
		{"a rename", auth.ErrInvalid, func() error { _, err := s.Update(ctx, "acme-read", readPolicy("other"), o); return err }},
		{"changing none", index.ErrNotFound, func() error { _, err := s.Update(ctx, "none", readPolicy(""), o); return err }},
		{"deleting none", index.ErrNotFound, func() error { return s.Delete(ctx, "none", o) }},
	} {
		if err := tt.f(); !errors.Is(err, tt.err) {
			t.Errorf("%s: %v, want %v", tt.name, err, tt.err)
		}
	}
}

// checkPolicyRows checks the ci.policy.* rows of TestService: a create, a change with the previous
// policy, and a delete with the policy it deleted, each by the admin with the origin.
func checkPolicyRows(t *testing.T, st index.Store) {
	t.Helper()
	rows, err := st.ListAudit(context.Background(), index.AuditFilter{})
	if err != nil {
		t.Fatal(err)
	}
	var actions []string
	for _, r := range rows {
		if !strings.HasPrefix(r.Action, "ci.policy.") {
			continue
		}
		actions = append(actions, r.Action)
		var d map[string]any
		_ = json.Unmarshal(r.Details, &d)
		if r.Principal != admin.String() || r.Subject != "acme-read" || r.IP != "192.0.2.7" || d["source"] != "api" ||
			d["requestId"] != "req-1" || d["issuer"] != "github" || d["claims"] == nil || d["grants"] == nil {
			t.Errorf("%s row %+v details %v", r.Action, r, d)
		}
		if _, ok := d["previous"]; ok != (r.Action == ActionPolicyUpdate) {
			t.Errorf("%s row: previous %v", r.Action, d["previous"])
		}
	}
	if strings.Join(actions, " ") != "ci.policy.delete ci.policy.update ci.policy.create" {
		t.Errorf("ci.policy rows %v", actions)
	}
}

// TestLive: the policies of the API count beside the configuration's from the next read after a
// change, on another Live over the same index too; a row that writes, or that has a name of the
// configuration, is left out with a warning; an index that does not
// answer is an error.
func TestLive(t *testing.T) {
	st := newStore(t)
	ctx := auth.WithPrincipal(context.Background(), admin)
	var logs bytes.Buffer
	replica, err := NewLive(serviceCI(), st, slog.New(slog.NewTextHandler(&logs, nil)))
	if err != nil {
		t.Fatal(err)
	}
	names := func() []string {
		t.Helper()
		ps, err := replica.Current(ctx)
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for n := range ps.policies {
			out = append(out, n)
		}
		return slices.Sorted(slices.Values(out))
	}
	if got := names(); fmt.Sprint(got) != "[from-config]" {
		t.Errorf("before: %v", got)
	}
	s := &Service{Index: st, Config: serviceCI()}
	if _, err := s.Create(ctx, readPolicy("acme-read"), auth.Origin{Source: "api"}); err != nil {
		t.Fatal(err)
	}
	if got := names(); fmt.Sprint(got) != "[acme-read from-config]" {
		t.Errorf("after a create on another service: %v", got)
	}
	for _, r := range []index.CIPolicy{
		{Name: "restored-write", Issuer: "github", Claims: map[string][]string{"repository_id": {"1"}, "ref": {"main"}},
			Grants: []index.CIGrant{{Role: "writer", Mask: "*/*"}}, CreatedBy: "user:x"},
		{Name: "from-config", Issuer: "github", Claims: map[string][]string{"repository_id": {"1"}},
			Grants: []index.CIGrant{{Role: "reader", Mask: "*/*"}}, CreatedBy: "user:x"},
	} {
		if err := st.CreateCIPolicy(ctx, &r); err != nil {
			t.Fatal(err)
		}
	}
	if got := names(); fmt.Sprint(got) != "[acme-read from-config]" {
		t.Errorf("rows left out: %v", got)
	}
	for _, w := range []string{"policy=restored-write", "a policy of the API reads only", "a policy of the configuration has its name"} {
		if !strings.Contains(logs.String(), w) {
			t.Errorf("no warning with %q:\n%s", w, logs.String())
		}
	}
	if err := s.Delete(ctx, "acme-read", auth.Origin{Source: "api"}); err != nil {
		t.Fatal(err)
	}
	if got := names(); fmt.Sprint(got) != "[from-config]" {
		t.Errorf("after a delete: %v", got)
	}
	_ = st.Close()
	if _, err := replica.Current(ctx); err == nil {
		t.Error("Current with the index closed: no error")
	}
}
