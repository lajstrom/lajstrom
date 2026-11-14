package storetest

import (
	"slices"
	"strings"
	"testing"

	"github.com/opencontainers/go-digest"

	"github.com/lajstrom/lajstrom/internal/index"
)

// attachment is a row of the referrer of digestOf, its layer made of layerOf, created at
// created seconds (none when negative) and indexed at indexed.
func attachment(digestOf, layerOf string, created, indexed int) index.Attachment {
	a := index.Attachment{Digest: dg(digestOf), Layer: dg(layerOf), MediaType: "application/vnd.dev.sigstore.bundle.v0.3+json",
		ArtifactType: "application/vnd.dev.sigstore.bundle.v0.3+json", Kind: index.AttachmentSBOM,
		PredicateType: "https://cyclonedx.org/bom", Size: 859,
		Annotations: map[string]string{"dev.sigstore.bundle.content": "dsse-envelope", "lajstrom.attachment.tool": "trivy"},
		Tool:        "trivy", ToolVersion: "0.60.0", Database: "2026-11-14", Input: dg("an sbom"),
		Verification: index.AttachmentVerified, SignedBy: "scanner", Trust: "t1", IndexedAt: at(indexed)}
	if created >= 0 {
		a.CreatedAt = ptr(at(created))
	}
	return a
}

