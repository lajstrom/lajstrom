package probe

import (
	"net/http"
	"testing"
)

func TestChunkOrderVerdict(t *testing.T) {
	cases := []struct {
		name string
		seen uploadSeen
		want string
	}{
		{"distribution", uploadSeen{outOfOrd: errAns(416, "RANGE_INVALID")}, "416 `RANGE_INVALID`"},
		{"ghcr", uploadSeen{outOfOrd: ans(416, "")}, "an empty 416"},
		{"harbor", uploadSeen{outOfOrd: ans(202, "", "Range", "0-3"), afterOdd: ans(204, "", "Range", "0-3")}, "**appended**, no 416"},
		{"taken elsewhere", uploadSeen{outOfOrd: ans(202, "", "Range", "0-7"), afterOdd: ans(204, "", "Range", "0-7")}, "**taken**: 202, `Range: 0-7`"},
	}
	for _, c := range cases {
		if got := chunkOrderVerdict(c.seen); got != c.want {
			t.Errorf("%s: chunkOrderVerdict = %q, want %q", c.name, got, c.want)
		}
	}
}

func TestStatusVerdict(t *testing.T) {
	first := ans(202, "", "Range", "0-2")
	cases := []struct {
		name string
		seen uploadSeen
		want string
	}{
		{"distribution", uploadSeen{first: first, status: ans(204, "", "Range", "0-2")}, "works, 204 with `Range`"},
		{"forgejo", uploadSeen{first: first, status: ans(204, "", "Range", "0-3")}, "204 with `Range`: **one byte too many** (`0-3` after `0-2`)"},
		{"ghcr", uploadSeen{first: first, status: ans(303, "", "Location", "https://github.com/x/y")}, "**303 to `github.com/x/y`**"},
		{"no range", uploadSeen{first: first, status: ans(204, "")}, "204 without `Range`"},
		{"refused", uploadSeen{first: first, status: errAns(405, "UNSUPPORTED")}, "405 `UNSUPPORTED`"},
	}
	for _, c := range cases {
		if got := statusVerdict(c.seen); got != c.want {
			t.Errorf("%s: statusVerdict = %q, want %q", c.name, got, c.want)
		}
	}
}

func TestCancelVerdict(t *testing.T) {
	unknown := errAns(404, "BLOB_UPLOAD_UNKNOWN")
	cases := []struct {
		name string
		seen uploadSeen
		want string
	}{
		{"distribution", uploadSeen{cancel: ans(204, ""), afterGet: unknown, afterPut: unknown}, "works: 204, then its `GET` 404 `BLOB_UPLOAD_UNKNOWN`"},
		{"nexus docker", uploadSeen{cancel: ans(204, ""), afterGet: unknown, afterPut: ans(500, "")}, "works: 204, then its `GET` 404 `BLOB_UPLOAD_UNKNOWN`; a `PATCH` after it **500**"},
		{"ghcr", uploadSeen{cancel: errAns(405, "UNSUPPORTED")}, "405 `UNSUPPORTED`"},
	}
	for _, c := range cases {
		if got := cancelVerdict(c.seen); got != c.want {
			t.Errorf("%s: cancelVerdict = %q, want %q", c.name, got, c.want)
		}
	}
}

func TestMountVerdict(t *testing.T) {
	cases := []struct {
		seen uploadSeen
		want string
	}{
		{uploadSeen{mount: ans(201, ""), mounted: ans(200, "")}, "works, 201"},
		{uploadSeen{mount: ans(201, ""), mounted: errAns(404, "BLOB_UNKNOWN")}, "**201, but the blob is not there**: its `HEAD` 404 `BLOB_UNKNOWN`"},
		{uploadSeen{mount: ans(202, "")}, "**202: a session instead**"},
		{uploadSeen{mount: errAns(405, "UNSUPPORTED")}, "405 `UNSUPPORTED`"},
	}
	for _, c := range cases {
		if got := mountVerdict(c.seen); got != c.want {
			t.Errorf("mountVerdict = %q, want %q", got, c.want)
		}
	}
}

func TestWrongDigestVerdict(t *testing.T) {
	stored := ans(http.StatusCreated, "")
	there := ans(200, "")
	cases := []struct {
		name string
		seen uploadSeen
		want string
	}{
		{"distribution", uploadSeen{wrong: errAns(400, "DIGEST_INVALID"), wrongGet: ans(204, ""), empty: stored, emptyHead: there},
			"400 `DIGEST_INVALID`, the session stays; the empty blob stored"},
		{"nexus oci", uploadSeen{wrong: errAns(400, "DIGEST_INVALID"), wrongGet: errAns(404, "BLOB_UPLOAD_UNKNOWN"), empty: stored, emptyHead: there},
			"400 `DIGEST_INVALID`, the session ends; the empty blob stored"},
		{"ghcr", uploadSeen{wrong: errAns(404, "BLOB_UPLOAD_INVALID"), wrongGet: ans(303, ""), empty: errAns(404, "BLOB_UNKNOWN")},
			"404 `BLOB_UPLOAD_INVALID`, the session then 303; the empty blob **refused**: 404 `BLOB_UNKNOWN`"},
		{"jcr", uploadSeen{wrong: stored, wrongHead: there, empty: stored, emptyHead: there},
			"**a wrong digest is 201** **and stored under it**; the empty blob stored"},
	}
	for _, c := range cases {
		if got := wrongDigestVerdict(c.seen); got != c.want {
			t.Errorf("%s: wrongDigestVerdict = %q\nwant %q", c.name, got, c.want)
		}
	}
}
