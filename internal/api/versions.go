package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/opencontainers/go-digest"

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
// Deleter by its digest, its referrers first, each with an audit row of its own. 204.
//
// Every delete is conditional (format.Condition), a compare-and-delete: If-Match lists the
// digests the client read, If-Unmodified-Since is its cut-off, and a version a pointer names is
// refused unless ?force=true. The conditions are checked for every variant before the first is
// deleted (conditions.check), and again by ingest for each under its locks. A repo, package,
// version or variant that does not exist is 404; a failed condition 412, or 409 naming the
// pointers, and nothing changed; a backend that cannot delete manifests 501 UNAVAILABLE naming
// the registry, before anything changed; the variants deleted before another failure stay
// deleted.
func (h *handler) deleteVersion(_ http.ResponseWriter, r *http.Request) (int, any, error) {
	f, name := r.PathValue("format"), r.PathValue("repo")
	if err := h.allow(r, auth.ActionDelete, f, name); err != nil {
		return 0, nil, err
	}
	if h.deleter == nil {
		return 0, nil, &clientError{msg: "deleting versions is not enabled in this server", err: format.ErrUnsupported}
	}
	c, err := conditionsOf(r)
	if err != nil {
		return 0, nil, err
	}
	ctx := r.Context()
	repo, err := h.repos.Index.GetRepo(ctx, f, name)
	if err != nil {
		return 0, nil, repoError("delete a version in", f, name, err)
	}
	var variant *string
	if v, ok := r.URL.Query()["variant"]; ok {
		variant = &v[0]
	}
	pkg, versions, err := h.versionOf(ctx, repo, r.PathValue("name"), r.PathValue("version"), variant)
	if err == nil {
		err = c.check(ctx, h.repos.Index, repo, pkg, versions)
	}
	if err == nil {
		err = h.deleteEach(r, repo, pkg, versions, c, "")
	}
	if err != nil {
		return 0, nil, err
	}
	return http.StatusNoContent, nil, nil
}

// versionOf returns the package name of repo and its variants of version, or the one variant
// names when it is not nil: index.ErrNotFound naming what is missing when there are none.
func (h *handler) versionOf(ctx context.Context, repo *index.Repo, name, version string, variant *string) (*index.Package, []index.Version, error) {
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
		if v.Version == version && (variant == nil || v.Variant == *variant) {
			versions = append(versions, v)
		}
	}
	if len(versions) == 0 {
		what := fmt.Sprintf("version %q", version)
		if variant != nil {
			what += fmt.Sprintf(" with the variant %q", *variant)
		}
		return nil, nil, &clientError{msg: fmt.Sprintf("package %q of %s/%s has no %s", name, repo.Format, repo.Name, what), err: index.ErrNotFound}
	}
	return pkg, versions, nil
}

// conditions are the conditions of a delete of a version (format.Condition): the digests the
// client read (nil: none), its cut-off (zero: none) and whether pointers are overruled.
type conditions struct {
	digests []digest.Digest
	since   time.Time
	force   bool
}

// conditionsOf returns the conditions of the request: If-Match, If-Unmodified-Since and
// ?force. A header that does not parse is 400, never ignored: a condition is not dropped
// silently (docs/api.md).
func conditionsOf(r *http.Request) (conditions, error) {
	var c conditions
	var err error
	if c.force, err = boolParam(r, "force"); err != nil {
		return c, err
	}
	if v := r.Header.Values("If-Match"); len(v) > 0 {
		if c.digests, err = parseIfMatch(strings.Join(v, ",")); err != nil {
			return c, err
		}
	}
	if v := r.Header.Get("If-Unmodified-Since"); v != "" {
		if c.since, err = http.ParseTime(v); err != nil {
			return c, invalid("If-Unmodified-Since: %q is not an HTTP-date, like %s", v, time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC).Format(http.TimeFormat))
		}
	}
	return c, nil
}

// parseIfMatch returns the digests of an If-Match list, each a quoted strong entity tag; *
// matches any and returns nil.
func parseIfMatch(v string) ([]digest.Digest, error) {
	if strings.TrimSpace(v) == "*" {
		return nil, nil
	}
	var digests []digest.Digest
	for _, tag := range strings.Split(v, ",") {
		tag = strings.TrimSpace(tag)
		unquoted, prefixed := strings.CutPrefix(tag, `"`)
		unquoted, suffixed := strings.CutSuffix(unquoted, `"`)
		if !prefixed || !suffixed || unquoted == "" {
			return nil, invalid(`If-Match: %q is not a list of quoted digests, like "sha256:…", "sha256:…"`, v)
		}
		d, err := digest.Parse(unquoted)
		if err != nil {
			return nil, invalid("If-Match: %q is not a digest: %s", unquoted, err)
		}
		digests = append(digests, d)
	}
	return digests, nil
}

