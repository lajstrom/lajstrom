package probe

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"
)

// orderNames are the repositories below ord/ of the catalog's order, pushed in this order, and
// the tags of the tag list's, pushed in this order: each order the matrix met sorts them
// differently.
var (
	orderNames = []string{"prea", "pre.y", "pre/sub", "pre-x", "pre"}
	orderTags  = []string{"b", "A", "1.10", "_x", "1.2", "Z", "a"}
)

// catalogSeen is what the catalog measurement found.
type catalogSeen struct {
	probe     answer   // _catalog?n=1
	order     []string // the ord/ names as listed, without the prefix
	page      answer   // n=2&last=<the first ord/ name>
	pageNames []string
	want      []string // the two names after the first, in the listed order
	n1000     answer
	n1001     answer
	nHuge     answer // n=100000, asked when n=1001 is taken
	whole     answer // _catalog without n
}

// measureCatalog measures "Catalog (`_catalog`) and its limits": five repositories below ord/,
// their order in the catalog, a page of two after the first with last, its Link, n=1000,
// n=1001 and n=100000, and the page without n.
func (r *run) measureCatalog(ctx context.Context) error {
	var s catalogSeen
	var err error
	if s.probe, err = r.get(ctx, "/v2/_catalog?n=1", nil); err != nil {
		return err
	}
	if _, ok := catalogPageOf(s.probe); !ok {
		r.found.catalog = false
		r.set("catalog", catalogVerdict(s))
		return nil
	}
	for _, n := range orderNames {
		if err := r.mustPush(ctx, "ord/"+n, "v1", r.newManifest("order "+n, "", nil)); err != nil {
			return err
		}
	}
	names, _, err := r.catalogBelow(ctx, r.name("ord/"))
	if err != nil {
		return err
	}
	for _, n := range names {
		s.order = append(s.order, strings.TrimPrefix(n, r.name("ord/")))
	}
	if len(names) >= 3 {
		s.want = names[1:3]
		if s.page, err = r.get(ctx, "/v2/_catalog?n=2&last="+url.QueryEscape(names[0]), nil); err != nil {
			return err
		}
		s.pageNames, _ = catalogPageOf(s.page)
	}
	for _, q := range []struct {
		dst *answer
		n   string
	}{{&s.n1000, "1000"}, {&s.n1001, "1001"}} {
		if *q.dst, err = r.get(ctx, "/v2/_catalog?n="+q.n, nil); err != nil {
			return err
		}
	}
	if s.n1001.status == http.StatusOK {
		if s.nHuge, err = r.get(ctx, "/v2/_catalog?n=100000", nil); err != nil {
			return err
		}
	}
	if s.whole, err = r.get(ctx, "/v2/_catalog", nil); err != nil {
		return err
	}
	r.set("catalog", catalogVerdict(s))
	return nil
}

// catalogVerdict is the cell "Catalog (`_catalog`) and its limits".
func catalogVerdict(s catalogSeen) string {
	if _, ok := catalogPageOf(s.probe); !ok {
		switch {
		case s.probe.status == http.StatusOK && s.probe.size == 0:
			return "**200 with an empty body**"
		case s.probe.status == http.StatusOK:
			return "**200 without a catalog**"
		}
		return "no: " + s.probe.refusal()
	}
	parts := []string{"yes", pagination(s.page, s.pageNames, s.want, nil)}
	parts = append(parts, orderOf(s.order, orderNames))
	if l := nextLink(s.whole); l != "" {
		names, _ := catalogPageOf(s.whole)
		parts = append(parts, fmt.Sprintf("%d a page without `n`", len(names)))
	}
	switch {
	case s.n1001.status == http.StatusOK && s.nHuge.status == http.StatusOK:
		parts = append(parts, "no largest `n`")
	case s.n1001.status == http.StatusOK:
		parts = append(parts, "`n=100000` is "+s.nHuge.refusal())
	case s.n1000.status == http.StatusOK:
		parts = append(parts, "`n` at most 1000 (1001: "+s.n1001.refusal()+")")
	default:
		parts = append(parts, "`n=1000` is "+s.n1000.refusal())
	}
	return strings.Join(parts, ", ")
}

