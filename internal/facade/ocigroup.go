package facade

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	"github.com/lajstrom/lajstrom/internal/auth"
	"github.com/lajstrom/lajstrom/internal/format"
	"github.com/lajstrom/lajstrom/internal/index"
	"github.com/lajstrom/lajstrom/internal/server"
)

// Groups of oci repos (ADR 0019, docs/spec/proxy-and-group.md §12): /v2/oci/<group>/<path> is
// answered by the one member the group's routes choose for <path>, as at the member's own URL,
// and nothing is written through the group. The routes are the core's (OCIProxy.OCIRoute), so
// that the facade does not import internal/proxy; without a core a group stays NAME_UNKNOWN.

// ociGroup reports whether repo is a group of oci repos that the facade serves.
func (h *handler) ociGroup(repo index.Repo) bool {
	return h.o.OCIProxy != nil && repo.Format == ociFormat && repo.Kind == index.KindGroup
}

// throughGroup answers what a request of the route kind on the group t decides before its
// route: the read on the group (authorize, the group's denial when it may not read), then 405
// UNSUPPORTED with Allow: GET, HEAD for every method but those and for every route below
// blobs/uploads/, a GET of a session's status included, whatever the path, so that a write does
// not tell a stranger that the group exists and does not depend on the routes. It returns the
// target of the routed member (route) for a read.
func (h *handler) throughGroup(w http.ResponseWriter, r *http.Request, t *target, kind routeKind) (*target, error) {
	if err := h.authorize(r.Context(), auth.ActionRead, t); err != nil {
		return nil, err
	}
	if (r.Method != http.MethodGet && r.Method != http.MethodHead) || kind == routeUploads || kind == routeUpload {
		w.Header().Set("Allow", "GET, HEAD")
		return nil, &apiError{status: http.StatusMethodNotAllowed, code: codeUnsupported,
			msg: fmt.Sprintf("%s/%s is a group of oci repos: it serves pulls only; push to the member's own URL", t.repo.Format, t.repo.Name)}
	}
	return h.route(r.Context(), t)
}

// routeRead is throughGroup for a read that is not the request's own route, the source of a
// mount: the read on the group, then its route.
func (h *handler) routeRead(ctx context.Context, t *target) (*target, error) {
	if err := h.authorize(ctx, auth.ActionRead, t); err != nil {
		return nil, err
	}
	return h.route(ctx, t)
}

// route returns the target of the member that the routes of the group t send t.path to: repo the
// member, via the group, name the client's, path and oci the member's path and OCI name. A path
// that no route selects, or that a proxy member's lists refuse, is NAME_UNKNOWN (§12.2). One
// ListGroupMembers per request; the routed member is logged at debug.
func (h *handler) route(ctx context.Context, t *target) (*target, error) {
	group := t.repo
	members, err := h.o.Index.ListGroupMembers(ctx, group.ID)
	if err != nil {
		return nil, fmt.Errorf("facade: the members of %s/%s: %w", group.Format, group.Name, err)
	}
	member, path, ok := h.o.OCIProxy.OCIRoute(group, members, t.path)
	if !ok {
		return nil, &apiError{status: http.StatusNotFound, code: codeNameUnknown,
			msg: fmt.Sprintf("repository name %s not known to registry: no route of the group %s/%s selects %s", t.name, group.Format, group.Name, t.path)}
	}
	h.o.Log.DebugContext(ctx, "facade: a group's path routed", "name", t.name, "member", member.Format+"/"+member.Name, "path", path,
		"requestId", server.RequestID(ctx))
	return &target{repo: member, via: &group, name: t.name, path: path, oci: ociName(member, path)}, nil
}

// proxyContext is ctx for the core's calls on t: through a group it carries the group
// (format.WithVia), so that a proxy member's proxy.fetch and proxy.reject name it in
// details.via, the background completions included, which keep the request's values. The
// request's own context stays without it: format.Authorize reads it as a write through a group.
func proxyContext(ctx context.Context, t *target) context.Context {
	if t.via == nil {
		return ctx
	}
	return format.WithVia(ctx, *t.via)
}

// groupRepositories returns the names the catalog lists for a group: for a group of oci repos,
// each member's repositories that the routes send to that member, as <format>/<group>/<path>:
// what the group serves without asking an upstream (§12.3). Another group lists nothing.
func (h *handler) groupRepositories(ctx context.Context, group index.Repo) ([]string, error) {
	if !h.ociGroup(group) {
		return nil, nil
	}
	members, err := h.o.Index.ListGroupMembers(ctx, group.ID)
	if err != nil {
		return nil, fmt.Errorf("facade: catalog: the members of %s/%s: %w", group.Format, group.Name, err)
	}
	var names []string
	for _, m := range members {
		held, err := h.repositories(ctx, m)
		if err != nil {
			return nil, err
		}
		prefix := m.Format + "/" + m.Name + "/"
		for _, name := range held {
			path := strings.TrimPrefix(name, prefix)
			if routed, at, ok := h.o.OCIProxy.OCIRoute(group, members, path); ok && routed.ID == m.ID && at == path {
				names = append(names, group.Format+"/"+group.Name+"/"+path)
			}
		}
	}
	return names, nil
}