// check checks c against the variants of a version before any is deleted: a *format.RefusedError
// when the variants do not have exactly the digests the client read (changed), one was
// published after the cut-off (young), or, unforced, a pointer names the version (protected).
// Ingest checks each variant again under its locks.
func (c conditions) check(ctx context.Context, rd index.Reader, repo *index.Repo, pkg *index.Package, versions []index.Version) error {
	subject := fmt.Sprintf("version %s of %q in %s/%s", versions[0].Version, pkg.Name, repo.Format, repo.Name)
	now := make([]digest.Digest, len(versions))
	var last time.Time
	for i, v := range versions {
		now[i] = v.Digest
		if v.CreatedAt.After(last) {
			last = v.CreatedAt
		}
	}
	if c.digests != nil && !sameDigests(c.digests, now) {
		return &format.RefusedError{Reason: format.RefusedChanged, Subject: subject, Digests: now}
	}
	if !c.since.IsZero() && last.Truncate(time.Second).After(c.since) {
		return &format.RefusedError{Reason: format.RefusedYoung, Subject: subject, PublishedAt: last}
	}
	if c.force {
		return nil
	}
	states, err := rd.ListJournalState(ctx, pkg.ID)
	if err != nil {
		return fmt.Errorf("api: dist-tags of %q in %s/%s: %w", pkg.Name, repo.Format, repo.Name, err)
	}
	if p := format.PointersOf(states, versions[0].Version); len(p) > 0 {
		return &format.RefusedError{Reason: format.RefusedProtected, Subject: subject, Pointers: p}
	}
	return nil
}

// sameDigests reports whether a and b hold the same digests, in any order.
func sameDigests(a, b []digest.Digest) bool {
	a, b = slices.Clone(a), slices.Clone(b)
	slices.Sort(a)
	slices.Sort(b)
	return slices.Equal(slices.Compact(a), slices.Compact(b))
}

// deleteEach deletes the variants of a version through the Deleter, each conditional on its
// digest and on c, in the batch job when it is not empty.
func (h *handler) deleteEach(r *http.Request, repo *index.Repo, pkg *index.Package, versions []index.Version, c conditions, job string) error {
	ctx := r.Context()
	for _, v := range versions {
		del := format.Deletion{Path: pkg.NameOCI, Reference: v.Digest.String(), ClientIP: clientIP(r), Job: job,
			Condition: &format.Condition{Version: v.Version, Variant: v.Variant, UnmodifiedSince: c.since, Force: c.force}}
		if _, err := h.deleter.Delete(ctx, *repo, del, auth.PrincipalFrom(ctx)); err != nil {
			return versionDeleteError(repo, pkg, v, err)
		}
	}
	return nil
}

// versionDeleteError is the answer to a failed delete of the version v: the message is the
// API's, the cause decides the status (format.StatusOf). A refusal keeps its own message and
// type, for the batch's status.
func versionDeleteError(repo *index.Repo, pkg *index.Package, v index.Version, err error) error {
	var refused *format.RefusedError
	switch {
	case errors.As(err, &refused):
		return refused
	case errors.Is(err, provider.ErrUnsupported):
		return &clientError{msg: fmt.Sprintf("the registry %q of %s/%s does not delete manifests; nothing was deleted", repo.Registry, repo.Format, repo.Name), err: err}
	case errors.Is(err, index.ErrNotFound):
		return &clientError{msg: fmt.Sprintf("version %s of %q in %s/%s was deleted meanwhile", v.Version, pkg.Name, repo.Format, repo.Name), err: err}
	case errors.Is(err, provider.ErrConflict):
		return &clientError{msg: fmt.Sprintf("the registry refused to delete version %s of %q, %s: another manifest refers to it", v.Version, pkg.Name, v.Digest), err: err}
	}
	return fmt.Errorf("api: delete version %s of %q in %s/%s: %w", v.Version, pkg.Name, repo.Format, repo.Name, err)
}
