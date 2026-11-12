//go:build integration

package reindex_test

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/lajstrom/lajstrom/internal/config"
	"github.com/lajstrom/lajstrom/internal/format"
	"github.com/lajstrom/lajstrom/internal/format/raw"
	"github.com/lajstrom/lajstrom/internal/index/reindex"
	"github.com/lajstrom/lajstrom/internal/ingest"
	"github.com/lajstrom/lajstrom/internal/provider"
	"github.com/lajstrom/lajstrom/internal/provider/generic"
)

// TestIntegrationNoCatalog is the acceptance of #37 against the registry at
// LAJSTROM_TEST_REGISTRY: a backend without a catalog and without the referrers API, in two
// cells. In probe, Lajstrom talks to the registry through a front that answers 404 to _catalog
// and to the referrers API, as Harbor answers a robot's _catalog and distribution its referrers
// API, and the probe finds neither; distribution 3.1.2 has no switch for its catalog (its
// configuration has catalog.maxentries only). In config, capabilities.catalog and .referrers are
// false in the registry's configuration. In each: two raw files published and a signature
// attached through ingest, which the provider writes with the sha256-<hex> tag schema and lists
// from it; a run reads the OCI repositories that oci_repos records; with the derived rows lost
// a run finds nothing, a run that attaches the repositories brings every row back, and the next
// run finds them without the list.
func TestIntegrationNoCatalog(t *testing.T) {
	addr := os.Getenv("LAJSTROM_TEST_REGISTRY")
	if addr == "" {
		t.Skip("set LAJSTROM_TEST_REGISTRY=host:port to run against a registry")
	}
	target, err := url.Parse("http://" + addr)
	must(t, err)
	front := httptest.NewServer(withoutCatalog(target))
	defer front.Close()
	off := false
	for _, c := range []struct {
		name string
		url  string
		caps config.CapabilityOverrides
	}{
		{"probe", frontURL(t, front, target), config.CapabilityOverrides{}},
		{"config", target.String(), config.CapabilityOverrides{Catalog: &off, Referrers: &off}},
	} {
		t.Run(c.name, func(t *testing.T) { noCatalog(t, c.url, c.caps) })
	}
}

// noCatalog is one cell of TestIntegrationNoCatalog.
func noCatalog(t *testing.T, registryURL string, caps config.CapabilityOverrides) {
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	prov := noCatalogProvider(ctx, t, registryURL, caps)
	e := newEnv(t)
	suffix := make([]byte, 4)
	_, _ = rand.Read(suffix)
	e.repo = e.newRepo(t, "raw", "nc-"+hex.EncodeToString(suffix))
	plugins := func(name string) (format.Plugin, bool) { return raw.Plugin{}, name == raw.Name }
	log := slog.New(slog.DiscardHandler)
	e.pipe = ingest.New(ingest.Options{Providers: provider.Map{"local": prov}, Index: e.st, Plugins: plugins, Log: log})
	e.w = reindex.New(reindex.Options{Index: e.st, Providers: provider.Map{"local": prov}, Inspector: e.pipe, Log: log})
	a := e.publish(t, "docs/a.txt", "a "+e.repo.Name)
	b := e.publish(t, "docs/b.txt", "b "+e.repo.Name)
	sig := attachSignature(ctx, t, e, prov, a)

	// The manifests are the two versions and the tag-schema index of a.
	r := e.run(t, reindex.Request{Prune: true})
	same(t, "a run over oci_repos", "oci=2 manifests=3 added=0 unchanged=2 removed=0 referrers=1 orphans=0 foreign=0 errors=0", counts(r))
	before := e.derived(t)

	must(t, e.st.ResetDerived(ctx, e.repo.ID))
	r = e.run(t, reindex.Request{Prune: true})
	same(t, "a run without the list", "oci=0 manifests=0 added=0 unchanged=0 removed=0 referrers=0 orphans=0 foreign=0 errors=0", counts(r))
	attach := []string{a.RepoPath, b.RepoPath, "raw/" + e.repo.Name + "/not-there"}
	r = e.run(t, reindex.Request{Prune: true, Repositories: attach})
	same(t, "the attach", "oci=3 manifests=3 added=2 unchanged=0 removed=0 referrers=1 orphans=0 foreign=0 errors=0", counts(r))
	same(t, "the rows after the attach", before, e.derived(t))
	r = e.run(t, reindex.Request{Prune: true})
	same(t, "the run after it", "oci=2 manifests=3 added=0 unchanged=2 removed=0 referrers=1 orphans=0 foreign=0 errors=0", counts(r))
	t.Logf("signature %s of %s through the tag schema; %d OCI repositories read from oci_repos", sig, a.Digest, r.OCIRepositories)
}

