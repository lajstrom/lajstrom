package facade

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
)

// TestPolicyPullRequirements is #1287 on /v2 (docs/spec/policy.md §6.5, §11): on a managed repo
// with enforcePull that requires an SPDX document signed by release, a push of a version is not
// refused for it; a consumer's read of its manifest, by tag or by digest, GET or HEAD, without a
// token or with one that grants pull alone, is 403 DENIED with the requirement in detail and an
// audit row, while a token that grants push or attach reads it, as cosign and oras do before they
// attach; a referrer, no version, is read by anyone; once the attachment is indexed, everyone
// reads the version.
func TestPolicyPullRequirements(t *testing.T) {
	trust, err := attest.NewTrust([]attest.Entry{attesttest.NewKey(t).Entry(t, "release")})
	must(t, err)
	e := newEnv(t, envOptions{policy: true, trust: trust})
	ctx := t.Context()
	e.raw.Policy = json.RawMessage(`{"enforcePull": true, "requireAttachments": [{"predicateType": "https://spdx.dev/Document", "signedBy": ["release"]}]}`)
	must(t, e.st.UpdateRepo(ctx, &e.raw))
	f := e.newRawFile("app", "the content of app")
	body, d := f.body(t)
	wantStatus(t, "the push of the version", e.put(f, f.tag(), body), http.StatusCreated)

	base := "/v2/raw/assets/app/manifests/"
	bearer := func(actions ...string) []string {
		return []string{"Authorization", "Bearer " + e.issue(userP("alice"), repoAccess("raw/assets/app", actions...))}
	}
	const reason = "requires an attachment https://spdx.dev/Document signed by release"
	for _, ref := range []string{f.tag(), d.String()} {
		for _, hdr := range [][]string{nil, bearer("pull")} {
			entry := wantEntry(t, "a consumer's GET of "+ref, e.do(e.h, http.MethodGet, base+ref, nil, hdr...), http.StatusForbidden, codeDenied)
			detail, _ := entry.Detail.(map[string]any)
			req, _ := detail["requirement"].(map[string]any)
			if entry.Message != "app "+f.tag()+" is denied by policy: "+reason || detail["reason"] != reason ||
				req["predicateType"] != "https://spdx.dev/Document" || len(req["signedBy"].([]any)) != 1 {
				t.Errorf("a consumer's GET of %s: %q, detail %v", ref, entry.Message, entry.Detail)
			}
			wantStatus(t, "a consumer's HEAD of "+ref, e.do(e.h, http.MethodHead, base+ref, nil, hdr...), http.StatusForbidden)
		}
		for _, actions := range [][]string{{"pull", "push"}, {"pull", "attach"}} {
			for _, method := range []string{http.MethodGet, http.MethodHead} {
				wantStatus(t, method+" of "+ref+" with "+strings.Join(actions, ","), e.do(e.h, method, base+ref, nil, bearer(actions...)...), http.StatusOK)
			}
		}
	}
	referrer := e.pushReferrer("raw/assets/app", d, "application/spdx+json", "an unsigned sbom")
	wantStatus(t, "a consumer's GET of a referrer", e.do(e.h, http.MethodGet, base+referrer.String(), nil), http.StatusOK)
	evs, err := e.st.ListAudit(ctx, index.AuditFilter{Action: policy.ActionRefuse})
	must(t, err)
	if len(evs) != 8 || !strings.Contains(string(evs[0].Details), `"requirement":{"predicateType":"https://spdx.dev/Document","signedBy":["release"]}`) ||
		!strings.Contains(string(evs[0].Details), `"surface":"pull"`) {
		t.Errorf("the audit rows of the refusals: %d, %s", len(evs), evs[0].Details)
	}

	v, err := e.st.GetVersionByDigest(ctx, e.raw.ID, d)
	must(t, err)
	sbom := digest.FromString("a signed sbom")
	must(t, e.st.ReplaceAttachments(ctx, v.ID, sbom, []index.Attachment{{Layer: sbom, MediaType: attest.BundleMediaType, Kind: index.AttachmentSBOM,
		PredicateType: "https://spdx.dev/Document", Verification: index.AttachmentVerified, SignedBy: "release", Trust: "t"}}))
	wantStatus(t, "a consumer's GET once the attachment is indexed", e.do(e.h, http.MethodGet, base+f.tag(), nil), http.StatusOK)
}
