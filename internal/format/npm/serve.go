package npm

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"mime"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/lajstrom/lajstrom/internal/auth"
	"github.com/lajstrom/lajstrom/internal/format"
	"github.com/lajstrom/lajstrom/internal/index"
	"github.com/lajstrom/lajstrom/internal/provider"
	"github.com/lajstrom/lajstrom/internal/server"
)

// get dispatches a GET (and a HEAD) once the principal may read the repo: the packument of a
// package, one of its versions, or a tarball (docs/spec/http-routing.md, npm).
func (h *handlers) get(w http.ResponseWriter, r *http.Request) {
	if segs := special(r.PathValue("path")); segs != nil {
		h.serveSpecial(w, r, segs)
		return
	}
	repo, ok := format.RepoFrom(r.Context())
	if !ok {
		h.fail(w, r, errors.New("npm: the route has no repo"))
		return
	}
	if err := format.Authorize(r.Context(), h.deps, auth.ActionRead, repo); err != nil {
		h.fail(w, r, err)
		return
	}
	t, ok := parseTarget(r.PathValue("path"))
	switch {
	case !ok:
		h.fail(w, r, fmt.Errorf("%w: %s", index.ErrNotFound, r.URL.Path))
	case len(t.rest) == 0:
		h.packument(w, r, repo, t.name)
	case len(t.rest) == 1:
		h.version(w, r, repo, t.name, t.rest[0])
	case len(t.rest) == 2 && t.rest[0] == "-":
		h.tarball(w, r, repo, t.name, t.rest[1])
	default:
		h.fail(w, r, fmt.Errorf("%w: %s", index.ErrNotFound, r.URL.Path))
	}
}

// packument serves the packument of a package: the abbreviated one when Accept names
// application/vnd.npm.install-v1+json with a q above 0 and the query has no write=true, else the
// full one, with ETag (If-None-Match is 304), Vary: Accept and Cache-Control: no-cache.
func (h *handlers) packument(w http.ResponseWriter, r *http.Request, repo index.Repo, name string) {
	kind, contentType := kindPackument, contentTypeJSON
	if abbreviated(r.Header.Values("Accept")) && r.URL.Query().Get("write") != "true" {
		kind, contentType = kindAbbreviated, contentTypeAbbreviated
	}
	w.Header().Add("Vary", "Accept")
	w.Header().Set("Cache-Control", "no-cache")
	document := h.document
	if repo.Kind == index.KindGroup && r.URL.Query().Get("write") == "true" {
		document = h.writeDocument
	}
	c, err := document(r, repo, name, kind)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	format.ServeDocument(w, r, onRepoHost(r, c), contentType)
}

// tarballLink is what precedes the URL of a dist.tarball in a packument as the plugin renders it.
const tarballLink = `"tarball":"`

// onRepoHost returns c with its tarball links below the repo host the request came on in the FQDN
// mode (docs/spec/http-routing.md §1.1), and the ETag of those bytes; c itself on the main host.
// The packument is rendered and kept under server.externalURL, one index_cache row for both.
func onRepoHost(r *http.Request, c *index.Cache) *index.Cache {
	if _, ok := server.RepoHostFrom(r.Context()); !ok {
		return c
	}
	out := *c
	out.Content = server.ClientLinks(r.Context(), c.Content, tarballLink)
	sum := sha256.Sum256(out.Content)
	out.SHA256 = hex.EncodeToString(sum[:])
	return &out
}

// abbreviated reports whether the Accept headers name the abbreviated packument with a q above 0.
func abbreviated(accept []string) bool {
	for _, header := range accept {
		for _, part := range strings.Split(header, ",") {
			mediaType, params, err := mime.ParseMediaType(strings.TrimSpace(part))
			if err != nil || mediaType != contentTypeAbbreviated {
				continue
			}
			q, err := strconv.ParseFloat(params["q"], 64)
			if params["q"] == "" || (err == nil && q > 0) {
				return true
			}
		}
	}
	return false
}

// document returns the packument of kind of the package: from index_cache, rendered on a miss,
// when the plugin has its ExternalURL; without one, rendered for this request with the base of
// its host, and not kept, since another host would see that base. A proxy's and a group's come
// from the core (proxyDocument).
func (h *handlers) document(r *http.Request, repo index.Repo, name, kind string) (*index.Cache, error) {
	if repo.Kind == index.KindProxy || repo.Kind == index.KindGroup {
		return h.proxyDocument(r, repo, name, kind)
	}
	ctx, scope := r.Context(), format.IndexScope(name)
	missing := fmt.Errorf("%w: the package %s is not in %s/%s", index.ErrNotFound, name, repo.Format, repo.Name)
	if h.plugin.ExternalURL != "" {
		c, err := format.CachedDocument(ctx, h.deps.Index, h.plugin, repo, scope, kind)
		if errors.Is(err, index.ErrNotFound) {
			return nil, missing
		}
		return c, err
	}
	p := h.plugin
	p.ExternalURL = requestBase(r)
	docs, err := p.RenderIndex(ctx, repo, scope, h.deps.Index)
	if err != nil {
		return nil, err
	}
	for _, d := range docs {
		if d.Kind == kind {
			sum := sha256.Sum256(d.Content)
			return &index.Cache{RepoID: repo.ID, Scope: name, Kind: kind, Content: d.Content, SHA256: hex.EncodeToString(sum[:]),
				RenderedAt: time.Now().UTC()}, nil
		}
	}
	return nil, missing
}

