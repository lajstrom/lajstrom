package deb

import (
	"net/http"
	"strings"
	"testing"

	"github.com/opencontainers/go-digest"

	"github.com/lajstrom/lajstrom/internal/index"
	"github.com/lajstrom/lajstrom/internal/policy"
	"github.com/lajstrom/lajstrom/internal/testutil"
)

// TestPolicyVerdict: a deny verdict on a hosted apt version, as the deny of the management API
// writes its rows, takes the version out of Packages, where the other version stays, and its
// pool file is 403 with the verdict's reason (the third criterion of #101 on the routes).
func TestPolicyVerdict(t *testing.T) {
	p := newPipeline(t)
	e := envOn(t, p, envOptions{policy: policy.New(policy.Options{Index: p.st})})
	ctx := t.Context()
	old, _ := testutil.Deb(testutil.DebOptions{Version: "1.0.0-1"})
	cur, _ := testutil.Deb(testutil.DebOptions{Version: "1.1.0-1"})
	p.upload(t, old, "jammy")
	p.upload(t, cur, "jammy")
	pkg, err := p.st.GetPackage(ctx, p.repo.ID, "hello")
	must(t, err)
	vs, err := p.st.ListVersions(ctx, pkg.ID)
	must(t, err)
	var v index.Version
	for _, each := range vs {
		if each.Version == "1.0.0-1" {
			v = each
		}
	}
	row := index.Attachment{Digest: digest.FromString("verdict"), Layer: digest.FromString("layer"),
		MediaType: "application/vnd.dev.sigstore.bundle.v0.3+json", ArtifactType: "application/vnd.dev.sigstore.bundle.v0.3+json",
		Kind: index.AttachmentAttestation, PredicateType: policy.PredicateType, Verification: index.AttachmentVerified,
		SignedBy: "lajstrom", Trust: "t",
		Verdict: &index.Verdict{Path: "hello", Subject: v.Digest, Verdict: index.PolicyDeny, Reason: "CVE-2026-1234",
			Source: policy.SourceManual, SignedBy: "lajstrom"}}
	must(t, p.st.ReplaceAttachments(ctx, v.ID, row.Digest, []index.Attachment{row}))
	must(t, p.st.InvalidateIndexCache(ctx, p.repo.ID, ""))

	packages := e.get("dists/jammy/main/binary-amd64/Packages").Body.String()
	if strings.Contains(packages, "Version: 1.0.0-1") || !strings.Contains(packages, "Version: 1.1.0-1") {
		t.Errorf("Packages: want 1.1.0-1 and not the denied 1.0.0-1:\n%s", packages)
	}
	rr := e.do(http.MethodGet, "pool/main/h/hello/hello_1.0.0-1_amd64.deb")
	if rr.Code != http.StatusForbidden || !strings.Contains(rr.Body.String(), "hello 1.0.0-1 is denied by policy: CVE-2026-1234") {
		t.Errorf("the denied pool file: %d %s", rr.Code, rr.Body)
	}
	if rr := e.do(http.MethodGet, "pool/main/h/hello/hello_1.1.0-1_amd64.deb"); rr.Code != http.StatusOK {
		t.Errorf("the allowed pool file: %d", rr.Code)
	}
}
