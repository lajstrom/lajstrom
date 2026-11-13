//go:build integration

package proxy_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"io"
	"log/slog"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/lajstrom/lajstrom/internal/config"
	"github.com/lajstrom/lajstrom/internal/provider"
	"github.com/lajstrom/lajstrom/internal/proxy"
)

// TestIntegrationOCIUpstreamMoto pulls through an oci upstream with auth.ecr from moto, the AWS
// mock, for the ECR API at LAJSTROM_TEST_ECR_ENDPOINT, and a distribution that takes only moto's
// token at LAJSTROM_TEST_ECR_REGISTRY (make dev-up PROFILES=ecr). It skips without both. A
// registry of type ecr seeds an image below a prefix of the run's; the oci proxy repo then
// fetches its manifest and its blob with the token that the SDK got from moto.
func TestIntegrationOCIUpstreamMoto(t *testing.T) {
	endpoint, registry := os.Getenv("LAJSTROM_TEST_ECR_ENDPOINT"), os.Getenv("LAJSTROM_TEST_ECR_REGISTRY")
	if endpoint == "" || registry == "" {
		t.Skip("set LAJSTROM_TEST_ECR_ENDPOINT and LAJSTROM_TEST_ECR_REGISTRY (make dev-up PROFILES=ecr)")
	}
	if os.Getenv("AWS_ACCESS_KEY_ID") == "" {
		t.Setenv("AWS_ACCESS_KEY_ID", "testing")
		t.Setenv("AWS_SECRET_ACCESS_KEY", "testing")
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	path := "it-" + hex.EncodeToString(b) + "/nginx"
	ecrCfg := config.ECR{Region: "us-east-1", Endpoint: endpoint}

	seed, err := provider.New(config.Registry{Name: "moto", Type: "ecr", URL: "http://" + registry, ECR: ecrCfg}, slog.Default())
	if err != nil {
		t.Fatal(err)
	}
	if err := seed.EnsureRepository(ctx, path); err != nil {
		t.Fatal(err)
	}
	repo, err := seed.Repository(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	layer := []byte("hello moto, through an oci upstream")
	ld := ocispec.Descriptor{MediaType: ocispec.MediaTypeImageLayer, Digest: digest.FromBytes(layer), Size: int64(len(layer))}
	empty := []byte("{}")
	cd := ocispec.Descriptor{MediaType: ocispec.MediaTypeEmptyJSON, Digest: digest.FromBytes(empty), Size: 2}
	for _, blob := range []struct {
		desc ocispec.Descriptor
		b    []byte
	}{{ld, layer}, {cd, empty}} {
		if err := repo.Push(ctx, blob.desc, bytes.NewReader(blob.b)); err != nil {
			t.Fatalf("push blob: %v", err)
		}
	}
	manifest := []byte(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json",` +
		`"config":{"mediaType":"application/vnd.oci.empty.v1+json","digest":"` + cd.Digest.String() + `","size":2},` +
		`"layers":[{"mediaType":"application/vnd.oci.image.layer.v1.tar","digest":"` + ld.Digest.String() + `","size":` +
		strconv.Itoa(len(layer)) + `}]}`)
	md := ocispec.Descriptor{MediaType: ocispec.MediaTypeImageManifest, Digest: digest.FromBytes(manifest), Size: int64(len(manifest))}
	if err := repo.PushReference(ctx, md, bytes.NewReader(manifest), "v1"); err != nil {
		t.Fatalf("push manifest: %v", err)
	}

	e := newOCIEnv(t, ociSettings)
	ups, err := proxy.LoadOCIUpstreams([]config.Upstream{{Name: "hub", Format: config.FormatOCI, URL: "http://" + registry,
		Auth: config.UpstreamAuth{ECR: &config.UpstreamECR{Region: ecrCfg.Region, Endpoint: endpoint}}}}, slog.Default())
	if err != nil {
		t.Fatal(err)
	}
	e.up = ups["hub"]
	c := e.core("a", proxy.Options{})
	desc, _, err := c.OCIManifest(ctx, e.repo, path, "v1")
	if err != nil || desc.Digest != md.Digest {
		t.Fatalf("OCIManifest = %s, %v; want %s", desc.Digest, err, md.Digest)
	}
	if _, _, err := c.OCIBlob(ctx, e.repo, path, ld.Digest, true, false); err != nil {
		t.Fatalf("OCIBlob: %v", err)
	}
	blob, err := e.reg.OpenBlob(ctx, "oci/hub/"+path, ld.Digest, "", false)
	if err != nil {
		t.Fatalf("the backend's blob: %v", err)
	}
	defer func() { _ = blob.Body.Close() }()
	if got, err := io.ReadAll(blob.Body); err != nil || !bytes.Equal(got, layer) {
		t.Errorf("the backend holds %q, %v; want the layer", got, err)
	}
}
