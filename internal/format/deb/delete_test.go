package deb

import (
	"net/http"
	"strings"
	"testing"

	"github.com/lajstrom/lajstrom/internal/format"
)

// TestDeleteInvalidatesIndexes is #61's cache criterion: once a GET has rendered and cached the
// suite, a version deleted through the write path is gone from the next Packages and Release,
// which are rendered again without it, and its pool file is 404; the other packages stay.
func TestDeleteInvalidatesIndexes(t *testing.T) {
	e := newAptEnv(t, envOptions{})
	fixtureSet(t, e.pipeline)
	const packages = "dists/jammy/main/binary-arm64/Packages"
	if before := e.get(packages).Body.String(); !strings.Contains(before, "Package: bonjour\n") {
		t.Fatalf("the cached Packages has no bonjour:\n%s", before)
	}
	release := e.get("dists/jammy/Release").Body.String()
	pkg, err := e.st.GetPackage(t.Context(), e.repo.ID, "bonjour")
	must(t, err)
	versions, err := e.st.ListVersions(t.Context(), pkg.ID)
	must(t, err)
	if len(versions) != 1 {
		t.Fatalf("bonjour has %d versions, want 1", len(versions))
	}
	_, err = e.pipe.Delete(t.Context(), e.repo, format.Deletion{Path: pkg.NameOCI, Reference: versions[0].Digest.String()}, alice)
	must(t, err)

	after := e.get(packages).Body.String()
	if strings.Contains(after, "bonjour") || !strings.Contains(after, "Package: hello\n") {
		t.Errorf("Packages after the delete:\n%s", after)
	}
	if got := e.get("dists/jammy/Release").Body.String(); got == release {
		t.Error("the Release was not rendered again after the delete")
	}
	if rr := e.do(http.MethodGet, "pool/main/b/bonjour/bonjour_0.1-1_arm64.deb"); rr.Code != http.StatusNotFound {
		t.Errorf("GET of the deleted pool file = %d, want 404", rr.Code)
	}
}
