package ingest_test

import (
	"encoding/json"
	"testing"

	"github.com/lajstrom/lajstrom/internal/attest/attesttest"
	"github.com/lajstrom/lajstrom/internal/format"
)

// TestRequirementsInvalidate: on a repo with required attachments, the rows of a referrer that
// ingest writes, and the delete of the referrer, invalidate the repo's documents in their
// transaction; on a repo without, neither does (docs/spec/policy.md §11, index-schema.md,
// Attachments).
func TestRequirementsInvalidate(t *testing.T) {
	ctx := t.Context()
	for _, requires := range []bool{false, true} {
		e := newAttachEnv(t, 1<<20)
		if requires {
			e.repo.Policy = json.RawMessage(`{"requireAttachments": [{"predicateType": "https://cyclonedx.org/bom", "signedBy": ["scanner"]}]}`)
			must(t, e.st.UpdateRepo(ctx, &e.repo))
		}
		gen := func() int64 {
			t.Helper()
			g, err := e.st.IndexCacheGeneration(ctx, e.repo.ID)
			must(t, err)
			return g
		}
		before := gen()
		sbom := e.referrer(t, e.subject.Digest, "", attesttest.BundleMediaType, map[string]string{"dev.sigstore.bundle.predicateType": cyclonedx},
			layerOf{attesttest.BundleMediaType, attesttest.Bundle(t, attesttest.Statement(e.subject.Digest, cyclonedx, `{"bomFormat":"CycloneDX"}`), e.scanner), nil})
		_, err := e.pipe.PutReferrer(ctx, e.repo, sbom, alice)
		must(t, err)
		if got := e.attachments(t); len(got) != 1 || got[0].SignedBy != "scanner" {
			t.Fatalf("the rows of the SBOM: %+v", got)
		}
		put := gen()
		_, err = e.pipe.Delete(ctx, e.repo, format.Deletion{Path: e.path, Reference: sbom.Desc.Digest.String()}, alice)
		must(t, err)
		deleted := gen()
		if requires != (put > before) || requires != (deleted > put) {
			t.Errorf("requirements %v: the generation %d, %d after the referrer, %d after its delete", requires, before, put, deleted)
		}
	}
}
