package facade

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"

	"github.com/lajstrom/lajstrom/internal/auth"
	"github.com/lajstrom/lajstrom/internal/auth/token"
	"github.com/lajstrom/lajstrom/internal/format"
	"github.com/lajstrom/lajstrom/internal/index"
	"github.com/lajstrom/lajstrom/internal/oci/profile"
	"github.com/lajstrom/lajstrom/internal/provider"
)

// page is the pagination of a listing: n results at most when limited, after last.
type page struct {
	n       int
	limited bool
	last    string
}

// parsePage reads n and last from the query; an n that is not a non-negative integer is 400.
func parsePage(q url.Values) (page, error) {
	p := page{last: q.Get("last")}
	if s := q.Get("n"); s != "" {
		n, err := strconv.Atoi(s)
		if err != nil || n < 0 {
			return p, &apiError{status: http.StatusBadRequest, code: codeUnsupported, msg: fmt.Sprintf("n=%q is not a non-negative integer", s)}
		}
		p.n, p.limited = n, true
	}
	return p, nil
}

// apply returns the names of sorted after last, n of them at most, and whether more follow.
// n=0 is an empty list without more: the spec forbids a Link then.
func (p page) apply(sorted []string) ([]string, bool) {
	i, found := slices.BinarySearch(sorted, p.last)
	if found {
		i++
	}
	if p.last == "" {
		i = 0
	}
	rest := sorted[i:]
	if p.limited && len(rest) > p.n {
		return rest[:p.n], p.n > 0
	}
	return rest, false
}

// setLink sets the Link header to the next page of path after last, n at a time.
func setLink(w http.ResponseWriter, path, last string, n int) {
	q := url.Values{"last": {last}, "n": {strconv.Itoa(n)}}
	w.Header().Set("Link", "<"+path+"?"+q.Encode()+`>; rel="next"`)
}

// tagList is the answer of …/tags/list.
type tagList struct {
	Name string   `json:"name"`
	Tags []string `json:"tags"`
}

// tags answers GET …/tags/list?n=&last=. For a repo Lajstrom manages, the tags come from the
// index: the versions whose manifest is at <path> (index.ManifestPath), sorted, of the package
// whose escaped path (name_oci) is <path> or, in a split, of the package at another form of it
// whose versions reindex found at <path> (index.PackageOf, #642); an unknown package is an empty
// list when oci_repos knows the OCI repository and NAME_UNKNOWN otherwise. For an oci repo they come from the backend, from last on. Both are paginated
// here with n and a Link.
func (h *handler) tags(w http.ResponseWriter, r *http.Request, t *target, _ string) error {
	ctx := r.Context()
	if err := h.authorize(ctx, auth.ActionRead, t); err != nil {
		return err
	}
	p, err := parsePage(r.URL.Query())
	if err != nil {
		return err
	}
	var all []string
	if t.repo.Format == ociFormat {
		all, err = h.backendTags(ctx, t, p)
	} else {
		all, err = h.indexTags(ctx, t)
	}
	if err != nil {
		return err
	}
	tags, more := p.apply(all)
	if more {
		setLink(w, "/v2/"+t.name+"/tags/list", tags[len(tags)-1], p.n)
	}
	writeJSON(w, http.StatusOK, tagList{Name: t.name, Tags: tags})
	return nil
}

// indexTags returns the sorted tags of the versions whose manifest is at the path of t, from the
// index.
func (h *handler) indexTags(ctx context.Context, t *target) ([]string, error) {
	pkg, err := index.PackageOf(ctx, h.o.Index, t.repo.ID, t.path)
	switch {
	case errors.Is(err, index.ErrNotFound):
		return h.unknownTags(ctx, t)
	case err != nil:
		return nil, fmt.Errorf("facade: package of %s: %w", t.name, err)
	}
	versions, err := h.o.Index.ListVersions(ctx, pkg.ID)
	if err != nil {
		return nil, fmt.Errorf("facade: versions of %s: %w", t.name, err)
	}
	tags := make([]string, 0, len(versions))
	for _, v := range versions {
		if index.ManifestPath(*pkg, v) == t.path {
			tags = append(tags, v.Tag)
		}
	}
	if len(tags) == 0 && pkg.NameOCI != t.path { // another form of the package's name, without a version
		return h.unknownTags(ctx, t)
	}
	slices.Sort(tags)
	return slices.Compact(tags), nil
}

// unknownTags is the tag list of a path where the index has no package: empty when oci_repos
// knows the OCI repository, NAME_UNKNOWN otherwise.
func (h *handler) unknownTags(ctx context.Context, t *target) ([]string, error) {
	known, err := h.ociRepoKnown(ctx, t)
	if err != nil {
		return nil, err
	}
	if !known {
		return nil, &apiError{status: http.StatusNotFound, code: codeNameUnknown, msg: fmt.Sprintf("repository name %s not known to registry", t.name)}
	}
	return []string{}, nil
}

// ociRepoKnown reports whether oci_repos has the OCI repository of t.
func (h *handler) ociRepoKnown(ctx context.Context, t *target) (bool, error) {
	repos, err := h.o.Index.ListOCIRepos(ctx, t.repo.ID)
	if err != nil {
		return false, fmt.Errorf("facade: OCI repositories of %s/%s: %w", t.repo.Format, t.repo.Name, err)
	}
	return slices.ContainsFunc(repos, func(o index.OCIRepo) bool { return o.Path == t.oci }), nil
}

// errEnoughTags ends a walk of the backend's tags early.
var errEnoughTags = errors.New("facade: enough tags")

