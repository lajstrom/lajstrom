package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/lajstrom/lajstrom/internal/config"
	"github.com/lajstrom/lajstrom/internal/format"
	"github.com/lajstrom/lajstrom/internal/index"
	"github.com/lajstrom/lajstrom/internal/proxy"
)

// Group repos (ADR 0013, docs/spec/proxy-and-group.md §2.3 to §2.5): the settings, the members
// and the checks that the API, lajstrom admin repo and the config bootstrap share.

// checkGroup checks the group of a create: a format with group repos, or oci, whose groups the
// facade serves without a plugin (docs/spec/proxy-and-group.md §12); no registry and no policy;
// and settings that proxy.ParseGroup takes. The members are checked against the index in the
// transaction of the create (checkMembers).
func (s *Repos) checkGroup(req CreateRequest) (proxy.GroupSettings, error) {
	switch {
	case req.Format != formatOCI && !s.grouper(req.Format):
		return proxy.GroupSettings{}, invalid("kind: format %s has no group repos", req.Format)
	case req.Registry != "":
		return proxy.GroupSettings{}, invalid("registry: a group has none; its members' registries hold what it serves")
	case !isNull(req.Policy):
		return proxy.GroupSettings{}, invalid("policy: a group has none; its members' policies apply")
	case isNull(req.Group):
		return proxy.GroupSettings{}, invalid("group: a group repo must name its members")
	}
	g, err := proxy.ParseGroup(req.Format, req.Group)
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
// or a proxy repo of the format, at most one of them hosted but in a group of oci repos, whose
// routes say which hosted member serves a path (§2.5, §12.1). Errors name the member.
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
		case m.Kind == index.KindHosted && hosted >= 0 && formatName != formatOCI:
			return nil, invalid("%s: %q is a second hosted member, after group.members[%d]; a group has at most one", field, name, hosted)
		case m.Kind == index.KindHosted:
			hosted = i
		}
		ids[i] = m.ID
	}
	return ids, nil
}

// groupOf returns the settings of the group row: its members' names from the index and the
// merge patterns of repos.group, or the routes of a group of oci repos.
func groupOf(ctx context.Context, r index.Reader, row *index.Repo) (proxy.GroupSettings, error) {
	members, err := r.ListGroupMembers(ctx, row.ID)
	if err != nil {
		return proxy.GroupSettings{}, err
	}
	g := proxy.GroupSettings{Members: make([]string, len(members))}
	if row.Format == formatOCI {
		g.Paths = proxy.StoredPaths(row.Group)
	} else {
		g.Merge = proxy.StoredMerge(row.Group)
	}
	for i, m := range members {
		g.Members[i] = m.Name
	}
	return g, nil
}

// errMembers refuses a change of group.members to a principal without the admin role, and
// errPaths one of group.paths.
var (
	errMembers = &clientError{msg: "group.members: only the admin role chooses the members of a group", err: format.ErrForbidden}
	errPaths   = &clientError{msg: "group.paths: only the admin role chooses the routes of a group", err: format.ErrForbidden}
)

// applyGroup merges patch into the settings of the group row as an RFC 7396 merge patch, each
// list replaced whole, checks the result whole and records it in changed when it differs. It
// returns the ids of the members when they change, for SetGroupMembers, and nil when they do not.
//
// Members that differ from the row's, the same names in another order included, since a group
// reads its members in order (docs/spec/proxy-and-group.md §8), are errMembers unless admin is
// set, refused before they are looked up, so that the answer does not tell which repos exist
// (docs/spec/rbac.md §3); the members the row has, sent back, are no change (§7). The routes of a
// group of oci repos that differ are errPaths unless admin is set, as members are (§12.1).
func applyGroup(ctx context.Context, w index.Writer, row *index.Repo, patch json.RawMessage, admin bool, changed map[string]any) ([]int64, error) {
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
	next, err := proxy.ParseGroup(row.Format, merged)
	if err != nil {
		return nil, &clientError{msg: err.Error(), err: err}
	}
	ids, same, err := groupChange(ctx, w, row.Format, current, next, admin)
	if err != nil || same {
		return nil, err
	}
	row.Group = next.Stored()
	changed["group"] = next.JSON()
	return ids, nil
}

// groupChange compares the settings next of a group of formatName with its current ones and
// returns the ids of the members when they change, and whether nothing changes: errPaths for
// other routes and errMembers for other members, unless admin is set; other members are checked
// against the index (checkMembers).
func groupChange(ctx context.Context, r index.Reader, formatName string, current, next proxy.GroupSettings, admin bool) ([]int64, bool, error) {
	samePaths := maps.EqualFunc(next.Paths, current.Paths, slices.Equal[[]string])
	if !samePaths && !admin {
		return nil, false, errPaths
	}
	var ids []int64
	if !slices.Equal(next.Members, current.Members) {
		if !admin {
			return nil, false, errMembers
		}
		var err error
		if ids, err = checkMembers(ctx, r, formatName, next.Members); err != nil {
			return nil, false, err
		}
	}
	return ids, ids == nil && slices.Equal(next.Merge, current.Merge) && samePaths, nil
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
