package facade

import (
	"bytes"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/opencontainers/go-digest"

	"github.com/lajstrom/lajstrom/internal/index"
	"github.com/lajstrom/lajstrom/internal/policy"
)

// pushImage pushes the layer and a manifest of it to oci/images/app under tag, and returns the
// manifest's digest.
func (e *env) pushImage(t *testing.T, layer []byte, tag string) digest.Digest {
	t.Helper()
	ld := digest.FromBytes(layer)
	wantStatus(t, "blob", e.do(e.h, http.MethodPost, "/v2/oci/images/app/blobs/uploads/?digest="+ld.String(), bytes.NewReader(layer),
		"Content-Type", "application/octet-stream"), http.StatusCreated)
	body, d := manifest(t, layer, tag)
	wantStatus(t, "PUT", e.do(e.h, http.MethodPut, "/v2/oci/images/app/manifests/"+tag, bytes.NewReader(body),
		"Content-Type", ocispecManifest), http.StatusCreated)
	return d
}

// TestPolicyPull is the fourth criterion of #101 on the facade: on an oci repo without
// enforcePull a denied digest is served; with it, its manifest is 403 DENIED by tag and by
// digest, HEAD too, with the verdict in detail and an audit row, while its blob and the tag list
// are served.
func TestPolicyPull(t *testing.T) {
	e := newEnv(t, envOptions{policy: true})
	ctx := t.Context()
	layer := []byte("a layer of the denied image")
	d := e.pushImage(t, layer, "v1")
	ref := digest.FromString("the verdict")
	must(t, e.st.ReplaceOCIVerdicts(ctx, e.oci.ID, ref, []index.Verdict{{Path: "app", Subject: d, Verdict: index.PolicyDeny,
		Reason: "CVE-2026-1234", Source: "trivy", SignedBy: "scanner"}}))

	wantStatus(t, "a pull without enforcePull", e.do(e.h, http.MethodGet, "/v2/oci/images/app/manifests/"+d.String(), nil), http.StatusOK)

	e.oci.Policy = json.RawMessage(`{"enforcePull": true}`)
	must(t, e.st.UpdateRepo(ctx, &e.oci))
	for _, path := range []string{"/v2/oci/images/app/manifests/" + d.String(), "/v2/oci/images/app/manifests/v1"} {
		entry := wantEntry(t, path, e.do(e.h, http.MethodGet, path, nil), http.StatusForbidden, codeDenied)
		if entry.Message != "app is denied by policy: CVE-2026-1234" && entry.Message != "app v1 is denied by policy: CVE-2026-1234" {
			t.Errorf("%s: the message %q", path, entry.Message)
		}
		detail, _ := entry.Detail.(map[string]any)
		if detail["verdict"] != ref.String() || detail["reason"] != "CVE-2026-1234" {
			t.Errorf("%s: the detail %v", path, entry.Detail)
		}
	}
	wantStatus(t, "HEAD", e.do(e.h, http.MethodHead, "/v2/oci/images/app/manifests/v1", nil), http.StatusForbidden)
	wantStatus(t, "the blob", e.do(e.h, http.MethodGet, "/v2/oci/images/app/blobs/"+digest.FromBytes(layer).String(), nil), http.StatusOK)
	wantStatus(t, "the tag list", e.do(e.h, http.MethodGet, "/v2/oci/images/app/tags/list", nil), http.StatusOK)
	evs, err := e.st.ListAudit(ctx, index.AuditFilter{Action: policy.ActionRefuse})
	must(t, err)
	if len(evs) != 3 || evs[0].Repo != "oci/images" {
		t.Errorf("the audit rows of the refusals: %d, %+v", len(evs), evs)
	}

	// Another image is served.
	other := e.pushImage(t, []byte("another layer"), "v2")
	wantStatus(t, "another image", e.do(e.h, http.MethodGet, "/v2/oci/images/app/manifests/"+other.String(), nil), http.StatusOK)
}

// TestPolicyPush: a rule refuses a push to an oci repo before it passes through, and a push to a
// managed repo in ingest, both 403 DENIED with the rule's reason; nothing reaches the registry.
func TestPolicyPush(t *testing.T) {
	e := newEnv(t, envOptions{policy: true})
	ctx := t.Context()
	must(t, e.st.CreatePolicyRule(ctx, &index.PolicyRule{RepoMask: "*/*", Name: "app", Version: "evil", Variant: "*",
		Effect: index.PolicyDeny, Reason: "no evil tags", CreatedBy: "user:u"}))
	layer := []byte("layer")
	ld := digest.FromBytes(layer)
	wantStatus(t, "blob", e.do(e.h, http.MethodPost, "/v2/oci/images/app/blobs/uploads/?digest="+ld.String(), bytes.NewReader(layer),
		"Content-Type", "application/octet-stream"), http.StatusCreated)
	body, _ := manifest(t, layer, "evil")
	entry := wantEntry(t, "the oci push", e.do(e.h, http.MethodPut, "/v2/oci/images/app/manifests/evil", bytes.NewReader(body),
		"Content-Type", ocispecManifest), http.StatusForbidden, codeDenied)
	if entry.Message != "app evil is denied by policy: no evil tags" {
		t.Errorf("the message: %q", entry.Message)
	}
	wantStatus(t, "the refused tag", e.do(e.h, http.MethodGet, "/v2/oci/images/app/manifests/evil", nil), http.StatusNotFound)

	f := e.newRawFile("app", "evil content")
	must(t, e.st.CreatePolicyRule(ctx, &index.PolicyRule{RepoMask: "raw/assets", Name: "app", Version: f.tag(), Variant: "*",
		Effect: index.PolicyDeny, Reason: "not this file", CreatedBy: "user:u"}))
	b, _ := f.body(t)
	wantEntry(t, "the managed push", e.put(f, f.tag(), b), http.StatusForbidden, codeDenied)
	if vs, err := e.st.ListPackages(ctx, e.raw.ID, ""); err != nil || len(vs) != 0 {
		t.Errorf("the refused push is indexed: %d packages, %v", len(vs), err)
	}
}