// noCatalogProvider returns the provider of the registry at registryURL with caps, and checks
// that it has neither a catalog nor the referrers API.
func noCatalogProvider(ctx context.Context, t *testing.T, registryURL string, caps config.CapabilityOverrides) provider.Provider {
	t.Helper()
	prov, err := generic.New(config.Registry{
		Name: "local", Type: generic.Type, URL: registryURL, Prefix: os.Getenv("LAJSTROM_TEST_REGISTRY_PREFIX"), Capabilities: caps,
		Auth: config.RegistryAuth{Username: os.Getenv("LAJSTROM_TEST_REGISTRY_USERNAME"), Password: os.Getenv("LAJSTROM_TEST_REGISTRY_PASSWORD")},
	}, slog.New(slog.DiscardHandler))
	must(t, err)
	got, err := prov.Capabilities(ctx)
	must(t, err)
	if got.Catalog || got.Referrers {
		t.Fatalf("capabilities %+v, want neither a catalog nor the referrers API", got)
	}
	if _, err := prov.ListRepositories(ctx, "raw/"); !errors.Is(err, provider.ErrUnsupported) {
		t.Fatalf("ListRepositories: %v, want ErrUnsupported", err)
	}
	return prov
}

// counts is the counts of r that TestIntegrationNoCatalog checks, on one line.
func counts(r *reindex.RepoReport) string {
	return fmt.Sprintf("oci=%d manifests=%d added=%d unchanged=%d removed=%d referrers=%d orphans=%d foreign=%d errors=%d",
		r.OCIRepositories, r.Manifests, r.Added, r.Unchanged, r.Removed, r.Referrers, len(r.OrphanReferrers), len(r.Foreign), len(r.Errors))
}

// attachSignature attaches a signature to the version pub through ingest, as the facade does
// for cosign sign, and checks that the provider lists it and wrote the tag sha256-<hex>.
func attachSignature(ctx context.Context, t *testing.T, e *env, prov provider.Provider, pub *format.Published) digest.Digest {
	t.Helper()
	subject := ocispec.Descriptor{MediaType: ocispec.MediaTypeImageManifest, Digest: pub.Digest, Size: manifestSize(ctx, t, prov, pub)}
	body, err := json.Marshal(ocispec.Manifest{Versioned: specs2, MediaType: ocispec.MediaTypeImageManifest,
		ArtifactType: "application/vnd.dev.cosign.artifact.sig.v1+json", Config: pushEmpty(ctx, t, prov, pub.RepoPath), Subject: &subject,
		Layers: []ocispec.Descriptor{pushBlob(ctx, t, prov, pub.RepoPath, "application/vnd.dev.cosign.simplesigning.v1+json", []byte(`{"critical":{}}`))}})
	must(t, err)
	desc := ocispec.Descriptor{MediaType: ocispec.MediaTypeImageManifest, Digest: digest.FromBytes(body), Size: int64(len(body))}
	_, err = e.pipe.PutReferrer(ctx, e.repo, ingest.Referrer{Path: strings.TrimPrefix(pub.RepoPath, "raw/"+e.repo.Name+"/"),
		Desc: desc, Manifest: body, Subject: pub.Digest}, alice)
	must(t, err)
	rr, err := prov.Repository(ctx, pub.RepoPath)
	must(t, err)
	var listed []digest.Digest
	must(t, rr.Referrers(ctx, subject, "", func(page []ocispec.Descriptor) error {
		for _, d := range page {
			listed = append(listed, d.Digest)
		}
		return nil
	}))
	same(t, "the referrers of the signed version", []digest.Digest{desc.Digest}, listed)
	var tags []string
	must(t, rr.Tags(ctx, "", func(page []string) error { tags = append(tags, page...); return nil }))
	if !slices.Contains(tags, "sha256-"+pub.Digest.Encoded()) {
		t.Errorf("tags %v without the tag-schema index of %s", tags, pub.Digest)
	}
	return desc.Digest
}

// frontURL is the URL of front named by the host name of the registry at target when that name
// is the loopback's (localhost), else front's own (127.0.0.1). The provider sends its
// credentials to a token realm on the host name of its URL only, any port, or over https to
// tokenRealmHosts: a registry at localhost whose realm is on localhost (Forgejo, Gitea, Harbor
// in the dev stack) gets them through a front named localhost, not one named 127.0.0.1 (#354,
// #800).
func frontURL(t *testing.T, front *httptest.Server, target *url.URL) string {
	t.Helper()
	host := target.Hostname()
	if ip := net.ParseIP(host); host != "localhost" && (ip == nil || !ip.IsLoopback()) {
		return front.URL
	}
	u, err := url.Parse(front.URL)
	must(t, err)
	u.Host = net.JoinHostPort(host, u.Port())
	return u.String()
}

// withoutCatalog is a front for the registry at target that answers 404 UNSUPPORTED to
// _catalog and to the referrers API, as a registry without either does, and passes everything
// else on with the Host the client sent, so that the registry's absolute upload locations name
// the front.
func withoutCatalog(target *url.URL) http.Handler {
	proxy := &httputil.ReverseProxy{Rewrite: func(r *httputil.ProxyRequest) {
		r.SetURL(target)
		r.Out.Host = r.In.Host
	}}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v2/_catalog" || strings.Contains(r.URL.Path, "/referrers/") {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, `{"errors":[{"code":"UNSUPPORTED","message":"not served by this registry"}]}`)
			return
		}
		proxy.ServeHTTP(w, r)
	})
}
