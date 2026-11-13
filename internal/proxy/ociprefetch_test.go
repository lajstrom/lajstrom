package proxy_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/opencontainers/go-digest"
	"github.com/opencontainers/image-spec/specs-go"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/lajstrom/lajstrom/internal/format"
	"github.com/lajstrom/lajstrom/internal/index"
	"github.com/lajstrom/lajstrom/internal/proxy"
)

// short is a result as "kind platform digest status[ httpStatus]", the digest cut to 12 hex digits
// and "-" for what a result does not have.
func short(r proxy.PrefetchResult) string {
	or := func(s string) string {
		if s == "" {
			return "-"
		}
		return s
	}
	d := ""
	if r.Digest != "" {
		d = r.Digest.Encoded()
	}
	if len(d) > 12 {
		d = d[:12]
	}
	s := fmt.Sprintf("%s %s %s %s %s", r.Name, or(r.Kind), or(r.Platform), or(d), r.Status)
	if r.HTTPStatus != 0 {
		s += fmt.Sprint(" ", r.HTTPStatus)
	}
	return s
}

func shorts(rs []proxy.PrefetchResult) string {
	out := make([]string, len(rs))
	for i, r := range rs {
		out[i] = short(r)
	}
	return strings.Join(out, "\n")
}

// line is the short form of a result of ref.
func line(ref, kind, platform string, d digest.Digest, status string) string {
	return short(proxy.PrefetchResult{Name: ref, Kind: kind, Platform: platform, Digest: d, Status: status})
}

// The blobs of a manifest come in the order listedBy gives them: its layers, then its config.

// TestOCIPrefetch is #886: a prefetch of two platforms of an image of three fetches the index, the
// two manifests and their blobs, each once, and holds the manifests under their keep-tags; the
// index, whose third platform is not held, stays in proxy_meta with its tag, and an offline repo
// serves the two platforms from what the prefetch kept. A second run is all held and asks the
// upstream nothing; a run of the three platforms completes the index and pushes the tag
// (docs/spec/proxy-and-group.md §11.10).
func TestOCIPrefetch(t *testing.T) {
	e := newOCIEnv(t, ociSettings)
	img := e.hub.image(t, "library/nginx", "1.27", "amd64", "arm64", "s390x")
	c := e.core("a", proxy.Options{})
	const ref = "nginx:1.27"
	got, err := c.PrefetchOCI(t.Context(), e.repo, []string{ref}, []string{"linux/amd64", "linux/arm64/v8"})
	must(t, err)
	want := []string{line(ref, "index", "", img.index.Digest, "fetched")}
	for i, p := range []string{"linux/amd64", "linux/arm64"} {
		want = append(want, line(ref, "manifest", p, img.manifests[i].Digest, "fetched"),
			line(ref, "blob", p, img.blobs[i][1].Digest, "fetched"), line(ref, "blob", p, img.blobs[i][0].Digest, "fetched"))
	}
	if g := shorts(got); g != strings.Join(want, "\n") {
		t.Fatalf("results:\n%s\nwant:\n%s", g, strings.Join(want, "\n"))
	}
	if b, w := got[1].Bytes, img.manifests[0].Size+img.blobs[0][0].Size+img.blobs[0][1].Size; b != w {
		t.Errorf("the amd64 manifest's bytes = %d, want its own and its blobs', %d", b, w)
	}
	if b := got[3].Bytes; b != img.blobs[0][0].Size {
		t.Errorf("the amd64 config's bytes = %d, want %d", b, img.blobs[0][0].Size)
	}
	for i := range 2 {
		if g := e.tagged(t, "library/nginx", "_keep.sha256."+img.manifests[i].Digest.Encoded()); g != img.manifests[i].Digest {
			t.Errorf("the keep-tag of platform %d names %q in the backend", i, g)
		}
	}
	if g := e.tagged(t, "library/nginx", "1.27"); g != "" {
		t.Errorf("1.27 names %s in the backend, with its index incomplete", g)
	}
	gets, blobs := e.hub.count("GET /v2/library/nginx/manifests/"), e.hub.count("GET /v2/library/nginx/blobs/")
	if gets != 3 || blobs != 4 {
		t.Errorf("%d GETs of manifests and %d of blobs, want 3 (the index, two platforms) and 4", gets, blobs)
	}

	asked := e.hub.count("GET ")
	got, err = c.PrefetchOCI(t.Context(), e.repo, []string{ref}, []string{"linux/amd64", "linux/arm64"})
	must(t, err)
	want = []string{line(ref, "index", "", img.index.Digest, "held"), line(ref, "manifest", "linux/amd64", img.manifests[0].Digest, "held"),
		line(ref, "manifest", "linux/arm64", img.manifests[1].Digest, "held")}
	if g := shorts(got); g != strings.Join(want, "\n") {
		t.Errorf("the second run:\n%s\nwant:\n%s", g, strings.Join(want, "\n"))
	}
	if n := e.hub.count("GET ") - asked; n != 0 {
		t.Errorf("the second run sent %d GETs to the upstream, want none", n)
	}

	e.servedOffline(t, c, "library/nginx", "1.27", img, 0, 1)

	got, err = c.PrefetchOCI(t.Context(), e.repo, []string{ref}, []string{"linux/amd64", "linux/arm64", "linux/s390x"})
	must(t, err)
	if g := short(got[len(got)-3]); g != line(ref, "manifest", "linux/s390x", img.manifests[2].Digest, "fetched") {
		t.Errorf("s390x: %s", g)
	}
	if g := e.tagged(t, "library/nginx", "1.27"); g != img.index.Digest {
		t.Errorf("1.27 names %q in the backend once every platform is held, want the index %s", g, img.index.Digest)
	}
}

