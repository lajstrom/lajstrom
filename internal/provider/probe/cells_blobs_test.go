package probe

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/url"
	"testing"
)

func TestRedirectVerdict(t *testing.T) {
	base, _ := url.Parse("http://localhost:5002")
	presigned := "http://localhost:9000/distribution/docker/registry/v2/blobs/sha256/f3/f3/data?X-Amz-Expires=1200&X-Amz-Signature=x"
	cases := []struct {
		name string
		seen blobSeen
		want string
	}{
		{"filesystem", blobSeen{get: ans(200, "bytes"), head: ans(200, "")}, "none: the bytes"},
		{"distribution on S3", blobSeen{redirected: true, get: ans(307, "", "Location", presigned), head: ans(307, "", "Location", presigned)},
			"307 to `localhost:9000`, presigned for 20 min"},
		{"zot on S3", blobSeen{redirected: true, get: ans(307, "", "Location", presigned), head: ans(200, "")},
			"307 to `localhost:9000`, presigned for 20 min; not for `HEAD`"},
		{"own host", blobSeen{redirected: true, get: ans(302, "", "Location", "/elsewhere"), head: ans(302, "", "Location", "/elsewhere")},
			"302 to the registry's own host"},
	}
	for _, c := range cases {
		if c.seen.get.url != nil {
			c.seen.get.url, _ = url.Parse("http://localhost:5002/v2/x/blobs/sha256:00")
		}
		if got := redirectVerdict(base, c.seen); got != c.want {
			t.Errorf("%s: redirectVerdict = %q, want %q", c.name, got, c.want)
		}
	}
}

func TestRangeVerdict(t *testing.T) {
	bytes := func(code int) answer { return ans(code, "x", "Accept-Ranges", "bytes") }
	cases := []struct {
		name string
		seen blobSeen
		want string
	}{
		{"distribution", blobSeen{final: bytes(200), head: bytes(200), ranged: ans(206, "x"), pastEnd: ans(416, "")},
			"206, 416 past the end; `Accept-Ranges: bytes`"},
		{"zot", blobSeen{final: ans(200, "x"), head: bytes(200), ranged: ans(206, "x"), pastEnd: ans(416, "")},
			"206, 416 past the end; `Accept-Ranges: bytes` on `HEAD` only, not on `GET`"},
		{"gitea", blobSeen{final: ans(200, "x"), head: ans(200, ""), ranged: ans(200, "x"), pastEnd: ans(200, "x")},
			"**none: 200 with the whole blob**; no `Accept-Ranges`"},
		{"nexus oci", blobSeen{final: ans(200, "x"), head: bytes(200), ranged: errAns(500, "UNKNOWN"), pastEnd: errAns(500, "UNKNOWN")},
			"**500 to a `Range`**; `Accept-Ranges: bytes` on `HEAD` only, not on `GET`"},
		{"after a redirect", blobSeen{redirected: true, final: bytes(200), head: bytes(200), ranged: ans(206, "x"), pastEnd: ans(416, "")},
			"206, 416 past the end, after the redirect; `Accept-Ranges: bytes`"},
	}
	for _, c := range cases {
		if got := rangeVerdict(c.seen); got != c.want {
			t.Errorf("%s: rangeVerdict = %q\nwant %q", c.name, got, c.want)
		}
	}
}

func TestPattern(t *testing.T) {
	read := func(seed string, size int64) []byte {
		b, err := io.ReadAll(newPattern(seed, size))
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	for _, size := range []int64{0, 5, patternBlock - 1, patternBlock + 3, 3*patternBlock + 7} {
		b := read("s", size)
		if int64(len(b)) != size {
			t.Fatalf("pattern of %d bytes read %d", size, len(b))
		}
		h := sha256.Sum256(b)
		d, err := patternDigest("s", size)
		if err != nil || d.String() != "sha256:"+hex.EncodeToString(h[:]) {
			t.Errorf("patternDigest(%d) = %s, %v", size, d, err)
		}
	}
	a, b := read("a", 2*patternBlock), read("b", 2*patternBlock)
	if string(a) == string(b) {
		t.Error("two seeds give the same bytes")
	}
	if string(a[:patternBlock]) == string(a[patternBlock:]) {
		t.Error("two blocks of one pattern are the same")
	}
	if string(read("a", 2*patternBlock)) != string(a) {
		t.Error("a pattern is not the same twice")
	}
}
