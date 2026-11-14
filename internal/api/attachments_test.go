package api

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/opencontainers/go-digest"

	"github.com/lajstrom/lajstrom/internal/index"
)

// attachDetailed adds attachments to the variants of requests 1.0 of publishDetailed: on the wheel
// a CycloneDX SBOM the trusted key signed, with a time and the tool's annotations, and a scan
// another key signed, a day later; on the sdist a raw SPDX without a time.
func attachDetailed(t *testing.T, st index.Store, repoID int64) {
	t.Helper()
	ctx := context.Background()
	wheel, err := st.GetVersionByDigest(ctx, repoID, digest.FromString("wheel"))
	must(t, err)
	sdist, err := st.GetVersionByDigest(ctx, repoID, digest.FromString("sdist"))
	must(t, err)
	created, later := testNow.Add(time.Hour), testNow.Add(25*time.Hour)
	const bundle = "application/vnd.dev.sigstore.bundle.v0.3+json"
	must(t, st.ReplaceAttachments(ctx, wheel.ID, digest.FromString("sbom"), []index.Attachment{{Layer: digest.FromString("sbom layer"),
		MediaType: bundle, ArtifactType: bundle, Kind: index.AttachmentSBOM, PredicateType: "https://cyclonedx.org/bom", Size: 859,
		Annotations: map[string]string{"lajstrom.attachment.tool": "trivy", "lajstrom.attachment.toolVersion": "0.60.0"},
		Tool:        "trivy", ToolVersion: "0.60.0", Verification: index.AttachmentVerified, SignedBy: "scanner", Trust: "t",
		CreatedAt: &created, IndexedAt: testNow}}))
	must(t, st.ReplaceAttachments(ctx, wheel.ID, digest.FromString("scan"), []index.Attachment{{Layer: digest.FromString("scan layer"),
		MediaType: bundle, ArtifactType: bundle, Kind: index.AttachmentScan, PredicateType: "https://cosign.sigstore.dev/attestation/vuln/v1",
		Size: 1024, Database: "2026-01-03", Input: digest.FromString("sbom"), Verification: index.AttachmentUntrusted,
		Reason: "no trusted key verifies its signature", Trust: "t", CreatedAt: &later, IndexedAt: testNow}}))
	must(t, st.ReplaceAttachments(ctx, sdist.ID, digest.FromString("spdx"), []index.Attachment{{Layer: digest.FromString("spdx layer"),
		MediaType: "application/spdx+json", ArtifactType: "application/spdx+json", Kind: index.AttachmentSBOM, Size: 26,
		Verification: index.AttachmentUnsigned, Reason: "it carries no signature", Trust: "t", IndexedAt: testNow}}))
}

// TestListAttachments: the attachments of a version's variants, newest first and those without
// a time last, each with its variant and label; the filters of the query, the limit, and the
// refusals of what is no value.
func TestListAttachments(t *testing.T) {
	f := newFixture(t, Options{})
	repo := f.create(t, "pypi", "libs")
	publishDetailed(t, f.st, repo.ID)
	attachDetailed(t, f.st, repo.ID)
	dg := func(s string) string { return digest.FromString(s).String() }
	scan := `{"variant": "py3-none-any", "subject": "` + dg("wheel") + `", "digest": "` + dg("scan") + `", "layer": "` + dg("scan layer") + `",
		"mediaType": "application/vnd.dev.sigstore.bundle.v0.3+json", "artifactType": "application/vnd.dev.sigstore.bundle.v0.3+json",
		"kind": "scan", "predicateType": "https://cosign.sigstore.dev/attestation/vuln/v1", "size": 1024, "annotations": {},
		"database": "2026-01-03", "input": "` + dg("sbom") + `", "verification": "untrusted",
		"reason": "no trusted key verifies its signature", "createdAt": "2026-01-03T13:00:00Z", "indexedAt": "2026-01-02T12:00:00Z"}`
	sbom := `{"variant": "py3-none-any", "subject": "` + dg("wheel") + `", "digest": "` + dg("sbom") + `", "layer": "` + dg("sbom layer") + `",
		"mediaType": "application/vnd.dev.sigstore.bundle.v0.3+json", "artifactType": "application/vnd.dev.sigstore.bundle.v0.3+json",
		"kind": "sbom", "predicateType": "https://cyclonedx.org/bom", "size": 859,
		"annotations": {"lajstrom.attachment.tool": "trivy", "lajstrom.attachment.toolVersion": "0.60.0"},
		"tool": "trivy", "toolVersion": "0.60.0", "verification": "verified", "signedBy": "scanner",
		"createdAt": "2026-01-02T13:00:00Z", "indexedAt": "2026-01-02T12:00:00Z"}`
	spdx := `{"variant": "sdist", "subject": "` + dg("sdist") + `", "digest": "` + dg("spdx") + `", "layer": "` + dg("spdx layer") + `",
		"mediaType": "application/spdx+json", "artifactType": "application/spdx+json", "kind": "sbom", "predicateType": "", "size": 26,
		"annotations": {}, "verification": "unsigned", "reason": "it carries no signature", "indexedAt": "2026-01-02T12:00:00Z"}`
	list := func(truncated bool, entries ...string) string {
		return `{"attachments": [` + strings.Join(entries, ",") + `], "truncated": ` + map[bool]string{true: "true", false: "false"}[truncated] + `}`
	}
	target := "/api/v1/repos/pypi/libs/packages/requests/versions/1.0/attachments"
	for _, tt := range []struct{ query, want string }{
		{"", list(false, scan, sbom, spdx)},
		{"?variant=sdist", list(false, spdx)},
		{"?variant=py3-none-any&kind=sbom", list(false, sbom)},
		{"?kind=sbom", list(false, sbom, spdx)},
		{"?kind=provenance", list(false)},
		{"?predicateType=https://cyclonedx.org/bom", list(false, sbom)},
		{"?verified=true", list(false, sbom)},
		{"?verified=false", list(false, scan, spdx)},
		{"?limit=2", list(true, scan, sbom)},
		{"?limit=3", list(false, scan, sbom, spdx)},
	} {
		var got any
		decodeJSON(t, f.do(http.MethodGet, target+tt.query, "", admin), http.StatusOK, &got)
		wantJSON(t, "attachments"+tt.query, got, tt.want)
	}
	for _, tt := range []struct{ target string }{
		{target + "?kind=report"},
		{target + "?verified=maybe"},
		{target + "?limit=0"},
		{target + "?limit=1001"},
	} {
		wantError(t, f.do(http.MethodGet, tt.target, "", admin), http.StatusBadRequest, "INVALID", "")
	}
	for _, tt := range []struct{ target, want string }{
		{"/api/v1/repos/pypi/libs/packages/requests/versions/2.0/attachments", `has no version "2.0"`},
		{target + "?variant=cp311", `with the variant "cp311"`},
		{"/api/v1/repos/pypi/none/packages/requests/versions/1.0/attachments", "repo pypi/none does not exist"},
	} {
		wantError(t, f.do(http.MethodGet, tt.target, "", admin), http.StatusNotFound, "NOT_FOUND", tt.want)
	}
}
