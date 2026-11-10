package proxy

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
)

// MaxMembers bounds the members of a group (docs/spec/proxy-and-group.md §2.3).
const MaxMembers = 16

// GroupSettings are a group's settings as the API takes and shows them (§2.3): its members by
// name, in order, and the names whose versions are merged even when the hosted member holds
// them. The index keeps Merge in repos.group and the members in group_members.
type GroupSettings struct {
	// Members are the names of the member repos, of the group's format, in order.
	Members []string
	// Merge are name patterns (§3): a name that one matches is not hidden by the hosted member.
	Merge []string
}

// groupJSON is the JSON form of GroupSettings.
type groupJSON struct {
	Members []string `json:"members,omitempty"`
	Merge   []string `json:"merge"`
}

// ParseGroup reads the JSON object of a group's settings, {"members", "merge"}, and checks it
// whole: 1 to MaxMembers members, each a name given once; at most MaxPatterns merge patterns,
// each as the patterns of a proxy's lists are. A key it does not know is refused. Errors are
// *SettingsError and name the field, group.<key>; whether the members exist, and are of the
// group's format and of kinds a member may have, is the caller's to check.
func ParseGroup(raw json.RawMessage) (GroupSettings, error) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || raw[0] != '{' {
		return GroupSettings{}, problem("group", "must be a JSON object")
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var j groupJSON
	if err := dec.Decode(&j); err != nil {
		return GroupSettings{}, groupDecodeError(err)
	}
	if dec.Decode(new(any)) != io.EOF {
		return GroupSettings{}, problem("group", "must be one JSON object, with nothing after it")
	}
	g := GroupSettings(j)
	switch {
	case len(g.Members) == 0:
		return GroupSettings{}, problem("group.members", "must name 1 to %d repos of the group's format", MaxMembers)
	case len(g.Members) > MaxMembers:
		return GroupSettings{}, problem("group.members", "has %d repos, at most %d are allowed", len(g.Members), MaxMembers)
	}
	seen := make(map[string]int, len(g.Members))
	for i, m := range g.Members {
		field := fmt.Sprintf("group.members[%d]", i)
		if m == "" {
			return GroupSettings{}, problem(field, "must not be empty")
		}
		if first, dup := seen[m]; dup {
			return GroupSettings{}, problem(field, "%q is already group.members[%d]", m, first)
		}
		seen[m] = i
	}
	return g, checkPatterns("group.merge", g.Merge)
}

// groupDecodeError turns an error of the JSON decoder into a *SettingsError.
func groupDecodeError(err error) error {
	var typeErr *json.UnmarshalTypeError
	switch msg := err.Error(); {
	case errors.As(err, &typeErr) && typeErr.Field != "":
		return problem("group."+typeErr.Field, "must not be a JSON %s", typeErr.Value)
	case strings.HasPrefix(msg, "json: unknown field "):
		return problem("group", "%s is not a key: the keys are members and merge", strings.TrimPrefix(msg, "json: unknown field "))
	default:
		return problem("group", "must be a JSON object: %s", strings.TrimPrefix(msg, "json: "))
	}
}

// JSON returns g as the API shows it, with both keys.
func (g GroupSettings) JSON() json.RawMessage {
	members, merge := g.Members, g.Merge
	if members == nil {
		members = []string{}
	}
	if merge == nil {
		merge = []string{}
	}
	b, _ := json.Marshal(struct {
		Members []string `json:"members"`
		Merge   []string `json:"merge"`
	}{members, merge}) // strings always marshal
	return b
}

// Stored returns what repos.group keeps of g: {"merge"}, the members being rows of their own.
func (g GroupSettings) Stored() json.RawMessage {
	merge := g.Merge
	if merge == nil {
		merge = []string{}
	}
	b, _ := json.Marshal(groupJSON{Merge: merge}) // strings always marshal
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

// Merged reports whether a pattern of merge matches the name key (§3, §8.2).
func Merged(merge []string, key string) bool {
	for _, p := range merge {
		if Match(p, key) {
			return true
		}
	}
	return false
}
