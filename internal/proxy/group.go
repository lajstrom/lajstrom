package proxy

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/lajstrom/lajstrom/internal/format"
	"github.com/lajstrom/lajstrom/internal/index"
	"github.com/lajstrom/lajstrom/internal/oci/profile"
)

// grouped is a group repo with what serving it takes (docs/spec/proxy-and-group.md §8): its
// plugin and hooks, its members in order, its hosted member, and its merge patterns.
type grouped struct {
	repo    index.Repo
	plugin  format.Plugin
	hooks   format.Grouper
	members []index.Repo
	hosted  *index.Repo
	merge   []string
}

// resolveGroup returns the group repo is, or an error wrapping format.ErrUnsupported for a
// format without the hooks of groups.
func (c *Core) resolveGroup(ctx context.Context, repo index.Repo) (*grouped, error) {
	id := repo.Format + "/" + repo.Name
	plugin, ok := c.o.Plugins(repo.Format)
	if !ok {
		return nil, fmt.Errorf("%w: proxy: no plugin of the format of %s", format.ErrUnsupported, id)
	}
	hooks, ok := plugin.(format.Grouper)
	if !ok {
		return nil, fmt.Errorf("%w: proxy: format %s has no group repos", format.ErrUnsupported, repo.Format)
	}
	members, err := c.o.Index.ListGroupMembers(ctx, repo.ID)
	if err != nil {
		return nil, fmt.Errorf("proxy: the members of %s: %w", id, err)
	}
	g := &grouped{repo: repo, plugin: plugin, hooks: hooks, members: members, merge: StoredMerge(repo.Group)}
	for i := range members {
		if members[i].Kind == index.KindHosted {
			g.hosted = &members[i]
		}
	}
	return g, nil
}

// hides reports whether the proxy members are hidden for name (§8.2): the hosted member has a
// package of the package path of name, and no merge pattern matches its key.
func (g *grouped) hides(ctx context.Context, r index.Reader, name, key string) (bool, error) {
	if g.hosted == nil || Merged(g.merge, key) {
		return false, nil
	}
	pkgPath, err := g.plugin.EscapeName(name)
	if err != nil {
		return false, nil //nolint:nilerr // a name the format cannot escape is held by no repo
	}
	_, err = index.PackageAt(ctx, r, g.hosted.ID, pkgPath)
	switch {
	case errors.Is(err, index.ErrNotFound):
		return false, nil
	case err != nil:
		return false, err
	}
	return true, nil
}

// groupDocument is Document of a group (§8): the documents of kind of name of the members that
// take part, merged by the plugin on every call and stored nowhere. The hosted member's is its
// own cached document; a proxy member's is Document on it, through the group (format.WithVia),
// unless the hosted member hides it. A proxy member that does not have the name, or whose lists
// refuse it, does not take part; one that fails does not either, and the first failure is the
// answer when no member has the name (§8.1).
func (c *Core) groupDocument(ctx context.Context, repo index.Repo, name, kind string) (*index.Cache, error) {
	g, err := c.resolveGroup(ctx, repo)
	if err != nil {
		return nil, err
	}
	key, err := g.hooks.Key(name)
	if err != nil {
		return nil, err
	}
	hidden, err := g.hides(ctx, c.o.Index, name, key)
	if err != nil {
		return nil, err
	}
	ctx = format.WithVia(ctx, repo)
	var (
		docs     []format.MemberDocument
		cached   []*index.Cache
		firstErr error
	)
	for _, m := range g.members {
		if m.Kind == index.KindProxy && hidden {
			continue
		}
		doc, err := c.memberDocument(ctx, g, m, name, key, kind)
		if err != nil {
			failed, fatal := c.passBy(ctx, g, m, name, kind, err)
			if fatal != nil {
				return nil, fatal
			}
			firstErr = cmp.Or(firstErr, failed)
			continue
		}
		docs = append(docs, format.MemberDocument{Repo: m, Content: doc.Content})
		cached = append(cached, doc)
	}
	switch {
	case len(docs) == 0 && firstErr != nil:
		return nil, firstErr
	case len(docs) == 0:
		return nil, fmt.Errorf("%w: no member of the group %s has %q", index.ErrNotFound, g.id(), name)
	}
	merged, err := g.hooks.MergeGroup(ctx, repo, name, kind, docs, c.base(repo))
	if err != nil {
		return nil, fmt.Errorf("proxy: %s: merge %s of %q: %w", g.id(), kind, name, err)
	}
	out := groupETag(repo, cached)
	out.Scope, out.Kind, out.Content = key, kind, merged.Content
	return out, nil
}

// passBy sorts the error err of the member m's document of name (§8.1): fatal, the request's
// end or the hosted member's failure, ends the answer; a member that does not have the name, or
// whose lists refuse it, gives neither; a proxy member that failed is logged, and its error is
// failed, the group's answer when no member has the name.
func (c *Core) passBy(ctx context.Context, g *grouped, m index.Repo, name, kind string, err error) (failed, fatal error) {
	switch {
	case ctx.Err() != nil:
		return nil, ctx.Err()
	case errors.Is(err, index.ErrNotFound), errors.Is(err, format.ErrForbidden):
		return nil, nil
	case m.Kind == index.KindHosted:
		return nil, err
	}
	c.o.Log.WarnContext(ctx, "proxy: a member of a group failed for a document; the group answers without it",
		"group", g.id(), "member", m.Format+"/"+m.Name, "name", name, "kind", kind, "error", err.Error())
	return err, nil
}

