package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/lajstrom/lajstrom/internal/auth"
	"github.com/lajstrom/lajstrom/internal/index"
)

// SearchResult is the answer of GET /api/v1/search: a page of hits, each a *PackageHit or a
// *VersionHit, and in Next the cursor of the page after it, absent on the last page.
type SearchResult struct {
	Hits []any  `json:"hits"`
	Next string `json:"next,omitempty"`
}

// PackageHit is a package that the search found by its name or a summary, with the facts of its
// row of the package list.
type PackageHit struct {
	Kind          string    `json:"kind"` // package
	Format        string    `json:"format"`
	Repo          string    `json:"repo"`
	Name          string    `json:"name"`
	LatestVersion *string   `json:"latestVersion"`
	Summary       *string   `json:"summary"`
	UpdatedAt     time.Time `json:"updatedAt"`
}

// VersionHit is a manifest that the search found by its digest or the digest of one of its
// files, which File then names.
type VersionHit struct {
	Kind    string   `json:"kind"` // version
	Format  string   `json:"format"`
	Repo    string   `json:"repo"`
	Name    string   `json:"name"`
	Version string   `json:"version"`
	Variant string   `json:"variant"`
	Digest  string   `json:"digest"`  // the manifest's
	Matched string   `json:"matched"` // manifest or file
	File    *FileHit `json:"file,omitempty"`
}

// FileHit is the file whose digest a VersionHit matched.
type FileHit struct {
	Name   string `json:"name"`
	Digest string `json:"digest"`
}

// maxSearch bounds the bytes of ?q= of the search.
const maxSearch = 256

// digestPrefix is a digest's prefix as ?q= may give it, its ASCII letters folded: sha256: or
// not, then 8 to 64 hex digits.
var digestPrefix = regexp.MustCompile(`^(?:sha256:)?([0-9a-f]{8,64})$`)

// search answers GET /api/v1/search: a page of the hits of ?q in the repos the caller may read,
// of ?format when it is given, rank after rank (docs/api.md, Search). A repo it may not read is
// left out as GET /api/v1/repos leaves it out, so the answer is the same whether such a repo
// exists or not. Anyone may ask.
func (h *handler) search(_ http.ResponseWriter, r *http.Request) (int, any, error) {
	limit, after, err := page(r)
	if err != nil {
		return 0, nil, err
	}
	params := r.URL.Query()
	text := params.Get("q")
	if text == "" || len(text) > maxSearch {
		return 0, nil, invalid("q: required, 1 to %d bytes", maxSearch)
	}
	var from *searchCursor
	if after != "" {
		from = &searchCursor{}
		if err := json.Unmarshal([]byte(after), from); err != nil {
			return 0, nil, invalid("cursor: %q is not the next of a page of this search", params.Get("cursor"))
		}
	}
	ids, err := h.searchedRepos(r, params.Get("format"))
	if err != nil {
		return 0, nil, err
	}
	hits, err := h.searchPage(r, searchQueries(text, ids), from, limit+1)
	if err != nil {
		return 0, nil, err
	}
	out := SearchResult{Hits: []any{}}
	if len(hits) > limit {
		hits = hits[:limit]
		next, _ := json.Marshal(searchCursor{Rank: hits[limit-1].rank, Key: hits[limit-1].Key}) // strings always marshal
		out.Next = cursor(string(next))
	}
	if err := h.searchHits(r, hits, &out); err != nil {
		return 0, nil, err
	}
	return http.StatusOK, out, nil
}

// rankedHit is a hit of the search with its rank.
type rankedHit struct {
	index.SearchHit
	rank index.SearchRank
}