// backendTags returns the backend's tags of t after p.last, sorted: n+1 of them when the page
// is limited, enough to tell whether more follow. A repository the backend does not know is
// NAME_UNKNOWN.
func (h *handler) backendTags(ctx context.Context, t *target, p page) ([]string, error) {
	rr, err := h.backend(ctx, t)
	if err != nil {
		return nil, err
	}
	tags := []string{}
	err = rr.Tags(ctx, p.last, func(batch []string) error {
		for _, tag := range batch {
			if p.last == "" || tag > p.last {
				tags = append(tags, tag)
			}
		}
		if p.limited && len(tags) > p.n {
			return errEnoughTags
		}
		return nil
	})
	switch {
	case errors.Is(err, provider.ErrNotFound):
		return nil, &apiError{code: codeNameUnknown, msg: fmt.Sprintf("repository name %s not known to registry", t.name), err: err}
	case err != nil && !errors.Is(err, errEnoughTags):
		return nil, &apiError{code: codeNameUnknown, err: err}
	}
	if h.ociProxy(t) { // the keep-tags of an oci proxy are its own (proxy-and-group.md §11.6)
		tags = slices.DeleteFunc(tags, profile.IsKeepTag)
	}
	slices.Sort(tags)
	return tags, nil
}

// catalogList is the answer of /v2/_catalog.
type catalogList struct {
	Repositories []string `json:"repositories"`
}

// catalog answers GET /v2/_catalog?n=&last=: the OCI repositories of every repo the principal
// may read (repositories), as <format>/<repo>/<path>, sorted and paginated with n and a Link.
// oci repos are there too: their manifest PUT records the repository. With a
// bearer that grants registry:catalog:*, the scope catalog clients ask for, the repos are those
// the token's principal may read; with any other bearer, the token's view: the repositories it
// grants pull on. The anonymous principal without a bearer gets 401 with the challenge of that
// scope, as on GET /v2/: crane and oras authenticate only when challenged, so a 200 to them
// would list what anonymous may read to a client that has logged in.
func (h *handler) catalog(w http.ResponseWriter, r *http.Request) error {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		return methodNotAllowed(r)
	}
	ctx := r.Context()
	p, err := parsePage(r.URL.Query())
	if err != nil {
		return err
	}
	g, bearer := grantFrom(ctx)
	if !bearer && auth.PrincipalFrom(ctx).Kind == auth.KindAnonymous {
		return &apiError{status: http.StatusUnauthorized, msg: "authentication required: get a token from the realm of the challenge"}
	}
	names, err := h.catalogNames(ctx, g, bearer && !g.Catalog())
	if err != nil {
		return err
	}
	slices.Sort(names)
	names, more := p.apply(slices.Compact(names))
	if more {
		setLink(w, "/v2/_catalog", names[len(names)-1], p.n)
	}
	writeJSON(w, http.StatusOK, catalogList{Repositories: names})
	return nil
}

// catalogNames returns the names of the catalog, unsorted: with tokenView, those of every repo
// that g grants pull on; otherwise those of every repo the principal may read.
func (h *handler) catalogNames(ctx context.Context, g *token.Grant, tokenView bool) ([]string, error) {
	repos, err := h.o.Index.ListRepos(ctx, "")
	if err != nil {
		return nil, fmt.Errorf("facade: catalog: %w", err)
	}
	names := []string{}
	for _, repo := range repos {
		if !tokenView {
			switch err := format.Authorize(ctx, h.deps, auth.ActionRead, repo); {
			case denied(err):
				continue
			case err != nil:
				return nil, fmt.Errorf("facade: catalog: %w", err)
			}
		}
		ocis, err := h.repositories(ctx, repo)
		if err != nil {
			return nil, err
		}
		for _, name := range ocis {
			if !tokenView || g.Allows(name, token.ActionPull) {
				names = append(names, name)
			}
		}
	}
	return names, nil
}

// repositories returns the names, <format>/<repo>/<path>, of the OCI repositories of repo that
// the catalog lists. For a managed repo they are the package paths of its packages, each of
// which has a version (a package goes with its last one), the repositories whose tags/list is
// not empty: a repository whose versions are all deleted, which the registry may keep and
// oci_repos with it, is not listed (#728). An oci repo indexes no tags: its repositories are
// those oci_repos records, which holds those with a tag: the delete of the last one and reindex
// drop the row of a repository the registry keeps without tags (#748). A group's are
// groupRepositories'.
func (h *handler) repositories(ctx context.Context, repo index.Repo) ([]string, error) {
	if repo.Kind == index.KindGroup {
		return h.groupRepositories(ctx, repo)
	}
	if repo.Format != ociFormat {
		pkgs, err := h.o.Index.ListPackages(ctx, repo.ID, "")
		if err != nil {
			return nil, fmt.Errorf("facade: catalog: %w", err)
		}
		names := make([]string, 0, len(pkgs))
		for _, p := range pkgs {
			names = append(names, repo.Format+"/"+repo.Name+"/"+p.NameOCI)
		}
		return names, nil
	}
	ocis, err := h.o.Index.ListOCIRepos(ctx, repo.ID)
	if err != nil {
		return nil, fmt.Errorf("facade: catalog: %w", err)
	}
	base := repoBase(repo) + "/"
	var names []string
	for _, o := range ocis {
		if path, ok := strings.CutPrefix(o.Path, base); ok && path != "" {
			names = append(names, repo.Format+"/"+repo.Name+"/"+path)
		}
	}
	return names, nil
}

// denied reports whether err is a denial of format.Authorize rather than a failure to decide.
func denied(err error) bool {
	return errors.Is(err, format.ErrUnauthenticated) || errors.Is(err, format.ErrForbidden) ||
		errors.Is(err, index.ErrNotFound)
}