// testAttachments: the rows of a referrer are replaced whole and listed newest first, those
// without a time last; a legacy tag takes the rows of the referrer it named before; touch, sweep
// and the delete of a referrer change only their rows; a missing version writes nothing.
func testAttachments(t *testing.T, s index.Store) {
	ctx := t.Context()
	repo := newRepo(t, s, "raw", "scratch")
	pkg := newPackage(t, s, repo.ID, "tool")
	v1, v2 := newVersion(t, s, pkg.ID, "1.0.0"), newVersion(t, s, pkg.ID, "2.0.0")
	otherRepo := newRepo(t, s, "raw", "other")
	otherVersion := newVersion(t, s, newPackage(t, s, otherRepo.ID, "tool").ID, "1.0.0")

	two := []index.Attachment{attachment("sbom", "layer a", 10, 100), attachment("sbom", "layer b", 10, 100)}
	two[1].SignedBy, two[1].Verification, two[1].Reason, two[1].Annotations = "", index.AttachmentUntrusted, "no trusted key", nil
	must(t, s.ReplaceAttachments(ctx, v1.ID, dg("sbom"), two))
	for _, a := range two {
		if a.ID == 0 || a.VersionID != v1.ID || a.Digest != dg("sbom") {
			t.Fatalf("ReplaceAttachments did not fill the row: %+v", a)
		}
	}
	newer := []index.Attachment{attachment("scan", "layer c", 20, 100)}
	timeless := []index.Attachment{attachment("note", "", -1, 100)}
	timeless[0].Layer = ""
	must(t, s.ReplaceAttachments(ctx, v1.ID, dg("scan"), newer))
	must(t, s.ReplaceAttachments(ctx, v1.ID, dg("note"), timeless))
	other := []index.Attachment{attachment("sbom", "layer a", 10, 100)}
	must(t, s.ReplaceAttachments(ctx, otherVersion.ID, dg("sbom"), other))
	onV2 := []index.Attachment{attachment("v2", "layer d", 5, 100)}
	must(t, s.ReplaceAttachments(ctx, v2.ID, dg("v2"), onV2))

	got, err := s.ListAttachments(ctx, []int64{v1.ID})
	must(t, err)
	byLayer := slices.Clone(two)
	slices.SortFunc(byLayer, func(a, b index.Attachment) int { return strings.Compare(string(a.Layer), string(b.Layer)) })
	same(t, "the rows of v1, newest first, then by digest and layer, the one without a time last",
		[]index.Attachment{newer[0], byLayer[0], byLayer[1], timeless[0]}, got)
	got, err = s.ListAttachments(ctx, []int64{v2.ID, v1.ID})
	must(t, err)
	if len(got) != 5 || got[0].Digest != dg("scan") || got[3].Digest != dg("v2") {
		t.Errorf("the rows of two versions: %d, %v", len(got), ids(got, func(a index.Attachment) int64 { return a.VersionID }))
	}
	got, err = s.ListAttachments(ctx, nil)
	must(t, err)
	same(t, "no versions", []index.Attachment{}, got)

	// The same referrer again replaces its rows.
	one := []index.Attachment{attachment("sbom", "layer a", 10, 200)}
	must(t, s.ReplaceAttachments(ctx, v1.ID, dg("sbom"), one))
	got, err = s.ListAttachments(ctx, []int64{v1.ID})
	must(t, err)
	same(t, "after the referrer's rows were replaced", []index.Attachment{newer[0], one[0], timeless[0]}, got)

	// cosign moved its legacy tag: the rows of the manifest it named before go.
	tag := "sha256-" + v1.Digest.Encoded() + ".att"
	first := []index.Attachment{attachment("att 1", "layer e", 30, 100)}
	first[0].Tag = tag
	must(t, s.ReplaceAttachments(ctx, v1.ID, dg("att 1"), first))
	second := []index.Attachment{attachment("att 2", "layer e", 30, 100), attachment("att 2", "layer f", 30, 100)}
	second[0].Tag, second[1].Tag = tag, tag
	must(t, s.ReplaceAttachments(ctx, v1.ID, dg("att 2"), second))
	got, err = s.ListAttachments(ctx, []int64{v1.ID})
	must(t, err)
	if len(got) != 5 || got[0].Digest != dg("att 2") || got[1].Digest != dg("att 2") {
		t.Errorf("after the legacy tag moved: %v", digests(got))
	}

	// Touch and sweep: the rows not seen since a time go, those of other versions stay.
	must(t, s.TouchAttachments(ctx, []int64{newer[0].ID, 1 << 40}, at(300)))
	n, err := s.SweepAttachments(ctx, v1.ID, at(250))
	must(t, err)
	got, err = s.ListAttachments(ctx, []int64{v1.ID, v2.ID})
	must(t, err)
	newer[0].IndexedAt = at(300)
	if n != 4 {
		t.Errorf("SweepAttachments removed %d rows, want 4", n)
	}
	same(t, "after the sweep", []index.Attachment{newer[0], onV2[0]}, got)

	// The delete of a referrer: its rows in the repo, not another repo's of the same digest.
	must(t, s.ReplaceAttachments(ctx, v2.ID, dg("sbom"), []index.Attachment{attachment("sbom", "layer a", 10, 100)}))
	n, err = s.DeleteAttachments(ctx, repo.ID, dg("sbom"))
	must(t, err)
	if n != 1 {
		t.Errorf("DeleteAttachments removed %d rows, want 1", n)
	}
	got, err = s.ListAttachments(ctx, []int64{otherVersion.ID})
	must(t, err)
	same(t, "another repo's rows of the digest", other, got)
	n, err = s.DeleteAttachments(ctx, repo.ID, dg("gone"))
	must(t, err)
	if n != 0 {
		t.Errorf("DeleteAttachments of a digest without rows removed %d", n)
	}

	wantErr(t, "the rows of a missing version", s.ReplaceAttachments(ctx, 1<<40, dg("x"), []index.Attachment{attachment("x", "y", 1, 1)}),
		index.ErrNotFound)

	// A version's rows go with it, and ResetDerived takes the rest.
	must(t, s.DeleteVersion(ctx, v2.ID))
	got, err = s.ListAttachments(ctx, []int64{v2.ID})
	must(t, err)
	same(t, "the rows of a deleted version", []index.Attachment{}, got)
	must(t, s.ResetDerived(ctx, 0))
	got, err = s.ListAttachments(ctx, []int64{v1.ID, otherVersion.ID})
	must(t, err)
	same(t, "the rows after ResetDerived", []index.Attachment{}, got)
}

// digests returns the digests of rows, for a message.
func digests(rows []index.Attachment) []digest.Digest {
	out := []digest.Digest{}
	for _, a := range rows {
		out = append(out, a.Digest)
	}
	return out
}
