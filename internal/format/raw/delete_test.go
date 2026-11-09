package raw

import (
	"encoding/json"
	"net/http"
	"testing"
)

// TestDeleteReferrers: DELETE of a path deletes the referrers of each version with it, as the
// DELETE of the manifest on /v2 does, and the audit row of each version counts them.
func TestDeleteReferrers(t *testing.T) {
	e := newEnv(t, envOptions{})
	v := e.put(fileTxt, "one")
	sig := e.sign(t, "raw/scratch/docs/file.txt", v.Digest)

	if resp := e.do(http.MethodDelete, target(fileTxt), nil); resp.Code != http.StatusNoContent {
		t.Fatalf("DELETE = %d %s", resp.Code, resp.Body)
	}
	if e.manifest("raw/scratch/docs/file.txt", sig) != nil {
		t.Error("the signature is still in the registry")
	}
	var details map[string]int
	if evs := e.audit("delete"); len(evs) != 1 || json.Unmarshal(evs[0].Details, &details) != nil || details["referrers"] != 1 {
		t.Errorf("delete audit rows %+v, want one with 1 referrer", evs)
	}
	if _, err := e.st.GetPackage(t.Context(), e.repo.ID, fileTxt); err == nil {
		t.Error("the package is still in the index")
	}
}
