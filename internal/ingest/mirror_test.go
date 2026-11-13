package ingest_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/opencontainers/go-digest"
	"github.com/opencontainers/image-spec/specs-go"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/lajstrom/lajstrom/internal/index"
	"github.com/lajstrom/lajstrom/internal/ingest"
	"github.com/lajstrom/lajstrom/internal/oci/profile"
	"github.com/lajstrom/lajstrom/internal/provider"
	"github.com/lajstrom/lajstrom/internal/provider/providertest"
)

// mirrorEnv is newEnv with an oci proxy repo, oci/hub, as the env's repo.
func mirrorEnv(t *testing.T) *env {
	t.Helper()
	e := newEnv(t)
	e.repo = index.Repo{Format: "oci", Name: "hub", Registry: "local", Prefix: "oci/hub", Kind: index.KindProxy,
		Upstream: []byte(`{"name":"dockerhub"}`)}
	must(t, e.st.CreateRepo(t.Context(), &e.repo))
	return e
}

// image is a manifest of one layer, with its config and layer pushed to the fake at path.
func image(t *testing.T, e *env, path, layer string) ingest.Mirror {
	t.Helper()
	ctx := t.Context()
	rr, err := e.prov.Repository(ctx, "oci/hub/"+path)
	must(t, err)
	config, data := []byte(`{"architecture":"arm64"}`), []byte(layer)
	cd := ocispec.Descriptor{MediaType: ocispec.MediaTypeImageConfig, Digest: digest.FromBytes(config), Size: int64(len(config))}
	ld := ocispec.Descriptor{MediaType: ocispec.MediaTypeImageLayerGzip, Digest: digest.FromBytes(data), Size: int64(len(data))}
	must(t, rr.Push(ctx, cd, bytes.NewReader(config)))
	must(t, rr.Push(ctx, ld, bytes.NewReader(data)))
	raw, err := json.Marshal(ocispec.Manifest{Versioned: specs.Versioned{SchemaVersion: 2}, MediaType: ocispec.MediaTypeImageManifest, Config: cd, Layers: []ocispec.Descriptor{ld}})
	must(t, err)
	return ingest.Mirror{Path: path, Upstream: "dockerhub", Manifest: raw,
		Desc: ocispec.Descriptor{MediaType: ocispec.MediaTypeImageManifest, Digest: digest.FromBytes(raw), Size: int64(len(raw))}}
}

// TestMirrorManifestRepoint: a mirrored manifest is held under its keep-tag; the upstream's
// tag is pushed, and moved when the upstream moves it, while the keep-tag of the first stays
// (docs/spec/proxy-and-group.md §11.6). oci_repos and the audit row proxy.fetch record it.
func TestMirrorManifestRepoint(t *testing.T) {
	e := mirrorEnv(t)
	ctx := t.Context()
	first, second := image(t, e, "library/nginx", "one"), image(t, e, "library/nginx", "two")
	first.Tag, second.Tag = "1.27", "1.27"
	must(t, e.pipe.MirrorManifest(ctx, e.repo, first, alice))
	if got := e.tagged(t, "oci/hub/library/nginx", "1.27"); got != first.Desc.Digest {
		t.Fatalf("1.27 names %s, want %s", got, first.Desc.Digest)
	}
	must(t, e.pipe.MirrorManifest(ctx, e.repo, second, alice))
	for tag, want := range map[string]digest.Digest{
		"1.27": second.Desc.Digest, profile.KeepTag(first.Desc.Digest): first.Desc.Digest, profile.KeepTag(second.Desc.Digest): second.Desc.Digest,
	} {
		if got := e.tagged(t, "oci/hub/library/nginx", tag); got != want {
			t.Errorf("%s names %s, want %s", tag, got, want)
		}
	}
	r := e.rows(t)
	if len(r.OCIRepos) != 1 || r.OCIRepos[0].Path != "oci/hub/library/nginx" {
		t.Errorf("oci_repos = %+v, want the full OCI name, oci/hub/library/nginx, as reindex writes it", r.OCIRepos)
	}
	if len(r.Audit) != 2 || r.Audit[0].Action != "proxy.fetch" || r.Audit[0].State != index.AuditOK || r.Audit[0].Subject != "oci/hub/library/nginx" ||
		!strings.Contains(string(r.Audit[0].Details), `"upstream":"dockerhub"`) || !strings.Contains(string(r.Audit[0].Details), `"tag":"1.27"`) {
		t.Errorf("audit rows = %+v", r.Audit)
	}
	// A repeat pushes nothing new: the tags name the manifest already.
	before := len(e.prov.PushReferences())
	must(t, e.pipe.MirrorManifest(ctx, e.repo, second, alice))
	if n := len(e.prov.PushReferences()) - before; n != 0 {
		t.Errorf("a repeat pushed %d times, want none", n)
	}
}

