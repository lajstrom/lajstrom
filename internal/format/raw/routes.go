package raw

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/opencontainers/go-digest"

	"github.com/lajstrom/lajstrom/internal/auth"
	"github.com/lajstrom/lajstrom/internal/format"
	"github.com/lajstrom/lajstrom/internal/index"
	"github.com/lajstrom/lajstrom/internal/provider"
	"github.com/lajstrom/lajstrom/internal/server"
)

// Routes mounts the raw frontend below /raw/ (docs/spec/http-routing.md §3, raw):
//
//	PUT    /{repo}/{path...}  upload one file (write)
//	GET    /{repo}/{path...}  the newest version of the file, or the one ?digest= names; HEAD
//	                          too; a path that is empty or ends with "/" is the listing (read)
//	DELETE /{repo}/{path...}  every version of the path (delete)
//
// Every handler asks format.Authorize first (a nil deps.Auth is the open dev stack of M0) and
// answers errors through format.WriteError; a 5xx is logged with its cause.
func (Plugin) Routes(r format.Router, deps format.Deps) {
	if deps.Log == nil {
		deps.Log = slog.Default().With("format", Name)
	}
	h := &handlers{deps: deps}
	r.HandleFunc("PUT /{repo}/{path...}", h.put)
	r.HandleFunc("GET /{repo}/{path...}", h.get)
	r.HandleFunc("DELETE /{repo}/{path...}", h.delete)
}

// handlers are the raw routes over one format.Deps.
type handlers struct {
	deps format.Deps
}

// start returns the repo of the request and the path below it once the principal may perform
// a on the repo; otherwise it answers the error and ok is false.
func (h *handlers) start(w http.ResponseWriter, r *http.Request, a auth.Action) (repo index.Repo, p string, ok bool) {
	repo, ok = format.RepoFrom(r.Context())
	if !ok {
		h.fail(w, r, errors.New("raw: the route has no repo"))
		return index.Repo{}, "", false
	}
	if err := format.Authorize(r.Context(), h.deps, a, repo); err != nil {
		h.fail(w, r, err)
		return index.Repo{}, "", false
	}
	return repo, r.PathValue("path"), true
}

// fail answers err with format.WriteError and logs it when the status is a 5xx, whose cause the
// client does not see.
func (h *handlers) fail(w http.ResponseWriter, r *http.Request, err error) {
	if status := format.WriteError(w, err); status >= http.StatusInternalServerError {
		h.deps.Log.ErrorContext(r.Context(), "raw: request failed", "method", r.Method, "path", r.URL.EscapedPath(),
			"status", status, "error", err.Error())
	}
}

// putResponse is the body of a successful PUT.
type putResponse struct {
	Path    string `json:"path"`
	Version string `json:"version"`
	Digest  string `json:"digest"` // the manifest digest
	Size    int64  `json:"size"`
	Created string `json:"created,omitempty"` // RFC 3339 UTC; empty when the index did not answer
}

// put uploads the request body as the file at the path: the body, bounded by the repo policy's
// maxFileSize (format.LimitBody, 413 above it), is spooled to a temporary file for the request
// and published through deps.Ingest, and the answer is 201 with Location (the request path),
// Docker-Content-Digest (the manifest digest) and a JSON body. The same bytes again are the
// idempotent repeat of ingest, answered alike with the digest of the manifest stored first. The
// spool is removed when the handler returns, whatever the outcome. A path that another spelling
// holds is 409 before the body is read (checkSpelling).
func (h *handlers) put(w http.ResponseWriter, r *http.Request) {
	repo, p, ok := h.start(w, r, auth.ActionWrite)
	if !ok {
		return
	}
	if err := checkSpelling(r.Context(), h.deps.Index, repo, p); err != nil {
		h.fail(w, r, err)
		return
	}
	if h.deps.Ingest == nil {
		h.fail(w, r, errors.New("raw: no publisher in format.Deps"))
		return
	}
	body, err := format.LimitBody(w, r, repo)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	sp, err := format.Spool(body, p, r.Header.Get("Content-Type"), r.ContentLength)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	defer func() {
		if err := sp.Close(); err != nil {
			h.deps.Log.WarnContext(r.Context(), "raw: upload spool not removed", "error", err.Error())
		}
	}()
	principal := auth.PrincipalFrom(r.Context())
	file := sp.File()
	pub, err := h.deps.Ingest.Upload(r.Context(), repo, format.Upload{
		Files:  []format.UploadedFile{file},
		Params: map[string]string{ParamPath: p, ParamUploadedBy: principal.String()},
	}, principal)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	resp := putResponse{Path: p, Version: pub.Entry.Version, Digest: pub.Digest.String(), Size: file.Size}
	if v, err := h.deps.Index.GetVersionByDigest(r.Context(), repo.ID, pub.Digest); err == nil {
		resp.Created = v.CreatedAt.UTC().Format(time.RFC3339)
	} else {
		h.deps.Log.WarnContext(r.Context(), "raw: published version not read back", "path", p, "digest", pub.Digest.String(), "error", err.Error())
	}
	hdr := w.Header()
	hdr.Set("Location", r.URL.EscapedPath())
	hdr.Set("Docker-Content-Digest", pub.Digest.String())
	hdr.Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(resp)
}

