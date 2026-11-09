package raw

import (
	"bytes"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/opencontainers/go-digest"
	"github.com/opencontainers/image-spec/specs-go"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

// TestDeleteReferrers: DELETE of a path deletes the referrers of each version with it, as the
// DELETE of the manifest on /v2 does, and the audit row of each version counts them.
func TestDeleteReferrers(t *testing.T) {
	e := newEnv(t, envOptions{})
	v := e.put(fileTxt, "one")
	name := "raw/scratch/docs/file.txt"
	subject := ocispec.Descriptor{MediaType: ocispec.MediaTypeImageManifest, Digest: digest.Digest(v.Digest), Size: 1}
	m, err := json.Marshal(ocispec.Manifest{Versioned: specs.Versioned{SchemaVersion: 2}, MediaType: ocispec.MediaTypeImageManifest,
		ArtifactType: "application/vnd.dev.sigstore.bundle.v0.3+json", Config: ocispec.DescriptorEmptyJSON, Layers: []ocispec.Descriptor{},
		Subject: &subject})
	must(t, err)
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
