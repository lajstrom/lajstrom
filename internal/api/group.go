package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/lajstrom/lajstrom/internal/config"
	"github.com/lajstrom/lajstrom/internal/format"
	"github.com/lajstrom/lajstrom/internal/index"
	"github.com/lajstrom/lajstrom/internal/proxy"
)

// Group repos (ADR 0013, docs/spec/proxy-and-group.md §2.3 to §2.5): the settings, the members
// and the checks that the API, lajstrom admin repo and the config bootstrap share.

// checkGroup checks the group of a create: a format with group repos, no registry and no
// policy, and settings that proxy.ParseGroup takes. The members are checked against the index
// in the transaction of the create (checkMembers).
func (s *Repos) checkGroup(req CreateRequest) (proxy.GroupSettings, error) {
	switch {
	case !s.grouper(req.Format):
		return proxy.GroupSettings{}, invalid("kind: format %s has no group repos", req.Format)
	case req.Registry != "":
		return proxy.GroupSettings{}, invalid("registry: a group has none; its members' registries hold what it serves")
	case !isNull(req.Policy):
		return proxy.GroupSettings{}, invalid("policy: a group has none; its members' policies apply")
	case isNull(req.Group):
		return proxy.GroupSettings{}, invalid("group: a group repo must name its members")
	}
	g, err := proxy.ParseGroup(req.Group)
	if err != nil {
		return proxy.GroupSettings{}, &clientError{msg: err.Error(), err: err} // the message names the field
	}
	return g, nil
}

// grouper reports whether the plugin of formatName has group repos (format.Grouper).
func (s *Repos) grouper(formatName string) bool {
	if s.Groupers != nil {
		return s.Groupers(formatName)
	}
	p, ok := format.Lookup(formatName)
	if !ok {
		return false
	}
	_, ok = p.(format.Grouper)
	return ok
}

// checkMembers returns the ids of the members of a group of formatName, in order: each a hosted
// or a proxy repo of the format, at most one of them hosted (§2.5). Errors name the member.
func checkMembers(ctx context.Context, r index.Reader, formatName string, names []string) ([]int64, error) {
	ids := make([]int64, len(names))
	hosted := -1
	for i, name := range names {
		field := fmt.Sprintf("group.members[%d]", i)
		m, err := r.GetRepo(ctx, formatName, name)
		switch {
		case errors.Is(err, index.ErrNotFound):
			return nil, invalid("%s: %q is not a repo of format %s", field, name, formatName)
		case err != nil:
			return nil, err
		case m.Kind == index.KindGroup:
			return nil, invalid("%s: %q is a group; a group is not a member", field, name)
		case m.Kind == index.KindHosted && hosted >= 0:
			return nil, invalid("%s: %q is a second hosted member, after group.members[%d]; a group has at most one", field, name, hosted)
		case m.Kind == index.KindHosted:
			hosted = i
		}
		ids[i] = m.ID
	}
	return ids, nil
}

// groupOf returns the settings of the group row: its members' names from the index and the
// merge patterns of repos.group.
func groupOf(ctx context.Context, r index.Reader, row *index.Repo) (proxy.GroupSettings, error) {
	members, err := r.ListGroupMembers(ctx, row.ID)
	if err != nil {
		return proxy.GroupSettings{}, err
	}
	g := proxy.GroupSettings{Members: make([]string, len(members)), Merge: proxy.StoredMerge(row.Group)}
	for i, m := range members {
		g.Members[i] = m.Name
	}
	return g, nil
}

// applyGroup merges patch into the settings of the group row as an RFC 7396 merge patch, each
// list replaced whole, checks the result whole and records it in changed when it differs. It
// returns the ids of the members when they change, for SetGroupMembers, and nil when they do not.
func applyGroup(ctx context.Context, w index.Writer, row *index.Repo, patch json.RawMessage, changed map[string]any) ([]int64, error) {
	if row.Kind != index.KindGroup {
		return nil, invalid("group: only a group repo has one, not a %s repo", row.Kind)
	}
	obj, err := decodeObject("group", patch)
	switch {
	case err != nil:
		return nil, err
	case obj == nil:
		return nil, invalid("group: a group repo must have one; it cannot be removed")
	}
	current, err := groupOf(ctx, w, row)
	if err != nil {
		return nil, err
	}
	base, err := decodeObject("group", current.JSON())
	if err != nil {
		return nil, err
	}
	merged, err := json.Marshal(mergeObject(base, obj))
	if err != nil { // the values are what json decoded
		return nil, invalid("group: %s", err.Error())
	}
	next, err := proxy.ParseGroup(merged)
	if err != nil {
		return nil, &clientError{msg: err.Error(), err: err}
	}
	var ids []int64
	if !slices.Equal(next.Members, current.Members) {
		if ids, err = checkMembers(ctx, w, row.Format, next.Members); err != nil {
			return nil, err
		}
	}
	if ids == nil && slices.Equal(next.Merge, current.Merge) {
		return nil, nil
	}
	row.Group = next.Stored()
	changed["group"] = next.JSON()
	return ids, nil
}

// notAMember refuses the delete of a repo that is a member of a group, force or not (§2.5): an
// error wrapping index.ErrConflict that names the groups.
func notAMember(ctx context.Context, r index.Reader, row *index.Repo) error {
	groups, err := r.ListGroupsOf(ctx, row.ID)
	if err != nil || len(groups) == 0 {
		return err
	}
	names := make([]string, len(groups))
	for i, g := range groups {
		names[i] = g.Format + "/" + g.Name
	}
	return &clientError{msg: fmt.Sprintf("repo %s/%s is a member of the group(s) %s; remove it from them first",
		row.Format, row.Name, strings.Join(names, ", ")), err: index.ErrConflict}
}

// groupsLast returns the indexes of repos with the groups after every other repo, each part in
// its order: the bootstrap creates a group's members before the group (§2.4).
func groupsLast(repos []config.Repo) []int {
	var first, last []int
	for i, r := range repos {
		if r.Kind == index.KindGroup {
			last = append(last, i)
		} else {
			first = append(first, i)
		}
	}
	return append(first, last...)
}