// checkSpelling applies the path rules to p (repoPath) and the rule that a raw path is unique
// without regard to case (docs/spec/manifest-profile.md §4.1, raw): OCI repository names are
// lowercase, so Docs/Readme.MD and docs/readme.md are one repository, and the first spelling
// holds it. A package of the repo with p's package path and another name is index.ErrConflict
// naming it (409).
//
// The check runs here and not in Build, whose signature (format.Plugin) has no index, and before
// the spool, so that a refused upload reads no byte of the body. Two spellings uploaded at once
// both pass it; the unique index on packages (repo_id, name_oci) settles that race in the
// pipeline, whose UpsertPackage answers the loser ErrConflict after its manifest is in the
// registry, for reindex (#29) to report.
func checkSpelling(ctx context.Context, st index.Reader, repo index.Repo, p string) error {
	pkgPath, _, err := repoPath(repo, p)
	if err != nil {
		return err
	}
	pkg, err := st.GetPackageByNameOCI(ctx, repo.ID, pkgPath)
	switch {
	case errors.Is(err, index.ErrNotFound):
		return nil
	case err != nil:
		return fmt.Errorf("raw: package path %q: %w", pkgPath, err)
	case pkg.Name != p:
		return fmt.Errorf("%w: path %q exists as %q; a raw path is unique without regard to case", index.ErrConflict, p, pkg.Name)
	}
	return nil
}

// get serves the file at the path, or the listing when the path is empty or ends with "/".
func (h *handlers) get(w http.ResponseWriter, r *http.Request) {
	repo, p, ok := h.start(w, r, auth.ActionRead)
	if !ok {
		return
	}
	if p == "" || strings.HasSuffix(p, "/") {
		h.list(w, r, repo, p)
		return
	}
	f, err := resolve(r.Context(), h.deps.Index, repo, p, r.URL.Query().Get("digest"))
	if err != nil {
		h.fail(w, r, err)
		return
	}
	h.serve(w, r, repo, f)
}

// list serves the listing of a directory prefix from index_cache, rendered on a miss; ingest
// and delete invalidate the repo's documents.
func (h *handlers) list(w http.ResponseWriter, r *http.Request, repo index.Repo, prefix string) {
	c, err := format.CachedDocument(r.Context(), h.deps.Index, Plugin{}, repo, format.IndexScope(prefix), kindListing)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	format.ServeDocument(w, r, c, listingContentType)
}

// resolved is a file of a raw repo as the index knows it.
type resolved struct {
	pkg         index.Package
	version     index.Version
	file        index.File
	contentType string
}

// resolve finds the file at path p: the newest version of the package (by created, then id),
// or the version whose manifest digest is pin, which must belong to the package. Everything
// missing is index.ErrNotFound; a pin that is not a digest is format.ErrInvalid.
func resolve(ctx context.Context, st index.Reader, repo index.Repo, p, pin string) (*resolved, error) {
	notFound := fmt.Errorf("%w: raw/%s has no file %q", index.ErrNotFound, repo.Name, p)
	pkg, err := st.GetPackage(ctx, repo.ID, p)
	if errors.Is(err, index.ErrNotFound) {
		return nil, notFound
	}
	if err != nil {
		return nil, fmt.Errorf("raw: package %q: %w", p, err)
	}
	v, err := pickVersion(ctx, st, repo, pkg, pin)
	if err != nil {
		return nil, err
	}
	if v == nil {
		return nil, notFound
	}
	files, err := st.ListFiles(ctx, v.ID)
	switch {
	case err != nil:
		return nil, fmt.Errorf("raw: files of %q %s: %w", p, v.Version, err)
	case len(files) != 1:
		return nil, fmt.Errorf("raw: %q %s has %d files in the index, want 1", p, v.Version, len(files))
	}
	return &resolved{pkg: *pkg, version: *v, file: files[0], contentType: contentTypeOf(v.Metadata)}, nil
}