// pagination describes a page asked with n=2 and last: whether it held the two names that
// follow last (want), and its Link: "`n`/`last`/`Link`", absolute or with a cursor. all is
// the whole list when it is known.
func pagination(page answer, got, want, all []string) string {
	if len(want) < 2 {
		return "pagination not measured"
	}
	link := nextLink(page)
	var v string
	switch {
	case slices.Equal(got, want):
		v = "`n`/`last`"
	case len(got) == 2:
		v = "`n`, **`last` ignored**"
	case all != nil && slices.Equal(got, all):
		v = "**`n` and `last` ignored**: the whole list"
	default:
		v = fmt.Sprintf("**`n=2` gave %d**", len(got))
	}
	switch {
	case link == "":
		return v + ", no `Link`"
	case !strings.Contains(link, "last="):
		v += "/`Link` with a cursor"
	default:
		v += "/`Link`"
	}
	if strings.HasPrefix(link, "http://") || strings.HasPrefix(link, "https://") {
		v += " (absolute)"
	}
	return v
}

// orderOf names the order of listed, the names of pushed as a registry listed them: lexical
// (byte order), component by component ("/" first, distribution on a filesystem), by object
// key ("/" appended, distribution on S3), the push order, or another one.
func orderOf(listed, pushed []string) string {
	if len(listed) < 2 {
		return "the order not measured"
	}
	key := func(f func(string) string) func(a, b string) int {
		return func(a, b string) int { return strings.Compare(f(a), f(b)) }
	}
	same := func(s string) string { return s }
	component := func(s string) string { return strings.ReplaceAll(s, "/", "\x00") }
	object := func(s string) string { return s + "/" }
	switch {
	case slices.IsSortedFunc(listed, key(same)):
		return "lexical"
	case slices.IsSortedFunc(listed, key(component)):
		return "component by component (`/` first)"
	case slices.IsSortedFunc(listed, key(object)):
		return "by object key (`/` appended)"
	case slices.Equal(listed, pushed):
		return "**creation order**"
	}
	return "**in its own order**: " + strings.Join(listed, ", ")
}

// tagsSeen is what the tag list measurement found.
type tagsSeen struct {
	all        []string // the whole list
	lowered    bool     // the list has lower-cased tags in place of those pushed
	foldedHead answer   // HEAD of a tag in the other case, asked when lowered
	page       answer   // n=2&last=<the first tag>
	pageTags   []string
	missing    answer // tags/list of a repository never pushed
}

// measureTags measures "Tag list order and pagination": one manifest under seven tags pushed
// out of order, the list, a page of two after the first with last, and the tag list of a
// repository that does not exist.
func (r *run) measureTags(ctx context.Context) error {
	const repo = "tags"
	m := r.newManifest("tags", "", nil)
	for _, t := range orderTags {
		if err := r.mustPush(ctx, repo, t, m); err != nil {
			return err
		}
	}
	var s tagsSeen
	var err error
	if s.all, _, err = r.listTags(ctx, r.path(repo, "tags/list")); err != nil {
		return err
	}
	s.lowered = slices.Contains(s.all, "z") && !slices.Contains(s.all, "Z")
	if s.lowered {
		if s.foldedHead, err = r.c.do(ctx, request{method: http.MethodHead, target: r.path(repo, "manifests/B"), header: manifestAccept}); err != nil {
			return err
		}
	}
	if len(s.all) >= 3 {
		if s.pageTags, s.page, err = r.listTags(ctx, r.path(repo, "tags/list?n=2&last="+url.QueryEscape(s.all[0]))); err != nil {
			return err
		}
	}
	if _, s.missing, err = r.listTags(ctx, r.path("tags-missing", "tags/list")); err != nil {
		return err
	}
	r.set("tags", tagsVerdict(s))
	return nil
}

// tagsVerdict is the cell "Tag list order and pagination".
func tagsVerdict(s tagsSeen) string {
	pushed := orderTags
	var parts []string
	if s.lowered {
		folded := "**lower-cased**"
		if s.foldedHead.status == http.StatusOK {
			folded = "**lower-cased, case-insensitive**"
		}
		parts = append(parts, folded)
		pushed = nil
		for _, t := range orderTags {
			if l := strings.ToLower(t); !slices.Contains(pushed, l) {
				pushed = append(pushed, l)
			}
		}
	}
	order := orderOf(s.all, pushed)
	if order == "lexical" && !s.lowered {
		order = "lexical, byte by byte"
	}
	parts = append(parts, order)
	var want []string
	if len(s.all) >= 3 {
		want = s.all[1:3]
	}
	v := strings.Join(parts, "; ") + "; " + pagination(s.page, s.pageTags, want, s.all)
	if s.missing.status == http.StatusOK {
		v += "; a missing repository is 200 `[]`"
	}
	return v
}
