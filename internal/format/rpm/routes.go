package rpm

import (
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"

	"github.com/lajstrom/lajstrom/internal/auth"
	"github.com/lajstrom/lajstrom/internal/format"
	"github.com/lajstrom/lajstrom/internal/index"
	"github.com/lajstrom/lajstrom/internal/provider"
)

// Routes mounts the routes of the rpm frontend below /rpm/ (docs/spec/http-routing.md §3, rpm):
// a GET (and so a HEAD) that needs read on the repo, or the PUT of an upload, which needs write:
//
//	GET /{repo}/repodata/repomd.xml     the repodata, and the documents it lists by location
//	GET /{repo}/repodata/repomd.xml.asc the detached signature of repomd.xml, with the repo's keys
//	GET /{repo}/repodata/repomd.xml.key the keyring of the repo, as zypper fetches it
//	GET /{repo}/key.asc                 the same keyring, for gpgkey=
//	GET /{repo}/Packages/{filename}     a package, through format.ServeBlob
//	PUT /{repo}/upload                  one .rpm, published through deps.Ingest
//
// The documents come from index_cache through format.CachedDocument and are served with
// format.ServeDocument (ETag, Last-Modified, If-None-Match, Range). deps.Signer signs; when it
// is nil (the server has no master key) the signature and the keyring are 404. A package
// reaches an rpm repo through the upload route or by oras push through the facade, whose hook
// runs Validate; ingest invalidates the repo's documents either way, and the next GET renders
// and signs the repodata again. The handlers render with a Plugin that keeps the providers of
// deps (RenderIndex reads a metadata layer the index left out) and the clock of p.
func (p Plugin) Routes(r format.Router, deps format.Deps) {
	if deps.Log == nil {
		deps.Log = slog.Default().With("format", Name)
	}
	svc := &services{providers: deps.Providers}
	if p.svc != nil {
		svc.now = p.svc.now
	}
	h := &handlers{deps: deps, plugin: Plugin{svc: svc}}
	r.HandleFunc("GET /{repo}/repodata/{file}", h.repodata)
	r.HandleFunc("GET /{repo}/key.asc", h.keyring)
	r.HandleFunc("GET /{repo}/Packages/{filename}", h.packageFile)
	r.HandleFunc("PUT /{repo}/upload", h.upload)
}

// handlers are the rpm routes over one format.Deps.
type handlers struct {
	deps   format.Deps
	plugin Plugin
}

// start returns the repo of the request once the principal may perform a on it; otherwise it
// answers the error and ok is false.
func (h *handlers) start(w http.ResponseWriter, r *http.Request, a auth.Action) (repo index.Repo, ok bool) {
	repo, ok = format.RepoFrom(r.Context())
	if !ok {
		h.fail(w, r, errors.New("rpm: the route has no repo"))
		return index.Repo{}, false
	}
	if err := format.Authorize(r.Context(), h.deps, a, repo); err != nil {
		h.fail(w, r, err)
		return index.Repo{}, false
	}
	return repo, true
}

// fail answers err with format.WriteError and logs it when the status is a 5xx, whose cause the
// client does not see.
func (h *handlers) fail(w http.ResponseWriter, r *http.Request, err error) {
	if status := format.WriteError(w, err); status >= http.StatusInternalServerError {
		h.deps.Log.ErrorContext(r.Context(), "rpm: request failed", "method", r.Method, "path", r.URL.EscapedPath(),
			"status", status, "error", err.Error())
	}
}

// registryOf returns the provider of the repo's registry.
func registryOf(deps format.Deps, repo index.Repo) (provider.Provider, error) {
	if deps.Providers != nil {
		if p, ok := deps.Providers.Provider(repo.Registry); ok && p != nil {
			return p, nil
		}
	}
	return nil, fmt.Errorf("rpm: repo rpm/%s: registry %q is not configured", repo.Name, repo.Registry)
}

// ociName is the OCI repository of a package as the provider names it: repo.Prefix, or
// <format>/<repo> when it is empty, and the package path (docs/spec/manifest-profile.md §4.1),
// as ingest computes it.
func ociName(repo index.Repo, pkgPath string) string {
	base := strings.Trim(repo.Prefix, "/")
	if base == "" {
		base = repo.Format + "/" + repo.Name
	}
	return base + "/" + pkgPath
}
