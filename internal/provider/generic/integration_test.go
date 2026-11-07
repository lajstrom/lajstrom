//go:build integration

package generic

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/opencontainers/go-digest"
	"github.com/opencontainers/image-spec/specs-go"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/lajstrom/lajstrom/internal/config"
	"github.com/lajstrom/lajstrom/internal/provider"
)

// TestIntegration runs the provider against the registry at LAJSTROM_TEST_REGISTRY
// (host:port; plain HTTP unless LAJSTROM_TEST_REGISTRY_TLS=1), with the credentials of
// LAJSTROM_TEST_REGISTRY_USERNAME and _PASSWORD and the instance prefix of
// LAJSTROM_TEST_REGISTRY_PREFIX when they are set. Every run works in repositories with a
// random name, so runs do not collide; it deletes only what the backend can delete.
func TestIntegration(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	p, cfg := integrationProvider(ctx, t)
	caps, err := p.Capabilities(ctx)
	if err != nil {
		t.Fatalf("Capabilities: %v", err)
	}
	t.Logf("capabilities of %s: %+v", cfg.URL, caps)

	run := randomHex(t)
	base := "raw/it-" + run
	name := base + "/pkg"
	repo, err := p.Repository(ctx, name)
	if err != nil {
		t.Fatal(err)
	}

	blob := []byte("hello lajstrom " + run)
	blobDesc := descriptor("application/octet-stream", blob)
	step(t, "push blob", func(t *testing.T) {
		if err := repo.Push(ctx, blobDesc, bytes.NewReader(blob)); err != nil {
			t.Fatal(err)
		}
		if ok, err := repo.Exists(ctx, blobDesc); err != nil || !ok {
			t.Fatalf("Exists(blob) = %v, %v", ok, err)
		}
	})

	var manifest []byte
	var manifestDesc ocispec.Descriptor
	step(t, "push manifest and tag", func(t *testing.T) {
		manifest, manifestDesc = pushTestManifest(ctx, t, repo, "v1", nil, blobDesc)
	})
	step(t, "resolve", func(t *testing.T) {
		d, err := repo.Resolve(ctx, "v1")
		if err != nil || d.Digest != manifestDesc.Digest {
			t.Fatalf("Resolve(v1) = %s, %v; want %s", d.Digest, err, manifestDesc.Digest)
		}
		if _, err := repo.Resolve(ctx, "missing"); !errors.Is(err, provider.ErrNotFound) {
			t.Errorf("Resolve(missing) error = %v, want ErrNotFound", err)
		}
	})
	step(t, "fetch", func(t *testing.T) {
		for _, c := range []struct {
			desc ocispec.Descriptor
			want []byte
		}{{manifestDesc, manifest}, {blobDesc, blob}} {
			if got := fetchAll(ctx, t, repo, c.desc); !bytes.Equal(got, c.want) {
				t.Errorf("Fetch(%s) = %q, want %q", c.desc.Digest, got, c.want)
			}
		}
		if ok, err := repo.Exists(ctx, manifestDesc); err != nil || !ok {
			t.Errorf("Exists(manifest) = %v, %v", ok, err)
		}
	})
	step(t, "tags", func(t *testing.T) {
		var tags []string
		if err := repo.Tags(ctx, "", func(page []string) error {
			tags = append(tags, page...)
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		if !slices.Contains(tags, "v1") {
			t.Errorf("tags = %q, want v1 among them", tags)
		}
	})

	var referrerDesc ocispec.Descriptor
	step(t, "referrers", func(t *testing.T) {
		_, referrerDesc = pushTestManifest(ctx, t, repo, "", &manifestDesc, blobDesc)
		var got []ocispec.Descriptor
		if err := repo.Referrers(ctx, manifestDesc, "", func(page []ocispec.Descriptor) error {
			got = append(got, page...)
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		if !slices.ContainsFunc(got, func(d ocispec.Descriptor) bool { return d.Digest == referrerDesc.Digest }) {
			t.Errorf("referrers = %v, want %s among them", got, referrerDesc.Digest)
		}
		t.Logf("referrers listed through the %s", map[bool]string{true: "referrers API", false: "sha256-<digest> tag schema"}[caps.Referrers])
	})

	step(t, "blob redirect", func(t *testing.T) {
		u, ok, err := p.BlobRedirectURL(ctx, name, blobDesc.Digest)
		if err != nil {
			t.Fatal(err)
		}
		if !ok {
			t.Logf("BlobRedirectURL: no redirect, the blob is streamed through Fetch")
			return
		}
		t.Logf("BlobRedirectURL: redirect to %s://%s", schemeOf(u), hostOf(u))
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
		if err != nil {
			t.Fatal(err)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		got, err := io.ReadAll(resp.Body)
		if err != nil || resp.StatusCode != http.StatusOK || !bytes.Equal(got, blob) {
			t.Errorf("GET of the redirect URL = %d %q, %v; want the blob", resp.StatusCode, got, err)
		}
	})

	step(t, "list repositories", func(t *testing.T) {
		if !caps.Catalog {
			t.Skip("the registry has no catalog (Capabilities.Catalog false)")
		}
		other := base + "/other"
		r, err := p.Repository(ctx, other)
		if err != nil {
			t.Fatal(err)
		}
		if err := r.Push(ctx, blobDesc, bytes.NewReader(blob)); err != nil {
			t.Fatal(err)
		}
		pushTestManifest(ctx, t, r, "v1", nil, blobDesc)
		got, err := p.ListRepositories(ctx, base+"/")
		if err != nil {
			t.Fatal(err)
		}
		if want := []string{other, name}; !slices.Equal(got, want) { // sorted: other < pkg
			t.Errorf("ListRepositories(%q) = %q, want %q", base+"/", got, want)
		}
	})

	step(t, "delete", func(t *testing.T) {
		if !caps.DeleteManifest {
			t.Skip("the registry does not delete manifests (Capabilities.DeleteManifest false)")
		}
		for _, d := range []ocispec.Descriptor{referrerDesc, manifestDesc} {
			err := repo.Delete(ctx, d)
			if errors.Is(err, provider.ErrUnsupported) {
				// The generic provider assumes deletes work until the registry refuses one.
				if c, cerr := p.Capabilities(ctx); cerr != nil || c.DeleteManifest {
					t.Fatalf("Capabilities after a refused delete = %+v, %v; want DeleteManifest false", c, cerr)
				}
				t.Skipf("the registry refused the delete and DeleteManifest is now false: %v", err)
			}
			if err != nil {
				t.Fatalf("Delete(%s): %v", d.Digest, err)
			}
		}
		if ok, err := repo.Exists(ctx, manifestDesc); err != nil || ok {
			t.Errorf("Exists after Delete = %v, %v; want false", ok, err)
		}
		if _, err := repo.Resolve(ctx, manifestDesc.Digest.String()); !errors.Is(err, provider.ErrNotFound) {
			t.Errorf("Resolve after Delete error = %v, want ErrNotFound", err)
		}
		if c, err := p.Capabilities(ctx); err != nil || !c.DeleteManifest {
			t.Errorf("Capabilities after a successful delete = %+v, %v", c, err)
		}
	})
}

// integrationProvider returns a Provider for the registry of LAJSTROM_TEST_REGISTRY, and its
// configuration; it skips the test when the variable is not set.
func integrationProvider(_ context.Context, t *testing.T) (*Provider, config.Registry) {
	t.Helper()
	addr := os.Getenv("LAJSTROM_TEST_REGISTRY")
	if addr == "" {
		t.Skip("set LAJSTROM_TEST_REGISTRY=host:port to run against a registry")
	}
	scheme := "http"
	if os.Getenv("LAJSTROM_TEST_REGISTRY_TLS") == "1" {
		scheme = "https"
	}
	cfg := config.Registry{
		Name:   "integration",
		Type:   Type,
		URL:    scheme + "://" + addr,
		Prefix: os.Getenv("LAJSTROM_TEST_REGISTRY_PREFIX"),
		Auth: config.RegistryAuth{
			Username: os.Getenv("LAJSTROM_TEST_REGISTRY_USERNAME"),
			Password: os.Getenv("LAJSTROM_TEST_REGISTRY_PASSWORD"),
		},
	}
	log := slog.New(slog.NewTextHandler(testWriter{t}, &slog.HandlerOptions{Level: slog.LevelInfo}))
	p, err := New(cfg, log)
	if err != nil {
		t.Fatal(err)
	}
	return p, cfg
}

// step runs fn as a subtest and stops the test when it fails: later steps build on earlier
// ones.
func step(t *testing.T, name string, fn func(t *testing.T)) {
	t.Helper()
	if !t.Run(name, fn) {
		t.FailNow()
	}
}

// descriptor describes content of mediaType.
func descriptor(mediaType string, content []byte) ocispec.Descriptor {
	return ocispec.Descriptor{MediaType: mediaType, Digest: digest.FromBytes(content), Size: int64(len(content))}
}

// pushTestManifest pushes the empty config and an OCI image manifest with layer, tagged tag
// (PushReference) or untagged (Push), with subject when it is not nil.
func pushTestManifest(ctx context.Context, t *testing.T, repo provider.Repository, tag string, subject *ocispec.Descriptor, layer ocispec.Descriptor) ([]byte, ocispec.Descriptor) {
	t.Helper()
	empty := ocispec.DescriptorEmptyJSON
	if err := repo.Push(ctx, empty, bytes.NewReader(empty.Data)); err != nil {
		t.Fatalf("push config: %v", err)
	}
	artifactType := "application/vnd.lajstrom.test.v1"
	if subject != nil {
		artifactType = "application/vnd.lajstrom.test.signature.v1"
	}
	m := ocispec.Manifest{
		Versioned:    specs.Versioned{SchemaVersion: 2},
		MediaType:    ocispec.MediaTypeImageManifest,
		ArtifactType: artifactType,
		Config:       ocispec.Descriptor{MediaType: empty.MediaType, Digest: empty.Digest, Size: empty.Size},
		Layers:       []ocispec.Descriptor{layer},
		Subject:      subject,
	}
	content, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	desc := descriptor(m.MediaType, content)
	desc.ArtifactType = artifactType
	if tag != "" {
		err = repo.PushReference(ctx, desc, bytes.NewReader(content), tag)
	} else {
		err = repo.Push(ctx, desc, bytes.NewReader(content))
	}
	if err != nil {
		t.Fatalf("push manifest: %v", err)
	}
	return content, desc
}

// fetchAll reads the content desc describes.
func fetchAll(ctx context.Context, t *testing.T, repo provider.Repository, desc ocispec.Descriptor) []byte {
	t.Helper()
	rc, err := repo.Fetch(ctx, desc)
	if err != nil {
		t.Fatalf("Fetch(%s): %v", desc.Digest, err)
	}
	defer func() { _ = rc.Close() }()
	b, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("read %s: %v", desc.Digest, err)
	}
	return b
}

// randomHex returns 8 random hex digits for repository names.
func randomHex(t *testing.T) string {
	t.Helper()
	b := make([]byte, 4)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(b)
}

// schemeOf and hostOf print a redirect URL without its query, which may carry a signature.
func schemeOf(u string) string {
	s, _, _ := strings.Cut(u, "://")
	return s
}

func hostOf(u string) string {
	_, rest, _ := strings.Cut(u, "://")
	host, _, _ := strings.Cut(rest, "/")
	return host
}

// testWriter sends log lines to t.Log.
type testWriter struct{ t *testing.T }

func (w testWriter) Write(p []byte) (int, error) {
	w.t.Log(strings.TrimRight(string(p), "\n"))
	return len(p), nil
}
