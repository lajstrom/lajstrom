package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/lajstrom/lajstrom/internal/auth"
	"github.com/lajstrom/lajstrom/internal/format"
	"github.com/lajstrom/lajstrom/internal/index"
	"github.com/lajstrom/lajstrom/internal/provider"
)

// Deleter deletes a manifest with its referrers, its rows in the index and an audit row: the
// ingest pipeline (format.Publisher.Delete). The API's own interface, as the facade has its
// Ingester, so that a test can stand in for it.
type Deleter interface {
	Delete(ctx context.Context, repo index.Repo, del format.Deletion, principal auth.Principal) (*format.Deleted, error)
}

// deleteVersion answers DELETE /api/v1/repos/{format}/{repo}/packages/{name}/versions/{version}
// (docs/spec/http-routing.md §4): delete on the repo is checked before anything is looked up;
// then every variant of the version, or the one ?variant= names, is deleted through the
// Deleter by its digest, its referrers first, each with an audit row of its own. 204. A repo,
// package, version or variant that does not exist is 404; a backend that cannot delete
// manifests 501 UNAVAILABLE naming the registry, before anything changed; the versions deleted
// before another failure stay deleted.
func (h *handler) deleteVersion(_ http.ResponseWriter, r *http.Request) (int, any, error) {
	f, name := r.PathValue("format"), r.PathValue("repo")
	if err := h.allow(r, auth.ActionDelete, f, name); err != nil {
		return 0, nil, err
	}
	if h.deleter == nil {
		return 0, nil, &clientError{msg: "deleting versions is not enabled in this server", err: format.ErrUnsupported}
	}
	ctx := r.Context()
	repo, err := h.repos.Index.GetRepo(ctx, f, name)
	if err != nil {
		return 0, nil, repoError("delete a version in", f, name, err)
	}
	pkg, versions, err := h.versionsOf(ctx, repo, r)
	if err != nil {
		return 0, nil, err
	}
	for _, v := range versions {
		del := format.Deletion{Path: pkg.NameOCI, Reference: v.Digest.String(), ClientIP: clientIP(r)}
		if _, err := h.deleter.Delete(ctx, *repo, del, auth.PrincipalFrom(ctx)); err != nil {
			return 0, nil, versionDeleteError(repo, pkg, v, err)
		}
	}
	return http.StatusNoContent, nil, nil
}

// versionsOf returns the package of the path's {name} in repo and its versions that the path's
// {version} and the query's variant, when it has one, name: index.ErrNotFound naming what is
// missing when there are none.
func (h *handler) versionsOf(ctx context.Context, repo *index.Repo, r *http.Request) (*index.Package, []index.Version, error) {
	name, version := r.PathValue("name"), r.PathValue("version")
	variant, oneVariant := r.URL.Query()["variant"]
	pkg, err := h.repos.Index.GetPackage(ctx, repo.ID, name)
	if errors.Is(err, index.ErrNotFound) {
		return nil, nil, &clientError{msg: fmt.Sprintf("%s/%s has no package %q", repo.Format, repo.Name, name), err: err}
	} else if err != nil {
		return nil, nil, fmt.Errorf("api: package %q of %s/%s: %w", name, repo.Format, repo.Name, err)
	}
	all, err := h.repos.Index.ListVersions(ctx, pkg.ID)
	if err != nil {
		return nil, nil, fmt.Errorf("api: versions of %q in %s/%s: %w", name, repo.Format, repo.Name, err)
	}
	var versions []index.Version
	for _, v := range all {
		if v.Version == version && (!oneVariant || v.Variant == variant[0]) {
			versions = append(versions, v)
		}
	}
	if len(versions) == 0 {
		what := fmt.Sprintf("version %q", version)
		if oneVariant {
			what += fmt.Sprintf(" with the variant %q", variant[0])
		}
		return nil, nil, &clientError{msg: fmt.Sprintf("package %q of %s/%s has no %s", name, repo.Format, repo.Name, what), err: index.ErrNotFound}
	}
	return pkg, versions, nil
}

// versionDeleteError is the answer to a failed delete of the version v: the message is the
// API's, the cause decides the status (format.StatusOf).
func versionDeleteError(repo *index.Repo, pkg *index.Package, v index.Version, err error) error {
	switch {
	case errors.Is(err, provider.ErrUnsupported):
		return &clientError{msg: fmt.Sprintf("the registry %q of %s/%s does not delete manifests; nothing was deleted", repo.Registry, repo.Format, repo.Name), err: err}
	case errors.Is(err, index.ErrNotFound):
		return &clientError{msg: fmt.Sprintf("version %s of %q in %s/%s was deleted meanwhile", v.Version, pkg.Name, repo.Format, repo.Name), err: err}
	case errors.Is(err, provider.ErrConflict):
		return &clientError{msg: fmt.Sprintf("the registry refused to delete version %s of %q, %s: another manifest refers to it", v.Version, pkg.Name, v.Digest), err: err}
	}
	return fmt.Errorf("api: delete version %s of %q in %s/%s: %w", v.Version, pkg.Name, repo.Format, repo.Name, err)
}
