package raw

import (
	"bytes"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

// TestDeleteReferrers: DELETE of a path deletes the referrers of each version with it, as the
// DELETE of the manifest on /v2 does, and the audit row of each version counts them.
func TestDeleteReferrers(t *testing.T) {
	e := newEnv(t, envOptions{})
	v := e.put(fileTxt, "one")
	name := "raw/scratch/docs/file.txt"
	// A cosign bundle of the version: an empty config, no layers, the version as its subject.
	m := []byte(`{"schemaVersion":2,"mediaType":"` + ocispec.MediaTypeImageManifest + `",` +
		`"artifactType":"application/vnd.dev.sigstore.bundle.v0.3+json",` +
		`"config":{"mediaType":"` + ocispec.MediaTypeEmptyJSON + `","digest":"` + ocispec.DescriptorEmptyJSON.Digest.String() + `","size":2},` +
		`"layers":[],"subject":{"mediaType":"` + ocispec.MediaTypeImageManifest + `","digest":"` + v.Digest + `","size":1}}`)
	sig := ocispec.Descriptor{MediaType: ocispec.MediaTypeImageManifest, Digest: digest.FromBytes(m), Size: int64(len(m))}
	rr, err := e.prov.Repository(t.Context(), name)
	must(t, err)
	must(t, rr.Push(t.Context(), sig, bytes.NewReader(m)))

	if resp := e.do(http.MethodDelete, target(fileTxt), nil); resp.Code != http.StatusNoContent {
		t.Fatalf("DELETE = %d %s", resp.Code, resp.Body)
	}
	if e.manifest(name, sig.Digest) != nil {
		t.Error("the signature is still in the registry")
	}
	evs := e.audit("delete")
	var details map[string]int
	if len(evs) != 1 || json.Unmarshal(evs[0].Details, &details) != nil || details["referrers"] != 1 {
		t.Errorf("delete audit rows %+v, want one with 1 referrer", evs)
	}
	if _, err := e.st.GetPackage(t.Context(), e.repo.ID, fileTxt); err == nil {
		t.Error("the package is still in the index")
	}
}