// servedOffline checks that the repo, offline, serves the tag and the platforms of img at path
// whole, with no request to the upstream.
func (e *ociEnv) servedOffline(t *testing.T, c *proxy.Core, path, tag string, img image, platforms ...int) {
	t.Helper()
	off := e.repo
	off.Upstream = json.RawMessage(`{"name": "hub", "offline": true}`)
	asked := e.hub.count("")
	_, _, err := c.OCIManifest(t.Context(), off, path, tag)
	must(t, err)
	for _, i := range platforms {
		_, _, err = c.OCIManifest(t.Context(), off, path, img.manifests[i].Digest.String())
		must(t, err)
		for _, b := range img.blobs[i] {
			if _, _, err := c.OCIBlob(t.Context(), off, path, b.Digest, true, false); err != nil {
				t.Fatalf("offline, OCIBlob %s: %v", b.Digest, err)
			}
		}
	}
	if n := e.hub.count("") - asked; n != 0 {
		t.Errorf("the offline repo sent the upstream %d requests", n)
	}
}

// TestOCIPrefetchPlatforms: platforms are required for an index, whose failure lists them, and not
// for a manifest; a platform the index does not list fails alone.
func TestOCIPrefetchPlatforms(t *testing.T) {
	e := newOCIEnv(t, ociSettings)
	img := e.hub.image(t, "library/nginx", "1.27", "amd64", "arm64")
	attest := e.hub.blob("library/nginx", ocispec.MediaTypeImageConfig, []byte(`{}`))
	raw, err := json.Marshal(ocispec.Manifest{Versioned: specs.Versioned{SchemaVersion: 2}, MediaType: ocispec.MediaTypeImageManifest,
		Config: attest, Layers: []ocispec.Descriptor{}})
	must(t, err)
	am := e.hub.put("library/nginx", ocispec.MediaTypeImageManifest, raw)
	am.Platform = &ocispec.Platform{OS: "unknown", Architecture: "unknown"}
	var idx ocispec.Index
	_, idxRaw, err := e.core("b", proxy.Options{}).OCIManifest(t.Context(), e.repo, "library/nginx", "1.27")
	must(t, err)
	must(t, json.Unmarshal(idxRaw, &idx))
	idx.Manifests = append(idx.Manifests, am)
	raw, err = json.Marshal(idx)
	must(t, err)
	attested := e.hub.put("library/nginx", ocispec.MediaTypeImageIndex, raw, "1.27-attested")
	c := e.core("a", proxy.Options{})

	got, err := c.PrefetchOCI(t.Context(), e.repo, []string{"nginx:1.27-attested"}, nil)
	must(t, err) // the index is fetched, to list its platforms, and kept
	if len(got) != 1 || got[0].Status != proxy.PrefetchFailed || got[0].HTTPStatus != http.StatusBadRequest ||
		got[0].Kind != proxy.PrefetchIndex || !strings.Contains(got[0].Message, "linux/amd64, linux/arm64, unknown/unknown") {
		t.Errorf("an index without platforms: %+v, want one failed 400 that lists its platforms", got)
	}

	ref := "library/nginx@" + img.manifests[1].Digest.String()
	got, err = c.PrefetchOCI(t.Context(), e.repo, []string{ref}, nil)
	must(t, err)
	want := strings.Join([]string{line(ref, "manifest", "", img.manifests[1].Digest, "fetched"),
		line(ref, "blob", "", img.blobs[1][1].Digest, "fetched"), line(ref, "blob", "", img.blobs[1][0].Digest, "fetched")}, "\n")
	if g := shorts(got); g != want {
		t.Errorf("a manifest by digest, without platforms:\n%s\nwant:\n%s", g, want)
	}

	ref = "nginx:1.27-attested"
	got, err = c.PrefetchOCI(t.Context(), e.repo, []string{ref}, []string{"linux/riscv64", "linux/arm64"})
	must(t, err)
	want = strings.Join([]string{line(ref, "index", "", attested.Digest, "held"), line(ref, "manifest", "linux/riscv64", "", "failed") + " 404",
		line(ref, "manifest", "linux/arm64", img.manifests[1].Digest, "held")}, "\n")
	if g := shorts(got); g != want {
		t.Errorf("a platform the index does not list:\n%s\nwant:\n%s", g, want)
	}
	if n := e.hub.count("GET /v2/library/nginx/manifests/" + am.Digest.String()); n != 0 {
		t.Errorf("the attestation manifest was asked %d times, unnamed", n)
	}
}

