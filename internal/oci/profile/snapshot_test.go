package profile

import (
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

// TestSnapshotTag: a snapshot's tag is its time to the second and the first 12 hex digits of its
// manifest's digest; the tags of later snapshots are greater.
func TestSnapshotTag(t *testing.T) {
	d := digest.FromString("manifest")
	at := time.Date(2026, 11, 14, 10, 30, 0, 999, time.FixedZone("CET", 3600))
	tag := SnapshotTag(at, d)
	if want := "20261114T093000Z-" + d.Encoded()[:12]; tag != want {
		t.Errorf("SnapshotTag = %q, want %q", tag, want)
	}
	if later := SnapshotTag(at.Add(time.Second), digest.FromString("other")); later <= tag {
		t.Errorf("the tag of a later snapshot %q does not sort after %q", later, tag)
	}
	for in, want := range map[string]bool{
		tag: true, "20261114T093000Z-ABCDEF012345": false, "20261114T093000Z-0123456789a": false, "latest": false,
		"20261114T0930Z-0123456789ab": false, "x20261114T093000Z-0123456789ab": false, "": false,
	} {
		if got := IsSnapshotTag(in); got != want {
			t.Errorf("IsSnapshotTag(%q) = %v, want %v", in, got, want)
		}
	}
}

// TestBuildSnapshot: the manifest of a snapshot of §9, sealed or not, parses back to what it was
// built from.
func TestBuildSnapshot(t *testing.T) {
	layer := ocispec.Descriptor{Digest: digest.FromString("layer"), Size: 5}
	for _, tt := range []struct {
		sealed           bool
		mediaType, title string
	}{
		{false, MediaTypeSnapshotDocument, "control-plane.json.gz"},
		{true, MediaTypeSnapshotSealed, "control-plane.sealed"},
	} {
		s := Snapshot{Version: 1, Schema: 21, Binary: "v0.2.0", Created: time.Date(2026, 11, 14, 10, 30, 0, 5e8, time.UTC),
			Sealed: tt.sealed, Layer: layer}
		m, err := BuildSnapshot(s)
		if err != nil {
			t.Fatal(err)
		}
		want := layer
		want.MediaType, want.Annotations = tt.mediaType, map[string]string{ocispec.AnnotationTitle: tt.title}
		if m.ArtifactType != ArtifactTypeSnapshot || !sameDescriptor(m.Config, EmptyConfig) || len(m.Layers) != 1 ||
			!reflect.DeepEqual(m.Layers[0], want) {
			t.Errorf("sealed %v: the manifest %+v", tt.sealed, m)
		}
		wantAnn := map[string]string{AnnotationSchema: "1", AnnotationSnapshotVersion: "1", AnnotationSnapshotSchema: "21",
			AnnotationSnapshotBinary: "v0.2.0", ocispec.AnnotationCreated: "2026-11-14T10:30:00Z"}
		if !reflect.DeepEqual(m.Annotations, wantAnn) {
			t.Errorf("sealed %v: the annotations %v, want %v", tt.sealed, m.Annotations, wantAnn)
		}
		back, err := ParseSnapshot(m)
		if err != nil {
			t.Fatal(err)
		}
		s.Created, s.Layer = s.Created.Truncate(time.Second), want
		if !reflect.DeepEqual(back, s) {
			t.Errorf("sealed %v: ParseSnapshot = %+v, want %+v", tt.sealed, back, s)
		}
	}
}

// TestParseSnapshotRefuses: a manifest that breaks a rule of §9 is ErrSnapshot, naming the rule.
func TestParseSnapshotRefuses(t *testing.T) {
	good, err := BuildSnapshot(Snapshot{Version: 1, Schema: 21, Created: time.Unix(0, 0), Layer: ocispec.Descriptor{
		Digest: digest.FromString("layer"), Size: 5}})
	if err != nil {
		t.Fatal(err)
	}
	for _, tt := range []struct {
		name   string
		change func(m *ocispec.Manifest)
		want   string
	}{
		{"an index", func(m *ocispec.Manifest) { m.MediaType = ocispec.MediaTypeImageIndex }, "not an OCI image manifest"},
		{"another artifact type", func(m *ocispec.Manifest) { m.ArtifactType = "application/vnd.lajstrom.raw.v1" }, "artifactType"},
		{"a subject", func(m *ocispec.Manifest) { m.Subject = &good.Layers[0] }, "no subject"},
		{"another config", func(m *ocispec.Manifest) { m.Config = good.Layers[0] }, "config"},
		{"two layers", func(m *ocispec.Manifest) { m.Layers = append(m.Layers, m.Layers[0]) }, "2 layers"},
		{"a layer of another type", func(m *ocispec.Manifest) { m.Layers[0].MediaType = "application/json" }, "media type"},
		{"a malformed layer", func(m *ocispec.Manifest) { m.Layers[0].Digest = "sha256:x" }, "malformed"},
		{"no lajstrom.schema", func(m *ocispec.Manifest) { delete(m.Annotations, AnnotationSchema) }, AnnotationSchema},
		{"version 0", func(m *ocispec.Manifest) { m.Annotations[AnnotationSnapshotVersion] = "0" }, AnnotationSnapshotVersion},
		{"schema 021", func(m *ocispec.Manifest) { m.Annotations[AnnotationSnapshotSchema] = "021" }, AnnotationSnapshotSchema},
		{"no created", func(m *ocispec.Manifest) { delete(m.Annotations, ocispec.AnnotationCreated) }, ocispec.AnnotationCreated},
	} {
		m := good
		m.Layers = append([]ocispec.Descriptor(nil), good.Layers...)
		m.Annotations = map[string]string{}
		for k, v := range good.Annotations {
			m.Annotations[k] = v
		}
		tt.change(&m)
		_, err := ParseSnapshot(m)
		if !errors.Is(err, ErrSnapshot) || !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), tt.want) {
			t.Errorf("%s: %v, want ErrSnapshot naming %q", tt.name, err, tt.want)
		}
	}
}
