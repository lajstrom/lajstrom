package proxy

import (
	"slices"

	"github.com/lajstrom/lajstrom/internal/index"
)

// OCIRoute returns the member, one of members, that the routes of the group of oci repos group
// send path to (Route over its repos.group), and the path under which that member keeps it
// (OCIPath: library/ of Docker Hub, after the route, which sees the path as the client sent
// it); false when no route selects path, when the route names a repo that is not among members,
// or when a proxy member's lists refuse the path, which that member does not have through the
// group (docs/spec/proxy-and-group.md §12.2). It asks nothing of the index, the registry or an
// upstream, so a path goes to the same member on every replica.
func (c *Core) OCIRoute(group index.Repo, members []index.Repo, path string) (index.Repo, string, bool) {
	if group.Format != ociFormat || group.Kind != index.KindGroup {
		return index.Repo{}, "", false
	}
	name, ok := Route(StoredPaths(group.Group), path)
	i := slices.IndexFunc(members, func(m index.Repo) bool { return m.Name == name && m.Format == ociFormat })
	if !ok || i < 0 {
		return index.Repo{}, "", false
	}
	member := members[i]
	at := c.OCIPath(member, path)
	if member.Kind == index.KindProxy {
		s, err := ParseSettings(member.Upstream)
		if err != nil {
			return index.Repo{}, "", false
		}
		if _, refused := s.Refuses(at); refused {
			return index.Repo{}, "", false
		}
	}
	return member, at, true
}
