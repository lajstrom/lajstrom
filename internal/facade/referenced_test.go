package facade

import (
	"bytes"
	"net/http"
	"strings"
	"testing"

	"github.com/opencontainers/go-digest"

	"github.com/lajstrom/lajstrom/internal/index"
)

// TestDeleteReferenced (#89): the DELETE of a referrer whose attachment a scan report of the same
// version names as its input is 409 DENIED naming the report in the message and in
// detail.reports, and the referrer stays; once the report is deleted, the referrer goes, 202.
func TestDeleteReferenced(t *testing.T) {
	e := newEnv(t, envOptions{})
	f, subject := e.signedFile("docs/sbom.txt")
	name := "/v2/raw/assets/" + f.path + "/manifests/"
	sbom, sbomDigest := referrerOf(t, subject, "application/vnd.cyclonedx+json", "signature")
	scan, scanDigest := referrerOf(t, subject, "application/vnd.dev.sigstore.bundle.v0.3+json", "signature")
	for _, r := range []struct {
		body []byte
		d    digest.Digest
	}{{sbom, sbomDigest}, {scan, scanDigest}} {
		wantStatus(t, "push "+r.d.String(), e.put(f, r.d.String(), r.body), http.StatusCreated)
	}
	v, err := e.st.GetVersionByDigest(t.Context(), e.raw.ID, subject)
	must(t, err)
	must(t, e.st.ReplaceAttachments(t.Context(), v.ID, scanDigest, []index.Attachment{{Layer: digest.FromString("signature"),
		MediaType: "application/vnd.dev.sigstore.bundle.v0.3+json", ArtifactType: "application/vnd.dev.sigstore.bundle.v0.3+json",
		Kind: index.AttachmentScan, Input: sbomDigest, Verification: index.AttachmentUnsigned, Trust: "t", IndexedAt: e.clock.Now()}}))
	rr := e.do(e.h, http.MethodDelete, name+sbomDigest.String(), nil, principalHeader, "alice")
	if msg := wantError(t, "the delete of the SBOM a report names", rr, http.StatusConflict, codeDenied); !strings.Contains(msg, scanDigest.String()) {
		t.Errorf("the refusal %q does not name the report %s", msg, scanDigest)
	}
	if !bytes.Contains(rr.Body.Bytes(), []byte(`"detail":{"reports":["`+scanDigest.String()+`"]}`)) {
		t.Errorf("the refusal's detail: %s", rr.Body)
	}
	wantStatus(t, "HEAD of the SBOM after the refusal", e.do(e.h, http.MethodHead, name+sbomDigest.String(), nil, principalHeader, "alice"), http.StatusOK)
	for _, d := range []digest.Digest{scanDigest, sbomDigest} {
		wantStatus(t, "delete "+d.String(), e.do(e.h, http.MethodDelete, name+d.String(), nil, principalHeader, "alice"), http.StatusAccepted)
	}
}
