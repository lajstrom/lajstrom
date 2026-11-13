package probe

import (
	"strings"
	"testing"
)

func TestOrderOf(t *testing.T) {
	cases := []struct {
		name   string
		listed []string
		want   string
	}{
		{"byte order", []string{"pre", "pre-x", "pre.y", "pre/sub", "prea"}, "lexical"},
		{"distribution and zot on a filesystem", []string{"pre", "pre/sub", "pre-x", "pre.y", "prea"}, "component by component (`/` first)"},
		{"distribution on S3", []string{"pre-x", "pre.y", "pre", "pre/sub", "prea"}, "by object key (`/` appended)"},
		{"creation order", orderNames, "**creation order**"},
		{"another", []string{"prea", "pre"}, "**in its own order**: prea, pre"},
		{"too few", []string{"pre"}, "the order not measured"},
	}
	for _, c := range cases {
		if got := orderOf(c.listed, orderNames); got != c.want {
			t.Errorf("%s: orderOf = %q, want %q", c.name, got, c.want)
		}
	}
}

func TestPagination(t *testing.T) {
	want := []string{"b", "c"}
	all := []string{"a", "b", "c", "d"}
	cases := []struct {
		name string
		page answer
		got  []string
		all  []string
		v    string
	}{
		{"distribution", ans(200, "", "Link", `</v2/x/tags/list?last=c&n=2>; rel="next"`), want, all, "`n`/`last`/`Link`"},
		{"nexus", ans(200, "", "Link", `<http://r.test/v2/x/tags/list?last=c&n=2>; rel="next"`), want, all, "`n`/`last`/`Link` (absolute)"},
		{"nexus oci catalog", ans(200, "", "Link", `</v2/_catalog?n=2&cursor=Zm9v>; rel="next"`), []string{"x", "y"}, nil, "`n`, **`last` ignored**/`Link` with a cursor"},
		{"gitlab A", ans(200, ""), all, all, "**`n` and `last` ignored**: the whole list, no `Link`"},
		{"no link", ans(200, ""), want, all, "`n`/`last`, no `Link`"},
		{"not measured", ans(200, ""), nil, nil, "pagination not measured"},
		{"ecr", errAns(405, "UNSUPPORTED"), nil, all, "**`last` a name: 405 `UNSUPPORTED`**"},
	}
	for _, c := range cases {
		w := want
		if c.v == "pagination not measured" {
			w = nil
		}
		if got := pagination(c.page, c.got, w, c.all); got != c.v {
			t.Errorf("%s: pagination = %q, want %q", c.name, got, c.v)
		}
	}
}

func TestCatalogVerdict(t *testing.T) {
	catalog := ans(200, `{"repositories":["a"]}`)
	page := ans(200, `{"repositories":["b","c"]}`, "Link", `</v2/_catalog?last=c&n=2>; rel="next"`)
	component := []string{"pre", "pre/sub", "pre-x", "pre.y", "prea"}
	cases := []struct {
		name string
		seen catalogSeen
		want string
	}{
		{"distribution", catalogSeen{probe: catalog, order: component, page: page, pageNames: []string{"b", "c"}, want: []string{"b", "c"},
			n1000: catalog, n1001: errAns(400, "PAGINATION_NUMBER_INVALID"),
			whole: ans(200, `{"repositories":["a"]}`, "Link", `</v2/_catalog?last=x&n=100>; rel="next"`)},
			"yes, `n`/`last`/`Link`, component by component (`/` first), 1 a page without `n`, `n` at most 1000 (1001: 400 `PAGINATION_NUMBER_INVALID`)"},
		{"zot", catalogSeen{probe: catalog, order: component, page: page, pageNames: []string{"b", "c"}, want: []string{"b", "c"},
			n1000: catalog, n1001: catalog, nHuge: catalog, whole: catalog},
			"yes, `n`/`last`/`Link`, component by component (`/` first), no largest `n`"},
		{"jcr", catalogSeen{probe: ans(200, "")}, "**200 with an empty body**"},
		{"harbor robot", catalogSeen{probe: errAns(401, "UNAUTHORIZED")}, "no: 401 `UNAUTHORIZED`"},
		{"something else", catalogSeen{probe: ans(200, `{"x":1}`)}, "**200 without a catalog**"},
	}
	for _, c := range cases {
		if got := catalogVerdict(c.seen); got != c.want {
			t.Errorf("%s: catalogVerdict = %q\nwant %q", c.name, got, c.want)
		}
	}
}

func TestTagsVerdict(t *testing.T) {
	bytes := []string{"1.10", "1.2", "A", "Z", "_x", "a", "b"}
	link := ans(200, "", "Link", `</v2/x/tags/list?last=A&n=2>; rel="next"`)
	lowered := []string{"1.10", "1.2", "_x", "a", "b", "z"}
	cases := []struct {
		name string
		seen tagsSeen
		want string
	}{
		{"distribution", tagsSeen{all: bytes, page: link, pageTags: bytes[1:3], missing: errAns(404, "NAME_UNKNOWN")},
			"lexical, byte by byte; `n`/`last`/`Link`"},
		{"distribution on S3", tagsSeen{all: bytes, page: link, pageTags: bytes[1:3], missing: ans(200, `{"tags":[]}`)},
			"lexical, byte by byte; `n`/`last`/`Link`; a missing repository is 200 `[]`"},
		{"gitea", tagsSeen{all: lowered, lowered: true, foldedHead: ans(200, ""), page: link, pageTags: lowered[1:3], missing: errAns(404, "NAME_UNKNOWN")},
			"**lower-cased, case-insensitive**; lexical; `n`/`last`/`Link`"},
		{"ghcr", tagsSeen{all: orderTags, page: link, pageTags: orderTags[1:3], missing: errAns(404, "NAME_UNKNOWN")},
			"**creation order**; `n`/`last`/`Link`"},
		{"gitlab A", tagsSeen{all: bytes, page: ans(200, ""), pageTags: bytes, missing: errAns(404, "NAME_UNKNOWN")},
			"lexical, byte by byte; **`n` and `last` ignored**: the whole list, no `Link`"},
	}
	for _, c := range cases {
		if got := tagsVerdict(c.seen); got != c.want {
			t.Errorf("%s: tagsVerdict = %q\nwant %q", c.name, got, c.want)
		}
	}
}

func TestOrderTagsAreNotSorted(t *testing.T) {
	// The push order must differ from every order orderOf names, or creation order hides.
	if got := orderOf(orderTags, nil); !strings.Contains(got, "own order") {
		t.Errorf("orderOf(orderTags) = %q", got)
	}
}
