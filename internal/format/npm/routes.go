package npm

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"

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
//	PUT /{repo}/<name>               npm publish (publish.go), write
//
// GET is HEAD too. Every answer is JSON, an error {"error": "<message>"}.
func (p Plugin) Routes(r format.Router, deps format.Deps) {
	if deps.Log == nil {
		deps.Log = slog.Default().With("format", Name)
	}
	h := &handlers{deps: deps, plugin: p}
	r.HandleFunc("GET /{repo}/{path...}", h.get)
	r.HandleFunc("PUT /{repo}/{path...}", h.put)
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

// put dispatches a PUT: the publish of a package; the packument update of npm deprecate and
// unpublish, PUT <name>/-rev/<rev>, is not implemented yet (#308).
func (h *handlers) put(w http.ResponseWriter, r *http.Request) {
	repo, ok := format.RepoFrom(r.Context())
	if !ok {
		h.fail(w, r, errors.New("npm: the route has no repo"))
		return
	}
	t, ok := parseTarget(r.PathValue("path"))
	switch {
	case !ok:
		h.fail(w, r, fmt.Errorf("%w: %s", index.ErrNotFound, r.URL.Path))
	case len(t.rest) == 0:
		h.publish(w, r, repo, t.name)
	case len(t.rest) == 2 && t.rest[0] == "-rev":
		h.fail(w, r, fmt.Errorf("%w: the packument update of npm deprecate and unpublish", format.ErrUnsupported))
	default:
		h.fail(w, r, fmt.Errorf("%w: %s", index.ErrNotFound, r.URL.Path))
	}
}

// fail answers err as npm reads an error, {"error": "<message>"}, with the status of
// format.StatusOf: the message for a 4xx, the status text for a 5xx, whose cause is logged; a 401
// carries the Basic challenge and a 503 Retry-After, as format.WriteError does.
func (h *handlers) fail(w http.ResponseWriter, r *http.Request, err error) {
	status := format.StatusOf(err)
	msg := http.StatusText(status)
	if status < http.StatusInternalServerError {
		msg = err.Error()
	} else {
		h.deps.Log.ErrorContext(r.Context(), "npm: request failed", "method", r.Method, "path", r.URL.EscapedPath(),
			"status", status, "error", err.Error())
	}
	if status == http.StatusUnauthorized {
		w.Header().Set("WWW-Authenticate", `Basic realm="lajstrom"`)
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
