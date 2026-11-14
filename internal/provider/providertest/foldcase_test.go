package providertest

import (
	"bytes"
	"slices"
	"testing"

	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

// TestFoldCase checks that the tag of a push is listed lower-cased, resolves in any case, and
// is re-pointed by a push under another case of it, as on Gitea, and that its capabilities say so.
func TestFoldCase(t *testing.T) {
	ctx := t.Context()
	p := FoldCase(&Fake{})
	if caps, err := p.Capabilities(ctx); err != nil || !caps.TagsFoldCase {
		t.Errorf("Capabilities = %+v, %v; want TagsFoldCase", caps, err)
	}
	rr, err := p.Repository(ctx, name)
	if err != nil {
		t.Fatal(err)
	}
	a := []byte(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json","annotations":{"n":"a"}}`)
	b := []byte(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json","annotations":{"n":"b"}}`)
	da, db := descriptor(ocispec.MediaTypeImageManifest, a), descriptor(ocispec.MediaTypeImageManifest, b)
	if err := rr.PushReference(ctx, da, bytes.NewReader(a), "5.4.Final"); err != nil {
		t.Fatal(err)
	}
	var tags []string
	if err := rr.Tags(ctx, "", func(page []string) error { tags = append(tags, page...); return nil }); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(tags, []string{"5.4.final"}) {
		t.Errorf("tags %q, want [5.4.final]", tags)
	}
	for _, ref := range []string{"5.4.Final", "5.4.final", "5.4.FINAL", da.Digest.String()} {
		if got, err := rr.Resolve(ctx, ref); err != nil || got.Digest != da.Digest {
			t.Errorf("Resolve(%s) = %s, %v; want %s", ref, got.Digest, err, da.Digest)
		}
	}
	if err := rr.PushReference(ctx, db, bytes.NewReader(b), "5.4.final"); err != nil {
		t.Fatal(err)
	}
	if got, err := rr.Resolve(ctx, "5.4.Final"); err != nil || got.Digest != db.Digest {
		t.Errorf("Resolve(5.4.Final) after a push to 5.4.final = %s, %v; want %s", got.Digest, err, db.Digest)
	}
}
