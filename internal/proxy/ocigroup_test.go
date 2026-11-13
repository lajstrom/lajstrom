package proxy

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/lajstrom/lajstrom/internal/format"
)

// TestParseOCIGroup: a group of oci repos has routes instead of merge patterns, each member at
// least one, of three forms, none on two members; nested patterns are allowed, and a group of
// another format has no routes (docs/spec/proxy-and-group.md §12.1).
func TestParseOCIGroup(t *testing.T) {
	g, err := ParseGroup(OCIFormat, []byte(`{"members": ["internal", "hub", "ghcr"],
		"paths": {"internal": ["acme/*", "acme"], "ghcr": ["oras-project/*"], "hub": ["*", "acme/public/*"]}}`))
	if err != nil {
		t.Fatal(err)
	}
	want := GroupSettings{Members: []string{"internal", "hub", "ghcr"}, Paths: map[string][]string{
		"internal": {"acme/*", "acme"}, "ghcr": {"oras-project/*"}, "hub": {"*", "acme/public/*"}}}
	if d := cmp.Diff(want, g); d != "" {
		t.Errorf("ParseGroup (-want +got):\n%s", d)
	}
	if got := string(g.JSON()); got != `{"members":["internal","hub","ghcr"],"paths":{"ghcr":["oras-project/*"],"hub":["*","acme/public/*"],"internal":["acme/*","acme"]}}` {
		t.Errorf("JSON = %s", got)
	}
	stored := g.Stored()
	if got := string(stored); got != `{"paths":{"ghcr":["oras-project/*"],"hub":["*","acme/public/*"],"internal":["acme/*","acme"]}}` {
		t.Errorf("Stored = %s", got)
	}
	if d := cmp.Diff(want.Paths, StoredPaths(stored)); d != "" || StoredMerge(stored) != nil {
		t.Errorf("StoredPaths (-want +got):\n%s; merge %v", d, StoredMerge(stored))
	}
	if _, err := ParseGroup("npm", []byte(`{"members": ["internal"], "paths": {"internal": ["*"]}}`)); err == nil ||
		!strings.Contains(err.Error(), "group.paths: only a group of oci repos has routes") {
		t.Errorf("paths on an npm group = %v", err)
	}
	if g, err := ParseGroup("npm", []byte(`{"members": ["internal"]}`)); err != nil || g.Paths != nil ||
		string(g.Stored()) != `{"merge":[]}` || string(g.JSON()) != `{"members":["internal"],"merge":[]}` {
		t.Errorf("an npm group = %+v, %v: stored %s", g, err, g.Stored())
	}
}

