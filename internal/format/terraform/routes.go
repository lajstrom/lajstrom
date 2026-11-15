package terraform

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
	"github.com/lajstrom/lajstrom/internal/provider"
	"github.com/lajstrom/lajstrom/internal/server"
)

// jsonContentType is the Content-Type of a service discovery document and of a versions
// document.
const jsonContentType = "application/json"

// Routes mounts the routes of the terraform frontend below /terraform/
// (docs/spec/http-routing.md §3, terraform): GETs (and so HEADs) that need read on the repo, and
// the PUT of a publish, which needs write:
//
//	GET /{repo}/.well-known/terraform.json                          the service discovery of a repo host
//	GET /{repo}/{name}/{system}/versions                            the versions of a module
//	GET /{repo}/{name}/{system}/{version}/download                  204 with X-Terraform-Get
//	GET /{repo}/{namespace}/{name}/{system}/versions                the same, the namespace the repo's name:
//	GET /{repo}/{namespace}/{name}/{system}/{version}/download      the routes Terraform asks on a repo host
//	GET /{repo}/{name}/{system}/{version}/{file}                    the archive, through format.ServeBlob
//	PUT /{repo}/{name}/{system}/{version}                           one archive, published through deps.Ingest
//
// The service discovery of the main host, /.well-known/terraform.json, is WellKnown's, which the
// server mounts. The versions documents come from index_cache through format.CachedDocument;
// ingest and delete invalidate them.
func (p Plugin) Routes(r format.Router, deps format.Deps) {
	if deps.Log == nil {
		deps.Log = slog.Default().With("format", Name)
	}
	h := &handlers{deps: deps}
	r.HandleFunc("GET /{repo}/.well-known/terraform.json", h.repoDiscovery)
	r.HandleFunc("GET /{repo}/{name}/{system}/versions", h.versions)
	r.HandleFunc("GET /{repo}/{name}/{system}/{version}/download", h.download)
	r.HandleFunc("GET /{repo}/{namespace}/{name}/{system}/versions", h.versions)
	r.HandleFunc("GET /{repo}/{namespace}/{name}/{system}/{version}/download", h.download)
	r.HandleFunc("GET /{repo}/{name}/{system}/{version}/{file}", h.archive)
	r.HandleFunc("PUT /{repo}/{name}/{system}/{version}", h.publish)
}

// WellKnown returns the documents of the host's /.well-known/ that the plugin serves, by name:
// terraform.json, the service discovery of the main host, {"modules.v1":"/terraform/"}, to any
// principal. The server mounts them as GET /.well-known/<name>; on a repo host the path is the
// repo's (Routes).
func (Plugin) WellKnown() map[string]http.Handler {
	return map[string]http.Handler{"terraform.json": http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeDiscovery(w, "/"+Name+"/")
	})}
}

// discovery is a service discovery document: the base URL of the module registry protocol.
type discovery struct {
	ModulesV1 string `json:"modules.v1"`
}

// writeDiscovery answers the service discovery document with the base modules.
func writeDiscovery(w http.ResponseWriter, modules string) {
	b, _ := json.Marshal(discovery{ModulesV1: modules}) // a struct of one string marshals
	w.Header().Set("Content-Type", jsonContentType)
	_, _ = w.Write(b)
}

// handlers are the terraform routes over one format.Deps.
type handlers struct {
	deps format.Deps
}

// start returns the repo of the request once the principal may perform a on it; otherwise it
// answers the error and ok is false.
func (h *handlers) start(w http.ResponseWriter, r *http.Request, a auth.Action) (repo index.Repo, ok bool) {
	repo, ok = format.RepoFrom(r.Context())
	if !ok {
		h.fail(w, r, errors.New("terraform: the route has no repo"))
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
		h.deps.Log.Log(r.Context(), server.FailLevel(r), "terraform: request failed", "method", r.Method, "path", r.URL.EscapedPath(),
			"status", status, "error", err.Error())
	}
}

// repoDiscovery answers the service discovery of a repo host: modules.v1 is the root of the
// repo's tree on the host of the request, / on a repo host, /terraform/<repo>/ on the main host.
// A module address on a repo host keeps the repo as its namespace, which the routes with
// {namespace} take.
func (h *handlers) repoDiscovery(w http.ResponseWriter, r *http.Request) {
	repo, ok := h.start(w, r, auth.ActionRead)
	if !ok {
		return
	}
	modules := "/" + Name + "/" + repo.Name + "/"
	if _, onRepoHost := server.RepoHostFrom(r.Context()); onRepoHost {
		modules = "/"
	}
	writeDiscovery(w, modules)
}

// module returns the module of a read route, the namespace being the repo's name in the routes
// that have one; anything else is index.ErrNotFound, since no module of the repo has that path.
func module(r *http.Request, repo index.Repo) (Module, error) {
	ns, name, system := r.PathValue("namespace"), r.PathValue("name"), r.PathValue("system")
	m, err := ParseModule(name, system)
	if err != nil || (ns != "" && ns != repo.Name) {
		return Module{}, fmt.Errorf("%w: terraform/%s has no module %s/%s/%s", index.ErrNotFound, repo.Name, ns, name, system)
	}
	return m, nil
}

// registryOf returns the provider of the repo's registry.
func registryOf(deps format.Deps, repo index.Repo) (provider.Provider, error) {
	if deps.Providers != nil {
		if p, ok := deps.Providers.Provider(repo.Registry); ok && p != nil {
			return p, nil
		}
	}
	return nil, fmt.Errorf("terraform: repo terraform/%s: registry %q is not configured", repo.Name, repo.Registry)
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
