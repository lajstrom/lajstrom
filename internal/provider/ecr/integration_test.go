//go:build integration

package ecr

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io"
	"log/slog"
	"os"
	"slices"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ecr"
	"github.com/aws/aws-sdk-go-v2/service/ecr/types"
	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/lajstrom/lajstrom/internal/config"
	"github.com/lajstrom/lajstrom/internal/provider"
)

// TestIntegrationMoto runs the provider against moto for the ECR API, at
// LAJSTROM_TEST_ECR_ENDPOINT (http://localhost:5010 after make dev-up PROFILES=ecr), and a
// distribution for /v2 that accepts only moto's token, at LAJSTROM_TEST_ECR_REGISTRY
// (localhost:5011). It skips without both. moto takes any AWS credentials; the test sets
// fake ones unless the environment has some. Each run works below an instance prefix of its
// own.
func TestIntegrationMoto(t *testing.T) {
	endpoint, registry := os.Getenv("LAJSTROM_TEST_ECR_ENDPOINT"), os.Getenv("LAJSTROM_TEST_ECR_REGISTRY")
	if endpoint == "" || registry == "" {
		t.Skip("set LAJSTROM_TEST_ECR_ENDPOINT and LAJSTROM_TEST_ECR_REGISTRY (make dev-up PROFILES=ecr)")
	}
	if os.Getenv("AWS_ACCESS_KEY_ID") == "" {
		t.Setenv("AWS_ACCESS_KEY_ID", "testing")
		t.Setenv("AWS_SECRET_ACCESS_KEY", "testing")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	prefix := "it-" + hex.EncodeToString(b) + "/"
	cfg := config.Registry{Name: "moto", Type: Type, URL: "http://" + registry, Prefix: prefix, ECR: config.ECR{
		Region: "us-east-1", Endpoint: endpoint, ImageTagMutability: "IMMUTABLE_WITH_EXCLUSION",
		ImageTagMutabilityExclusions: []string{"sha256-*"}, Encryption: "KMS", KMSKey: "alias/lajstrom-it",
	}}
	np, err := provider.New(cfg, slog.Default())
	if err != nil {
		t.Fatal(err)
	}
	p := np.(*Provider)
	client := p.api.(*ecr.Client)
	name := "raw/scratch/notes"

	// The token of moto reaches /v2 as Basic credentials: the probe pings with it.
	caps, err := p.Capabilities(ctx)
	if err != nil {
		t.Fatalf("Capabilities: %v", err)
	}
	if caps.CreateOnPush || !caps.Catalog || !caps.ImmutableTags {
		t.Errorf("Capabilities = %+v", caps)
	}

	// Create on push: the repository is created with the configured settings, once.
	for range 2 {
		if err := p.EnsureRepository(ctx, name); err != nil {
			t.Fatalf("EnsureRepository: %v", err)
		}
	}
	got, err := client.DescribeRepositories(ctx, &ecr.DescribeRepositoriesInput{RepositoryNames: []string{prefix + name}})
	if err != nil || len(got.Repositories) != 1 {
		t.Fatalf("DescribeRepositories = %v, %v", got, err)
	}
	r := got.Repositories[0]
	if r.ImageTagMutability != types.ImageTagMutabilityImmutableWithExclusion || len(r.ImageTagMutabilityExclusionFilters) != 1 ||
		r.EncryptionConfiguration.EncryptionType != types.EncryptionTypeKms || aws.ToString(r.EncryptionConfiguration.KmsKey) != "alias/lajstrom-it" {
		t.Errorf("repository created as %+v, %+v", r.ImageTagMutability, r.EncryptionConfiguration)
	}
	// Created beforehand, outside Lajstrom: EnsureRepository leaves it as it is.
	if _, err := client.CreateRepository(ctx, &ecr.CreateRepositoryInput{RepositoryName: aws.String(prefix + "raw/scratch/terraform")}); err != nil {
		t.Fatal(err)
	}
	if err := p.EnsureRepository(ctx, "raw/scratch/terraform"); err != nil {
		t.Errorf("EnsureRepository of an existing repository: %v", err)
	}

	pushAndFetch(ctx, t, p, name)

	names, err := p.ListRepositories(ctx, "raw/")
	if want := []string{"raw/scratch/notes", "raw/scratch/terraform"}; err != nil || !slices.Equal(names, want) {
		t.Errorf("ListRepositories = %q, %v; want %q", names, err, want)
	}

	// moto keeps no images of /v2: PutImage makes the repository hold one.
	manifest := `{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json","config":{"mediaType":"application/vnd.oci.empty.v1+json","digest":"sha256:44136fa355b3678a1146ad16f7e8649e94fb4fc21fe77e8310c060f61caaff8a","size":2},"layers":[]}`
	if _, err := client.PutImage(ctx, &ecr.PutImageInput{RepositoryName: aws.String(prefix + name), ImageManifest: aws.String(manifest), ImageTag: aws.String("v1")}); err != nil {
		t.Fatal(err)
	}
	if err := p.DeleteRepository(ctx, name); !errors.Is(err, provider.ErrConflict) {
		t.Errorf("DeleteRepository of a repository with an image = %v, want ErrConflict", err)
	}
	cfg.ECR.ForceDelete = true
	force, err := provider.New(cfg, slog.Default())
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{name, "raw/scratch/terraform"} {
		if err := force.DeleteRepository(ctx, n); err != nil {
			t.Errorf("DeleteRepository(%s) with forceDelete: %v", n, err)
		}
	}
	if err := p.DeleteRepository(ctx, name); !errors.Is(err, provider.ErrNotFound) {
		t.Errorf("DeleteRepository of a deleted repository = %v, want ErrNotFound", err)
	}
}

// TestIntegrationECRLive is TestIntegrationMoto against a real ECR registry, the live run of
// #19: LAJSTROM_TEST_ECR_LIVE is its host, <account>.dkr.ecr.<region>.amazonaws.com, and the
// credentials are the AWS SDK's (docs/registries/ecr.md, "Measure a real ECR"). Without it the
// test skips, as it does in CI. Nothing is moto's: no endpoint, https, the region of the host,
// the default encryption (AES256), and IMMUTABLE_WITH_EXCLUSION for the sha256-* tags of the tag
// schema (ADR 0018). Each run works below an instance prefix of its own, lajstrom/it-<random>/,
// and deletes the repositories it created there, with their images, also when it fails.
func TestIntegrationECRLive(t *testing.T) {
	host := os.Getenv("LAJSTROM_TEST_ECR_LIVE")
	if host == "" {
		t.Skip("set LAJSTROM_TEST_ECR_LIVE=<account>.dkr.ecr.<region>.amazonaws.com, with AWS credentials, to run against a real ECR")
	}
	if !IsRegistryHost(host) {
		t.Fatalf("LAJSTROM_TEST_ECR_LIVE=%q is not the host of an ECR registry", host)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	prefix := "lajstrom/it-" + hex.EncodeToString(b) + "/" // below lajstrom/, which the IAM policy of ecr.md covers
	cfg := config.Registry{Name: "ecr-live", Type: Type, URL: "https://" + host, Prefix: prefix, ECR: config.ECR{
		ImageTagMutability: "IMMUTABLE_WITH_EXCLUSION", ImageTagMutabilityExclusions: []string{"sha256-*"}, ForceDelete: true,
	}}
	np, err := provider.New(cfg, slog.Default())
	if err != nil {
		t.Fatal(err)
	}
	p := np.(*Provider)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		names, err := p.ListRepositories(ctx, "")
		if err != nil {
			t.Errorf("the repositories below %s, to delete: %v", prefix, err)
		}
		for _, n := range names {
			if err := p.DeleteRepository(ctx, n); err != nil {
				t.Errorf("delete %s%s: %v", prefix, n, err)
			}
		}
	})
	name := "raw/scratch/notes"

	caps, err := p.Capabilities(ctx)
	if err != nil {
		t.Fatalf("Capabilities: %v", err)
	}
	t.Logf("capabilities of %s: %+v", host, caps)
	if caps.CreateOnPush || !caps.Catalog || !caps.ImmutableTags {
		t.Errorf("Capabilities = %+v; want no create on push, a catalog, immutable tags", caps)
	}

	for range 2 {
		if err := p.EnsureRepository(ctx, name); err != nil {
			t.Fatalf("EnsureRepository: %v", err)
		}
	}
	got, err := p.api.DescribeRepositories(ctx, &ecr.DescribeRepositoriesInput{RepositoryNames: []string{prefix + name}})
	if err != nil || len(got.Repositories) != 1 {
		t.Fatalf("DescribeRepositories = %v, %v", got, err)
	}
	r := got.Repositories[0]
	if r.ImageTagMutability != types.ImageTagMutabilityImmutableWithExclusion || len(r.ImageTagMutabilityExclusionFilters) != 1 ||
		r.EncryptionConfiguration.EncryptionType != types.EncryptionTypeAes256 {
		t.Errorf("repository created as %+v, %+v", r.ImageTagMutability, r.EncryptionConfiguration)
	}

	pushAndFetch(ctx, t, p, name)

	if names, err := p.ListRepositories(ctx, "raw/"); err != nil || !slices.Equal(names, []string{name}) {
		t.Errorf("ListRepositories = %q, %v; want %q", names, err, name)
	}
	if err := p.DeleteRepository(ctx, name); err != nil {
		t.Errorf("DeleteRepository with forceDelete of a repository with an image: %v", err)
	}
	if err := p.DeleteRepository(ctx, name); !errors.Is(err, provider.ErrNotFound) {
		t.Errorf("DeleteRepository of a deleted repository = %v, want ErrNotFound", err)
	}
}

