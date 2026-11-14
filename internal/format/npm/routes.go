package npm

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"

	"github.com/lajstrom/lajstrom/internal/auth"
	"github.com/lajstrom/lajstrom/internal/format"
	"github.com/lajstrom/lajstrom/internal/index"
	"github.com/lajstrom/lajstrom/internal/server"
)

// Routes mounts the npm frontend below /npm/ (docs/spec/http-routing.md, npm). A package name is
// one segment, lodash or @scope%2fname, or two, @scope/name; the path below /npm/{repo}/ is read
// by parseTarget:
//
//	GET /{repo}/<name>               the packument, full or abbreviated (serve.go), read
//	GET /{repo}/<name>/<version>     one version of the full packument, or of the dist-tag
//	GET /{repo}/<name>/-/<file>      a tarball, through format.ServeBlob
//	PUT /{repo}/<name>               npm publish (publish.go), write; without _attachments, and
//	PUT /{repo}/<name>/-rev/<rev>    the packument update of npm deprecate and unpublish (unpublish.go)
//	DELETE /{repo}/<name>/-rev/<rev>             npm unpublish of the package, delete
//	DELETE /{repo}/<name>/-/<file>/-rev/<rev>    npm unpublish of a version, delete
//	    /{repo}/-/…                  the registry's own routes (special): dist-tags (disttags.go);
//	                                 ping, whoami, search, audit (cli.go); login, logout (login.go)
//
// GET is HEAD too. Every answer is JSON, an error {"error": "<message>"}, that of a repo that
// does not exist too: the patterns name the repo {repoName}, not {repo}, so that the router's
// repo middleware, which answers in text/plain, leaves the lookup to withRepo (#1091).
func (p Plugin) Routes(r format.Router, deps format.Deps) {
	if deps.Log == nil {
		deps.Log = slog.Default().With("format", Name)
	}
	h := &handlers{deps: deps, plugin: p}
	r.HandleFunc("GET /{repoName}/{path...}", h.withRepo(h.get))
	r.HandleFunc("PUT /{repoName}/{path...}", h.withRepo(h.put))
	r.HandleFunc("DELETE /{repoName}/{path...}", h.withRepo(h.del))
	r.HandleFunc("POST /{repoName}/{path...}", h.withRepo(h.post))
}

// withRepo is the repo middleware of format.NewRouter, answering as npm does: one
// deps.Index.GetRepo per request, and next served with the repo in the context
// (format.WithRepo). A repo that does not exist, or has another format, is answered as the
// router's middleware answers it, 404, or 401 to the anonymous principal when deps.Auth is set,
// with its message, so that the answer is the one Authorize gives on a repo the principal may
// not read (docs/spec/rbac.md §3), but as npm's JSON error, as that one is (#1091). The routes
// whose answer does not depend on the repo (repoless) are served all the same, without a repo in
// the context. A failed lookup is 500, logged.
func (h *handlers) withRepo(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		name := r.PathValue("repoName")
		repo, err := h.deps.Index.GetRepo(ctx, Name, name)
		if err == nil && (repo == nil || repo.Format != Name) {
			err = index.ErrNotFound
		}
		switch {
		case err == nil:
			next(w, r.WithContext(format.WithRepo(ctx, *repo)))
		case !errors.Is(err, index.ErrNotFound):
			h.fail(w, r, fmt.Errorf("npm: repo lookup of %s/%s: %w", Name, name, err))
		case h.repoless(r.Method, special(r.PathValue("path"))) != nil:
			next(w, r)
		case h.deps.Auth != nil && auth.PrincipalFrom(ctx).Kind == auth.KindAnonymous:
			h.fail(w, r, fmt.Errorf("%w: repository %s/%s", format.ErrUnauthenticated, Name, name))
		default:
			h.fail(w, r, fmt.Errorf("%w: repository %s/%s", index.ErrNotFound, Name, name))
		}
	}
}

// special returns the segments of a path below /npm/<repo>/-/, the registry's own routes, and
// nil for any other path.
func special(path string) []string {
	rest, ok := strings.CutPrefix(path, "-/")
	if !ok {
		return nil
	}
	return strings.Split(rest, "/")
}