// TestMirrorManifestRefused: MirrorManifest takes an oci proxy repo's manifest, as it came.
func TestMirrorManifestRefused(t *testing.T) {
	e := mirrorEnv(t)
	m := image(t, e, "library/nginx", "one")
	bad := m
	bad.Manifest = append(bytes.Clone(m.Manifest), ' ')
	hosted := e.repo
	hosted.Kind = index.KindHosted
	for name, tc := range map[string]struct {
		repo index.Repo
		m    ingest.Mirror
	}{
		"bytes that are not the digest": {e.repo, bad},
		"a hosted oci repo":             {hosted, m},
		"a proxy of another format":     {index.Repo{Format: "npm", Name: "npmjs", Registry: "local", Kind: index.KindProxy}, m},
	} {
		if err := e.pipe.MirrorManifest(t.Context(), tc.repo, tc.m, alice); !errors.Is(err, ingest.ErrInvalid) {
			t.Errorf("%s: %v, want ErrInvalid", name, err)
		}
	}
	if n := len(e.prov.PushReferences()); n != 0 {
		t.Errorf("%d pushes, want none", n)
	}
}

// immutable is a registry whose tags do not move: a push of a tag that names another digest is
// provider.ErrConflict, as Quay's 409 TAG_IMMUTABLE; keep-tags pass.
type immutable struct{ *providertest.Fake }

func (r immutable) Repository(ctx context.Context, name string) (provider.Repository, error) {
	rr, err := r.Fake.Repository(ctx, name)
	return immutableRepo{rr}, err
}

type immutableRepo struct{ provider.Repository }

func (r immutableRepo) PushReference(ctx context.Context, desc ocispec.Descriptor, content io.Reader, ref string) error {
	if d, err := r.Resolve(ctx, ref); err == nil && d.Digest != desc.Digest {
		return provider.ErrConflict
	}
	return r.Repository.PushReference(ctx, desc, content, ref)
}

// TestMirrorManifestTagRefused: a backend that refuses to move the tag leaves the manifest held
// under its keep-tag, and the error is ErrConflict, for the caller to remember.
func TestMirrorManifestTagRefused(t *testing.T) {
	e := mirrorEnv(t)
	e.pipe = ingest.New(e.options(immutable{e.prov}))
	ctx := t.Context()
	first, second := image(t, e, "library/nginx", "one"), image(t, e, "library/nginx", "two")
	first.Tag, second.Tag = "latest", "latest"
	must(t, e.pipe.MirrorManifest(ctx, e.repo, first, alice))
	if err := e.pipe.MirrorManifest(ctx, e.repo, second, alice); !errors.Is(err, ingest.ErrConflict) {
		t.Fatalf("a tag the backend will not move: %v, want ErrConflict", err)
	}
	if got := e.tagged(t, "oci/hub/library/nginx", profile.KeepTag(second.Desc.Digest)); got != second.Desc.Digest {
		t.Errorf("the keep-tag names %s, want %s", got, second.Desc.Digest)
	}
	if got := e.tagged(t, "oci/hub/library/nginx", "latest"); got != first.Desc.Digest {
		t.Errorf("latest names %s, want the first digest", got)
	}
	if ev := e.rows(t).Audit[0]; ev.State != index.AuditOK || !strings.Contains(string(ev.Details), `"tagRefused":true`) {
		t.Errorf("the audit row of the refused tag: %s %s", ev.State, ev.Details)
	}
}

