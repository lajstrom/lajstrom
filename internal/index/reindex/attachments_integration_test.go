//go:build integration

package reindex_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/lajstrom/lajstrom/internal/attest"
	"github.com/lajstrom/lajstrom/internal/attest/attesttest"
	"github.com/lajstrom/lajstrom/internal/config"
	"github.com/lajstrom/lajstrom/internal/format"
	"github.com/lajstrom/lajstrom/internal/format/raw"
	"github.com/lajstrom/lajstrom/internal/index/reindex"
	"github.com/lajstrom/lajstrom/internal/ingest"
	"github.com/lajstrom/lajstrom/internal/provider"
	"github.com/lajstrom/lajstrom/internal/provider/generic"
)

// TestIntegrationAttachments is TestAttachmentsRebuild against the registry at
// LAJSTROM_TEST_REGISTRY: a sigstore bundle by digest (the referrers API on Zot, the tag schema on
// distribution) and cosign's legacy .att, both pushed through ingest; the whole index dropped,
// a run writes the same attachment rows.
func TestIntegrationAttachments(t *testing.T) {
	addr := os.Getenv("LAJSTROM_TEST_REGISTRY")
	if addr == "" {
		t.Skip("set LAJSTROM_TEST_REGISTRY=host:port to run against a registry")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	prov, err := generic.New(config.Registry{
		Name: "local", Type: generic.Type, URL: "http://" + addr, Prefix: os.Getenv("LAJSTROM_TEST_REGISTRY_PREFIX"),
		Auth: config.RegistryAuth{Username: os.Getenv("LAJSTROM_TEST_REGISTRY_USERNAME"), Password: os.Getenv("LAJSTROM_TEST_REGISTRY_PASSWORD")},
	}, slog.New(slog.DiscardHandler))
	must(t, err)
	e := newEnv(t)
	suffix := make([]byte, 4)
	_, _ = rand.Read(suffix)
	e.repo = e.newRepo(t, "raw", "it-"+hex.EncodeToString(suffix))
	e.repo.Prefix = ""
	must(t, e.st.UpdateRepo(ctx, &e.repo))
	scanner := attesttest.NewKey(t)
	trust, err := attest.NewTrust([]attest.Entry{scanner.Entry(t, "scanner")})
	must(t, err)
	providers := provider.Map{"local": prov}
	plugins := func(name string) (format.Plugin, bool) { return raw.Plugin{}, name == raw.Name }
	log := slog.New(slog.DiscardHandler)
	e.pipe = ingest.New(ingest.Options{Providers: providers, Index: e.st, Plugins: plugins, Log: log, Trust: trust})
	e.w = reindex.New(reindex.Options{Index: e.st, Providers: providers, Inspector: e.pipe, Log: log, Now: e.clock.now, Attacher: e.pipe})

	pub := e.publish(t, "tool", "tool "+e.repo.Name)
	rr, err := prov.Repository(ctx, pub.RepoPath)
	must(t, err)
	subject, err := rr.Resolve(ctx, pub.Digest.String())
	must(t, err)
	must(t, rr.Push(ctx, ocispec.DescriptorEmptyJSON, bytes.NewReader(ocispec.DescriptorEmptyJSON.Data)))
	statement := attesttest.Statement(pub.Digest, cyclonedx, `{"bomFormat":"CycloneDX"}`)
	put := func(tag string, layerType string, content []byte, m ocispec.Manifest) {
		t.Helper()
		layer := ocispec.Descriptor{MediaType: layerType, Digest: digest.FromBytes(content), Size: int64(len(content))}
		must(t, rr.Push(ctx, layer, bytes.NewReader(content)))
		m.Versioned, m.MediaType, m.Config = specs2, ocispec.MediaTypeImageManifest, ocispec.DescriptorEmptyJSON
		m.Layers = []ocispec.Descriptor{layer}
		if tag != "" {
			m.Layers[0].Annotations = map[string]string{"predicateType": cyclonedx}
		}
		body, err := json.Marshal(m)
		must(t, err)
		_, err = e.pipe.PutReferrer(ctx, e.repo, ingest.Referrer{Path: "tool", Tag: tag, Manifest: body, Subject: pub.Digest,
			Desc: ocispec.Descriptor{MediaType: ocispec.MediaTypeImageManifest, Digest: digest.FromBytes(body), Size: int64(len(body))}}, alice)
		must(t, err)
	}
	put("", attesttest.BundleMediaType, attesttest.Bundle(t, statement, scanner), ocispec.Manifest{ArtifactType: attesttest.BundleMediaType,
		Subject: &subject, Annotations: map[string]string{"dev.sigstore.bundle.predicateType": cyclonedx}})
	put("sha256-"+pub.Digest.Encoded()+".att", attesttest.DSSEMediaType, attesttest.Envelope(t, statement, scanner), ocispec.Manifest{})
	written := e.attachmentRows(t)
	if n := len(written["tool:"+pub.Tag]); n != 2 {
		t.Fatalf("ingest wrote %d rows, want 2", n)
	}

	must(t, e.st.ResetDerived(ctx, e.repo.ID))
	r := e.run(t, reindex.Request{Prune: true})
	if r.Attachments != 2 || len(r.Errors) != 0 {
		t.Errorf("the run: %d attachments, errors %+v", r.Attachments, r.Errors)
	}
	same(t, "the attachment rows rebuilt", written, e.attachmentRows(t))
}
