package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/lajstrom/lajstrom/internal/auth"
	"github.com/lajstrom/lajstrom/internal/format"
	"github.com/lajstrom/lajstrom/internal/index"
	"github.com/lajstrom/lajstrom/internal/proxy"
)

// actionCachePurge is the audit action of DELETE …/upstream-cache (docs/spec/rbac.md §7).
const actionCachePurge = "repo.cache.purge"

// errUpstreamName refuses a change of upstream.name to a principal without the admin role.
var errUpstreamName = &clientError{msg: "upstream.name: only the admin role chooses the upstream of a proxy repo", err: format.ErrForbidden}

// applyUpstream merges patch into the settings of the proxy row as an RFC 7396 merge patch, as
// a policy is merged, checks the result whole (parseUpstream, checkUpstreamName) and records it
// in changed when it differs. renamed reports a change of the upstream's name, whose cache the
// caller drops. The settings of a proxy cannot be removed, and only a proxy has them.
//
// A name that differs from the row's, both decoded from JSON and compared as they are, is
// errUpstreamName unless admin is set, refused before the name is looked up in the
// configuration; the name the row has, sent back, is no change (docs/spec/rbac.md §7).
func (s *Repos) applyUpstream(row *index.Repo, patch json.RawMessage, admin bool, changed map[string]any) (renamed bool, err error) {
	if row.Kind != index.KindProxy {
		return false, invalid("upstream: only a proxy repo has one, not a %s repo", row.Kind)
	}
	obj, err := decodeObject("upstream", patch)
	switch {
	case err != nil:
		return false, err
	case obj == nil:
		return false, invalid("upstream: a proxy repo must have one; it cannot be removed")
	}
	base, err := decodeObject("upstream", row.Upstream)
	if err != nil || base == nil { // settings that do not read are replaced
		base = policyObject{}
	}
	stored, _ := base["name"].(string) // before the merge, which changes base
	merged, err := json.Marshal(mergeObject(base, obj))
	if err != nil { // the values are what json decoded
		return false, invalid("upstream: %s", err.Error())
	}
	settings, err := parseUpstream(merged)
	if err != nil {
		return false, err
	}
	if settings.Upstream != stored && !admin {
		return false, errUpstreamName
	}
	if err := s.checkUpstreamName(row.Format, settings); err != nil {
		return false, err
	}
	next := settings.JSON()
	if samePolicy(row.Upstream, next) {
		return false, nil
	}
	old, _ := proxy.ParseSettings(row.Upstream) // a zero name when they do not read: a new upstream
	row.Upstream = next
	changed["upstream"] = next
	return old.Upstream != settings.Upstream, nil
}

// dropUpstreamCache removes on w a proxy repo's cached upstream documents of the name key, or
// all of them when key is "", with the documents rendered from them (index_cache of the scope
// key, the scope of a proxy's documents, or every scope for "").
func dropUpstreamCache(ctx context.Context, w index.Writer, repoID int64, key string) error {
	if _, err := w.DeleteProxyMeta(ctx, repoID, key); err != nil {
		return err
	}
	return w.InvalidateIndexCache(ctx, repoID, key)
}

// PurgeUpstreamCache drops the cache of the upstream of the proxy repo formatName/name, or its
// documents of the package name pkg only when pkg is not "", with one repo.cache.purge audit
// event whose details have the name (docs/spec/http-routing.md §6). The next read asks the
// upstream again. A repo that is not a proxy, or a name the format does not allow, is an error
// wrapping format.ErrInvalid; a repo that does not exist one wrapping index.ErrNotFound.
func (s *Repos) PurgeUpstreamCache(ctx context.Context, formatName, name, pkg string, o Origin) error {
	var ev index.AuditEvent
	err := s.Index.Tx(ctx, func(w index.Writer) (err error) {
		row, err := w.GetRepo(ctx, formatName, name)
		if err != nil {
			return err
		}
		if row.Kind != index.KindProxy {
			return invalid("repo %s/%s is a %s repo: only a proxy repo has an upstream cache", formatName, name, row.Kind)
		}
		key, details := "", map[string]any{}
		if pkg != "" {
			px, ok := s.proxier(formatName)
			if !ok {
				return fmt.Errorf("api: the proxy repo %s/%s has a format without proxy hooks", formatName, name)
			}
			if key, err = px.Key(pkg); err != nil {
				return invalid("name: %q is not a name of format %s", pkg, formatName)
			}
			details["name"] = pkg
		}
		if err := dropUpstreamCache(ctx, w, row.ID, key); err != nil {
			return err
		}
		ev, err = s.record(ctx, w, actionCachePurge, row, o, details)
		return err
	})
	if err != nil {
		return repoError("purge the upstream cache of", formatName, name, err)
	}
	s.writeAudit(ctx, ev)
	return nil
}

// purgeUpstreamCache drops the cached upstream documents of a proxy repo, or those of one name
// with ?name=: 204 (manage on the repo).
func (h *handler) purgeUpstreamCache(_ http.ResponseWriter, r *http.Request) (int, any, error) {
	f, name := r.PathValue("format"), r.PathValue("repo")
	if err := h.allow(r, auth.ActionManage, f, name); err != nil {
		return 0, nil, err
	}
	if err := h.repos.PurgeUpstreamCache(r.Context(), f, name, r.URL.Query().Get("name"), origin(r)); err != nil {
		return 0, nil, err
	}
	return http.StatusNoContent, nil, nil
}

// hasKey reports whether a member of a PATCH, an object, has key: a body with upstream.name or
// group.members may change what every reader of a proxy or a group gets, which only the admin
// role does (docs/spec/rbac.md §7), and the handler asks for the role then. Whether the value is
// a change, Update decides against the stored one.
func hasKey(patch json.RawMessage, key string) bool {
	var obj map[string]json.RawMessage
	if json.Unmarshal(patch, &obj) != nil {
		return false // Update refuses it
	}
	_, ok := obj[key]
	return ok
}
