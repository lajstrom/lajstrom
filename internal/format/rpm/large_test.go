package rpm

import (
	"strings"
	"testing"

	"github.com/lajstrom/lajstrom/internal/format"
	"github.com/lajstrom/lajstrom/internal/testutil"
)

// TestLargeMetadataNotIndexed builds and validates a package whose metadata layer is over 4 MiB,
// a long changelog: the manifest carries the layer, and the entry gives the index no metadata
// (INV-1), so the renderer reads the layer from the registry (fetchMetadata). The fake registry
// keeps no blob over 1 MiB, so the render itself is left to a real one.
func TestLargeMetadataNotIndexed(t *testing.T) {
	text := strings.Repeat("- a long changelog entry\n", 5<<20/25)
	big, _ := testutil.RPM(testutil.RPMOptions{Name: "big",
		Changelogs: []testutil.RPMChangelog{{Time: 1767225600, Author: "A <a@x> - 1.0.0-1.el9", Text: text}}})
	built, err := Plugin{}.Build(t.Context(), el9, format.Upload{Files: []format.UploadedFile{rpmFile(big)}})
	must(t, err)
	if md := built.Manifest.Layers[1]; md.Size <= maxIndexedMetadata || built.Entry.Metadata != nil {
		t.Fatalf("a metadata layer of %d bytes, %d of them in the entry", md.Size, len(built.Entry.Metadata))
	}
	entry, err := Plugin{}.Validate(t.Context(), el9, built.Manifest, fetcherOf(built, big))
	must(t, err)
	if entry.Metadata != nil {
		t.Errorf("Validate gives the index %d bytes of metadata", len(entry.Metadata))
	}
}