// pushAndFetch pushes a blob and a manifest to name through the data path and reads them back.
func pushAndFetch(ctx context.Context, t *testing.T, p *Provider, name string) {
	t.Helper()
	repo, err := p.Repository(ctx, name)
	if err != nil {
		t.Fatal(err)
	}
	blob := []byte("hello moto")
	bd := ocispec.Descriptor{MediaType: "application/octet-stream", Digest: digest.FromBytes(blob), Size: int64(len(blob))}
	if err := repo.Push(ctx, bd, bytes.NewReader(blob)); err != nil {
		t.Fatalf("push blob: %v", err)
	}
	b, err := p.OpenBlob(ctx, name, bd.Digest, "", false)
	if err != nil {
		t.Fatalf("OpenBlob: %v", err)
	}
	defer func() { _ = b.Body.Close() }()
	if got, err := io.ReadAll(b.Body); err != nil || !bytes.Equal(got, blob) {
		t.Errorf("OpenBlob read %q, %v", got, err)
	}
	manifest := []byte(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json",` +
		`"config":{"mediaType":"application/octet-stream","digest":"` + bd.Digest.String() + `","size":10},"layers":[]}`)
	md := ocispec.Descriptor{MediaType: ocispec.MediaTypeImageManifest, Digest: digest.FromBytes(manifest), Size: int64(len(manifest))}
	if err := repo.PushReference(ctx, md, bytes.NewReader(manifest), "v1"); err != nil {
		t.Fatalf("push manifest: %v", err)
	}
	if d, err := repo.Resolve(ctx, "v1"); err != nil || d.Digest != md.Digest {
		t.Errorf("Resolve(v1) = %s, %v; want %s", d.Digest, err, md.Digest)
	}
}