// groupETag returns the Cache of a group's document but its content: the group's RepoID,
// SHA256 the sha256 of the group's updatedAt and of the sha256 of each member document that
// took part, in order (§8.5), and RenderedAt the newest of theirs.
func groupETag(repo index.Repo, docs []*index.Cache) *index.Cache {
	h := sha256.New()
	_, _ = fmt.Fprintf(h, "%s\n", repo.UpdatedAt.UTC().Format(time.RFC3339Nano)) // a hash does not fail
	var newest time.Time
	for _, d := range docs {
		_, _ = fmt.Fprintf(h, "%s\n", d.SHA256)
		if d.RenderedAt.After(newest) {
			newest = d.RenderedAt
		}
	}
	return &index.Cache{RepoID: repo.ID, SHA256: hex.EncodeToString(h.Sum(nil)), RenderedAt: newest}
}

// memberDocument is the document of kind of name of the member m of a group: the hosted
// member's cached document of scope key, or what Document answers for a proxy member.
func (c *Core) memberDocument(ctx context.Context, g *grouped, m index.Repo, name, key, kind string) (*index.Cache, error) {
	if m.Kind == index.KindProxy {
		return c.Document(ctx, m, name, kind)
	}
	doc, err := format.CachedDocument(ctx, c.o.Index, g.plugin, m, format.IndexScope(key), kind)
	if err != nil && !errors.Is(err, index.ErrNotFound) {
		return nil, fmt.Errorf("proxy: the document of %q in %s/%s: %w", name, m.Format, m.Name, err)
	}
	return doc, err
}

// groupFile is File of a group (§8.3): the file of the first member taking part that holds it,
// or, for a proxy member that lists it, File on that member, through the group. The hosted
// member that has the name and not the version is 404; a proxy member whose lists refuse the
// name, or that does not list the file, is passed by, and so is one that fails, whose error is
// the answer when no other member has the file.
func (c *Core) groupFile(ctx context.Context, repo index.Repo, path string) (*index.FileMatch, error) {
	g, a, err := c.groupArtifact(ctx, repo, path)
	if err != nil {
		return nil, err
	}
	hidden, err := g.hides(ctx, c.o.Index, a.ref.Name, a.key)
	if err != nil {
		return nil, err
	}
	ctx = format.WithVia(ctx, repo)
	var firstErr error
	for _, m := range g.members {
		if m.Kind == index.KindProxy && hidden {
			continue
		}
		match, err := c.memberFile(ctx, m, a, path, hidden)
		switch {
		case match != nil && err == nil:
			return match, nil
		case m.Kind == index.KindHosted && err != nil:
			return nil, err
		case err == nil, errors.Is(err, index.ErrNotFound), errors.Is(err, errListsRefuse):
		case ctx.Err() != nil:
			return nil, ctx.Err()
		default:
			firstErr = cmp.Or(firstErr, err)
		}
	}
	if firstErr != nil {
		return nil, firstErr
	}
	return nil, fmt.Errorf("%w: no member of the group %s has %s", index.ErrNotFound, g.id(), a.subject())
}

// groupArtifact returns the group repo is and the artifact path names (FileRequest).
func (c *Core) groupArtifact(ctx context.Context, repo index.Repo, path string) (*grouped, artifact, error) {
	g, err := c.resolveGroup(ctx, repo)
	if err != nil {
		return nil, artifact{}, err
	}
	ref, err := g.hooks.FileRequest(path)
	if err != nil {
		return nil, artifact{}, err
	}
	key, err := g.hooks.Key(ref.Name)
	if err != nil {
		return nil, artifact{}, err
	}
	pkgPath, err := g.plugin.EscapeName(ref.Name)
	if err != nil {
		return nil, artifact{}, fmt.Errorf("%w: %w", index.ErrNotFound, err)
	}
	return g, artifact{ref: ref, key: key, pkgPath: pkgPath, tag: profile.Tag(ref.Version, ref.Variant)}, nil
}

// errListsRefuse is the refusal of a proxy member's lists for a name, through a group: the member
// does not have the name (§3).
var errListsRefuse = errors.New("proxy: the member's lists refuse the name")

// memberFile is the file of the artifact in the member m of a group: the hosted member's held
// file, 404 when hidden says it has the name and it does not hold the version; File on a proxy
// member, once its lists take the name.
func (c *Core) memberFile(ctx context.Context, m index.Repo, a artifact, path string, hidden bool) (*index.FileMatch, error) {
	if m.Kind == index.KindHosted {
		match, err := heldFile(ctx, c.o.Index, m, a)
		if match == nil && err == nil && hidden {
			err = fmt.Errorf("%w: %s/%s has %s and not %s", index.ErrNotFound, m.Format, m.Name, a.ref.Name, a.subject())
		}
		return match, err
	}
	p, err := c.resolve(m)
	if err != nil {
		return nil, err
	}
	if _, err := p.key(a.ref.Name); errors.Is(err, format.ErrForbidden) {
		return nil, errListsRefuse
	}
	return c.File(ctx, m, path)
}

// id is the group's <format>/<name>.
func (g *grouped) id() string { return g.repo.Format + "/" + g.repo.Name }
