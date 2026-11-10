package probe

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"
)

const (
	// catalogPage is the page size of the probe's catalog walks.
	catalogPage = 100
	// catalogPages bounds a catalog walk: a registry whose catalog ignores last, or is all of
	// a hosted service, is not walked to its end.
	catalogPages = 50
)

// repositories are the repositories below the scratch prefix that the run pushes to and that
// must not exist before it.
var repositories = []string{"create", "ref", "ord", "tags", "del", "imm", "blob", "size", "profile", "up", "mount", "replica"}

// checkScratch refuses a scratch prefix that already holds something: a repository below it
// in the catalog, or a tag in one of the repositories the run pushes to.
func (r *run) checkScratch(ctx context.Context) error {
	names, ok, err := r.catalogBelow(ctx, r.scratch)
	if err != nil {
		return err
	}
	r.found.catalog = ok
	if len(names) > 0 {
		return fmt.Errorf("%w: %s holds %s", ErrNotEmpty, r.scratch, strings.Join(names, ", "))
	}
	for _, repo := range repositories {
		tags, _, err := r.listTags(ctx, r.path(repo, "tags/list"))
		if err != nil {
			return err
		}
		if len(tags) > 0 {
			return fmt.Errorf("%w: %s has tags", ErrNotEmpty, r.name(repo))
		}
	}
	return nil
}

// catalog is the body of _catalog.
type catalog struct {
	Repositories *[]string `json:"repositories"`
}

// catalogPageOf reads a catalog page; ok is false when the answer is no catalog.
func catalogPageOf(a answer) (names []string, ok bool) {
	var c catalog
	if a.status != http.StatusOK || json.Unmarshal(firstJSON(a.body), &c) != nil || c.Repositories == nil {
		return nil, false
	}
	return *c.Repositories, true
}

// catalogBelow walks the catalog from just before prefix on, following Link, and returns the
// names that start with prefix in the catalog's order. ok is false when _catalog answers no
// catalog. The walk stops at the first name past prefix when the registry honours last, or
// after catalogPages pages.
func (r *run) catalogBelow(ctx context.Context, prefix string) (names []string, ok bool, err error) {
	last := prefix[:len(prefix)-1] // every name that starts with prefix sorts after it
	target := fmt.Sprintf("/v2/_catalog?n=%d&last=%s", catalogPage, url.QueryEscape(last))
	seen := map[string]bool{}
	honoured := false
	for page := 0; page < catalogPages; page++ {
		a, err := r.get(ctx, target, nil)
		if err != nil {
			return nil, false, err
		}
		list, isCatalog := catalogPageOf(a)
		if !isCatalog {
			return nil, ok, nil
		}
		ok = true
		if page == 0 && len(list) > 0 {
			honoured = list[0] > last
		}
		passed := false
		for _, n := range list {
			switch {
			case strings.HasPrefix(n, prefix):
				if !seen[n] {
					seen[n] = true
					names = append(names, n)
				}
			case honoured && n > prefix:
				passed = true
			}
		}
		next := nextLink(a)
		if passed || next == "" {
			break
		}
		target = next
	}
	return names, ok, nil
}

// nextLink returns the URL of the answer's Link with rel="next" (quoted or not), "" without
// one.
func nextLink(a answer) string {
	for _, l := range a.header.Values("Link") {
		for part := range strings.SplitSeq(l, ",") {
			u, params, ok := strings.Cut(strings.TrimSpace(part), ";")
			for p := range strings.SplitSeq(params, ";") {
				k, v, _ := strings.Cut(strings.TrimSpace(p), "=")
				if ok && strings.EqualFold(k, "rel") && strings.Trim(v, `"`) == "next" {
					return strings.Trim(strings.TrimSpace(u), "<>")
				}
			}
		}
	}
	return ""
}

// cleanup deletes what the run pushed, the manifests last pushed first (referrers and indexes
// before what they name), then the blobs, and measures "Delete a repository": what stays of
// the repositories once their content is gone.
func (r *run) cleanup(ctx context.Context) {
	var left []string
	done := map[pushed]bool{}
	for _, m := range slices.Backward(r.manifests) {
		if done[m] {
			continue
		}
		done[m] = true
		if reason := r.remove(ctx, m.repo, "manifests/"+m.digest.String()); reason != "" {
			left = append(left, fmt.Sprintf("%s@%s (%s)", r.name(m.repo), shorten(m.digest.String()), reason))
		}
	}
	for _, b := range r.blobs {
		if done[b] {
			continue
		}
		done[b] = true
		if reason := r.remove(ctx, b.repo, "blobs/"+b.digest.String()); reason != "" {
			left = append(left, fmt.Sprintf("blob %s@%s (%s)", r.name(b.repo), shorten(b.digest.String()), reason))
		}
	}
	r.report.LeftBehind = left
	if err := r.measureRepositoryDelete(ctx); err != nil {
		r.failed("deleteRepository", err)
	}
}

// remove deletes what rest names in repo and returns why it is still there, "" when it is
// gone (a 404 too: a cell deleted it already).
func (r *run) remove(ctx context.Context, repo, rest string) string {
	a, err := r.send(ctx, http.MethodDelete, r.path(repo, rest))
	switch {
	case err != nil:
		return err.Error()
	case a.ok(http.StatusAccepted, http.StatusOK, http.StatusNoContent, http.StatusNotFound):
		return ""
	}
	return a.refusal()
}

// measureRepositoryDelete measures "Delete a repository" after the cleanup: the distribution
// API has no repository delete, so what is measured is what stays of a repository whose
// manifests and blobs were deleted, in the catalog and in its tag list.
func (r *run) measureRepositoryDelete(ctx context.Context) error {
	if len(r.manifests) == 0 {
		return fmt.Errorf("nothing was pushed")
	}
	repo := r.manifests[0].repo
	_, a, err := r.listTags(ctx, r.path(repo, "tags/list"))
	if err != nil {
		return err
	}
	names, ok, err := r.catalogBelow(ctx, r.scratch)
	if err != nil {
		return err
	}
	r.set("deleteRepository", repositoryDeleteVerdict(ok, len(names) > 0, a))
	return nil
}

// repositoryDeleteVerdict is the cell "Delete a repository": whether the catalog still lists
// the emptied repositories, and what their tag list answers.
func repositoryDeleteVerdict(catalog, listed bool, tags answer) string {
	v := "no `/v2` API; "
	switch {
	case catalog && listed:
		v += "an emptied repository stays in `_catalog`"
	case catalog:
		v += "it leaves `_catalog` with its last manifest"
	default:
		v += "no catalog to look in"
	}
	if tags.status == http.StatusOK {
		return v + ", its tag list 200"
	}
	return v + ", its tag list " + tags.refusal()
}
