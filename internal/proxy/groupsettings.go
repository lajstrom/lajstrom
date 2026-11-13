package proxy

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"slices"
	"strings"
)

// MaxMembers bounds the members of a group (docs/spec/proxy-and-group.md §2.3).
const MaxMembers = 16

// OCIFormat is the format of the pass-through repos, which has no plugin: its groups route each
// path to one member (§12) instead of merging their documents.
const OCIFormat = "oci"

// GroupSettings are a group's settings as the API takes and shows them (§2.3): its members by
// name, in order, and the names whose versions are merged even when the hosted member holds
// them, or, on a group of oci repos, its routes (§12.1). The index keeps Merge or Paths in
// repos.group and the members in group_members.
type GroupSettings struct {
	// Members are the names of the member repos, of the group's format, in order.
	Members []string
	// Merge are name patterns (§3): a name that one matches is not hidden by the hosted member.
	Merge []string
	// Paths are the routes of a group of oci repos: each member's name to the patterns of the
	// paths it serves (Route); nil on a group of another format.
	Paths map[string][]string
}

// groupJSON is the JSON form of GroupSettings, as repos.group keeps it.
type groupJSON struct {
	Members []string            `json:"members,omitempty"`
	Merge   []string            `json:"merge"`
	Paths   map[string][]string `json:"paths,omitempty"`
}

// groupInput is the JSON object ParseGroup reads: a key present with a value is not nil.
type groupInput struct {
	Members []string             `json:"members"`
	Merge   *[]string            `json:"merge"`
	Paths   *map[string][]string `json:"paths"`
}

// ParseGroup reads the JSON object of the settings of a group of formatName, {"members",
// "merge"}, or {"members", "paths"} for a group of oci repos, and checks it whole: 1 to
// MaxMembers members, each a name given once; at most MaxPatterns merge patterns, each as the
// patterns of a proxy's lists are; an oci group's routes as checkRoutes says, and no merge; a
// group of another format no paths. A key it does not know is refused. Errors are
// *SettingsError and name the field, group.<key>; whether the members exist, and are of the
// group's format and of kinds a member may have, is the caller's to check.
func ParseGroup(formatName string, raw json.RawMessage) (GroupSettings, error) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || raw[0] != '{' {
		return GroupSettings{}, problem("group", "must be a JSON object")
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var j groupInput
	if err := dec.Decode(&j); err != nil {
		return GroupSettings{}, groupDecodeError(err)
	}
	if dec.Decode(new(any)) != io.EOF {
		return GroupSettings{}, problem("group", "must be one JSON object, with nothing after it")
	}
	g := GroupSettings{Members: j.Members}
	if err := checkGroupMembers(g.Members); err != nil {
		return GroupSettings{}, err
	}
	if formatName == OCIFormat {
		switch {
		case j.Merge != nil:
			return GroupSettings{}, problem("group.merge", "a group of oci repos merges nothing: each path is served by the one member group.paths routes it to")
		case j.Paths == nil || len(*j.Paths) == 0:
			return GroupSettings{}, problem("group.paths", "must give each member the patterns of the paths it serves")
		}
		g.Paths = *j.Paths
		return g, checkRoutes(g.Members, g.Paths)
	}
	if j.Paths != nil {
		return GroupSettings{}, problem("group.paths", "only a group of oci repos has routes; a group of %s merges its members (group.merge)", formatName)
	}
	if j.Merge != nil {
		g.Merge = *j.Merge
	}
	return g, checkPatterns("group.merge", g.Merge)
}

// checkGroupMembers checks the members of a group: 1 to MaxMembers names, none empty, none twice.
func checkGroupMembers(members []string) error {
	switch {
	case len(members) == 0:
		return problem("group.members", "must name 1 to %d repos of the group's format", MaxMembers)
	case len(members) > MaxMembers:
		return problem("group.members", "has %d repos, at most %d are allowed", len(members), MaxMembers)
	}
	seen := make(map[string]int, len(members))
	for i, m := range members {
		field := fmt.Sprintf("group.members[%d]", i)
		if m == "" {
			return problem(field, "must not be empty")
		}
		if first, dup := seen[m]; dup {
			return problem(field, "%q is already group.members[%d]", m, first)
		}
		seen[m] = i
	}
	return nil
}

// pathGrammar is the grammar of the path below <format>/<repo>/ of an OCI name (distribution
// spec, "Pulling manifests"), which a route's <path> and <prefix> follow.
var pathGrammar = regexp.MustCompile(`^[a-z0-9]+((\.|_|__|-+)[a-z0-9]+)*(/[a-z0-9]+((\.|_|__|-+)[a-z0-9]+)*)*$`)

