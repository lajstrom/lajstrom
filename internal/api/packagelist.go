package api

import (
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/lajstrom/lajstrom/internal/auth"
	"github.com/lajstrom/lajstrom/internal/index"
)

// PackageList is the answer of GET …/packages: a page of the repo's packages, and in Next the
// cursor of the page after it, absent on the last page. Facets, by key, are there when ?facet=
// asked for them.
type PackageList struct {
	Packages []PackageEntry          `json:"packages"`
	Facets   map[string][]FacetValue `json:"facets,omitempty"`
	Next     string                  `json:"next,omitempty"`
}

// FacetValue is a value of an attr in the repo and how many packages have a manifest with it.
type FacetValue struct {
	Value string `json:"value"`
	Count int64  `json:"count"`
}

const (
	// maxFilters bounds the ?attr= and the ?facet= of the package list, each.
	maxFilters = 16
	// maxFacetValues bounds the values of one facet, the first in byte order.
	maxFacetValues = 1000
)

// attrKeyRE is the form of the key of an ?attr= or a ?facet=: an annotation's full key.
var attrKeyRE = regexp.MustCompile(`^lajstrom\.[A-Za-z0-9._-]+$`)

// listPackages answers GET /api/v1/repos/{format}/{repo}/packages: a page of the repo's packages
// that ?prefix, ?q and ?attr keep, in the order of ?sort and ?order, ?limit of them after the
// place of ?cursor, with the facets of ?facet over the whole repo (docs/api.md, List packages).
// The cursor is a place, a name or a sort's value and a name, so packages published or deleted
// meanwhile shift nothing. It needs read.
func (h *handler) listPackages(_ http.ResponseWriter, r *http.Request) (int, any, error) {
	f, name := r.PathValue("format"), r.PathValue("repo")
	if err := h.allow(r, auth.ActionRead, f, name); err != nil {
		return 0, nil, err
	}
	limit, after, err := page(r)
	if err != nil {
		return 0, nil, err
	}
	params := r.URL.Query()
	q, err := packageQuery(params, after)
	if err != nil {
		return 0, nil, err
	}
	facets, err := attrKeys(params, "facet")
	if err != nil {
		return 0, nil, err
	}
	ctx := r.Context()
	repo, err := h.repos.Index.GetRepo(ctx, f, name)
	if err != nil {
		return 0, nil, repoError("list the packages of", f, name, err)
	}
	// One more than the page: whether another follows. The store orders names by bytes, as the
	// cursor compares.
	q.Limit = limit + 1
	places, err := h.repos.Index.ListPackagePage(ctx, repo.ID, q)
	if err != nil {
		return 0, nil, fmt.Errorf("api: packages of %s/%s: %w", f, name, err)
	}
	list := PackageList{}
	if len(places) > limit {
		places = places[:limit]
		list.Next = cursor(packageCursor(q.Sort, places[limit-1].Key))
	}
	pkgs := make([]index.Package, len(places))
	for i, p := range places {
		pkgs[i] = p.Package
	}
	latest := h.latestOf(pkgs, map[int64]string{repo.ID: repo.Format})
	if list.Packages, err = packageEntries(ctx, h.repos.Index, pkgs, latest); err != nil {
		return 0, nil, err
	}
	if len(facets) > 0 {
		list.Facets = map[string][]FacetValue{}
	}
	for _, key := range facets {
		values, err := h.repos.Index.ListAttrValues(ctx, repo.ID, key)
		if err != nil {
			return 0, nil, fmt.Errorf("api: the values of %s in %s/%s: %w", key, f, name, err)
		}
		out := make([]FacetValue, 0, min(len(values), maxFacetValues))
		for _, v := range values[:min(len(values), maxFacetValues)] {
			out = append(out, FacetValue{Value: v.Value, Count: v.Packages})
		}
		list.Facets[key] = out
	}
	return http.StatusOK, list, nil
}

// packageQuery returns the query of the package list's parameters, its key the place of the
// cursor's key after ("": the first page), without its limit.
func packageQuery(params url.Values, after string) (index.PackageQuery, error) {
	q := index.PackageQuery{Prefix: params.Get("prefix"), Text: params.Get("q"), Sort: index.SortName}
	switch s := index.PackageSort(params.Get("sort")); s {
	case "", index.SortName:
	case index.SortUpdated, index.SortVersions, index.SortSize:
		q.Sort, q.Desc = s, true
	default:
		return q, invalid("sort: %q is none of name, updated, versions and size", s)
	}
	switch o := params.Get("order"); o {
	case "":
	case "asc", "desc":
		q.Desc = o == "desc"
	default:
		return q, invalid("order: %q is neither asc nor desc", o)
	}
	if after != "" {
		k, err := parsePackageCursor(q.Sort, after)
		if err != nil {
			return q, invalid("cursor: %q is not the next of a page of this list in this order", params.Get("cursor"))
		}
		q.After = k
	}
	attrs := params["attr"]
	if len(attrs) > maxFilters {
		return q, invalid("attr: %d of them, at most %d", len(attrs), maxFilters)
	}
	for _, a := range attrs {
		key, value, ok := strings.Cut(a, ":")
		if !ok || !attrKeyRE.MatchString(key) {
			return q, invalid("attr: %q is not <key>:<value> with a key of lajstrom. and letters, digits, '.', '_' and '-'", a)
		}
		if q.Attrs == nil {
			q.Attrs = map[string][]string{}
		}
		q.Attrs[key] = append(q.Attrs[key], value)
	}
	return q, nil
}

// attrKeys returns the distinct keys of the parameter name, in their order: each an annotation's
// full key, at most maxFilters of them.
func attrKeys(params url.Values, name string) ([]string, error) {
	keys := params[name]
	if len(keys) > maxFilters {
		return nil, invalid("%s: %d of them, at most %d", name, len(keys), maxFilters)
	}
	out := []string{}
	for _, key := range keys {
		if !attrKeyRE.MatchString(key) {
			return nil, invalid("%s: %q is not a key of lajstrom. and letters, digits, '.', '_' and '-'", name, key)
		}
		if !slices.Contains(out, key) {
			out = append(out, key)
		}
	}
	return out, nil
}

// packageCursor is the key k of the package list in the order of sort as a cursor holds it: the
// name by name; else the value, |, the name: the time in RFC 3339 with nanoseconds, a count in
// decimal.
func packageCursor(sort index.PackageSort, k index.PackageKey) string {
	switch sort {
	case index.SortUpdated:
		return k.Updated.UTC().Format(time.RFC3339Nano) + "|" + k.Name
	case index.SortVersions, index.SortSize:
		return strconv.FormatInt(k.N, 10) + "|" + k.Name
	case index.SortName:
	}
	return k.Name
}

// parsePackageCursor is the inverse of packageCursor.
func parsePackageCursor(sort index.PackageSort, s string) (*index.PackageKey, error) {
	if sort == index.SortName {
		return &index.PackageKey{Name: s}, nil
	}
	value, name, ok := strings.Cut(s, "|")
	if !ok || name == "" {
		return nil, fmt.Errorf("no |")
	}
	if sort == index.SortUpdated {
		t, err := time.Parse(time.RFC3339Nano, value)
		return &index.PackageKey{Updated: t, Name: name}, err
	}
	n, err := strconv.ParseInt(value, 10, 64)
	return &index.PackageKey{N: n, Name: name}, err
}
