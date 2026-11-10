package facade

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/lajstrom/lajstrom/internal/oci/profile"
	"github.com/lajstrom/lajstrom/internal/provider"
	"github.com/lajstrom/lajstrom/internal/provider/providertest"
)

// countingRegistry is the fake that counts what its repositories ask the backend: Exists,
// Fetch, Resolve, Push and PushReference.
type countingRegistry struct {
	*providertest.Fake
	calls *atomic.Int64
}

func (c countingRegistry) Repository(ctx context.Context, name string) (provider.Repository, error) {
	rr, err := c.Fake.Repository(ctx, name)
	return countingRepo{Repository: rr, calls: c.calls}, err
}

type countingRepo struct {
	provider.Repository
	calls *atomic.Int64
}

func (r countingRepo) Exists(ctx context.Context, d ocispec.Descriptor) (bool, error) {
	r.calls.Add(1)
	return r.Repository.Exists(ctx, d)
}

func (r countingRepo) Fetch(ctx context.Context, d ocispec.Descriptor) (io.ReadCloser, error) {
	r.calls.Add(1)
	return r.Repository.Fetch(ctx, d)
}

func (r countingRepo) Resolve(ctx context.Context, ref string) (ocispec.Descriptor, error) {
	r.calls.Add(1)
	return r.Repository.Resolve(ctx, ref)
}

func (r countingRepo) Push(ctx context.Context, d ocispec.Descriptor, content io.Reader) error {
	r.calls.Add(1)
	return r.Repository.Push(ctx, d, content)
}

func (r countingRepo) PushReference(ctx context.Context, d ocispec.Descriptor, content io.Reader, ref string) error {
	r.calls.Add(1)
	return r.Repository.PushReference(ctx, d, content, ref)
}

// TestManifestLayersBound: a manifest with more than maxLayers layers is 400 MANIFEST_INVALID
// naming the bound, on a managed repo and on an oci repo, and costs no request to the
// registry; so does a managed manifest that the profile refuses, whose layers are not asked
// for. maxLayers layers pass the bound (threat model M-2).
func TestManifestLayersBound(t *testing.T) {
	var calls atomic.Int64
	e := newEnv(t, envOptions{registry: func(f *providertest.Fake) provider.Provider { return countingRegistry{Fake: f, calls: &calls} }})
	layers := func(n int) []ocispec.Descriptor {
		ls := make([]ocispec.Descriptor, n)
		for i := range ls {
			ls[i] = ocispec.Descriptor{MediaType: octetStream, Digest: digest.FromString(fmt.Sprint("layer ", i)), Size: 1}
		}
		return ls
	}
	raw := rawFileOf("docs/layers.txt", "a file with many layers")
	ociManifest := func(n int) ocispec.Manifest {
		var m ocispec.Manifest
		body, _ := manifest(t, []byte("one"), "layers")
		must(t, json.Unmarshal(body, &m))
		m.Layers = layers(n)
		return m
	}
	tests := []struct {
		name    string
		target  string // the path below /v2/
		m       ocispec.Manifest
		status  int
		code    string
		message string // in the message
		calls   bool   // the registry is asked
	}{
		{name: "raw, one layer too many", target: "raw/assets/docs/layers.txt/manifests/" + raw.tag(),
			m:      func() ocispec.Manifest { m := raw.m; m.Layers = layers(maxLayers + 1); return m }(),
			status: http.StatusBadRequest, code: codeManifestInvalid, message: fmt.Sprintf("%d layers, the limit is %d", maxLayers+1, maxLayers)},
		{name: "oci, one layer too many", target: "oci/images/app/manifests/v1", m: ociManifest(maxLayers + 1),
			status: http.StatusBadRequest, code: codeManifestInvalid, message: fmt.Sprintf("the limit is %d", maxLayers)},
		{name: "raw, refused by the profile with a layer not uploaded", target: "raw/assets/docs/layers.txt/manifests/" + raw.tag(),
			m: func() ocispec.Manifest {
				m := raw.m
				m.Annotations = map[string]string{}
				for k, v := range raw.m.Annotations {
					m.Annotations[k] = v
				}
				delete(m.Annotations, profile.AnnotationSchema)
				return m
			}(),
			status: http.StatusBadRequest, code: codeManifestInvalid, message: "not a valid raw artifact"},
		{name: "oci, at the bound", target: "oci/images/app/manifests/v2", m: ociManifest(maxLayers),
			status: http.StatusCreated, calls: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			body, err := json.Marshal(tt.m)
			must(t, err)
			calls.Store(0)
			rr := e.do(e.h, http.MethodPut, "/v2/"+tt.target, bytes.NewReader(body), "Content-Type", ocispecManifest, principalHeader, "alice")
			if tt.code != "" {
				entry := wantEntry(t, tt.name, rr, tt.status, tt.code)
				if !strings.Contains(entry.Message, tt.message) {
					t.Errorf("message %q does not contain %q", entry.Message, tt.message)
				}
			} else {
				wantStatus(t, tt.name, rr, tt.status)
			}
			if n := calls.Load(); (n > 0) != tt.calls {
				t.Errorf("%d requests to the registry, want them: %v", n, tt.calls)
			}
		})
	}
}