// TestOCIPrefetchFailures: each image fails with the status a pull would have had, and the others
// go on; a blob that failed fails its manifest, and a second run fetches it.
func TestOCIPrefetchFailures(t *testing.T) {
	e := newOCIEnv(t, `{"name": "hub", "metadataTTL": "10m", "negativeTTL": "2m", "deny": ["private/*"]}`)
	img := e.hub.image(t, "library/nginx", "1.27", "amd64")
	e.hub.image(t, "library/redis", "7.4", "amd64")
	broken := img.blobs[0][1].Digest
	e.hub.hook = func(w http.ResponseWriter, r *http.Request) bool {
		if r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, broken.String()) {
			hubError(w, http.StatusInternalServerError, "UNKNOWN")
			return true
		}
		return false
	}
	c := e.core("a", proxy.Options{})
	refs := []string{"nginx:1.27", "nothere:1", "nginx:9.9", "private/app:1", "Nginx:1.27", "docker.io/library/nginx:1.27",
		"app/index.json:1", "nginx@sha256:nope"}
	got, err := c.PrefetchOCI(t.Context(), e.repo, refs, []string{"linux/amd64"})
	must(t, err)
	want := strings.Join([]string{
		line("nginx:1.27", "index", "", img.index.Digest, "fetched"),
		line("nginx:1.27", "manifest", "linux/amd64", img.manifests[0].Digest, "failed") + " 502",
		line("nginx:1.27", "blob", "linux/amd64", broken, "failed") + " 502",
		line("nginx:1.27", "blob", "linux/amd64", img.blobs[0][0].Digest, "fetched"),
		line("nothere:1", "", "", "", "failed") + " 404",
		line("nginx:9.9", "", "", "", "failed") + " 404",
		line("private/app:1", "", "", "", "failed") + " 403",
		line("Nginx:1.27", "", "", "", "failed") + " 400",
		line("docker.io/library/nginx:1.27", "", "", "", "failed") + " 400",
		line("app/index.json:1", "", "", "", "failed") + " 400",
		line("nginx@sha256:nope", "", "", "", "failed") + " 400",
	}, "\n")
	if g := shorts(got); g != want {
		t.Fatalf("results:\n%s\nwant:\n%s", g, want)
	}
	if !strings.Contains(got[1].Message, "1 of its 2 blobs failed; "+broken.String()) {
		t.Errorf("the manifest's message: %q", got[1].Message)
	}
	if strings.Contains(got[2].Message, e.hub.srv.URL) {
		t.Errorf("the blob's message names the upstream's URL: %q", got[2].Message)
	}

	e.hub.hook = nil
	got, err = c.PrefetchOCI(t.Context(), e.repo, refs[:1], []string{"linux/amd64"})
	must(t, err)
	want = strings.Join([]string{line("nginx:1.27", "index", "", img.index.Digest, "held"),
		line("nginx:1.27", "manifest", "linux/amd64", img.manifests[0].Digest, "fetched"),
		line("nginx:1.27", "blob", "linux/amd64", broken, "fetched")}, "\n")
	if g := shorts(got); g != want {
		t.Errorf("the second run:\n%s\nwant:\n%s", g, want)
	}
	if g := e.tagged(t, "library/nginx", "1.27"); g != img.index.Digest {
		t.Errorf("1.27 names %q in the backend, want the index", g)
	}

	e.hub.tooMany.Store(true)
	got, err = c.PrefetchOCI(t.Context(), e.repo, []string{"redis:7.4"}, []string{"linux/amd64"})
	must(t, err)
	if len(got) != 1 || got[0].HTTPStatus != http.StatusServiceUnavailable || got[0].RetryAfter != 7 {
		t.Errorf("a 429: %+v, want one failed 503 with retryAfter 7", got)
	}
}

