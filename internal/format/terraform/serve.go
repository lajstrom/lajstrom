package terraform

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/lajstrom/lajstrom/internal/auth"
	"github.com/lajstrom/lajstrom/internal/format"
	"github.com/lajstrom/lajstrom/internal/index"
	"github.com/lajstrom/lajstrom/internal/server"
)

// archiveContentType is the Content-Type of a module archive.
const archiveContentType = "application/gzip"

// resolved is a version of a module and its archive, as the index knows them.
type resolved struct {
	pkg     index.Package
	version index.Version
	file    index.File
}

// resolve finds version v of the module m in the repo: the package at the module's package path,
// so that a spelling of the name in another case finds it, and its version v, which has one file,
// the archive. Everything missing, and a v that is no version of CheckVersion, is
// index.ErrNotFound.
func (h *handlers) resolve(ctx context.Context, repo index.Repo, m Module, v string) (*resolved, error) {
	notFound := fmt.Errorf("%w: terraform/%s has no version %s of %s", index.ErrNotFound, repo.Name, v, m.PackageName())
	if CheckVersion(v) != nil {
		return nil, notFound
	}
	pkg, err := index.PackageAt(ctx, h.deps.Index, repo.ID, m.packagePath())
	if errors.Is(err, index.ErrNotFound) {
		return nil, notFound
	}
	if err != nil {
		return nil, fmt.Errorf("terraform: the module %s: %w", m.PackageName(), err)
	}
	ver, err := h.deps.Index.GetVersion(ctx, pkg.ID, v, "")
	if errors.Is(err, index.ErrNotFound) {
		return nil, notFound
	}
	if err != nil {
		return nil, fmt.Errorf("terraform: version %s of %s: %w", v, pkg.Name, err)
	}
	files, err := h.deps.Index.ListFiles(ctx, ver.ID)
	switch {
	case err != nil:
		return nil, fmt.Errorf("terraform: the files of %s %s: %w", pkg.Name, v, err)
	case len(files) != 1:
		return nil, fmt.Errorf("terraform: %s %s has %d files in the index, want 1", pkg.Name, v, len(files))
	}
	return &resolved{pkg: *pkg, version: *ver, file: files[0]}, nil
}

// archivePath returns the path of the archive route of version v of m, the file being the
// archive's title: /terraform/<repo>/<name>/<system>/<version>/<file>, without /terraform/<repo>
// on a repo host, as a Location is written there (docs/spec/http-routing.md §1.1).
func archivePath(ctx context.Context, repo index.Repo, m Module, v, file string) string {
	p := "/" + strings.Join([]string{m.Name, m.System, v, file}, "/")
	if _, onRepoHost := server.RepoHostFrom(ctx); onRepoHost {
		return p
	}
	return "/" + Name + "/" + repo.Name + p
}

// download answers the download of a module version: 204 with X-Terraform-Get, the path of the
// version's archive, which Terraform resolves against the URL of the download and fetches with
// go-getter, which takes it as a gzip-compressed tar by its extension.
func (h *handlers) download(w http.ResponseWriter, r *http.Request) {
	repo, ok := h.start(w, r, auth.ActionRead)
	if !ok {
		return
	}
	m, err := module(r, repo)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	v := r.PathValue("version")
	f, err := h.resolve(r.Context(), repo, m, v)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	w.Header().Set("X-Terraform-Get", archivePath(r.Context(), repo, m, v, f.file.Name))
	w.WriteHeader(http.StatusNoContent)
}

// archive serves the archive of a module version, whose title the path names, through
// format.ServeBlob: one request to the backend at most, a 307 to the backend's storage when the
// provider gives a URL and the repo's policy.blobRedirect allows it, else the backend's 200, 206
// or 416 streamed. The ETag is the blob digest, Last-Modified the version's created,
// Docker-Content-Digest its manifest digest. HEAD asks the backend for the headers. A stream that
// breaks after the headers is logged.
func (h *handlers) archive(w http.ResponseWriter, r *http.Request) {
	repo, ok := h.start(w, r, auth.ActionRead)
	if !ok {
		return
	}
	ctx := r.Context()
	m, err := module(r, repo)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	v, file := r.PathValue("version"), r.PathValue("file")
	f, err := h.resolve(ctx, repo, m, v)
	if err == nil && f.file.Name != file {
		err = fmt.Errorf("%w: version %s of %s has no file %s; its archive is %s", index.ErrNotFound, v, m.PackageName(), file, f.file.Name)
	}
	if err != nil {
		h.fail(w, r, err)
		return
	}
	prov, err := registryOf(h.deps, repo)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	err = format.ServeBlob(w, r, prov, ociName(repo, index.ManifestPath(f.pkg, f.version)), f.file.Digest, format.ServeBlobOptions{
		ContentType:    archiveContentType,
		LastModified:   f.version.CreatedAt,
		ManifestDigest: f.version.Digest,
		Redirect:       format.BlobRedirect(repo),
		Fail:           func(w http.ResponseWriter, err error) { h.fail(w, r, err) },
	})
	if err != nil {
		h.deps.Log.WarnContext(ctx, "terraform: the blob stream failed after the response started", "repo", repo.Format+"/"+repo.Name,
			"file", f.file.Name, "digest", f.file.Digest.String(), "error", err.Error())
	}
}
