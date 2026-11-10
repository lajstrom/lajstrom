package probe

import "testing"

func TestDigestDeleteVerdict(t *testing.T) {
	ok, gone, there := ans(202, ""), errAns(404, "MANIFEST_UNKNOWN"), ans(200, "{}")
	cases := []struct {
		name string
		seen deleteSeen
		want string
	}{
		{"distribution", deleteSeen{byDigest: ok, tagAfter: gone, blobAfter: there, subject: ok, refAfter: there, indexed: ok},
			"yes, 202, with its tags; its referrers stay"},
		{"zot", deleteSeen{byDigest: ok, tagAfter: gone, blobAfter: there, subject: ok, refAfter: there, indexed: errAns(405, "DENIED")},
			"yes, 202, with its tags; its referrers stay; **405 `DENIED`** for a manifest an index lists"},
		{"harbor", deleteSeen{byDigest: ok, tagAfter: gone, blobAfter: there, subject: ok, refAfter: gone, indexed: ok},
			"yes, 202, with its tags; its referrers go with it"},
		{"nexus docker", deleteSeen{byDigest: ok, tagAfter: there, blobAfter: there, subject: ok, refAfter: there, indexed: ok},
			"yes, 202, **but its tags stay and serve it**; its referrers stay"},
		{"gitea", deleteSeen{byDigest: ok, tagAfter: gone, blobAfter: ans(404, ""), subject: ok, refAfter: there, indexed: ok},
			"yes, 202, with its tags **and the blobs only it used**; its referrers stay"},
		{"gitlab B", deleteSeen{byDigest: ok, tagAfter: gone, blobAfter: there, subject: errAns(500, "UNKNOWN"), indexed: errAns(409, "MANIFEST_REFERENCED")},
			"yes, 202, with its tags; **a manifest with a referrer: 500 `UNKNOWN`**; **409 `MANIFEST_REFERENCED`** for a manifest an index lists"},
		{"ghcr", deleteSeen{byDigest: errAns(405, "UNSUPPORTED")}, "no: 405 `UNSUPPORTED`"},
	}
	for _, c := range cases {
		if got := digestDeleteVerdict(c.seen); got != c.want {
			t.Errorf("%s: digestDeleteVerdict = %q\nwant %q", c.name, got, c.want)
		}
	}
}

func TestTagDeleteVerdict(t *testing.T) {
	ok, gone, there := ans(202, ""), errAns(404, "MANIFEST_UNKNOWN"), ans(200, "{}")
	cases := []struct {
		name string
		seen deleteSeen
		want string
	}{
		{"distribution", deleteSeen{byTag: ok, tagged: there}, "yes, 202: the manifest stays, untagged"},
		{"gitea", deleteSeen{byTag: ok, tagged: gone, byDigTag: there}, "the tag; **the manifest too**, when it was pushed by that tag only"},
		{"gone always", deleteSeen{byTag: ok, tagged: gone, byDigTag: gone}, "the tag; **the manifest too**"},
		{"harbor", deleteSeen{byTag: errAns(400, "BAD_REQUEST")}, "no: 400 `BAD_REQUEST`"},
	}
	for _, c := range cases {
		if got := tagDeleteVerdict(c.seen); got != c.want {
			t.Errorf("%s: tagDeleteVerdict = %q, want %q", c.name, got, c.want)
		}
	}
}

func TestImmutableVerdict(t *testing.T) {
	created, there, gone := ans(201, ""), ans(200, "{}"), errAns(404, "MANIFEST_UNKNOWN")
	cases := []struct {
		name                 string
		again, repoint, kept answer
		want                 string
	}{
		{"distribution", created, created, there, "none: a tag re-points with 201"},
		{"gitea", created, created, gone, "none: a tag re-points with 201, and **the manifest it named is deleted**"},
		{"harbor", errAns(412, "PRECONDITION"), errAns(412, "PRECONDITION"), answer{}, "a tag cannot re-point: 412 `PRECONDITION`; the same manifest again too"},
		{"nexus allow_once", created, errAns(409, "REDEPLOY_DISABLED"), answer{}, "a tag cannot re-point: 409 `REDEPLOY_DISABLED`; the same manifest again is taken"},
	}
	for _, c := range cases {
		if got := immutableVerdict(c.again, c.repoint, c.kept); got != c.want {
			t.Errorf("%s: immutableVerdict = %q, want %q", c.name, got, c.want)
		}
	}
}