// TestOCIPrefetchShared: two images of one request that share layers at two paths fetch each
// shared layer once, and the second path mounts it, so that it is held whole at its own path;
// on a backend that does not mount, the second path fetches it again.
func TestOCIPrefetchShared(t *testing.T) {
	for _, mounts := range []bool{true, false} {
		t.Run(fmt.Sprintf("mounts %t", mounts), func(t *testing.T) {
			e := newOCIEnv(t, ociSettings)
			a := e.hub.sharedImage(t, kcm, "v1.37.1", k8sShared, "arm64")
			b := e.hub.sharedImage(t, kas, "v1.37.1", k8sShared, "arm64")
			c := e.core("a", proxy.Options{})
			if !mounts {
				c = e.coreOn(noMount{e.reg}, "a", proxy.Options{})
			}
			got, err := c.PrefetchOCI(t.Context(), e.repo, []string{kcm + ":v1.37.1", kas + ":v1.37.1"}, []string{"linux/arm64"})
			must(t, err)
			for _, r := range got {
				if r.Status != "fetched" {
					t.Errorf("%s, want fetched", short(r))
				}
			}
			kasBlobs := 0
			for _, r := range got {
				if r.Name == kas+":v1.37.1" && r.Kind == proxy.PrefetchBlob {
					kasBlobs++
				}
			}
			want, mountsWant := 2, 0
			if mounts {
				mountsWant = 2
			} else {
				want = 4
			}
			if kasBlobs != want {
				t.Errorf("%d blob results of kube-apiserver, want %d:\n%s", kasBlobs, want, shorts(got))
			}
			if n := e.blobGETs(kas, b.blobs[0][1:3]...); n != 2-mountsWant {
				t.Errorf("the upstream was asked %d times for the layers kube-apiserver shares, want %d", n, 2-mountsWant)
			}
			for path, im := range map[string]image{kcm: a, kas: b} {
				if g := e.tagged(t, path, "v1.37.1"); g != im.index.Digest {
					t.Errorf("v1.37.1 of %s names %q in the backend, want its index", path, g)
				}
			}
		})
	}
}

// TestOCIPrefetchRefused: what PrefetchOCI refuses before any image.
func TestOCIPrefetchRefused(t *testing.T) {
	e := newOCIEnv(t, ociSettings)
	c := e.core("a", proxy.Options{})
	hosted := e.repo
	hosted.Kind = index.KindHosted
	off := e.repo
	off.Upstream = json.RawMessage(`{"name": "hub", "offline": true}`)
	many := make([]string, proxy.MaxPrefetchNames+1)
	for i := range many {
		many[i] = fmt.Sprint("img", i)
	}
	for _, tc := range []struct {
		name      string
		repo      index.Repo
		refs      []string
		platforms []string
		want      error
	}{
		{"a hosted repo", hosted, []string{"nginx"}, nil, format.ErrInvalid},
		{"no image", e.repo, nil, nil, format.ErrInvalid},
		{"too many", e.repo, many, nil, format.ErrInvalid},
		{"a platform of one part", e.repo, []string{"nginx"}, []string{"linux"}, format.ErrInvalid},
		{"a platform of four", e.repo, []string{"nginx"}, []string{"linux/arm/v7/x"}, format.ErrInvalid},
		{"offline", off, []string{"nginx"}, nil, index.ErrConflict},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := c.PrefetchOCI(t.Context(), tc.repo, tc.refs, tc.platforms); !errors.Is(err, tc.want) {
				t.Errorf("err = %v, want %v", err, tc.want)
			}
		})
	}
	if n := e.hub.count(""); n != 0 {
		t.Errorf("%d requests to the upstream", n)
	}

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	got, err := c.PrefetchOCI(ctx, e.repo, []string{"nginx:1.27", "redis"}, nil)
	must(t, err)
	if len(got) != 2 || got[0].Status != proxy.PrefetchFailed || got[1].Message != "not attempted: the request ended first" {
		t.Errorf("a request that ended: %+v, want each image failed, not attempted", got)
	}
}
