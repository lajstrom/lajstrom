package facade

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/opencontainers/go-digest"
	"github.com/opencontainers/image-spec/specs-go"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/lajstrom/lajstrom/internal/ingest"
	"github.com/lajstrom/lajstrom/internal/provider"
	"github.com/lajstrom/lajstrom/internal/provider/providertest"
)

// refusedBlobUnknown is the message of the registry's refusal of the tests' manifests, as the
// facade answers it when it names no blob.
const refusedBlobUnknown = "registry local refused the request: 400 MANIFEST_BLOB_UNKNOWN"

// blobPart is the descriptor and the bytes that the name s stands for in a test's manifest: an
// image manifest with s as its layer for a name that starts with "manifest", a layer with urls
// for one that starts with "foreign", a malformed digest for one that starts with "bad", and
// the blob of the bytes of s otherwise.
func blobPart(t *testing.T, s string) (ocispec.Descriptor, []byte) {
	t.Helper()
	switch {
	case strings.HasPrefix(s, "manifest"):
		b, d := manifest(t, []byte(s), s)
		return ocispec.Descriptor{MediaType: ocispecManifest, Digest: d, Size: int64(len(b))}, b
	case strings.HasPrefix(s, "foreign"):
		return ocispec.Descriptor{MediaType: "application/vnd.oci.image.layer.nondistributable.v1.tar",
			Digest: digest.FromString(s), Size: int64(len(s)), URLs: []string{"https://example.com/" + s}}, []byte(s)
	case strings.HasPrefix(s, "bad"):
		return ocispec.Descriptor{MediaType: octetStream, Digest: digest.Digest("sha256:" + s), Size: int64(len(s))}, []byte(s)
	}
	return ocispec.Descriptor{MediaType: octetStream, Digest: digest.FromString(s), Size: int64(len(s))}, []byte(s)
}

// TestPassThroughNamesMissingBlob: the last MANIFEST_BLOB_UNKNOWN of a manifest push of an oci
// repo names the first blob of the manifest that the registry does not have, in the message
// after the registry's and in detail.digest, after one HEAD per distinct blob up to it: the
// config, then the layers, or the manifests of an index; a layer with urls and a malformed
// digest are not asked. When the registry has them all, or a HEAD fails, the answer is the
// registry's name, status and code alone. On a registry not classified strong the HEADs come
// after the retries; a manifest with a subject, which goes through ingest, is named alike
// (#850).
func TestPassThroughNamesMissingBlob(t *testing.T) {
	tests := []missingCase{
		{name: "a layer missing", config: "config", layers: []string{"a", "b", "c"}, stored: []string{"config", "a"},
			want: "b", heads: []string{"config", "a", "b"}},
		{name: "the config missing", config: "config", layers: []string{"a"}, stored: []string{"a"},
			want: "config", heads: []string{"config"}},
		{name: "every blob there", config: "config", layers: []string{"a"}, stored: []string{"config", "a"},
			heads: []string{"config", "a"}},
		{name: "a blob listed twice", config: "config", layers: []string{"a", "a", "b"}, stored: []string{"config", "a"},
			want: "b", heads: []string{"config", "a", "b"}},
		{name: "a layer with urls and a malformed digest", config: "config", layers: []string{"foreign", "bad", "a"},
			stored: []string{"config"}, want: "a", heads: []string{"config", "a"}},
		{name: "a HEAD that fails", config: "config", layers: []string{"a"}, failHeads: true, heads: []string{"config"}},
		{name: "an index", index: []string{"manifest-1", "manifest-2", "manifest-3"}, stored: []string{"manifest-1"},
			want: "manifest-2", heads: []string{"manifest-1", "manifest-2"}},
		{name: "a referrer", referrer: true, config: "config", layers: []string{"a"}, stored: []string{"config"},
			want: "a", heads: []string{"config", "a"}},
		{name: "after the retries of a lagging registry", lagging: true, config: "config", layers: []string{"a"},
			stored: []string{"config"}, want: "a", heads: []string{"config", "a"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var late *blobsLate
			e := newEnv(t, envOptions{registry: func(f *providertest.Fake) provider.Provider {
				late = &blobsLate{Fake: f, strong: !tt.lagging}
				late.refusals.Store(-1)
				return late
			}})
			log := slog.New(slog.NewTextHandler(e.logs, nil))
			pipe := ingest.New(ingest.Options{Providers: provider.Map{"local": e.reg}, Index: e.st, Log: log, Now: e.clock.Now,
				VisibilityTimeout: 300 * time.Millisecond})
			h := New(Options{Index: e.st, Providers: provider.Map{"local": e.reg}, Ingest: pipe, Tokens: e.tokens,
				Log: log, Now: e.clock.Now, VisibilityTimeout: 300 * time.Millisecond})
			const name = "oci/images/app"
			rr, err := e.prov.Repository(t.Context(), name)
			must(t, err)
			for _, s := range tt.stored {
				desc, b := blobPart(t, s)
				must(t, rr.Push(t.Context(), desc, bytes.NewReader(b)))
			}
			if tt.failHeads {
				e.prov.FailExists = provider.ErrUnavailable
			}
			body, contentType, ref := tt.body(t)

			rec := e.do(h, http.MethodPut, "/v2/"+name+"/manifests/"+ref, bytes.NewReader(body), "Content-Type", contentType)
			entry := wantEntry(t, "PUT", rec, http.StatusBadRequest, codeManifestBlobUnknown)
			msg, detail := tt.answer(t, name)
			if entry.Message != msg {
				t.Errorf("message %q, want %q", entry.Message, msg)
			}
			if fmt.Sprint(entry.Detail) != fmt.Sprint(detail) {
				t.Errorf("detail %#v, want %#v", entry.Detail, detail)
			}
			late.mu.Lock()
			defer late.mu.Unlock()
			if heads := digestsOf(t, tt.heads); !slices.Equal(late.heads, heads) {
				t.Errorf("HEADs %v, want %v (%v)", late.heads, heads, tt.heads)
			}
			if n := late.puts.Load(); tt.lagging && n < 2 || !tt.lagging && n != 1 {
				t.Errorf("%d manifest PUTs to the registry, want one on a strong registry and more on a lagging one", n)
			}
		})
	}
}

