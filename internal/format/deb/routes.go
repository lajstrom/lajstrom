package deb

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"

	"github.com/lajstrom/lajstrom/internal/auth"
	"github.com/lajstrom/lajstrom/internal/format"
	"github.com/lajstrom/lajstrom/internal/index"
)

// Routes mounts the dists/ routes of the apt frontend below /apt/ (docs/spec/http-routing.md
// §3, apt), each a GET (and so a HEAD) that needs read on the repo:
//
//	/{repo}/dists/{suite}/Release                                    the Release of the suite
//	/{repo}/dists/{suite}/InRelease, Release.gpg                     404 until #41 signs the Release
//	/{repo}/dists/{suite}/{component}/binary-{arch}/Packages         and Packages.gz, Packages.xz
//	/{repo}/dists/{suite}/{component}/binary-{arch}/by-hash/SHA256/{hash}
//
// The documents come from index_cache through format.CachedDocument and are served with
// format.ServeDocument (ETag, Last-Modified, If-None-Match, Range). pool/, key.asc and the
// upload route are #41 and #42; until then those paths are 404, and a package reaches an apt
// repo by oras push through the facade, whose hook runs Validate. The handlers render with a
// Plugin that keeps the providers of deps (RenderIndex reads a metadata layer the index left
// out) and the clock of p.
func (p Plugin) Routes(r format.Router, deps format.Deps) {
	if deps.Log == nil {
		deps.Log = slog.Default().With("format", Name)
	}
	svc := &services{providers: deps.Providers}
	if p.svc != nil {
		svc.now = p.svc.now
	}
	h := &handlers{deps: deps, plugin: Plugin{svc: svc}}
	r.HandleFunc("GET /{repo}/dists/{suite}/{file}", h.release)
	r.HandleFunc("GET /{repo}/dists/{suite}/{component}/{dir}/{file}", h.packages)
	r.HandleFunc("GET /{repo}/dists/{suite}/{component}/{dir}/by-hash/SHA256/{hash}", h.byHash)
}

// handlers are the apt routes over one format.Deps.
type handlers struct {
	deps   format.Deps
	plugin Plugin
}

// start returns the repo of the request once the principal may read it; otherwise it answers
// the error and ok is false.
func (h *handlers) start(w http.ResponseWriter, r *http.Request) (repo index.Repo, ok bool) {
	repo, ok = format.RepoFrom(r.Context())
	if !ok {
		h.fail(w, r, errors.New("deb: the route has no repo"))
		return index.Repo{}, false
	}
	if err := format.Authorize(r.Context(), h.deps, auth.ActionRead, repo); err != nil {
		h.fail(w, r, err)
		return index.Repo{}, false
	}
	return repo, true
}

// fail answers err with format.WriteError and logs it when the status is a 5xx, whose cause the
// client does not see.
func (h *handlers) fail(w http.ResponseWriter, r *http.Request, err error) {
	if status := format.WriteError(w, err); status >= http.StatusInternalServerError {
		h.deps.Log.ErrorContext(r.Context(), "apt: request failed", "method", r.Method, "path", r.URL.EscapedPath(),
			"status", status, "error", err.Error())
	}
}

// release serves dists/{suite}/Release. InRelease and Release.gpg are the signed forms, which
// #41 renders in the same scope from the bytes of this Release; until then they are 404, and
// apt takes the unsigned Release from a source marked [trusted=yes].
func (h *handlers) release(w http.ResponseWriter, r *http.Request) {
	repo, ok := h.start(w, r)
	if !ok {
		return
	}
	suite, file := r.PathValue("suite"), r.PathValue("file")
	switch file {
	case kindRelease:
		h.serve(w, r, repo, suite, kindRelease)
	case "InRelease", "Release.gpg":
		h.fail(w, r, fmt.Errorf("%w: apt/%s dists/%s/%s: the Release is not signed until #41; use deb [trusted=yes] until then",
			index.ErrNotFound, repo.Name, suite, file))
	default:
		h.fail(w, r, notFound(repo, suite, file))
	}
}

