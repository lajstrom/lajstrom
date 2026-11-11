package generic

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"strconv"
	"testing"

	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"oras.land/oras-go/v2/content"

	"github.com/lajstrom/lajstrom/internal/config"
	"github.com/lajstrom/lajstrom/internal/provider"
)

// TestStatManifest checks the answers of StatManifest: the one that is ErrManifestUnknown (a
// HEAD 404 and a GET 404 MANIFEST_UNKNOWN), and every other failure, which is not.
func TestStatManifest(t *testing.T) {
	const name = "raw/scratch/notes"
	const repo = "art/" + name
	data := []byte(`{"schemaVersion":2}`)
	d := digest.FromBytes(data)
	head := "HEAD /v2/" + repo + "/manifests/" + d.String()
	get := "GET /v2/" + repo + "/manifests/" + d.String()
	// answer answers HEAD with headStatus and GET with getStatus and the code (none when empty).
	answer := func(headStatus, getStatus int, code string) func(http.ResponseWriter, *http.Request) bool {
		return func(w http.ResponseWriter, r *http.Request) bool {
			status := headStatus
			if r.Method == http.MethodGet {
				status = getStatus
			}
			switch {
			case status == http.StatusOK:
				w.Header().Set("Content-Type", ocispec.MediaTypeImageManifest)
				w.Header().Set("Content-Length", strconv.Itoa(len(data)))
				w.WriteHeader(http.StatusOK)
				if r.Method == http.MethodGet {
					_, _ = w.Write(data)
				}
			case code != "":
				writeErr(w, status, code, "from the test")
			default:
				w.WriteHeader(status)
			}
			return true
		}
	}
	present := ocispec.Descriptor{MediaType: ocispec.MediaTypeImageManifest, Digest: d, Size: int64(len(data))}
	tests := []struct {
		name     string
		hook     func(http.ResponseWriter, *http.Request) bool
		want     ocispec.Descriptor
		err      error // errors.Is; nil for success
		unknown  bool  // the error wraps ErrManifestUnknown
		requests []string
	}{
		{name: "present", hook: answer(200, 0, ""), want: present, requests: []string{head}},
		{name: "a 404 and MANIFEST_UNKNOWN", hook: answer(404, 404, "MANIFEST_UNKNOWN"),
			err: provider.ErrNotFound, unknown: true, requests: []string{head, get}},
		{name: "a 404 and NAME_UNKNOWN", hook: answer(404, 404, "NAME_UNKNOWN"),
			err: provider.ErrNotFound, requests: []string{head, get}},
		{name: "a 404 without a code", hook: answer(404, 404, ""), err: provider.ErrNotFound, requests: []string{head, get}},
		{name: "a HEAD 404 that GET finds", hook: answer(404, 200, ""), want: present, requests: []string{head, get}},
		{name: "MANIFEST_UNKNOWN with another status", hook: answer(404, 400, "MANIFEST_UNKNOWN"),
			err: provider.ErrNotFound, requests: []string{head, get}},
		{name: "a 5xx", hook: answer(503, 0, ""), err: errAny, requests: []string{head}},
		{name: "a 5xx on the GET", hook: answer(404, 500, ""), err: errAny, requests: []string{head, get}},
		{name: "a 401", hook: answer(401, 0, "UNAUTHORIZED"), err: provider.ErrUnauthorized, requests: []string{head}},
		{name: "a 403", hook: answer(403, 0, "DENIED"), err: provider.ErrUnauthorized, requests: []string{head}},
		{name: "another digest", hook: func(w http.ResponseWriter, _ *http.Request) bool {
			w.Header().Set("Docker-Content-Digest", digest.FromString("other").String())
			return true
		}, err: content.ErrMismatchedDigest, requests: []string{head}},
	}
	for i, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			log, _ := testLogger()
			f := newFakeRegistry(t)
			f.hook = tt.hook
			p := f.provider(t, log, func(c *config.Registry) { c.Name, c.Retries = "stat-manifest-"+strconv.Itoa(i), -1 })
			got, err := p.StatManifest(context.Background(), name, d)
			wantStat(t, got, err, tt.want, tt.err, tt.unknown)
			if got := f.log(); !slices.Equal(got, tt.requests) {
				t.Errorf("requests %q, want %q", got, tt.requests)
			}
		})
	}
}

// wantStat fails t when StatManifest's answer is not want, or its error not wantErr's (nil:
// none; errAny: any error but ErrNotFound), or it wraps ErrManifestUnknown other than unknown
// says.
func wantStat(t *testing.T, got ocispec.Descriptor, err error, want ocispec.Descriptor, wantErr error, unknown bool) {
	t.Helper()
	switch {
	case wantErr == nil && (err != nil || got.Digest != want.Digest || got.MediaType != want.MediaType || got.Size != want.Size):
		t.Errorf("StatManifest = %+v, %v; want %+v", got, err, want)
	case wantErr == nil:
	case errors.Is(wantErr, errAny) && (err == nil || errors.Is(err, provider.ErrNotFound)):
		t.Errorf("StatManifest: %v, want an error that is not ErrNotFound", err)
	case !errors.Is(wantErr, errAny) && !errors.Is(err, wantErr):
		t.Errorf("StatManifest: %v, want %v", err, wantErr)
	}
	if errors.Is(err, provider.ErrManifestUnknown) != unknown {
		t.Errorf("StatManifest: %v; ErrManifestUnknown %v, want %v", err, !unknown, unknown)
	}
}

// TestStatManifestFake checks StatManifest against the fake's own manifests: the Accept it
// sends and the answer for a manifest it does not have.
func TestStatManifestFake(t *testing.T) {
	const name = "raw/scratch/notes"
	log, _ := testLogger()
	f := newFakeRegistry(t)
	data := []byte(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json"}`)
	d := digest.FromBytes(data)
	f.manifests["art/"+name] = map[string]*fakeManifest{d.String(): {
		desc: ocispec.Descriptor{MediaType: ocispec.MediaTypeImageManifest, Digest: d, Size: int64(len(data))}, content: data}}
	var accept string
	f.hook = func(_ http.ResponseWriter, r *http.Request) bool {
		accept = r.Header.Get("Accept")
		return false
	}
	p := f.provider(t, log, nil)
	if got, err := p.StatManifest(context.Background(), name, d); err != nil || got.Size != int64(len(data)) {
		t.Errorf("StatManifest = %+v, %v", got, err)
	}
	if accept != manifestAccept {
		t.Errorf("Accept %q, want %q", accept, manifestAccept)
	}
	if _, err := p.StatManifest(context.Background(), name, digest.FromString("gone")); !errors.Is(err, provider.ErrManifestUnknown) {
		t.Errorf("StatManifest of a manifest the registry does not have: %v, want ErrManifestUnknown", err)
	}
}