// missingCase is a case of TestPassThroughNamesMissingBlob; its names stand for blobs and
// manifests (blobPart).
type missingCase struct {
	name      string
	config    string   // the config of an image manifest
	layers    []string // its layers
	index     []string // the manifests of an index instead
	referrer  bool     // the image manifest has a subject
	stored    []string // what the registry has
	failHeads bool     // every HEAD fails
	lagging   bool     // the registry is not classified strong
	want      string   // the blob named; "" none
	heads     []string // the HEADs, in order
}

// body returns the manifest of c, its Content-Type, and the reference it is pushed under: a tag,
// the digest for a referrer.
func (c missingCase) body(t *testing.T) ([]byte, string, string) {
	t.Helper()
	if c.index != nil {
		idx := ocispec.Index{Versioned: specs.Versioned{SchemaVersion: 2}, MediaType: ocispec.MediaTypeImageIndex}
		for _, s := range c.index {
			desc, _ := blobPart(t, s)
			idx.Manifests = append(idx.Manifests, desc)
		}
		body, err := json.Marshal(idx)
		must(t, err)
		return body, ocispec.MediaTypeImageIndex, "v1"
	}
	config, _ := blobPart(t, c.config)
	config.MediaType = "application/vnd.example.config.v1+json"
	m := ocispec.Manifest{Versioned: specs.Versioned{SchemaVersion: 2}, MediaType: ocispecManifest,
		ArtifactType: "application/vnd.example.test", Config: config}
	for _, s := range c.layers {
		desc, _ := blobPart(t, s)
		m.Layers = append(m.Layers, desc)
	}
	ref := "v1"
	if c.referrer {
		m.Subject = &ocispec.Descriptor{MediaType: ocispecManifest, Digest: digest.FromString("a subject"), Size: 2}
	}
	body, err := json.Marshal(m)
	must(t, err)
	if c.referrer {
		ref = digest.FromBytes(body).String()
	}
	return body, ocispecManifest, ref
}

// answer returns the message and the detail of the answer that c wants for the OCI name name.
func (c missingCase) answer(t *testing.T, name string) (string, any) {
	t.Helper()
	if c.want == "" {
		return refusedBlobUnknown, nil
	}
	desc, _ := blobPart(t, c.want)
	what := "blob %s was not uploaded to %s"
	if c.index != nil {
		what = "manifest %s was not pushed to %s"
	}
	return refusedBlobUnknown + "; " + fmt.Sprintf(what, desc.Digest, name), map[string]any{"digest": desc.Digest.String()}
}

// digestsOf returns the digests that the names stand for (blobPart).
func digestsOf(t *testing.T, names []string) []digest.Digest {
	t.Helper()
	var ds []digest.Digest
	for _, s := range names {
		desc, _ := blobPart(t, s)
		ds = append(ds, desc.Digest)
	}
	return ds
}

