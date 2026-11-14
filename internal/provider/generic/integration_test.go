//go:build integration

package generic

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
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
	step(t, "stat manifest", func(t *testing.T) {
		if d, err := p.StatManifest(ctx, name, manifestDesc.Digest); err != nil || d.Digest != manifestDesc.Digest {
			t.Errorf("StatManifest = %+v, %v; want %s", d, err, manifestDesc.Digest)
		}
		// What prune counts as unknown (guarantees.md, INV-5): MANIFEST_UNKNOWN in a repository the
		// registry has, and not the NAME_UNKNOWN of one it does not.
		if _, err := p.StatManifest(ctx, name, digest.FromString("never pushed "+run)); !errors.Is(err, provider.ErrManifestUnknown) {
			t.Errorf("StatManifest of a manifest never pushed: %v, want ErrManifestUnknown", err)
		}
		_, err := p.StatManifest(ctx, base+"/no-such-repository", manifestDesc.Digest)
		if !errors.Is(err, provider.ErrNotFound) {
			t.Errorf("StatManifest in a repository never pushed to: %v, want ErrNotFound", err)
		}
		t.Logf("StatManifest in a repository never pushed to: %v", err)
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

	step(t, "open blob", func(t *testing.T) {
		b, err := p.OpenBlob(ctx, name, blobDesc.Digest, "", true)
		if err != nil {
			t.Fatal(err)
		}
		if b.RedirectURL == "" {
			t.Logf("OpenBlob: no redirect; the first answer streams (%d, Accept-Ranges %v)", b.Status, b.AcceptRanges)
			wantBlobBody(t, b, http.StatusOK, blob)
			return
		}
		t.Logf("OpenBlob: a redirect to %s://%s%s", schemeOf(b.RedirectURL), hostOf(b.RedirectURL), pathOf(b.RedirectURL))
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, b.RedirectURL, nil)
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
		// redirect false: the provider follows the redirect and streams.
		b, err = p.OpenBlob(ctx, name, blobDesc.Digest, "", false)
		if err != nil {
			t.Fatal(err)
		}
		wantBlobBody(t, b, http.StatusOK, blob)
	})
	step(t, "open blob with a range", func(t *testing.T) {
		b, err := p.OpenBlob(ctx, name, blobDesc.Digest, "bytes=6-13", false)
		if err != nil {
			t.Fatal(err)
		}
		if p.quirk().wholeBlobs {
			// A vendor that fails ranges (Nexus's oci format, the vendor table) is asked for the
			// whole blob on purpose: a range is answered 200 with every byte, past the end too,
			// and nothing says Accept-Ranges (#352, #800).
			if b.AcceptRanges {
				t.Error("Accept-Ranges on a registry whose blobs are read whole")
			}
			wantBlobBody(t, b, http.StatusOK, blob)
			b, err = p.OpenBlob(ctx, name, blobDesc.Digest, fmt.Sprintf("bytes=%d-", len(blob)), false)
			if err != nil {
				t.Fatal(err)
			}
			wantBlobBody(t, b, http.StatusOK, blob)
			return
		}
		wantBlobBody(t, b, http.StatusPartialContent, blob[6:14])
		if want := fmt.Sprintf("bytes 6-13/%d", len(blob)); b.ContentRange != want {
			t.Errorf("Content-Range %q, want %q", b.ContentRange, want)
		}
		b, err = p.OpenBlob(ctx, name, blobDesc.Digest, fmt.Sprintf("bytes=%d-", len(blob)), false)
		if err != nil || b.Status != http.StatusRequestedRangeNotSatisfiable || b.Body != nil {
			t.Errorf("a range past the end = %+v, %v; want 416", b, err)
		}
	})
	step(t, "stat blob", func(t *testing.T) {
		b, err := p.StatBlob(ctx, name, blobDesc.Digest)
		if err != nil || b.Status != http.StatusOK || b.Size != int64(len(blob)) || b.Digest != blobDesc.Digest || b.Body != nil {
			t.Errorf("StatBlob = %+v, %v", b, err)
		}
		if _, err := p.StatBlob(ctx, name, digest.FromString("missing "+run)); !errors.Is(err, provider.ErrNotFound) {
			t.Errorf("StatBlob of a missing blob: %v, want ErrNotFound", err)
		}
		if _, err := p.OpenBlob(ctx, name, digest.FromString("missing "+run), "", true); !errors.Is(err, provider.ErrNotFound) {
			t.Errorf("OpenBlob of a missing blob: %v, want ErrNotFound", err)
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

	step(t, "delete a manifest that an index lists", func(t *testing.T) {
		if !caps.DeleteManifest {
			t.Skip("the registry does not delete manifests (Capabilities.DeleteManifest false)")
		}
		r, err := p.Repository(ctx, base+"/indexed")
		if err != nil {
			t.Fatal(err)
		}
		if err := r.Push(ctx, blobDesc, bytes.NewReader(blob)); err != nil {
			t.Fatal(err)
		}
		_, listed := pushTestManifest(ctx, t, r, "v1", nil, blobDesc)
		index := pushTestIndex(ctx, t, r, "list", listed)
		// Zot refuses the delete with 405 DENIED (docs/registries/zot.md, Delete), distribution
		// deletes the manifest; neither turns DeleteManifest false (#153).
		err = r.Delete(ctx, listed)
		refused := errors.Is(err, provider.ErrConflict)
		switch {
		case refused:
			t.Logf("the registry keeps a manifest that an index lists: %v", err)
			if ok, err := r.Exists(ctx, listed); err != nil || !ok {
				t.Errorf("Exists after a refused Delete = %v, %v; want true", ok, err)
			}
		case err != nil:
			t.Fatalf("Delete of the listed manifest: %v, want ErrConflict or success", err)
		default:
			t.Logf("the registry deletes a manifest that an index lists")
		}
		if c, err := p.Capabilities(ctx); err != nil || !c.DeleteManifest {
			t.Fatalf("Capabilities after the delete = %+v, %v; want DeleteManifest", c, err)
		}
		if err := r.Delete(ctx, index); err != nil {
			t.Fatalf("Delete(index): %v", err)
		}
		if refused {
			if err := r.Delete(ctx, listed); err != nil {
				t.Fatalf("Delete of the manifest after its index: %v", err)
			}
		}
		for _, d := range []ocispec.Descriptor{index, listed} {
			if ok, err := r.Exists(ctx, d); err != nil || ok {
				t.Errorf("Exists(%s) after Delete = %v, %v; want false", d.MediaType, ok, err)
			}
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
		if _, err := p.StatManifest(ctx, name, manifestDesc.Digest); !errors.Is(err, provider.ErrManifestUnknown) {
			t.Errorf("StatManifest after Delete: %v, want ErrManifestUnknown", err)
		}
		if c, err := p.Capabilities(ctx); err != nil || !c.DeleteManifest {
			t.Errorf("Capabilities after a successful delete = %+v, %v", c, err)
		}
	})
}

// TestIntegrationNameLength: Capabilities says 255 bytes less the instance prefix, and the
// registry takes a name of exactly that many below the prefix, 255 bytes with it: a manifest
// pushed there resolves (#661). A manifest is deleted where the registry deletes them.
func TestIntegrationNameLength(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	p, cfg := integrationProvider(ctx, t)
	caps, err := p.Capabilities(ctx)
	if err != nil {
		t.Fatalf("Capabilities: %v", err)
	}
	if want := maxNameLen - len(normalisePrefix(cfg.Prefix)); caps.MaxNameLength != want {
		t.Fatalf("MaxNameLength = %d with the prefix %q, want %d", caps.MaxNameLength, cfg.Prefix, want)
	}
	name := "raw/it-" + randomHex(t)
	for len(name) < caps.MaxNameLength {
		if k := caps.MaxNameLength - len(name) - 1; k > 0 {
			name += "/" + strings.Repeat("n", min(60, k))
		} else {
			name += "n"
		}
	}
	if len(p.repoName(name)) != maxNameLen {
		t.Fatalf("the name %q is %d bytes, %d with the prefix", name, len(name), len(p.repoName(name)))
	}
	repo, err := p.Repository(ctx, name)
	if err != nil {
		t.Fatal(err)
	}
	blob := fmt.Appendf(nil, "a name of %d bytes", len(name))
	blobDesc := descriptor("application/octet-stream", blob)
	if err := repo.Push(ctx, blobDesc, bytes.NewReader(blob)); err != nil {
		t.Fatalf("push a blob to a name of %d bytes, %d with the prefix: %v", len(name), maxNameLen, err)
	}
	_, m := pushTestManifest(ctx, t, repo, "v1", nil, blobDesc)
	if d, err := repo.Resolve(ctx, "v1"); err != nil || d.Digest != m.Digest {
		t.Errorf("Resolve(v1) = %s, %v; want %s", d.Digest, err, m.Digest)
	}
	if caps.DeleteManifest {
		if err := repo.Delete(ctx, m); err != nil && !errors.Is(err, provider.ErrUnsupported) {
			t.Errorf("Delete: %v", err)
		}
	}
}

// TestIntegrationBuildRoutes pushes a blob to new repositories whose names end in a build route
// (provider.BuildRoute) and to one that does not, through the provider alone, without CheckNew:
// what the registry answers (#662). A registry that refuses them, as Quay's master does, answers
// 400 NAME_INVALID before any byte, and its capabilities must say buildRoutes; one that takes
// them, every other registry and Quay 3.18, is logged, and a vendor table that says buildRoutes
// there (Quay) is the cautious default, which registries[].capabilities.buildRoutes: false lifts.
// The control takes the blob everywhere.
func TestIntegrationBuildRoutes(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	p, cfg := integrationProvider(ctx, t)
	caps, err := p.Capabilities(ctx)
	if err != nil {
		t.Fatalf("Capabilities: %v", err)
	}
	base := "raw/it-" + randomHex(t)
	push := func(name string) error {
		repo, err := p.Repository(ctx, name)
		if err != nil {
			t.Fatal(err)
		}
		blob := []byte("a blob in " + name)
		return repo.Push(ctx, descriptor("application/octet-stream", blob), bytes.NewReader(blob))
	}
	if err := push(base + "/build/linux/app.tar"); err != nil {
		t.Fatalf("the control, no build route: %v", err)
	}
	for _, name := range []string{base + "/build", base + "/build/app.tar", base + "/build/x/logs"} {
		if !provider.BuildRoute(name) {
			t.Fatalf("BuildRoute(%q) = false", name)
		}
		if caps.BuildRoutes && caps.CheckNew(name) == nil {
			t.Errorf("CheckNew(%q) = nil with buildRoutes", name)
		}
		err := push(name)
		var rej *provider.RejectedError
		switch {
		case err == nil:
			t.Logf("%s: the registry creates it (capabilities.buildRoutes %v)", name, caps.BuildRoutes)
		case errors.As(err, &rej) && rej.Status == http.StatusBadRequest && rej.Code == "NAME_INVALID":
			t.Logf("%s: the registry refuses to create it: %v", name, err)
			if !caps.BuildRoutes {
				t.Errorf("%s of %s: the registry creates no such name, and capabilities.buildRoutes is off", name, cfg.URL)
			}
		default:
			t.Errorf("%s: %v (%T), want nil or a 400 NAME_INVALID", name, err, err)
		}
	}
}

// integrationProvider returns a Provider for the registry of LAJSTROM_TEST_REGISTRY, and its
// configuration; it skips the test when the variable is not set.
func integrationProvider(_ context.Context, t *testing.T, edit ...func(*config.Registry)) (*Provider, config.Registry) {
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
		// Gitea and JFrog need it (#425).
		Uploads: config.RegistryUploads{SpoolUnknownLength: os.Getenv("LAJSTROM_TEST_REGISTRY_SPOOL_UNKNOWN_LENGTH") == "1"},
	}
	for _, e := range edit {
		e(&cfg)
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

// pushTestIndex pushes an OCI image index of manifests, tagged tag.
func pushTestIndex(ctx context.Context, t *testing.T, repo provider.Repository, tag string, manifests ...ocispec.Descriptor) ocispec.Descriptor {
	t.Helper()
	content, err := json.Marshal(ocispec.Index{
		Versioned: specs.Versioned{SchemaVersion: 2},
		MediaType: ocispec.MediaTypeImageIndex,
		Manifests: manifests,
	})
	if err != nil {
		t.Fatal(err)
	}
	desc := descriptor(ocispec.MediaTypeImageIndex, content)
	if err := repo.PushReference(ctx, desc, bytes.NewReader(content), tag); err != nil {
		t.Fatalf("push index: %v", err)
	}
	return desc
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

// wantBlobBody reads and closes the body of b and fails t unless the answer is status with
// want, its size and its digest.
func wantBlobBody(t *testing.T, b provider.Blob, status int, want []byte) {
	t.Helper()
	if b.Body == nil {
		t.Fatalf("OpenBlob = %+v, want a body", b)
	}
	got, err := io.ReadAll(b.Body)
	_ = b.Body.Close()
	if err != nil || b.Status != status || !bytes.Equal(got, want) || b.Size != int64(len(want)) || b.RedirectURL != "" {
		t.Errorf("OpenBlob = %d, %d bytes (%+v), %v; want %d with %q", b.Status, len(got), b, err, status, want)
	}
}

// schemeOf, hostOf and pathOf print a redirect URL without its query, which may carry a
// signature.
func schemeOf(u string) string {
	s, _, _ := strings.Cut(u, "://")
	return s
}

func hostOf(u string) string {
	_, rest, _ := strings.Cut(u, "://")
	host, _, _ := strings.Cut(rest, "/")
	return host
}

func pathOf(u string) string {
	_, rest, _ := strings.Cut(u, "://")
	_, p, _ := strings.Cut(rest, "/")
	p, _, _ = strings.Cut(p, "?")
	return "/" + p
}

// testWriter sends log lines to t.Log.
type testWriter struct{ t *testing.T }

func (w testWriter) Write(p []byte) (int, error) {
	w.t.Log(strings.TrimRight(string(p), "\n"))
	return len(p), nil
}
