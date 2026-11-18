package pypi

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/opencontainers/go-digest"

	"github.com/lajstrom/lajstrom/internal/attest"
	"github.com/lajstrom/lajstrom/internal/attest/attesttest"
	"github.com/lajstrom/lajstrom/internal/index"
	"github.com/lajstrom/lajstrom/internal/policy"
	"github.com/lajstrom/lajstrom/internal/testutil"
)

// TestPolicyRequirements is #1287 on a hosted pypi repo that requires an SPDX document signed by
// release (docs/spec/policy.md §11): the upload of a version is not refused for it; the version is
// left out of the simple index, whatever an allow rule says, and its download is 403 naming the
// requirement, with an audit row that carries it; once a verified attachment of a longer version
// of the type is indexed and the documents are rendered again, it is listed and served.
func TestPolicyRequirements(t *testing.T) {
	e := newEnv(t)
	trust, err := attest.NewTrust([]attest.Entry{attesttest.NewKey(t).Entry(t, "release")})
	must(t, err)
	e.withPolicyTrust(trust)
	ctx := t.Context()
	e.repo.Policy = json.RawMessage(`{"requireAttachments": [{"predicateType": "https://spdx.dev/Document", "signedBy": ["release"]}]}`)
	must(t, e.st.UpdateRepo(ctx, &e.repo))
	must(t, e.st.CreatePolicyRule(ctx, &index.PolicyRule{RepoMask: "pypi/internal", Name: "*", Version: "*", Variant: "*",
		Effect: "allow", Reason: "ours", CreatedBy: "user:u"}))
	whl, info := testutil.Wheel(testutil.WheelOptions{Name: "requests", Version: "2.32.0"})
	e.publish(info.Filename, whl)

	const reason = "requests 2.32.0 is denied by policy: requires an attachment https://spdx.dev/Document signed by release"
	if rr := e.do(http.MethodGet, "simple/requests/"); rr.Code != http.StatusOK || strings.Contains(rr.Body.String(), info.Filename) {
		t.Errorf("the simple page before the attachment: %d %s", rr.Code, rr.Body)
	}
	if rr := e.do(http.MethodGet, "files/requests/2.32.0/"+info.Filename); rr.Code != http.StatusForbidden || !strings.Contains(rr.Body.String(), reason) {
		t.Errorf("the download before the attachment: %d %s", rr.Code, rr.Body)
	}
	evs, err := e.st.ListAudit(ctx, index.AuditFilter{Action: policy.ActionRefuse})
	must(t, err)
	if len(evs) != 1 || !strings.Contains(string(evs[0].Details), `"requirement":{"predicateType":"https://spdx.dev/Document","signedBy":["release"]}`) {
		t.Errorf("the audit row of the refusal: %+v", evs)
	}

	ms, err := e.st.FindFiles(ctx, e.repo.ID, info.Filename)
	must(t, err)
	if len(ms) != 1 {
		t.Fatalf("the upload is not indexed: %d matches", len(ms))
	}
	d := digest.FromString("an sbom")
	must(t, e.st.ReplaceAttachments(ctx, ms[0].Version.ID, d, []index.Attachment{{Layer: d, MediaType: attest.BundleMediaType,
		Kind: index.AttachmentSBOM, PredicateType: "https://spdx.dev/Document/v2.3", Verification: index.AttachmentVerified, SignedBy: "release", Trust: "t"}}))
	must(t, e.st.InvalidateIndexCache(ctx, e.repo.ID, ""))
	if rr := e.do(http.MethodGet, "simple/requests/"); rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), info.Filename) {
		t.Errorf("the simple page with the attachment: %d %s", rr.Code, rr.Body)
	}
	if rr := e.do(http.MethodGet, "files/requests/2.32.0/"+info.Filename); rr.Code != http.StatusOK {
		t.Errorf("the download with the attachment: %d %s", rr.Code, rr.Body)
	}
}
