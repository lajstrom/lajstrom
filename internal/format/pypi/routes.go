package pypi

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"mime"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"

	"github.com/lajstrom/lajstrom/internal/auth"
	"github.com/lajstrom/lajstrom/internal/format"
	"github.com/lajstrom/lajstrom/internal/index"
	"github.com/lajstrom/lajstrom/internal/provider"
)

// Content types of the files a client downloads.
const (
	contentTypeWheel    = "application/zip"
	contentTypeTarGz    = "application/gzip"
	contentTypeMetadata = "text/plain; charset=utf-8"
	metadataSuffix      = ".metadata"
)

// Routes mounts the pypi frontend below /pypi/ (docs/spec/http-routing.md §3, pypi): the simple
// index, every route a GET (and so a HEAD) that needs read on the repo, and the upload of twine,
// which needs write:
//
//	GET /{repo}/simple/                                      the project list
//	GET /{repo}/simple/{project}/                            the files of a project; another spelling is 301 to the normalized one
//	GET /{repo}/files/{project}/{version}/{filename}         a distribution, through format.ServeBlob
//	GET /{repo}/files/{project}/{version}/{filename}.metadata its core metadata (PEP 658), from the index
//	POST /{repo}/                                            the legacy upload API (upload.go)
//
// The pages come from index_cache through format.CachedDocument, in the form the Accept header
// asks for (PEP 691: JSON, or HTML by default), with Vary: Accept. Ingest drops the repo's
// documents on every publish, so the next GET renders the page again.
func (p Plugin) Routes(r format.Router, deps format.Deps) {
	if deps.Log == nil {
		deps.Log = slog.Default().With("format", Name)
	}
	h := &handlers{deps: deps, plugin: p}
	r.HandleFunc("GET /{repo}/simple/{$}", h.projects)
	r.HandleFunc("GET /{repo}/simple", h.slash)
	r.HandleFunc("GET /{repo}/simple/{project}/{$}", h.project)
	r.HandleFunc("GET /{repo}/simple/{project}", h.slash)
	r.HandleFunc("GET /{repo}/files/{project}/{version}/{filename}", h.file)
	r.HandleFunc("POST /{repo}/{$}", h.upload)
	r.HandleFunc("POST /{repo}", h.upload)
}

// handlers are the pypi routes over one format.Deps.
type handlers struct {
	deps   format.Deps
	plugin Plugin
}

// start returns the repo of the request once the principal may read it; otherwise it answers
// the error and ok is false.
func (h *handlers) start(w http.ResponseWriter, r *http.Request) (index.Repo, bool) {
	repo, ok := format.RepoFrom(r.Context())
	if !ok {
		h.fail(w, r, errors.New("pypi: the route has no repo"))
		return index.Repo{}, false
	}
	if err := format.Authorize(r.Context(), h.deps, auth.ActionRead, repo); err != nil {
		h.fail(w, r, err)
		return index.Repo{}, false
	}
	return repo, true
}

// fail answers err with format.WriteError and logs a 5xx, whose cause the client does not see.
func (h *handlers) fail(w http.ResponseWriter, r *http.Request, err error) {
	if status := format.WriteError(w, err); status >= http.StatusInternalServerError {
		h.deps.Log.ErrorContext(r.Context(), "pypi: request failed", "method", r.Method, "path", r.URL.EscapedPath(),
			"status", status, "error", err.Error())
	}
}

// slash answers a page asked for without its final slash with 301 to the page with it, the
// project normalized.
func (h *handlers) slash(w http.ResponseWriter, r *http.Request) {
	repo, ok := h.start(w, r)
	if !ok {
		return
	}
	http.Redirect(w, r, pageURL(repo, r.PathValue("project")), http.StatusMovedPermanently) //nolint:gosec // a path of this server: pageURL escapes the name
}

// pageURL is the path of the project list, or of the page of project, normalized: built from the
// repo and the name, never from the request's path. It is always a path from the root of this
// server, /pypi/<repo>/simple/[<project>/], the name escaped, so no redirect leaves the host.
func pageURL(repo index.Repo, project string) string {
	u := "/" + Name + "/" + url.PathEscape(repo.Name) + "/simple/"
	if project != "" {
		u += url.PathEscape(Normalize(project)) + "/"
	}
	return u
}

// projects serves the project list.
func (h *handlers) projects(w http.ResponseWriter, r *http.Request) {
	repo, ok := h.start(w, r)
	if !ok {
		return
	}
	h.page(w, r, repo, scopeProjects)
}

// project serves the page of a project: its files, oldest version first. A project name that is
// not normalized is 301 to the normalized one (PEP 503); a project the repo does not have is 404.
func (h *handlers) project(w http.ResponseWriter, r *http.Request) {
	repo, ok := h.start(w, r)
	if !ok {
		return
	}
	project := r.PathValue("project")
	if Normalize(project) != project {
		http.Redirect(w, r, pageURL(repo, project), http.StatusMovedPermanently) //nolint:gosec // a path of this server: pageURL escapes the name
		return
	}
	h.page(w, r, repo, format.IndexScope(scopeProjectsPath+project))
}

// page serves a scope of the simple index in the kind the request accepts.
func (h *handlers) page(w http.ResponseWriter, r *http.Request, repo index.Repo, scope format.IndexScope) {
	w.Header().Add("Vary", "Accept")
	kind, contentType, ok := negotiate(r.Header.Values("Accept"))
	if !ok {
		http.Error(w, "not acceptable: "+contentTypeJSON+", "+contentTypeHTML+" or text/html\n", http.StatusNotAcceptable)
		return
	}
	c, err := format.CachedDocument(r.Context(), h.deps.Index, h.plugin, repo, scope, kind)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	format.ServeDocument(w, r, c, contentType)
}