// serveSpecial dispatches a route below /npm/<repo>/-/; each checks its own action. Everything
// else there (npm token, access, owner, team, star, profile) is 404 to a principal that may read
// the repo (noRoute), and so is the web login to everyone (repoless).
func (h *handlers) serveSpecial(w http.ResponseWriter, r *http.Request, segs []string) {
	if serve := h.repoless(r.Method, segs); serve != nil {
		serve(w, r)
		return
	}
	repo, ok := format.RepoFrom(r.Context())
	if !ok {
		h.fail(w, r, errors.New("npm: the route has no repo"))
		return
	}
	method := r.Method
	if method == http.MethodHead {
		method = http.MethodGet
	}
	if serve := h.specialRoute(method, segs); serve != nil {
		serve(w, r, repo)
		return
	}
	h.noRoute(w, r, repo)
}

// repoless returns the handler of a route below /-/ whose answer does not depend on the repo,
// for method and the segments below /-/, nil for any other route; withRepo serves it whether the
// repo exists or not. They are npm login, PUT below /-/user/, decided as the user of its body,
// which reads the repo from the context (login); npm logout, DELETE /-/user/token/<token>,
// decided by the request's token (logout); and the web login, POST /-/v1/login, 404 on every
// repo, after which npm asks for a password and logs in with the legacy route.
func (h *handlers) repoless(method string, segs []string) http.HandlerFunc {
	switch {
	case method == http.MethodPut && len(segs) == 2 && segs[0] == "user":
		return func(w http.ResponseWriter, r *http.Request) { h.login(w, r, segs[1]) }
	case method == http.MethodDelete && len(segs) == 3 && segs[0] == "user" && segs[1] == "token":
		return func(w http.ResponseWriter, r *http.Request) { h.logout(w, r, segs[2]) }
	case method == http.MethodPost && len(segs) == 2 && segs[0] == "v1" && segs[1] == "login":
		return func(w http.ResponseWriter, r *http.Request) {
			h.fail(w, r, fmt.Errorf("%w: %s", index.ErrNotFound, r.URL.Path))
		}
	}
	return nil
}

// noRoute answers a path below /npm/<repo>/ that names no route of a repo: 404 to a principal
// that may read the repo, and the denial of Authorize to any other, which is the answer of a repo
// that does not exist (withRepo), so that the 404 does not tell that the repo exists (#1091).
func (h *handlers) noRoute(w http.ResponseWriter, r *http.Request, repo index.Repo) {
	if err := format.Authorize(r.Context(), h.deps, auth.ActionRead, repo); err != nil {
		h.fail(w, r, err)
		return
	}
	h.fail(w, r, fmt.Errorf("%w: %s", index.ErrNotFound, r.URL.Path))
}

// specialHandler serves a route below /npm/<repo>/-/ of a repo.
type specialHandler func(http.ResponseWriter, *http.Request, index.Repo)

// specialRoute returns the handler of method and the segments below /-/, nil for none.
func (h *handlers) specialRoute(method string, segs []string) specialHandler {
	if segs[0] == "package" {
		return func(w http.ResponseWriter, r *http.Request, repo index.Repo) { h.distTags(w, r, repo, segs[1:]) }
	}
	route := strings.Join(segs, "/")
	if method == http.MethodPost && (route == auditBulk || route == auditFull || route == auditQuick) {
		return func(w http.ResponseWriter, r *http.Request, repo index.Repo) { h.audit(w, r, repo, route) }
	}
	return map[string]specialHandler{
		"GET ping":      h.ping,
		"GET whoami":    h.whoami,
		"GET v1/search": h.search,
	}[method+" "+route]
}

// post dispatches a POST: the registry's own routes.
func (h *handlers) post(w http.ResponseWriter, r *http.Request) {
	if segs := special(r.PathValue("path")); segs != nil {
		h.serveSpecial(w, r, segs)
		return
	}
	repo, ok := format.RepoFrom(r.Context())
	if !ok {
		h.fail(w, r, errors.New("npm: the route has no repo"))
		return
	}
	h.noRoute(w, r, repo)
}

