package profile

import (
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// dashRun is a run of two or more dashes, which ADR 0017 escapes.
var dashRun = regexp.MustCompile(`-{2,}`)

// fullName is the grammar of ADR 0017 for a whole OCI name: Gitea's and Forgejo's.
var fullName = regexp.MustCompile(`^[a-z0-9]+(?:[._-][a-z0-9]+)*(?:/[a-z0-9]+(?:[._-][a-z0-9]+)*)*$`)

// escapeLast escapes the last byte of e.
func escapeLast(e string) string { return e[:len(e)-1] + fmt.Sprintf("_%02x", e[len(e)-1]) }

// v2Component is EscapeComponent of ADR 0017 before its amendment, written out: the dash runs
// and Zot's words escaped, Harbor's not.
func v2Component(s string) string {
	e := Escape(lowerASCII(s))
	if !specComponentGrammar.MatchString(e) {
		return hashedComponent(s)
	}
	e = dashRun.ReplaceAllStringFunc(e, func(r string) string { return strings.Repeat("_2d", len(r)) })
	if strings.HasSuffix(e, "blobs") || strings.HasSuffix(e, ".uploads") || strings.HasSuffix(e, ".sync") ||
		e == "index.json" || e == "oci-layout" {
		return escapeLast(e)
	}
	return e
}

// v3Component is EscapeComponent now, written out: v2Component and Harbor's two words.
func v3Component(s string) string {
	if c := v2Component(s); c != "manifests" && c != "referrers" {
		return c
	}
	return escapeLast(lowerASCII(s))
}

// formsOf returns the package path of the name of components now (the oracle of PackagePath
// over EscapeComponent), before the amendment of ADR 0017, and before ADR 0017.
func formsOf(components []string) (now, v2, v1 string) {
	n, two, one := make([]string, len(components)), make([]string, len(components)), make([]string, len(components))
	for i, c := range components {
		n[i], two[i], one[i] = v3Component(c), v2Component(c), v1Component(c)
	}
	for i := range len(n) - 1 {
		if n[i] == "tags" && strings.HasPrefix(n[i+1], "list") {
			n[i] = "tag_73"
		}
	}
	return strings.Join(n, "/"), strings.Join(two, "/"), strings.Join(one, "/")
}

// earlier is what EarlierPackagePaths must give for the forms now, v2, v1.
func earlier(now, v2, v1 string) []string {
	var out []string
	if v2 != now {
		out = append(out, v2)
	}
	if v1 != v2 {
		out = append(out, v1)
	}
	return out
}

func TestPackagePath(t *testing.T) {
	for _, tc := range []struct {
		components []string
		want       string
	}{
		{[]string{"x", "tags", "list"}, "x/tag_73/list"},
		{[]string{"tags", "listing.txt"}, "tag_73/listing.txt"},
		{[]string{"tags", "tags", "list"}, "tags/tag_73/list"},
		{[]string{"tags", "list", "tags", "list"}, "tag_73/list/tag_73/list"},
		{[]string{"tags"}, "tags"},
		{[]string{"x", "tags"}, "x/tags"},
		{[]string{"x", "tags", "other"}, "x/tags/other"},
		{[]string{"x", "mytags", "list"}, "x/mytags/list"},
		{[]string{"list"}, "list"},
		{[]string{"tags", hashedComponent(".list")}, "tags/" + hashedComponent(".list")},
		{nil, ""},
	} {
		if got := PackagePath(tc.components...); got != tc.want {
			t.Errorf("PackagePath(%q) = %q, want %q", tc.components, got, tc.want)
		}
	}
	// The examples of the amendment, as the plugins escape them.
	for path, want := range map[string]string{
		"k8s/manifests/app.yaml": "k8s/manifest_73/app.yaml",
		"x/referrers/y":          "x/referrer_73/y",
		"x/tags/list":            "x/tag_73/list",
		"X/Tags/Listing.TXT":     "x/tag_73/listing.txt",
		"k8s/manifest/app.yaml":  "k8s/manifest/app.yaml",
	} {
		if got := escapePath(path); got != want {
			t.Errorf("the package path of %q = %q, want %q", path, got, want)
		}
	}
}

// escapePath is PackagePath over EscapeComponent of the components of a /-separated path.
func escapePath(path string) string {
	parts := strings.Split(path, "/")
	for i, c := range parts {
		parts[i] = EscapeComponent(c)
	}
	return PackagePath(parts...)
}

// TestEarlierPackagePaths: every kind of name whose package path ADR 0017 or its amendment
// changes, with its earlier forms, newest first; a path in an earlier form, and a path that no
// writer produces, has none.
func TestEarlierPackagePaths(t *testing.T) {
	git := hashedComponent(".gitignore")
	gitV1 := "h0__" + git[4:]
	for _, tc := range []struct {
		path string
		want []string
	}{
		{"k8s/manifests/app.yaml", []string{"k8s/manifests/app.yaml"}},
		{"x/referrers/y", []string{"x/referrers/y"}},
		{"x/tags/list", []string{"x/tags/list"}},
		{"x/tags/listing.txt", []string{"x/tags/listing.txt"}},
		{"netlify/blobs", []string{"netlify/blobs"}},
		{"site/.gitignore", []string{"site/" + gitV1}},
		{"k8s/manifests/.gitignore", []string{"k8s/manifests/" + git, "k8s/manifests/" + gitV1}},
		{"a--b/tags/list.sync", []string{"a_2d_2db/tags/list.syn_63", "a--b/tags/list.sync"}},
		{"docs/readme.md", nil},
		{"x/tags/other", nil},
	} {
		p := escapePath(tc.path)
		if got := EarlierPackagePaths(p); !slices.Equal(got, tc.want) {
			t.Errorf("EarlierPackagePaths(%q), of %s, = %q, want %q", p, tc.path, got, tc.want)
		}
		last := p
		if len(tc.want) > 0 {
			last = tc.want[len(tc.want)-1]
		}
		if got := LegacyPackagePath(p); got != last {
			t.Errorf("LegacyPackagePath(%q) = %q, want %q", p, got, last)
		}
		for _, f := range tc.want {
			if got := EarlierPackagePaths(f); len(got) != 0 && slices.Contains(got, p) {
				t.Errorf("EarlierPackagePaths(%q), an earlier form, = %q", f, got)
			}
		}
	}
	for _, p := range []string{"x/tag_73/other", "manifest_73/x/manifests", "x/tag_73", "x/manifest_74"} {
		if got := EarlierPackagePaths(p); len(got) != 0 {
			t.Errorf("EarlierPackagePaths(%q), no writer's, = %q", p, got)
		}
	}
}

// TestPackagePathForms: from any form of a package path, every form, the newest first; a path
// that no writer produces has itself only.
func TestPackagePathForms(t *testing.T) {
	for _, path := range []string{"k8s/manifests/app.yaml", "x/referrers/y", "x/tags/list", "x/tags/listing.txt",
		"netlify/blobs", "site/.gitignore", "k8s/manifests/.gitignore", "a--b/tags/list.sync", "Docs/Readme.MD",
		"tags/tags/listing.txt", "cache/my.sync", "x/index.json", "libstdc++6", "com.example/lib-core"} {
		p := escapePath(path)
		want := append([]string{p}, EarlierPackagePaths(p)...)
		for _, f := range want {
			if got := PackagePathForms(f); !slices.Equal(got, want) {
				t.Errorf("PackagePathForms(%q), a form of %s, = %q, want %q", f, path, got, want)
			}
		}
	}
	for _, p := range []string{"x/tag_73/other", "manifest_73/x/manifests", "x/tag_73", "x/manifest_74", "a/b_2d/c"} {
		if got := PackagePathForms(p); !slices.Equal(got, []string{p}) {
			t.Errorf("PackagePathForms(%q), no writer's, = %q, want itself", p, got)
		}
	}
}

// TestUnescapePackagePath: a name decodes the same from its package path now and from each
// earlier form; a hashed component, and a path that is no form of any name, does not decode.
func TestUnescapePackagePath(t *testing.T) {
	for _, path := range []string{"k8s/manifests/app.yaml", "x/referrers/y", "x/tags/list", "tags/tags/listing.txt",
		"netlify/blobs", "a--b/tags/list.sync", "Docs/Readme.MD", "manifests"} {
		components := strings.Split(lowerASCII(path), "/")
		p := escapePath(path)
		for _, f := range append([]string{p}, EarlierPackagePaths(p)...) {
			if got, err := UnescapePackagePath(f); err != nil || !slices.Equal(got, components) {
				t.Errorf("UnescapePackagePath(%q), a form of %s, = %q, %v", f, path, got, err)
			}
		}
	}
	for _, p := range []string{"x/" + hashedComponent(".x"), "x/tag_73/other", "x/tag_73", "blobs/manifest_73", "a/_2ex", "a//b"} {
		if got, err := UnescapePackagePath(p); !errors.Is(err, ErrInvalidEscape) {
			t.Errorf("UnescapePackagePath(%q) = %q, %v, want ErrInvalidEscape", p, got, err)
		}
	}
}

// TestPackagePathInjective: over every path of up to three components of names that meet the
// rules of ADR 0017 and its amendment, or look like their marks, no package path, in any form,
// belongs to two names.
func TestPackagePathInjective(t *testing.T) {
	names := []string{"tags", "Tags", "list", "listx", "manifests", "referrers", "x", "blobs", "a--b", ".g", "tag_73",
		"manifest_73", "tag-73", "h0_habc"}
	seen := map[string]string{}
	add := func(p, name string) {
		if other, ok := seen[p]; ok && other != name {
			t.Errorf("%q is a package path of %q and of %q", p, other, name)
		}
		seen[p] = name
	}
	var walk func(prefix []string)
	walk = func(prefix []string) {
		if len(prefix) > 0 {
			keys := make([]string, len(prefix))
			for i, n := range prefix {
				keys[i] = lowerASCII(n)
				if IsHashedComponent(EscapeComponent(n)) { // of the spelling, not the lower case
					keys[i] = n
				}
			}
			key := strings.Join(keys, "/")
			now, v2, v1 := formsOf(prefix)
			add(now, key)
			add(v2, key)
			add(v1, key)
		}
		if len(prefix) == 3 {
			return
		}
		for _, n := range names {
			walk(append(slices.Clone(prefix), n))
		}
	}
	walk(nil)
}

// checkPackagePath asserts the properties of PackagePath over EscapeComponent for the
// /-separated path s, against the oracles formsOf and earlier: the grammar of ADR 0017 for the
// whole name, no reserved word and no "tags" before a "list…", the earlier forms, and the round
// trip from each form. It is shared by FuzzEscape.
func checkPackagePath(t *testing.T, s string) {
	t.Helper()
	components := strings.Split(s, "/")
	p := escapePath(s)
	now, v2, v1 := formsOf(components)
	switch {
	case p != now:
		t.Fatalf("the package path of %q = %q, want %q", s, p, now)
	case !fullName.MatchString("raw/r/" + p):
		t.Fatalf("the package path of %q = %q does not match the grammar of ADR 0017", s, p)
	case strings.Contains("/"+p, "/tags/list"):
		t.Fatalf("the package path of %q = %q has tags before list", s, p)
	case !slices.Equal(EarlierPackagePaths(p), earlier(now, v2, v1)):
		t.Fatalf("EarlierPackagePaths(%q) = %q, want %q", p, EarlierPackagePaths(p), earlier(now, v2, v1))
	}
	for _, c := range strings.Split(p, "/") {
		if ReservedComponent(c) {
			t.Fatalf("the package path of %q = %q has the reserved component %q", s, p, c)
		}
	}
	for _, c := range components {
		if IsHashedComponent(EscapeComponent(c)) {
			return // no name to decode
		}
	}
	for _, f := range append([]string{p}, EarlierPackagePaths(p)...) {
		if got, err := UnescapePackagePath(f); err != nil || strings.Join(got, "/") != lowerASCII(s) {
			t.Fatalf("UnescapePackagePath(%q), a form of %q, = %q, %v", f, s, got, err)
		}
	}
}

// checkUnescapePackagePath checks that UnescapePackagePath decodes a package path of any form,
// and nothing else: a string it decodes to components is a form of their package path (formsOf),
// so no package path belongs to two names; any other is ErrInvalidEscape.
func checkUnescapePackagePath(t *testing.T, s string) {
	t.Helper()
	got, err := UnescapePackagePath(s)
	if err != nil {
		if !errors.Is(err, ErrInvalidEscape) {
			t.Fatalf("UnescapePackagePath(%q) = %v, want ErrInvalidEscape", s, err)
		}
		return
	}
	if now, v2, v1 := formsOf(got); s != now && s != v2 && s != v1 {
		t.Fatalf("UnescapePackagePath(%q) = %q, whose package paths are %q, %q and %q", s, got, now, v2, v1)
	}
}