// pickVersion returns the version of pkg that pin names, or the newest without a pin; nil when
// there is none.
func pickVersion(ctx context.Context, st index.Reader, repo index.Repo, pkg *index.Package, pin string) (*index.Version, error) {
	if pin == "" {
		versions, err := st.ListVersions(ctx, pkg.ID)
		if err != nil || len(versions) == 0 {
			return nil, err
		}
		return &versions[len(versions)-1], nil // ordered by created, then id
	}
	d, err := digest.Parse(pin)
	if err != nil {
		return nil, fmt.Errorf("%w: digest %q: %w", format.ErrInvalid, pin, err)
	}
	v, err := st.GetVersionByDigest(ctx, repo.ID, d)
	switch {
	case errors.Is(err, index.ErrNotFound):
		return nil, nil
	case err != nil:
		return nil, fmt.Errorf("raw: version %s: %w", d, err)
	case v.PackageID != pkg.ID:
		return nil, nil
	}
	return v, nil
}

// serve answers the file f through format.ServeBlob: Content-Type from the metadata, ETag the
// blob digest (so If-None-Match works without a request to the backend), Last-Modified the
// version's created, Docker-Content-Digest the manifest digest, and the redirect as the repo
// policy's blobRedirect allows. HEAD asks the backend for the headers; GET is one request,
// with the client's Range: a 307 to the backend's storage when the provider gives a URL,
// otherwise the backend's answer, 200, 206 or 416, streamed. A stream that breaks after the
// headers is logged.
func (h *handlers) serve(w http.ResponseWriter, r *http.Request, repo index.Repo, f *resolved) {
	ctx := r.Context()
	prov, err := registryOf(h.deps, repo)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	err = format.ServeBlob(w, r, prov, ociName(repo, f.pkg.NameOCI), f.file.Digest, format.ServeBlobOptions{
		ContentType:    f.contentType,
		LastModified:   f.version.CreatedAt,
		ManifestDigest: f.version.Digest,
		Redirect:       format.BlobRedirect(repo),
		Fail:           func(w http.ResponseWriter, err error) { h.fail(w, r, err) },
	})
	if err != nil {
		h.deps.Log.WarnContext(ctx, "raw: the blob stream failed after the response started", "repo", repo.Format+"/"+repo.Name,
			"path", f.pkg.Name, "digest", f.file.Digest.String(), "error", err.Error())
	}
}

// registryOf returns the provider of the repo's registry.
func registryOf(deps format.Deps, repo index.Repo) (provider.Provider, error) {
	if deps.Providers != nil {
		if p, ok := deps.Providers.Provider(repo.Registry); ok && p != nil {
			return p, nil
		}
	}
	return nil, fmt.Errorf("raw: repo %s/%s: registry %q is not configured", repo.Format, repo.Name, repo.Registry)
}

// ociName is the OCI repository of a package as the provider names it: repo.Prefix, or
// <format>/<repo> when it is empty, and the package path (as ingest computes it;
// docs/spec/manifest-profile.md §4.1).
func ociName(repo index.Repo, pkgPath string) string {
	base := strings.Trim(repo.Prefix, "/")
	if base == "" {
		base = repo.Format + "/" + repo.Name
	}
	return base + "/" + pkgPath
}

// delete removes every version of the path (deletePath).
func (h *handlers) delete(w http.ResponseWriter, r *http.Request) {
	repo, p, ok := h.start(w, r, auth.ActionDelete)
	if !ok {
		return
	}
	who := actor{principal: auth.PrincipalFrom(r.Context()), ip: clientIP(r)}
	if err := deletePath(r.Context(), h.deps, repo, p, who); err != nil {
		h.fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// clientIP is the address of the client without the port: the server's (server.ClientAddr,
// which believes X-Forwarded-For from server.trustedProxies only, ADR 0011), or the peer of the
// connection for a request that did not come through the server's middleware.
func clientIP(r *http.Request) string {
	if addr := server.ClientAddr(r.Context()); addr != "" {
		return addr
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}