// del dispatches a DELETE: the registry's own routes, and npm unpublish of a package or of a
// version's tarball, which a proxy repo does not take, 405, and a group sends to its hosted
// member (writeTarget).
func (h *handlers) del(w http.ResponseWriter, r *http.Request) {
	if segs := special(r.PathValue("path")); segs != nil {
		h.serveSpecial(w, r, segs)
		return
	}
	repo, ok := format.RepoFrom(r.Context())
	if !ok {
		h.fail(w, r, errors.New("npm: the route has no repo"))
		return
	}
	r, repo, err := h.writeTarget(r, repo)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	t, ok := parseTarget(r.PathValue("path"))
	switch {
	case ok && len(t.rest) == 2 && t.rest[0] == "-rev":
		h.unpublishPackage(w, r, repo, t.name, t.rest[1])
	case ok && len(t.rest) == 4 && t.rest[0] == "-" && t.rest[2] == "-rev":
		h.unpublishFile(w, r, repo, t.name, t.rest[1])
	default:
		h.noRoute(w, r, repo)
	}
}

// handlers are the npm routes over one format.Deps.
type handlers struct {
	deps   format.Deps
	plugin Plugin
}

// target is what a path below /npm/<repo>/ names: a package and the segments after its name.
type target struct {
	name string
	rest []string
}

// parseTarget reads the path below /npm/<repo>/, unescaped: the package name is its first
// segment, or its first two when the first begins with @ (@scope/name, which a client may send
// as @scope%2fname). ok is false for a path that names no package: empty, or below /-/, the
// routes of the registry itself.
func parseTarget(p string) (target, bool) {
	segs := strings.Split(p, "/")
	n := 1
	if strings.HasPrefix(segs[0], "@") {
		n = 2
	}
	if segs[0] == "" || segs[0] == "-" || len(segs) < n {
		return target{}, false
	}
	return target{name: strings.Join(segs[:n], "/"), rest: segs[n:]}, true
}

// put dispatches a PUT: the publish of a package, and the packument update of npm deprecate
// and unpublish (PUT <name> without _attachments, PUT <name>/-rev/<rev>); a proxy repo takes
// neither, 405, and a group sends both to its hosted member (writeTarget).
func (h *handlers) put(w http.ResponseWriter, r *http.Request) {
	if segs := special(r.PathValue("path")); segs != nil {
		h.serveSpecial(w, r, segs)
		return
	}
	repo, ok := format.RepoFrom(r.Context())
	if !ok {
		h.fail(w, r, errors.New("npm: the route has no repo"))
		return
	}
	r, repo, err := h.writeTarget(r, repo)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	t, ok := parseTarget(r.PathValue("path"))
	switch {
	case ok && len(t.rest) == 0:
		h.publish(w, r, repo, t.name, "")
	case ok && len(t.rest) == 2 && t.rest[0] == "-rev":
		h.publish(w, r, repo, t.name, t.rest[1])
	default:
		h.noRoute(w, r, repo)
	}
}

// fail answers err as npm reads an error, {"error": "<message>"}, with the status of
// format.StatusOf: the message for a 4xx, the status text for a 5xx, whose cause is logged; a 401
// carries the Basic challenge, a 405 Allow: GET, HEAD and a 503 Retry-After, as
// format.WriteError does.
func (h *handlers) fail(w http.ResponseWriter, r *http.Request, err error) {
	status := format.StatusOf(err)
	msg := format.ClientMessage(err)
	if status >= http.StatusInternalServerError {
		h.deps.Log.Log(r.Context(), server.FailLevel(r), "npm: request failed", "method", r.Method, "path", auth.RedactSecrets(r.URL.EscapedPath()),
			"status", status, "error", err.Error())
	}
	switch status {
	case http.StatusUnauthorized:
		w.Header().Set("WWW-Authenticate", `Basic realm="lajstrom"`)
	case http.StatusMethodNotAllowed:
		w.Header().Set("Allow", "GET, HEAD")
	}
	format.SetRetryAfter(w.Header(), err)
	writeJSON(w, status, map[string]string{"error": msg})
}

// writeJSON answers v as JSON with status.
func writeJSON(w http.ResponseWriter, status int, v any) {
	b, _ := json.Marshal(v) // maps of strings and bools
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	_, _ = w.Write(append(b, '\n'))
}