// TestMirrorBlob: a blob streams into the backend and is committed only when its bytes are the
// size and the digest its manifest says; otherwise nothing is kept and no session is left.
func TestMirrorBlob(t *testing.T) {
	e := mirrorEnv(t)
	ctx := t.Context()
	data := strings.Repeat("layer ", 1000)
	desc := ocispec.Descriptor{MediaType: ocispec.MediaTypeImageLayerGzip, Digest: digest.FromString(data), Size: int64(len(data))}
	for name, body := range map[string]string{"other bytes": strings.Repeat("LAYER ", 1000), "short": data[:10], "long": data + "x"} {
		if err := e.pipe.MirrorBlob(ctx, e.repo, "library/nginx", desc, strings.NewReader(body)); !errors.Is(err, ingest.ErrInvalid) {
			t.Errorf("%s: %v, want ErrInvalid", name, err)
		}
	}
	if _, err := e.prov.StatBlob(ctx, "oci/hub/library/nginx", desc.Digest); !errors.Is(err, provider.ErrNotFound) {
		t.Fatalf("the blob after refused copies: %v, want none", err)
	}
	must(t, e.pipe.MirrorBlob(ctx, e.repo, "library/nginx", desc, strings.NewReader(data)))
	if b, err := e.prov.StatBlob(ctx, "oci/hub/library/nginx", desc.Digest); err != nil || b.Size != desc.Size {
		t.Fatalf("StatBlob: %+v, %v", b, err)
	}
	if n := e.prov.OpenUploads(); n != 0 {
		t.Errorf("%d upload sessions left open", n)
	}
}

// TestMirrorMount: a blob the backend holds at another path of the repo is mounted, no byte
// sent; a source that does not hold it is no mount, and the session the registry opened instead
// is cancelled (docs/spec/proxy-and-group.md §11.5).
func TestMirrorMount(t *testing.T) {
	e := mirrorEnv(t)
	ctx := t.Context()
	data := strings.Repeat("shared layer ", 1000)
	desc := ocispec.Descriptor{MediaType: ocispec.MediaTypeImageLayerGzip, Digest: digest.FromString(data), Size: int64(len(data))}
	must(t, e.pipe.MirrorBlob(ctx, e.repo, "k8s/kube-controller-manager", desc, strings.NewReader(data)))
	if mounted, err := e.pipe.MirrorMount(ctx, e.repo, "k8s/kube-apiserver", "k8s/etcd", desc); mounted || err != nil {
		t.Fatalf("a mount from a path without the blob: %t, %v; want no mount and no error", mounted, err)
	}
	if n := e.prov.OpenUploads(); n != 0 {
		t.Errorf("%d upload sessions left open", n)
	}
	if _, err := e.pipe.MirrorMount(ctx, e.repo, "k8s/kube-apiserver", "k8s/kube-apiserver", desc); !errors.Is(err, ingest.ErrInvalid) {
		t.Errorf("a mount from the path itself: %v, want ErrInvalid", err)
	}
	if mounted, err := e.pipe.MirrorMount(ctx, e.repo, "k8s/kube-apiserver", "k8s/kube-controller-manager", desc); !mounted || err != nil {
		t.Fatalf("MirrorMount: %t, %v", mounted, err)
	}
	if b, err := e.prov.StatBlob(ctx, "oci/hub/k8s/kube-apiserver", desc.Digest); err != nil || b.Size != desc.Size {
		t.Fatalf("StatBlob of the mounted blob: %+v, %v", b, err)
	}
}

// TestMirrorVisibility: on a lagging registry the push is read back until the registry serves
// it, and one it does not serve in time is provider.ErrUnavailable with the audit row failed.
func TestMirrorVisibility(t *testing.T) {
	e := mirrorEnv(t)
	m := image(t, e, "library/nginx", "one")
	lag := &providertest.Lag{Fake: e.prov, Manifests: 2}
	e.pipe = ingest.New(e.options(lag))
	must(t, e.pipe.MirrorManifest(t.Context(), e.repo, m, alice))
	if lag.Misses() == 0 {
		t.Error("the barrier read nothing back")
	}
	other := image(t, e, "library/redis", "two")
	opts := e.options(&providertest.Lag{Fake: e.prov, Manifests: -1})
	opts.VisibilityTimeout = 1
	e.pipe = ingest.New(opts)
	if err := e.pipe.MirrorManifest(t.Context(), e.repo, other, alice); !errors.Is(err, provider.ErrUnavailable) {
		t.Fatalf("a push the registry does not serve: %v, want ErrUnavailable", err)
	}
	if ev := e.rows(t).Audit[0]; ev.State != index.AuditFailed {
		t.Errorf("the audit row of a push not served: %s", ev.State)
	}
}