// blobsChecked is a registry that refuses a manifest PUT with 400 MANIFEST_BLOB_UNKNOWN while it
// does not serve a blob the manifest lists, its config or a layer (Exists), as distribution
// checks them. Over a providertest.Lag a blob pushed is not served for a while, as on ECR.
type blobsChecked struct {
	provider.Provider
	puts atomic.Int64
}

func (b *blobsChecked) Repository(ctx context.Context, name string) (provider.Repository, error) {
	rr, err := b.Provider.Repository(ctx, name)
	return checkedRepository{Repository: rr, b: b}, err
}

// checkedRepository is a repository of blobsChecked.
type checkedRepository struct {
	provider.Repository
	b *blobsChecked
}

// check reads the manifest of a PUT and refuses it while a blob it lists is not served.
func (r checkedRepository) check(ctx context.Context, content io.Reader) ([]byte, error) {
	r.b.puts.Add(1)
	body, err := io.ReadAll(content)
	if err != nil {
		return nil, err
	}
	var m ocispec.Manifest
	if err := json.Unmarshal(body, &m); err != nil {
		return nil, err
	}
	for _, l := range append([]ocispec.Descriptor{m.Config}, m.Layers...) {
		ok, err := r.Exists(ctx, l)
		if err != nil {
			return nil, err
		}
		if !ok {
			return nil, &provider.RejectedError{Registry: "local", Status: http.StatusBadRequest, Code: codeManifestBlobUnknown}
		}
	}
	return body, nil
}

func (r checkedRepository) Push(ctx context.Context, desc ocispec.Descriptor, content io.Reader) error {
	if desc.MediaType == ocispecManifest {
		body, err := r.check(ctx, content)
		if err != nil {
			return err
		}
		content = bytes.NewReader(body)
	}
	return r.Repository.Push(ctx, desc, content)
}

func (r checkedRepository) PushReference(ctx context.Context, desc ocispec.Descriptor, content io.Reader, ref string) error {
	body, err := r.check(ctx, content)
	if err != nil {
		return err
	}
	return r.Repository.PushReference(ctx, desc, bytes.NewReader(body), ref)
}

// TestPassThroughNamesLaggingBlob: on a registry that checks the blobs of a manifest, over a
// providertest.Lag that does not serve a layer pushed through it within the visibility timeout,
// the manifest is put again until the timeout, and the answer then names that layer, not the
// config the registry serves (#850).
func TestPassThroughNamesLaggingBlob(t *testing.T) {
	var checked *blobsChecked
	e := newEnv(t, envOptions{registry: func(f *providertest.Fake) provider.Provider {
		checked = &blobsChecked{Provider: &providertest.Lag{Fake: f, Blobs: -1}}
		return checked
	}})
	h := New(Options{Index: e.st, Providers: provider.Map{"local": e.reg}, Ingest: e.ingest, Tokens: e.tokens,
		Log: slog.New(slog.NewTextHandler(e.logs, nil)), Now: e.clock.Now, VisibilityTimeout: 300 * time.Millisecond})
	const name = "oci/images/app"
	config := ocispec.DescriptorEmptyJSON
	e.pushBlob(name, config.Data) // into the registry's store: served at once
	layer := []byte("a layer the registry does not serve yet")
	desc := ocispec.Descriptor{MediaType: octetStream, Digest: digest.FromBytes(layer), Size: int64(len(layer))}
	rr, err := e.reg.Repository(t.Context(), name)
	must(t, err)
	must(t, rr.Push(t.Context(), desc, bytes.NewReader(layer))) // through the Lag: not served
	m := ocispec.Manifest{Versioned: specs.Versioned{SchemaVersion: 2}, MediaType: ocispecManifest,
		ArtifactType: "application/vnd.example.test", Config: config, Layers: []ocispec.Descriptor{desc}}
	body, err := json.Marshal(m)
	must(t, err)

	rec := e.do(h, http.MethodPut, "/v2/"+name+"/manifests/v1", bytes.NewReader(body), "Content-Type", ocispecManifest)
	entry := wantEntry(t, "PUT", rec, http.StatusBadRequest, codeManifestBlobUnknown)
	if want := refusedBlobUnknown + "; blob " + desc.Digest.String() + " was not uploaded to " + name; entry.Message != want {
		t.Errorf("message %q, want %q", entry.Message, want)
	}
	if detail, ok := entry.Detail.(map[string]any); !ok || detail["digest"] != desc.Digest.String() {
		t.Errorf("detail %#v, want digest %s", entry.Detail, desc.Digest)
	}
	if n := checked.puts.Load(); n < 2 {
		t.Errorf("%d manifest PUTs to the registry, want more than one: a lagging registry's refusal is put again", n)
	}
}
