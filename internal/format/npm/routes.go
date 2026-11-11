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
// GET is HEAD too. Every answer is JSON, an error {"error": "<message>"}.
func (p Plugin) Routes(r format.Router, deps format.Deps) {
	if deps.Log == nil {
		deps.Log = slog.Default().With("format", Name)
	}
	h := &handlers{deps: deps, plugin: p}
	r.HandleFunc("GET /{repo}/{path...}", h.get)
	r.HandleFunc("PUT /{repo}/{path...}", h.put)
	r.HandleFunc("DELETE /{repo}/{path...}", h.del)
	r.HandleFunc("POST /{repo}/{path...}", h.post)
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
// else there (npm token, access, owner, team, star, profile, and the web login, after whose 404
// npm asks for a password) is 404.
func (h *handlers) serveSpecial(w http.ResponseWriter, r *http.Request, segs []string) {
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
	h.fail(w, r, fmt.Errorf("%w: %s", index.ErrNotFound, r.URL.Path))
}

// specialHandler serves a route below /npm/<repo>/-/ of a repo.
type specialHandler func(http.ResponseWriter, *http.Request, index.Repo)

// specialRoute returns the handler of method and the segments below /-/, nil for none.
func (h *handlers) specialRoute(method string, segs []string) specialHandler {
	switch {
	case segs[0] == "package":
		return func(w http.ResponseWriter, r *http.Request, repo index.Repo) { h.distTags(w, r, repo, segs[1:]) }
	case method == http.MethodPut && len(segs) == 2 && segs[0] == "user":
		return func(w http.ResponseWriter, r *http.Request, repo index.Repo) { h.login(w, r, repo, segs[1]) }
	case method == http.MethodDelete && len(segs) == 3 && segs[0] == "user" && segs[1] == "token":
		return func(w http.ResponseWriter, r *http.Request, _ index.Repo) { h.logout(w, r, segs[2]) }
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
	segs := special(r.PathValue("path"))
	if segs == nil {
		h.fail(w, r, fmt.Errorf("%w: %s", index.ErrNotFound, r.URL.Path))
		return
	}
	h.serveSpecial(w, r, segs)
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
		h.fail(w, r, fmt.Errorf("%w: %s", index.ErrNotFound, r.URL.Path))
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
	case !ok:
		h.fail(w, r, fmt.Errorf("%w: %s", index.ErrNotFound, r.URL.Path))
	case len(t.rest) == 0:
		h.publish(w, r, repo, t.name, "")
	case len(t.rest) == 2 && t.rest[0] == "-rev":
		h.publish(w, r, repo, t.name, t.rest[1])
	default:
		h.fail(w, r, fmt.Errorf("%w: %s", index.ErrNotFound, r.URL.Path))
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
		h.deps.Log.ErrorContext(r.Context(), "npm: request failed", "method", r.Method, "path", auth.RedactSecrets(r.URL.EscapedPath()),
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