// version serves one version of the full packument, or the version a dist-tag names.
func (h *handlers) version(w http.ResponseWriter, r *http.Request, repo index.Repo, name, ref string) {
	c, err := h.document(r, repo, name, kindPackument)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	var doc struct {
		DistTags map[string]string          `json:"dist-tags"`
		Versions map[string]json.RawMessage `json:"versions"`
	}
	if err := json.Unmarshal(c.Content, &doc); err != nil {
		h.fail(w, r, fmt.Errorf("npm: the packument of %s: %w", name, err))
		return
	}
	v, ok := doc.Versions[ref]
	if !ok {
		v, ok = doc.Versions[doc.DistTags[ref]]
	}
	if !ok {
		h.fail(w, r, fmt.Errorf("%w: %s has no version or dist-tag %s", index.ErrNotFound, name, ref))
		return
	}
	v = server.ClientLinks(r.Context(), v, tarballLink)
	w.Header().Set("Content-Type", contentTypeJSON)
	w.Header().Set("Content-Length", strconv.Itoa(len(v)))
	if r.Method != http.MethodHead {
		_, _ = w.Write(v)
	}
}

// requestBase is the base of the links of a packument rendered for one request: the request's
// scheme and host.
func requestBase(r *http.Request) string {
	if r.TLS != nil {
		return "https://" + r.Host
	}
	return "http://" + r.Host
}

// tarball serves the tarball file of the package's version whose tarball layer has that title,
// as the facade serves a blob (format.ServeBlob): 307 to the backend's storage when it redirects
// and policy.blobRedirect allows it, else streamed. ETag the blob digest, Last-Modified the
// version's created, Docker-Content-Digest the manifest digest. A proxy's file comes from the
// core, fetched from the upstream on a miss, and a group's from the member that holds it
// (proxyTarball), whose registry serves it.
func (h *handlers) tarball(w http.ResponseWriter, r *http.Request, repo index.Repo, name, file string) {
	ctx := r.Context()
	find := h.hostedTarball
	if repo.Kind == index.KindProxy || repo.Kind == index.KindGroup {
		find = h.proxyTarball
	}
	m, err := find(ctx, repo, name, file)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	if m.Package.RepoID != repo.ID {
		member, err := h.deps.Index.GetRepoByID(ctx, m.Package.RepoID)
		if err != nil {
			h.fail(w, r, fmt.Errorf("npm: the member of %s/%s that holds %s: %w", repo.Format, repo.Name, file, err))
			return
		}
		repo = *member
	}
	prov, err := h.registry(ctx, repo)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	err = format.ServeBlob(w, r, prov, ociName(repo, m.Package.NameOCI), m.File.Digest, format.ServeBlobOptions{
		ContentType: "application/octet-stream", LastModified: m.Version.CreatedAt, ManifestDigest: m.Version.Digest,
		Redirect: format.BlobRedirect(repo), Fail: func(w http.ResponseWriter, err error) { h.fail(w, r, err) },
	})
	if err != nil {
		h.deps.Log.WarnContext(ctx, "npm: the tarball stream failed after the response started", "repo", repo.Format+"/"+repo.Name,
			"file", file, "digest", m.File.Digest.String(), "error", err.Error())
	}
}

// hostedTarball returns the file of the package's version whose tarball is file.
func (h *handlers) hostedTarball(ctx context.Context, repo index.Repo, name, file string) (*index.FileMatch, error) {
	matches, err := h.deps.Index.FindFiles(ctx, repo.ID, file)
	if err != nil {
		return nil, err
	}
	for i := range matches {
		if matches[i].Package.Name == name { // the same file name in @acme/widgets and widgets
			return &matches[i], nil
		}
	}
	return nil, fmt.Errorf("%w: %s has no tarball %s", index.ErrNotFound, name, file)
}

// registry returns the provider of the repo's registry.
func (h *handlers) registry(_ context.Context, repo index.Repo) (provider.Provider, error) {
	if h.deps.Providers != nil {
		if p, ok := h.deps.Providers.Provider(repo.Registry); ok && p != nil {
			return p, nil
		}
	}
	return nil, fmt.Errorf("npm: repo npm/%s: registry %q is not configured", repo.Name, repo.Registry)
}

// ociName is the OCI repository of a package as the provider names it: repo.Prefix, or
// npm/<repo> when it is empty, and the package path, as ingest computes it.
func ociName(repo index.Repo, pkgPath string) string {
	base := strings.Trim(repo.Prefix, "/")
	if base == "" {
		base = repo.Format + "/" + repo.Name
	}
	return base + "/" + pkgPath
}