// TestParseOCIGroupRefused: the routes ParseGroup refuses, each naming its field (§12.1).
func TestParseOCIGroupRefused(t *testing.T) {
	many := `"p0"`
	for i := 1; i <= MaxPatterns; i++ {
		many += fmt.Sprintf(`, "p%d"`, i)
	}
	const two = `"members": ["internal", "hub"]`
	for _, tt := range []struct{ name, raw, want string }{
		{"no paths", `{` + two + `}`, "group.paths: must give each member the patterns"},
		{"empty paths", `{` + two + `, "paths": {}}`, "group.paths: must give each member the patterns"},
		{"merge", `{` + two + `, "merge": [], "paths": {"internal": ["*"], "hub": ["a"]}}`, "group.merge: a group of oci repos merges nothing"},
		{"a member without patterns", `{` + two + `, "paths": {"internal": ["acme/*"]}}`, "group.paths.hub: must name at least one pattern"},
		{"an empty list", `{` + two + `, "paths": {"internal": ["acme/*"], "hub": []}}`, "group.paths.hub: must name at least one pattern"},
		{"a key that is no member", `{` + two + `, "paths": {"internal": ["a"], "hub": ["*"], "ghcr": ["b"]}}`,
			`group.paths.ghcr: "ghcr" is not one of group.members`},
		{"the same pattern on two members", `{` + two + `, "paths": {"internal": ["*"], "hub": ["*"]}}`,
			`group.paths.hub[0]: "*" is also group.paths.internal[0]: a path is served by one member`},
		{"the same path on two members", `{` + two + `, "paths": {"internal": ["acme/*", "acme/api"], "hub": ["*", "acme/api"]}}`,
			`group.paths.hub[1]: "acme/api" is also group.paths.internal[1]`},
		{"a pattern twice", `{` + two + `, "paths": {"internal": ["acme/*", "acme/*"], "hub": ["*"]}}`,
			`group.paths.internal[1]: "acme/*" is already group.paths.internal[0]`},
		{"a star inside", `{` + two + `, "paths": {"internal": ["acme/*/api"], "hub": ["*"]}}`, `group.paths.internal[0]: "acme/*/api" is not a route`},
		{"a star in a component", `{` + two + `, "paths": {"internal": ["acme-*"], "hub": ["*"]}}`, `group.paths.internal[0]: "acme-*" is not a route`},
		{"upper case", `{` + two + `, "paths": {"internal": ["Acme/*"], "hub": ["*"]}}`, `group.paths.internal[0]: "Acme/*" is not a route`},
		{"a slash first", `{` + two + `, "paths": {"internal": ["/*"], "hub": ["*"]}}`, `group.paths.internal[0]: "/*" is not a route`},
		{"empty", `{` + two + `, "paths": {"internal": [""], "hub": ["*"]}}`, `group.paths.internal[0]: "" is not a route`},
		{"too many", `{` + two + `, "paths": {"internal": [` + many + `], "hub": ["*"]}}`, "group.paths: has more than 256 patterns"},
		{"too long", `{` + two + `, "paths": {"internal": ["` + strings.Repeat("a", MaxPatternLen) + `/*"], "hub": ["*"]}}`,
			"is longer than 256 bytes"},
		{"not a list", `{` + two + `, "paths": {"internal": "*", "hub": ["*"]}}`, "group.paths"},
		{"an unknown key", `{` + two + `, "paths": {"internal": ["*"], "hub": ["a"]}, "order": 1}`, `"order" is not a key`},
	} {
		t.Run(tt.name, func(t *testing.T) {
			_, err := ParseGroup(OCIFormat, []byte(tt.raw))
			var se *SettingsError
			if !errors.As(err, &se) || !errors.Is(err, format.ErrInvalid) || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("ParseGroup = %v, want a *SettingsError with %q", err, tt.want)
			}
		})
	}
}

// TestRoute: the most specific pattern wins, the path itself, then the longest prefix, then *;
// a prefix matches below itself at a component boundary, never the prefix alone (§12.2).
func TestRoute(t *testing.T) {
	paths := map[string][]string{
		"internal": {"acme/*", "acme"},
		"public":   {"acme/public/*"},
		"pinned":   {"acme/public/old"},
		"ghcr":     {"oras-project/*"},
		"hub":      {"*"},
	}
	for path, want := range map[string]string{
		"acme/api":              "internal",
		"acme":                  "internal",
		"acme/tools/lint":       "internal",
		"acme/public/web":       "public",
		"acme/public/web/x":     "public",
		"acme/public":           "internal",
		"acme/public/old":       "pinned",
		"acme/public/old/x":     "public",
		"acmecorp/api":          "hub",
		"oras-project/oras":     "ghcr",
		"oras-project":          "hub",
		"library/nginx":         "hub",
		"nginx":                 "hub",
		"oras-project/oras/sub": "ghcr",
	} {
		if got, ok := Route(paths, path); !ok || got != want {
			t.Errorf("Route(%q) = %q, %v; want %q", path, got, ok, want)
		}
	}
	narrow := map[string][]string{"internal": {"acme/*"}, "pinned": {"tools/lint"}}
	for _, path := range []string{"acme", "tools", "tools/lint/x", "library/nginx"} {
		if got, ok := Route(narrow, path); ok {
			t.Errorf("Route(%q) = %q, want no route", path, got)
		}
	}
	if got, ok := Route(narrow, "tools/lint"); !ok || got != "pinned" {
		t.Errorf("Route(tools/lint) = %q, %v", got, ok)
	}
}