// offers are what a page can be served as (PEP 691), in the order a tie is broken: the JSON
// form, its HTML form, text/html. A range, */* or text/*, means text/html, the default.
var offers = []struct{ mediaType, kind, contentType string }{
	{contentTypeJSON, kindJSON, contentTypeJSON},
	{contentTypeHTML, kindHTML, contentTypeHTML},
	{"text/html", kindHTML, contentTypeText},
}

// negotiate picks the kind of a page and its Content-Type from the Accept header (PEP 691): the
// offer of the highest quality, ties in the order of offers; HTML when the header is absent. ok
// is false when it accepts none of them (406).
func negotiate(accept []string) (kind, contentType string, ok bool) {
	if len(accept) == 0 {
		return kindHTML, contentTypeText, true
	}
	best, bestQ := -1, 0.0
	for _, a := range accepted(accept) {
		i := slices.IndexFunc(offers, func(o struct{ mediaType, kind, contentType string }) bool { return o.mediaType == a.mediaType })
		if a.mediaType == "*/*" || a.mediaType == "text/*" {
			i = 2
		}
		if i >= 0 && a.q > 0 && (a.q > bestQ || (a.q == bestQ && i < best)) {
			best, bestQ = i, a.q
		}
	}
	if best < 0 {
		return "", "", false
	}
	return offers[best].kind, offers[best].contentType, true
}

// accept is a media range of an Accept header with its quality.
type accept struct {
	mediaType string
	q         float64
}

// accepted reads the media ranges of Accept headers; a range that does not parse is skipped.
func accepted(headers []string) []accept {
	var out []accept
	for _, header := range headers {
		for _, part := range strings.Split(header, ",") {
			mediaType, params, err := mime.ParseMediaType(strings.TrimSpace(part))
			if err != nil {
				continue
			}
			q := 1.0
			if s, has := params["q"]; has {
				if q, err = strconv.ParseFloat(s, 64); err != nil {
					continue
				}
			}
			out = append(out, accept{mediaType, q})
		}
	}
	return out
}

// file serves a distribution of a project, or with the .metadata suffix its core metadata.
func (h *handlers) file(w http.ResponseWriter, r *http.Request) {
	repo, ok := h.start(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	project, version, filename := r.PathValue("project"), r.PathValue("version"), r.PathValue("filename")
	name, metadata := strings.CutSuffix(filename, metadataSuffix)
	matches, err := h.deps.Index.FindFiles(ctx, repo.ID, name)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	var m *index.FileMatch
	for i := range matches {
		if matches[i].Package.Name == Normalize(project) && matches[i].Version.Version == version {
			m = &matches[i]
		}
	}
	if m == nil {
		h.fail(w, r, fmt.Errorf("%w: %s %s has no file %s", index.ErrNotFound, project, version, name))
		return
	}
	if metadata {
		h.metadata(w, r, m)
		return
	}
	prov, err := registryOf(h.deps, repo)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	contentType := contentTypeWheel
	if strings.HasSuffix(name, SdistTarGz) {
		contentType = contentTypeTarGz
	}
	err = format.ServeBlob(w, r, prov, ociName(repo, m.Package.NameOCI), m.File.Digest, format.ServeBlobOptions{
		ContentType: contentType, LastModified: m.Version.CreatedAt, ManifestDigest: m.Version.Digest,
		Redirect: format.BlobRedirect(repo), Filename: name,
		Fail: func(w http.ResponseWriter, err error) { h.fail(w, r, err) },
	})
	if err != nil {
		h.deps.Log.WarnContext(ctx, "pypi: the blob stream failed after the response started", "repo", repo.Format+"/"+repo.Name,
			"file", name, "digest", m.File.Digest.String(), "error", err.Error())
	}
}

// metadata serves the core metadata of a file (PEP 658) from the metadata the index holds: the
// bytes whose sha256 the page announced as data-core-metadata. A version whose metadata the
// index left out has no core-metadata on the page, and is 404 here.
func (h *handlers) metadata(w http.ResponseWriter, r *http.Request, m *index.FileMatch) {
	var md struct {
		CoreMetadata string `json:"core_metadata"`
	}
	if m.Version.Metadata == nil || json.Unmarshal(m.Version.Metadata, &md) != nil || md.CoreMetadata == "" {
		h.fail(w, r, fmt.Errorf("%w: the index has no core metadata of %s", index.ErrNotFound, m.File.Name))
		return
	}
	w.Header().Set("Content-Type", contentTypeMetadata)
	w.Header().Set("Content-Length", strconv.Itoa(len(md.CoreMetadata)))
	w.Header().Set("ETag", `"`+m.Version.Digest.Encoded()+`"`)
	if r.Method == http.MethodHead {
		return
	}
	_, _ = w.Write([]byte(md.CoreMetadata))
}

// registryOf returns the provider of the repo's registry.
func registryOf(deps format.Deps, repo index.Repo) (provider.Provider, error) {
	if deps.Providers != nil {
		if p, ok := deps.Providers.Provider(repo.Registry); ok && p != nil {
			return p, nil
		}
	}
	return nil, fmt.Errorf("pypi: repo pypi/%s: registry %q is not configured", repo.Name, repo.Registry)
}

// ociName is the OCI repository of a package as the provider names it: repo.Prefix, or
// pypi/<repo> when it is empty, and the package path, as ingest computes it.
func ociName(repo index.Repo, pkgPath string) string {
	base := strings.Trim(repo.Prefix, "/")
	if base == "" {
		base = repo.Format + "/" + repo.Name
	}
	return base + "/" + pkgPath
}