// checkRoutes checks the routes of a group of oci repos whose members are members (§12.1): each
// member has at least one pattern and every key is a member; a pattern is "*", "<path>" or
// "<prefix>/*", at most MaxPatternLen bytes; none is twice in one list, nor on two members, so
// that Route always has one most specific pattern; at most MaxPatterns in all. Nested patterns
// are allowed. Errors name the field, group.paths.<member>[<i>].
func checkRoutes(members []string, paths map[string][]string) error {
	keys := make([]string, 0, len(paths))
	for k := range paths {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	for _, k := range keys {
		if !slices.Contains(members, k) {
			return problem("group.paths."+k, "%q is not one of group.members", k)
		}
	}
	total := 0
	type place struct {
		member string
		i      int
	}
	first := map[string]place{}
	for _, m := range members {
		list := paths[m]
		if len(list) == 0 {
			return problem("group.paths."+m, "must name at least one pattern: each member serves the paths of its patterns")
		}
		if total += len(list); total > MaxPatterns {
			return problem("group.paths", "has more than %d patterns in all", MaxPatterns)
		}
		for i, p := range list {
			field := fmt.Sprintf("group.paths.%s[%d]", m, i)
			if err := checkRoute(p); err != nil {
				return problem(field, "%q %s", p, err.Error())
			}
			switch prev, dup := first[p]; {
			case dup && prev.member == m:
				return problem(field, "%q is already group.paths.%s[%d]", p, m, prev.i)
			case dup:
				return problem(field, "%q is also group.paths.%s[%d]: a path is served by one member", p, prev.member, prev.i)
			}
			first[p] = place{m, i}
		}
	}
	return nil
}

// checkRoute checks one route pattern: "*", "<path>" or "<prefix>/*".
func checkRoute(p string) error {
	if p == "*" {
		return nil
	}
	path := strings.TrimSuffix(p, "/*")
	switch {
	case len(p) > MaxPatternLen:
		return fmt.Errorf("is longer than %d bytes", MaxPatternLen)
	case !pathGrammar.MatchString(path):
		return errors.New(`is not a route: "*", a path of the OCI name grammar, or such a path and "/*"`)
	}
	return nil
}

// Route returns the member whose pattern matching path is the most specific (§12.2): the path
// itself, then the longest "<prefix>/*" whose prefix is a component prefix of path, then "*".
// False when no pattern matches. The routes are those checkRoutes took, so no two members tie.
func Route(paths map[string][]string, path string) (string, bool) {
	best, score := "", -1
	for m, list := range paths {
		for _, p := range list {
			s := -1
			switch prefix, sub := strings.CutSuffix(p, "/*"); {
			case p == path:
				s = len(path) + 1
			case p == "*":
				s = 0
			case sub && strings.HasPrefix(path, prefix+"/"):
				s = len(prefix)
			}
			if s > score {
				best, score = m, s
			}
		}
	}
	return best, score >= 0
}

// groupDecodeError turns an error of the JSON decoder into a *SettingsError.
func groupDecodeError(err error) error {
	var typeErr *json.UnmarshalTypeError
	switch msg := err.Error(); {
	case errors.As(err, &typeErr) && typeErr.Field != "":
		return problem("group."+typeErr.Field, "must not be a JSON %s", typeErr.Value)
	case strings.HasPrefix(msg, "json: unknown field "):
		return problem("group", "%s is not a key: the keys are members, and merge, or paths on a group of oci repos",
			strings.TrimPrefix(msg, "json: unknown field "))
	default:
		return problem("group", "must be a JSON object: %s", strings.TrimPrefix(msg, "json: "))
	}
}

// JSON returns g as the API shows it: members and merge, or members and paths on a group of oci
// repos.
func (g GroupSettings) JSON() json.RawMessage {
	members := g.Members
	if members == nil {
		members = []string{}
	}
	var b []byte
	if g.Paths != nil {
		b, _ = json.Marshal(struct {
			Members []string            `json:"members"`
			Paths   map[string][]string `json:"paths"`
		}{members, g.Paths}) // strings always marshal
		return b
	}
	merge := g.Merge
	if merge == nil {
		merge = []string{}
	}
	b, _ = json.Marshal(struct {
		Members []string `json:"members"`
		Merge   []string `json:"merge"`
	}{members, merge}) // strings always marshal
	return b
}

// Stored returns what repos.group keeps of g: {"merge"}, or {"paths"} on a group of oci repos,
// the members being rows of their own.
func (g GroupSettings) Stored() json.RawMessage {
	var b []byte
	if g.Paths != nil {
		b, _ = json.Marshal(struct {
			Paths map[string][]string `json:"paths"`
		}{g.Paths}) // strings always marshal
		return b
	}
	merge := g.Merge
	if merge == nil {
		merge = []string{}
	}
	b, _ = json.Marshal(groupJSON{Merge: merge}) // strings always marshal
	return b
}

// StoredMerge reads the merge patterns of repos.group as Stored wrote it; a column that does not
// read has none.
func StoredMerge(raw json.RawMessage) []string {
	var j groupJSON
	if len(raw) == 0 || json.Unmarshal(raw, &j) != nil {
		return nil
	}
	return j.Merge
}

// StoredPaths reads the routes of a group of oci repos from repos.group as Stored wrote them; a
// column that does not read has none, and routes nothing.
func StoredPaths(raw json.RawMessage) map[string][]string {
	var j groupJSON
	if len(raw) == 0 || json.Unmarshal(raw, &j) != nil {
		return nil
	}
	return j.Paths
}

// Merged reports whether a pattern of merge matches the name key (§3, §8.2).
func Merged(merge []string, key string) bool {
	for _, p := range merge {
		if Match(p, key) {
			return true
		}
	}
	return false
}
