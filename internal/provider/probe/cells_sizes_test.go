package probe

import (
	"slices"
	"testing"
)

func TestHumanSize(t *testing.T) {
	cases := map[int64]string{
		8 << 20: "8 MiB", 4<<20 + 1: "4 MiB + 1 byte", 1 << 30: "1 GiB", 64 << 10: "64 KiB", 1000: "1000 bytes",
	}
	for n, want := range cases {
		if got := humanSize(n); got != want {
			t.Errorf("humanSize(%d) = %q, want %q", n, got, want)
		}
	}
}

func TestManifestLadder(t *testing.T) {
	cases := []struct {
		bound int64
		want  []int64
	}{
		{4<<20 + 1, []int64{4 << 20, 4<<20 + 1}},
		{32 << 20, []int64{4 << 20, 4<<20 + 1, 8 << 20, 16 << 20, 32 << 20}},
		{10 << 20, []int64{4 << 20, 4<<20 + 1, 8 << 20, 10 << 20}},
		{1 << 20, []int64{1 << 20}},
	}
	for _, c := range cases {
		if got := manifestLadder(c.bound); !slices.Equal(got, c.want) {
			t.Errorf("manifestLadder(%d) = %v, want %v", c.bound, got, c.want)
		}
	}
}

func TestPaddedManifest(t *testing.T) {
	r := &run{id: "0011aabb"}
	for _, size := range []int64{4 << 20, 4<<20 + 1, 600} {
		if got := int64(len(r.paddedManifest(size).body)); got != size {
			t.Errorf("paddedManifest(%d) is %d bytes", size, got)
		}
	}
}

func TestManifestVerdict(t *testing.T) {
	cases := []struct {
		name  string
		tries []sizeTry
		want  string
	}{
		{"distribution", []sizeTry{{4 << 20, ""}, {4<<20 + 1, "400 `MANIFEST_INVALID`"}}, "4 MiB; one byte more is 400 `MANIFEST_INVALID`"},
		{"gitea", []sizeTry{{4 << 20, ""}, {4<<20 + 1, ""}}, "none met: 4 MiB + 1 byte taken"},
		{"nexus", []sizeTry{{4 << 20, ""}, {4<<20 + 1, ""}, {8 << 20, ""}, {16 << 20, ""}, {32 << 20, "a 400"}}, "16 MiB taken; 32 MiB is a 400"},
		{"small", []sizeTry{{4 << 20, "413 `MANIFEST_INVALID`"}}, "4 MiB is 413 `MANIFEST_INVALID`"},
	}
	for _, c := range cases {
		if got := manifestVerdict(c.tries); got != c.want {
			t.Errorf("%s: manifestVerdict = %q, want %q", c.name, got, c.want)
		}
	}
}

func TestBlobSizeVerdicts(t *testing.T) {
	const bound = 8 << 20
	whole := []sizeTry{{bound, ""}}
	ghcr := []sizeTry{{bound, "416 `BLOB_UPLOAD_INVALID`"}, {4 << 20, ""}}
	none := []sizeTry{{bound, "a 413"}, {4 << 20, "a 413"}, {2 << 20, "a 413"}, {1 << 20, "a 413"}}
	cases := []struct {
		name         string
		put, patch   []sizeTry
		largest, chk string
	}{
		{"no limit", whole, whole, "8 MiB in one `PUT` and in one `PATCH`; no limit met", "none met: 8 MiB in one `PATCH`"},
		{"ghcr", whole, ghcr, "8 MiB in one `PUT`; 4 MiB in one `PATCH`; 8 MiB is 416 `BLOB_UPLOAD_INVALID`",
			"4 MiB a `PATCH`; 8 MiB is 416 `BLOB_UPLOAD_INVALID`"},
		{"a quota", none, none, "1 MiB in one `PUT` is a 413; 1 MiB in one `PATCH` is a 413", "1 MiB a `PATCH` is a 413"},
	}
	for _, c := range cases {
		if got := largestVerdict(bound, c.put, c.patch); got != c.largest {
			t.Errorf("%s: largestVerdict = %q\nwant %q", c.name, got, c.largest)
		}
		if got := chunkVerdict(bound, c.patch); got != c.chk {
			t.Errorf("%s: chunkVerdict = %q\nwant %q", c.name, got, c.chk)
		}
	}
}

func TestHalving(t *testing.T) {
	var asked []int64
	tries, err := halving(8<<20, func(size int64) (string, error) {
		asked = append(asked, size)
		if size > 2<<20 {
			return "a 413", nil
		}
		return "", nil
	})
	if err != nil || !slices.Equal(asked, []int64{8 << 20, 4 << 20, 2 << 20}) || taken(tries) != 2<<20 {
		t.Errorf("halving asked %v, took %d, %v", asked, taken(tries), err)
	}
}
