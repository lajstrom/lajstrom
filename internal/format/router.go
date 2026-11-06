package format

import (
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"

	"github.com/lajstrom/lajstrom/internal/index"
)

// NewRouter returns the Router the server hands to Plugin.Routes of the plugin named name.
// Patterns are ServeMux patterns relative to /<name>/ ("GET /{repo}/simple/{project}/"); they
// are registered on mux as "GET /<name>/{repo}/simple/{project}/". A pattern that has a {repo}
// wildcard gets the repo middleware: one deps.Index.GetRepo(ctx, name, {repo}) per request,
// 404 when the repo does not exist or has another format, 500 (logged through deps.Log) when
// the lookup fails, and the repo in the context (RepoFrom) otherwise.
//
// deps.Log is expected to carry the format attribute, as the logger the server passes does; nil
// means slog.Default() with it. NewRouter panics on a nil mux, a nil deps.Index or a name that
// CheckName rejects, and Handle panics on a malformed pattern or a nil handler: they are
// programming errors, found when the server starts. Conflicting patterns make the ServeMux
// panic in the same way.
func NewRouter(mux *http.ServeMux, name string, deps Deps) Router {
	switch {
	case mux == nil:
		panic("format: NewRouter: nil mux")
	case deps.Index == nil:
		panic("format: NewRouter: nil Deps.Index")
	}
	if err := CheckName(name); err != nil {
		panic("format: NewRouter: " + err.Error())
	}
	if deps.Log == nil {
		deps.Log = slog.Default().With("format", name)
	}
	return &router{mux: mux, name: name, deps: deps}
}

// router is the Router of one plugin.
type router struct {
	mux  *http.ServeMux
	name string
	deps Deps
}

// Handle registers h for pattern below /<name>.
func (r *router) Handle(pattern string, h http.Handler) {
	if h == nil {
		panic(fmt.Sprintf("format: Router.Handle(%q): nil handler", pattern))
	}
	mounted, err := mountPattern(r.name, pattern)
	if err != nil {
		panic("format: Router.Handle: " + err.Error())
	}
	if strings.Contains(pattern, "{repo}") {
		h = r.withRepo(h)
	}
	r.mux.Handle(mounted, h)
}

// HandleFunc registers h for pattern below /<name>.
func (r *router) HandleFunc(pattern string, h http.HandlerFunc) {
	if h == nil {
		panic(fmt.Sprintf("format: Router.HandleFunc(%q): nil handler", pattern))
	}
	r.Handle(pattern, h)
}

// mountPattern returns pattern, relative to /<name>/, as the ServeMux pattern of the whole
// path: "GET /{repo}/x" becomes "GET /<name>/{repo}/x" and "/" becomes "/<name>/". The method,
// when there is one, is kept; the grammar is the ServeMux's, [METHOD ]/path, without a host.
func mountPattern(name, pattern string) (string, error) {
	if pattern == "" {
		return "", errors.New("empty pattern")
	}
	method, path := "", pattern
	if i := strings.IndexAny(pattern, " \t"); i >= 0 && !strings.Contains(pattern[:i], "/") {
		method, path = pattern[:i], strings.TrimLeft(pattern[i+1:], " \t")
	}
	switch i := strings.IndexByte(path, '/'); {
	case i < 0:
		return "", fmt.Errorf("pattern %q has no /path", pattern)
	case i > 0:
		return "", fmt.Errorf("pattern %q has a host; patterns are paths below /%s/", pattern, name)
	}
	if method != "" {
		method += " "
	}
	return method + "/" + name + path, nil
}

// withRepo is the repo middleware: it resolves the {repo} wildcard of the request to the repo
// of the router's format, once per request.
func (r *router) withRepo(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		ctx := req.Context()
		name := req.PathValue("repo")
		repo, err := r.deps.Index.GetRepo(ctx, r.name, name)
		if err == nil && (repo == nil || repo.Format != r.name) {
			err = index.ErrNotFound
		}
		switch {
		case errors.Is(err, index.ErrNotFound):
			WriteError(w, fmt.Errorf("%w: repository %s/%s", index.ErrNotFound, r.name, name))
			return
		case err != nil:
			r.deps.Log.ErrorContext(ctx, "repo lookup failed", "repo", name, "error", err.Error())
			WriteError(w, err)
			return
		}
		next.ServeHTTP(w, req.WithContext(WithRepo(ctx, *repo)))
	})
}
