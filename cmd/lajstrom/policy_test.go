package main

import (
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/lajstrom/lajstrom/internal/index"
	"github.com/lajstrom/lajstrom/internal/index/sqlite"
	"github.com/lajstrom/lajstrom/internal/policy"
)

// TestVersionOrder: every format of a package has the order of its versions for the ranges of
// a policy rule, raw has none.
func TestVersionOrder(t *testing.T) {
	order := versionOrder(lookupIn(servedPlugins("https://lajstrom.example", nil)))
	for _, name := range []string{"npm", "maven", "pypi", "apt", "rpm"} {
		if order(name) == nil {
			t.Errorf("%s has no order", name)
		}
	}
	if order("raw") != nil {
		t.Error("raw has an order")
	}
}

// TestVersionOrderRanges: a range rule on pypi, apt and rpm is taken and compares in the
// format's order, on versions whose bytes order them otherwise: 2.0rc1 before 2.0 and 10.0
// after it (PEP 440), 1.0~rc1-1 before 1.0-1 (dpkg), 1.9-1 and 1.10~rc1-1 before 1.10-1 and
// 1.10^git1-1 after it (rpm).
func TestVersionOrderRanges(t *testing.T) {
	ctx := t.Context()
	st, err := sqlite.Open(ctx, "file:"+filepath.Join(t.TempDir(), "lajstrom.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	if err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	order := versionOrder(lookupIn(servedPlugins("https://lajstrom.example", nil)))
	now := time.Now()
	for i, tt := range []struct {
		format, constraint string
		denied             map[string]bool
	}{
		{"pypi", "<2.0", map[string]bool{"1.10": true, "2.0rc1": true, "2.0.dev1": true, "2.0": false, "2.0.post1": false, "10.0": false}},
		{"pypi", ">=1.10,!=1.10.0", map[string]bool{"1.9": false, "1.10": false, "1.10.1": true, "2.0a1": true}},
		{"apt", ">=1.0-1", map[string]bool{"1.0~rc1-1": false, "0.9-1": false, "1.0-1": true, "1.0-1+deb12u1": true, "1:0.5-1": true}},
		{"rpm", "<1.10-1", map[string]bool{"1.9-1": true, "1.10~rc1-1": true, "1.10-1": false, "1.10^git1-1": false, "1:0.1-1": false}},
	} {
		repo := index.Repo{Format: tt.format, Name: fmt.Sprintf("libs%d", i), Registry: "local", Kind: index.KindHosted}
		if err := st.CreateRepo(ctx, &repo); err != nil {
			t.Fatal(err)
		}
		rule := index.PolicyRule{RepoMask: repo.Format + "/" + repo.Name, Name: "pkg", Version: tt.constraint, Variant: "*",
			Effect: index.PolicyDeny, Reason: "r", CreatedBy: "user:admin"}
		if err := policy.ParseRule(rule, now, order, func(string) bool { return true }); err != nil {
			t.Errorf("%s %s: %v", tt.format, tt.constraint, err)
			continue
		}
		if err := st.CreatePolicyRule(ctx, &rule); err != nil {
			t.Fatal(err)
		}
		d := policy.New(policy.Options{Index: st, Order: order})
		for version, want := range tt.denied {
			v, err := d.Decide(ctx, policy.Subject{Repo: repo, Name: "pkg", Version: version})
			if err != nil {
				t.Fatal(err)
			}
			if got := !v.Allowed(); got != want {
				t.Errorf("%s %s: %s denied %v, want %v", tt.format, tt.constraint, version, got, want)
			}
		}
	}
}
