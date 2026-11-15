package harbor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"

	"github.com/lajstrom/lajstrom/internal/provider"
)

const (
	// pageSize is the page size of the repository listing, Harbor's maximum (a larger one is 422).
	pageSize = 100
	// maxPages stops a listing after this many pages, logged at warn: Harbor must not keep
	// Lajstrom paging forever.
	maxPages = 1000
	// maxPageBody bounds one page of the listing: 100 repositories with their descriptions.
	maxPageBody = 4 << 20
)

// ListRepositories lists the project's repositories through the REST API,
// GET /api/v2.0/projects/<project>/repositories, sorted by name and every page, and returns the
// names below the instance prefix that start with prefix, without the instance prefix, sorted
// and without duplicates. Harbor's names carry the project; its filter q=name=~ matches a part
// of the name, so the prefix is applied here as well. The walk stops after maxPages pages,
// logged at warn: the result is then what the pages so far listed. It returns an error
// wrapping provider.ErrUnsupported when capabilities.catalog is false, and when Harbor answers
// 401 or 403: the account lacks repository:list, and a reindex reads oci_repos instead.
func (p *Provider) ListRepositories(ctx context.Context, prefix string) ([]string, error) {
	op := "list repositories " + prefix
	if p.override.Catalog != nil && !*p.override.Catalog {
		return nil, fmt.Errorf("harbor: %s: capabilities.catalog is false: %w", op, provider.ErrUnsupported)
	}
	want := p.prefix + prefix
	q := url.Values{"sort": {"name"}, "page_size": {strconv.Itoa(p.pageSize)}}
	if want != p.project+"/" {
		q.Set("q", "name=~"+want)
	}
	found := map[string]struct{}{}
	for page := 1; ; page++ {
		if page > p.maxPages {
			p.log.WarnContext(ctx, "the Harbor project has more pages of repositories than Lajstrom reads; the listing is incomplete",
				"pages", p.maxPages)
			break
		}
		q.Set("page", strconv.Itoa(page))
		names, more, err := p.listPage(ctx, op, q)
		if err != nil {
			return nil, err
		}
		for _, n := range names {
			if strings.HasPrefix(n, want) {
				found[strings.TrimPrefix(n, p.prefix)] = struct{}{}
			}
		}
		if !more {
			break
		}
	}
	return slices.Sorted(maps.Keys(found)), nil
}

// listPage reads one page of the listing: the names, and whether another page follows (a full
// page with a Link rel="next").
func (p *Provider) listPage(ctx context.Context, op string, q url.Values) ([]string, bool, error) {
	resp, err := p.api.do(ctx, http.MethodGet, p.repositoriesPath(), q, nil)
	if err != nil {
		return nil, false, failed(op, err)
	}
	defer drain(resp)
	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusUnauthorized, http.StatusForbidden:
		err := p.api.translate(op, resp, "repository:list")
		var e *apiError
		if errors.As(err, &e) {
			e.kind = provider.ErrUnsupported
		}
		return nil, false, err
	default:
		return nil, false, p.api.translate(op, resp, "repository:list")
	}
	var page []struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxPageBody)).Decode(&page); err != nil {
		return nil, false, fmt.Errorf("harbor: %s: read a page: %w", op, err)
	}
	names := make([]string, 0, len(page))
	for _, r := range page {
		names = append(names, r.Name)
	}
	return names, len(page) >= p.pageSize && hasNext(resp.Header.Values("Link")), nil
}

// hasNext reports whether a Link header names a next page: rel="next".
func hasNext(links []string) bool {
	for _, l := range links {
		for part := range strings.SplitSeq(l, ",") {
			if strings.Contains(part, `rel="next"`) {
				return true
			}
		}
	}
	return false
}

// DeleteRepository deletes the repository name below the instance prefix with its artifacts
// and their tags, DELETE /api/v2.0/projects/<project>/repositories/<name below the project>,
// the name's slashes escaped twice as Harbor wants them. A repository that does not exist is
// provider.ErrNotFound; one that holds a tag an immutability rule matches is
// provider.ErrConflict (412). The account needs repository:delete. Harbor frees the space of
// the blobs when its garbage collector runs.
func (p *Provider) DeleteRepository(ctx context.Context, name string) error {
	if name == "" {
		return errors.New("harbor: delete repository: empty name")
	}
	op := "delete repository " + name
	below := strings.TrimPrefix(p.prefix+name, p.project+"/")
	resp, err := p.api.do(ctx, http.MethodDelete, p.repositoriesPath()+"/"+url.PathEscape(url.PathEscape(below)), nil, nil)
	if err != nil {
		return failed(op, err)
	}
	defer drain(resp)
	if resp.StatusCode != http.StatusOK {
		return p.api.translate(op, resp, "repository:delete")
	}
	p.log.InfoContext(ctx, "Harbor repository deleted", "repository", name)
	return nil
}
