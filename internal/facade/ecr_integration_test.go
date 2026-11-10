//go:build integration

package facade

import (
	"context"
	"fmt"
	"log/slog"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"testing"

	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"oras.land/oras-go/v2"
	"oras.land/oras-go/v2/content/memory"
	"oras.land/oras-go/v2/registry/remote"

	"github.com/lajstrom/lajstrom/internal/config"
	"github.com/lajstrom/lajstrom/internal/index"
	"github.com/lajstrom/lajstrom/internal/index/sqlite"
	"github.com/lajstrom/lajstrom/internal/keys"
	"github.com/lajstrom/lajstrom/internal/provider"
	"github.com/lajstrom/lajstrom/internal/provider/ecr"
)

// TestIntegrationECRNewPath is #356 on the ecr profile (make dev-up PROFILES=ecr): moto for the
// ECR API at LAJSTROM_TEST_ECR_ENDPOINT, and a distribution behind moto's token at
// LAJSTROM_TEST_ECR_REGISTRY. oras-go pushes a tagged artifact to a new path of an oci repo
// through the facade, as oras push does, and the facade creates the repository in moto before
// the first blob. The distribution creates on push and would take the blobs anyway, so moto's
// list of repositories is what shows it. It skips without both variables.
func TestIntegrationECRNewPath(t *testing.T) {
	endpoint, registry := os.Getenv("LAJSTROM_TEST_ECR_ENDPOINT"), os.Getenv("LAJSTROM_TEST_ECR_REGISTRY")
	if endpoint == "" || registry == "" {
		t.Skip("set LAJSTROM_TEST_ECR_ENDPOINT and LAJSTROM_TEST_ECR_REGISTRY (make dev-up PROFILES=ecr)")
	}
	if os.Getenv("AWS_ACCESS_KEY_ID") == "" {
		t.Setenv("AWS_ACCESS_KEY_ID", "testing")
		t.Setenv("AWS_SECRET_ACCESS_KEY", "testing")
	}
	ctx := t.Context()
	p, err := provider.New(config.Registry{Name: "moto", Type: ecr.Type, URL: "http://" + registry, Prefix: "it-" + randomHex(t) + "/",
		ECR: config.ECR{Region: "us-east-1", Endpoint: endpoint, ForceDelete: true}}, slog.Default())
	must(t, err)
	st, err := sqlite.Open(ctx, fmt.Sprintf("file:facade-ecr-%d?mode=memory&cache=shared", memoryDBs.Add(1)))
	must(t, err)
	t.Cleanup(func() { _ = st.Close() })
	must(t, st.Migrate(ctx))
	must(t, st.CreateRepo(ctx, &index.Repo{Format: "oci", Name: "images", Registry: "moto", Prefix: "oci/images", Kind: "hosted"}))
	srv := httptest.NewServer(New(Options{Index: st, Providers: provider.Map{"moto": p}, Tokens: keys.NewEphemeralTokens()}))
	t.Cleanup(srv.Close)

	path := "oci/images/new-" + randomHex(t)
	src := memory.New()
	layer, err := oras.PushBytes(ctx, src, "application/vnd.example.layer", []byte("a new path on ECR"))
	must(t, err)
	desc, err := oras.PackManifest(ctx, src, oras.PackManifestVersion1_1, "application/vnd.example.test",
		oras.PackManifestOptions{Layers: []ocispec.Descriptor{layer}})
	must(t, err)
	must(t, src.Tag(ctx, desc, "v1"))
	dst, err := remote.NewRepository(strings.TrimPrefix(srv.URL, "http://") + "/" + path)
	must(t, err)
	dst.PlainHTTP = true
	if _, err := oras.Copy(ctx, src, "v1", dst, "v1", oras.DefaultCopyOptions); err != nil {
		t.Fatalf("oras push to %s: %v", path, err)
	}
	t.Cleanup(func() { _ = p.DeleteRepository(context.Background(), path) }) // t.Context() has ended by then
	names, err := p.ListRepositories(ctx, "oci/images/")
	if err != nil || !slices.Contains(names, path) {
		t.Errorf("moto's repositories below oci/images/ = %q, %v; want %s among them", names, err, path)
	}
	if got, err := dst.Resolve(ctx, "v1"); err != nil || got.Digest != desc.Digest {
		t.Errorf("Resolve(v1) = %s, %v; want %s", got.Digest, err, desc.Digest)
	}
}