// searchQueries returns the query of each rank of the search of text over the repos ids, in
// their order: a manifest's and a file's digest first when text is a digest's prefix.
func searchQueries(text string, ids []int64) []index.SearchQuery {
	var qs []index.SearchQuery
	if m := digestPrefix.FindStringSubmatch(strings.ToLower(text)); m != nil {
		for _, rank := range []index.SearchRank{index.RankManifest, index.RankFile} {
			qs = append(qs, index.SearchQuery{RepoIDs: ids, Rank: rank, Text: "sha256:" + m[1]})
		}
	}
	for _, rank := range []index.SearchRank{index.RankExact, index.RankPrefix, index.RankSubstring, index.RankSummary} {
		qs = append(qs, index.SearchQuery{RepoIDs: ids, Rank: rank, Text: text})
	}
	return qs
}

// searchPage returns at most limit hits of the queries, rank after rank, from the place of
// from (nil: the first).
func (h *handler) searchPage(r *http.Request, qs []index.SearchQuery, from *searchCursor, limit int) ([]rankedHit, error) {
	hits := []rankedHit{}
	for _, q := range qs {
		if from != nil && q.Rank < from.Rank {
			continue
		}
		if from != nil && q.Rank == from.Rank {
			q.After = &from.Key
		}
		q.Limit = limit - len(hits)
		found, err := h.repos.Index.Search(r.Context(), q)
		if err != nil {
			return nil, fmt.Errorf("api: search %q: %w", q.Text, err)
		}
		for _, hit := range found {
			hits = append(hits, rankedHit{SearchHit: hit, rank: q.Rank})
		}
		if len(hits) >= limit {
			break
		}
	}
	return hits, nil
}

// searchCursor is the place of a search's hit, as its cursor holds it in JSON.
type searchCursor struct {
	Rank index.SearchRank `json:"r"`
	Key  index.SearchKey  `json:"k"`
}

// searchedRepos returns the ids of the repos of format ("": every format) that the request's
// principal may read, as listRepos keeps them; nil when that is every repo of every format.
func (h *handler) searchedRepos(r *http.Request, format string) ([]int64, error) {
	repos, err := h.repos.Index.ListRepos(r.Context(), format)
	if err != nil {
		return nil, fmt.Errorf("api: the repos to search: %w", err)
	}
	ids := []int64{}
	for _, repo := range repos {
		switch err := h.allow(r, auth.ActionRead, repo.Format, repo.Name); {
		case err == nil:
			ids = append(ids, repo.ID)
		case !denied(err):
			return nil, err
		}
	}
	if format == "" && len(ids) == len(repos) {
		return nil, nil
	}
	return ids, nil
}

// searchHits adds the hits to out, the facts of the packages read at once, each latest version
// picked by the plugin of its repo's format.
func (h *handler) searchHits(r *http.Request, hits []rankedHit, out *SearchResult) error {
	var pkgs []index.Package
	formats := map[int64]string{}
	for _, hit := range hits {
		if hit.Version == nil {
			pkgs = append(pkgs, hit.Package)
			formats[hit.Package.RepoID] = hit.Key.Format
		}
	}
	entries, err := packageEntries(r.Context(), h.repos.Index, pkgs, h.latestOf(pkgs, formats))
	if err != nil {
		return err
	}
	for _, hit := range hits {
		if hit.Version == nil {
			e := entries[0]
			entries = entries[1:]
			out.Hits = append(out.Hits, &PackageHit{Kind: "package", Format: hit.Key.Format, Repo: hit.Key.Repo, Name: e.Name,
				LatestVersion: e.LatestVersion, Summary: e.Summary, UpdatedAt: e.UpdatedAt})
			continue
		}
		v := &VersionHit{Kind: "version", Format: hit.Key.Format, Repo: hit.Key.Repo, Name: hit.Package.Name,
			Version: hit.Version.Version, Variant: hit.Version.Variant, Digest: hit.Version.Digest.String(), Matched: "manifest"}
		if hit.File != nil {
			v.Matched, v.File = "file", &FileHit{Name: hit.File.Name, Digest: hit.File.Digest.String()}
		}
		out.Hits = append(out.Hits, v)
	}
	return nil
}
