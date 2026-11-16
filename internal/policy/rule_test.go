package policy

import (
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/lajstrom/lajstrom/internal/index"
)

// numeric orders versions of dotted integers, the order of the tests' "npm".
func numeric(a, b string) (int, error) {
	pa, pb := strings.Split(a, "."), strings.Split(b, ".")
	for i := 0; i < max(len(pa), len(pb)); i++ {
		var x, y int
		var err error
		if i < len(pa) {
			if x, err = strconv.Atoi(pa[i]); err != nil {
				return 0, err
			}
		}
		if i < len(pb) {
			if y, err = strconv.Atoi(pb[i]); err != nil {
				return 0, err
			}
		}
		if x != y {
			return x - y, nil
		}
	}
	return 0, nil
}

// orders gives npm the numeric order and every other format none.
func orders(format string) VersionOrder {
	if format == "npm" {
		return numeric
	}
	return nil
}

// TestParseRule: the fields docs/spec/policy.md §1.3 refuses, each named, and the rules it takes.
func TestParseRule(t *testing.T) {
	now := time.Date(2026, 11, 15, 10, 0, 0, 0, time.UTC)
	ok := index.PolicyRule{RepoMask: "pypi/proxy-pypi", Name: "requests", Version: "==2.32.0", Variant: "*", Effect: "deny",
		Reason: "CVE-2026-1234"}
	for _, tc := range []struct {
		name  string
		edit  func(r *index.PolicyRule)
		field string // "" accepted
	}{
		{"the rule of the issue", func(*index.PolicyRule) {}, ""},
		{"every version of every repo", func(r *index.PolicyRule) { r.RepoMask, r.Version = "*/*", "*" }, ""},
		{"a glob", func(r *index.PolicyRule) { r.Version = "==2.32.*" }, ""},
		{"several clauses", func(r *index.PolicyRule) { r.Version = "!=2.32.0, !=2.32.1" }, ""},
		{"a range on an ordered format", func(r *index.PolicyRule) { r.RepoMask, r.Version = "npm/*", ">=1.0, <2.0" }, ""},
		{"an allow", func(r *index.PolicyRule) { r.Effect = "allow" }, ""},
		{"an expiry ahead", func(r *index.PolicyRule) { r.ExpiresAt = ptr(now.Add(time.Hour)) }, ""},
		{"no mask", func(r *index.PolicyRule) { r.RepoMask = "pypi" }, "repoMask"},
		{"a format not served", func(r *index.PolicyRule) { r.RepoMask = "cargo/x" }, "repoMask"},
		{"a repo name that is none", func(r *index.PolicyRule) { r.RepoMask = "pypi/Bad Name" }, "repoMask"},
		{"no name", func(r *index.PolicyRule) { r.Name = "" }, "name"},
		{"a long name", func(r *index.PolicyRule) { r.Name = strings.Repeat("a", 257) }, "name"},
		{"a long variant", func(r *index.PolicyRule) { r.Variant = strings.Repeat("a", 257) }, "variant"},
		{"no version", func(r *index.PolicyRule) { r.Version = "" }, "version"},
		{"an empty clause", func(r *index.PolicyRule) { r.Version = "==1.0," }, "version"},
		{"a version with a space", func(r *index.PolicyRule) { r.Version = "==1 0" }, "version"},
		{"a glob in a range", func(r *index.PolicyRule) { r.RepoMask, r.Version = "npm/x", ">=1.*" }, "version"},
		{"a range without an order", func(r *index.PolicyRule) { r.Version = ">=2.0" }, "version"},
		{"a range over every format", func(r *index.PolicyRule) { r.RepoMask, r.Version = "*/x", "<2" }, "version"},
		{"another effect", func(r *index.PolicyRule) { r.Effect = "quarantine" }, "effect"},
		{"no reason", func(r *index.PolicyRule) { r.Reason = "" }, "reason"},
		{"a long reason", func(r *index.PolicyRule) { r.Reason = strings.Repeat("r", 513) }, "reason"},
		{"a control character", func(r *index.PolicyRule) { r.Reason = "line\nbreak" }, "reason"},
		{"an expiry passed", func(r *index.PolicyRule) { r.ExpiresAt = ptr(now) }, "expiresAt"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := ok
			tc.edit(&r)
			err := ParseRule(r, now, orders, func(f string) bool { return f != "cargo" })
			var re *RuleError
			switch {
			case tc.field == "" && err != nil:
				t.Errorf("refused: %v", err)
			case tc.field != "" && (!errors.As(err, &re) || re.Field != tc.field || !errors.Is(err, ErrInvalid)):
				t.Errorf("err %v, want a RuleError naming %s", err, tc.field)
			}
		})
	}
}

// TestConstraint: what a constraint matches, with an order and without.
func TestConstraint(t *testing.T) {
	for _, tc := range []struct {
		constraint string
		order      VersionOrder
		version    string
		want       bool
	}{
		{"*", nil, "anything", true},
		{"==2.32.0", nil, "2.32.0", true},
		{"2.32.0", nil, "2.32.0", true},
		{"==2.32.0", nil, "2.32.1", false},
		{"==2.32", nil, "2.32.0", false},
		{"==2.32", numeric, "2.32.0", true},
		{"==2.32.*", nil, "2.32.7", true},
		{"==2.32.*", nil, "2.33.0", false},
		{"!=2.32.0", nil, "2.32.1", true},
		{"!=2.32.0, !=2.32.1", nil, "2.32.1", false},
		{">=1.0, <2.0", numeric, "1.10", true},
		{">=1.0, <2.0", numeric, "2.0", false},
		{"<= 1.5", numeric, "1.5", true},
		{"> 1.5", numeric, "1.5", false},
		{">=1.0", nil, "1.0", false},
		{">=1.0", numeric, "not-a-version", false},
		{"==1.0", numeric, "not-a-version", false},
		{"==not-a-version", numeric, "not-a-version", true},
	} {
		c, err := parseConstraint(tc.constraint)
		if err != nil {
			t.Fatalf("%q: %v", tc.constraint, err)
		}
		if got := c.matches(tc.version, tc.order); got != tc.want {
			t.Errorf("%q matches %q (order %v): %v, want %v", tc.constraint, tc.version, tc.order != nil, got, tc.want)
		}
	}
	if !matchName("@Evil/*", "@evil/pkg") || matchName("@evil/*", "@good/pkg") || !matchName("*-internal", "a:b-INTERNAL") {
		t.Error("matchName: a glob of the name, ignoring ASCII case")
	}
}

func ptr[T any](v T) *T { return &v }