// packages serves dists/{suite}/{component}/{dir}/{file}: a Packages, Packages.gz or
// Packages.xz that the Release of the suite lists.
func (h *handlers) packages(w http.ResponseWriter, r *http.Request) {
	repo, ok := h.start(w, r)
	if !ok {
		return
	}
	kind := r.PathValue("component") + "/" + r.PathValue("dir") + "/" + r.PathValue("file")
	h.serve(w, r, repo, r.PathValue("suite"), kind)
}

// serve answers the document kind of the suite. The Release comes first, from index_cache or
// rendered with the whole suite, and a kind it does not list is 404 without a render: the
// documents of a suite are stored together, so the Release is the list of what the cache holds,
// and a request for a component or an architecture that the suite does not have does not
// render the suite again on every try.
func (h *handlers) serve(w http.ResponseWriter, r *http.Request, repo index.Repo, suite, kind string) {
	c, err := h.cached(r.Context(), repo, suite, kindRelease)
	if err == nil && kind != kindRelease {
		if _, ok := listed(c.Content)[kind]; !ok {
			err = notFound(repo, suite, kind)
		} else {
			c, err = h.cached(r.Context(), repo, suite, kind)
		}
	}
	if err != nil {
		h.fail(w, r, err)
		return
	}
	format.ServeDocument(w, r, c, contentType(kind))
}

// byHash serves dists/{suite}/{component}/{dir}/by-hash/SHA256/{hash}: the Packages,
// Packages.gz or Packages.xz of that directory whose SHA256 the current Release lists as hash
// (apt asks for it so when the Release says Acquire-By-Hash: yes). A hash of an older render is
// 404, and apt falls back to the file by its name.
func (h *handlers) byHash(w http.ResponseWriter, r *http.Request) {
	repo, ok := h.start(w, r)
	if !ok {
		return
	}
	suite, hash := r.PathValue("suite"), r.PathValue("hash")
	dir := r.PathValue("component") + "/" + r.PathValue("dir") + "/"
	rel, err := h.cached(r.Context(), repo, suite, kindRelease)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	kind := ""
	for path, sum := range listed(rel.Content) {
		if sum == hash && strings.HasPrefix(path, dir) && !strings.Contains(path[len(dir):], "/") {
			kind = path
		}
	}
	var c *index.Cache
	if kind != "" {
		c, err = h.cached(r.Context(), repo, suite, kind)
	}
	switch {
	case err != nil:
		h.fail(w, r, err)
	case c == nil || c.SHA256 != hash: // not listed, or rendered again since the Release was read
		h.fail(w, r, fmt.Errorf("%w: apt/%s dists/%s/%s has no index with the SHA256 %s in the current Release",
			index.ErrNotFound, repo.Name, suite, strings.TrimSuffix(dir, "/"), hash))
	default:
		format.ServeDocument(w, r, c, contentType(kind))
	}
}

// cached returns the document kind of the suite from index_cache, rendered with the whole
// suite on a miss (format.CachedDocument). Nothing here invalidates: ingest and delete
// invalidate every document of the repo in the transaction that changes its versions, so the
// first GET after a publish renders the suite again. On Postgres a render that read the
// versions before such a commit can store documents without the new version until the next
// invalidation; format.CachedDocument says so, and the replicas criteria (A-09) need that
// window closed there.
func (h *handlers) cached(ctx context.Context, repo index.Repo, suite, kind string) (*index.Cache, error) {
	return format.CachedDocument(ctx, h.deps.Index, h.plugin, repo, format.IndexScope(suite), kind)
}

// notFound is the error for a document a suite does not have.
func notFound(repo index.Repo, suite, kind string) error {
	return fmt.Errorf("%w: apt/%s has no dists/%s/%s", index.ErrNotFound, repo.Name, suite, kind)
}
