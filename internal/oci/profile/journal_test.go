package profile

import (
	"encoding/json"
	"errors"
	"maps"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

func TestJournalTag(t *testing.T) {
	tests := []struct {
		tag  string
		seq  int64 // 0: not a journal tag
		back bool  // JournalTag(seq) == tag
	}{
		{"_j00000001", 1, true},
		{"_j00000007", 7, true},
		{"_j99999999", 99999999, true},
		{"_j100000000", 100000000, true},
		{"_j000000001", 0, false}, // nine digits for 1: not the tag of its number
		{"_j00000000", 0, false},
		{"_j0000001", 0, false},
		{"_j0000000a", 0, false},
		{"1.0.0", 0, false},
		{"_h0123456789", 0, false},
		{"_5fj00000001", 0, false},
	}
	for _, tt := range tests {
		t.Run(tt.tag, func(t *testing.T) {
			seq, ok := JournalSeq(tt.tag)
			if seq != tt.seq || ok != (tt.seq != 0) {
				t.Errorf("JournalSeq = %d, %v; want %d", seq, ok, tt.seq)
			}
			if tt.back && JournalTag(tt.seq) != tt.tag {
				t.Errorf("JournalTag(%d) = %s", tt.seq, JournalTag(tt.seq))
			}
		})
	}
	for _, v := range []string{"1.0.0", "2.0.0-rc.1", "_j", "j00000001", "1.0.0+build_1"} {
		if _, ok := JournalSeq(Escape(v)); ok {
			t.Errorf("the tag of version %q, %s, is a journal tag", v, Escape(v))
		}
	}
}

func TestJournalChangeCheck(t *testing.T) {
	long := strings.Repeat("x", MaxJournalMessage)
	tests := []struct {
		name string
		c    JournalChange
		ok   bool
	}{
		{"add", JournalChange{ArtifactTypeNpmDistTags, JournalAdd, "beta", "1.3.0-beta.2", ""}, true},
		{"rm", JournalChange{ArtifactTypeNpmDistTags, JournalRm, "beta", "", ""}, true},
		{"deprecate", JournalChange{ArtifactTypeNpmDeprecation, JournalDeprecate, "", "1.0.0", "use 2"}, true},
		{"deprecate with the longest message", JournalChange{ArtifactTypeNpmDeprecation, JournalDeprecate, "", "1.0.0", long}, true},
		{"undeprecate", JournalChange{ArtifactTypeNpmDeprecation, JournalUndeprecate, "", "1.0.0", ""}, true},
		{"add latest", JournalChange{ArtifactTypeNpmDistTags, JournalAdd, "latest", "1.0.0", ""}, true},
		{"rm latest", JournalChange{ArtifactTypeNpmDistTags, JournalRm, "latest", "", ""}, false},
		{"add without a version", JournalChange{ArtifactTypeNpmDistTags, JournalAdd, "beta", "", ""}, false},
		{"rm with a version", JournalChange{ArtifactTypeNpmDistTags, JournalRm, "beta", "1.0.0", ""}, false},
		{"add with a message", JournalChange{ArtifactTypeNpmDistTags, JournalAdd, "beta", "1.0.0", "m"}, false},
		{"a tag that begins with a digit", JournalChange{ArtifactTypeNpmDistTags, JournalAdd, "1beta", "1.0.0", ""}, false},
		{"a tag with a slash", JournalChange{ArtifactTypeNpmDistTags, JournalAdd, "a/b", "1.0.0", ""}, false},
		{"a tag of 129 characters", JournalChange{ArtifactTypeNpmDistTags, JournalAdd, "a" + strings.Repeat("b", 128), "1.0.0", ""}, false},
		{"deprecate without a message", JournalChange{ArtifactTypeNpmDeprecation, JournalDeprecate, "", "1.0.0", ""}, false},
		{"a message too long", JournalChange{ArtifactTypeNpmDeprecation, JournalDeprecate, "", "1.0.0", long + "x"}, false},
		{"a message that is not UTF-8", JournalChange{ArtifactTypeNpmDeprecation, JournalDeprecate, "", "1.0.0", "\xff"}, false},
		{"deprecate with a tag", JournalChange{ArtifactTypeNpmDeprecation, JournalDeprecate, "beta", "1.0.0", "m"}, false},
		{"an op of the other type", JournalChange{ArtifactTypeNpmDeprecation, JournalAdd, "beta", "1.0.0", ""}, false},
		{"another artifact type", JournalChange{ArtifactType("npm"), JournalAdd, "beta", "1.0.0", ""}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.c.Check()
			if (err == nil) != tt.ok || (err != nil && !errors.Is(err, ErrJournal)) {
				t.Errorf("Check = %v, want ok %v", err, tt.ok)
			}
		})
	}
}

// entry is the 7th entry of @acme/widgets in the repo internal, the example of §8.
func entry() JournalEntry {
	return JournalEntry{JournalChange: JournalChange{ArtifactTypeNpmDistTags, JournalAdd, "beta", "1.3.0-beta.2", ""},
		Format: "npm", Repo: "internal", Name: "@acme/widgets", Seq: 7,
		UploadedBy: "user:019a6b1e-5c3d-7f00-8a11-2b3c4d5e6f70", Created: time.Date(2026, 11, 10, 10, 0, 0, 0, time.UTC)}
}

// TestBuildJournal builds the example of manifest-profile.md §8 and reads it back.
func TestBuildJournal(t *testing.T) {
	m, err := BuildJournal(entry())
	if err != nil {
		t.Fatal(err)
	}
	b, _, err := Encode(m)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(b, &doc); err != nil {
		t.Fatal(err)
	}
	want := map[string]any{
		"schemaVersion": 2.0, "mediaType": ocispec.MediaTypeImageManifest, "artifactType": ArtifactTypeNpmDistTags,
		"config": map[string]any{"mediaType": ocispec.MediaTypeEmptyJSON, "digest": EmptyConfig.Digest.String(), "size": 2.0},
		"layers": []any{map[string]any{"mediaType": ocispec.MediaTypeEmptyJSON, "digest": EmptyConfig.Digest.String(), "size": 2.0}},
		"annotations": map[string]any{
			"lajstrom.schema": "1", "lajstrom.format": "npm", "lajstrom.repo": "internal", "lajstrom.name": "@acme/widgets",
			"lajstrom.npm.seq": "7", "lajstrom.npm.op": "add", "lajstrom.npm.tag": "beta", "lajstrom.npm.version": "1.3.0-beta.2",
			"lajstrom.uploaded-by":             "user:019a6b1e-5c3d-7f00-8a11-2b3c4d5e6f70",
			"org.opencontainers.image.created": "2026-11-10T10:00:00Z",
		},
	}
	if d := cmp.Diff(want, doc); d != "" {
		t.Errorf("the manifest (-want +got):\n%s", d)
	}
	got, err := ParseJournal(m)
	if err != nil {
		t.Fatal(err)
	}
	if d := cmp.Diff(entry(), got); d != "" {
		t.Errorf("ParseJournal (-want +got):\n%s", d)
	}
	if d := cmp.Diff(map[string]string{"lajstrom.npm.seq": "7", "lajstrom.npm.op": "add", "lajstrom.npm.tag": "beta",
		"lajstrom.npm.version": "1.3.0-beta.2"}, got.Attrs()); d != "" {
		t.Errorf("Attrs (-want +got):\n%s", d)
	}
	c, seq, err := JournalChangeOf(m.ArtifactType, got.Attrs())
	if err != nil || seq != 7 || c != entry().JournalChange {
		t.Errorf("JournalChangeOf = %+v, %d, %v; want %+v, 7", c, seq, err, entry().JournalChange)
	}
	bad := entry()
	bad.Op = JournalDeprecate
	if _, err := BuildJournal(bad); !errors.Is(err, ErrJournal) {
		t.Errorf("BuildJournal of a deprecate of the dist-tags type = %v", err)
	}
}

func TestParseJournal(t *testing.T) {
	good, err := BuildJournal(entry())
	if err != nil {
		t.Fatal(err)
	}
	other := ocispec.Descriptor{MediaType: "application/octet-stream", Digest: digest.FromString("x"), Size: 1}
	tests := []struct {
		name   string
		change func(m *ocispec.Manifest)
	}{
		{"an index", func(m *ocispec.Manifest) { m.MediaType = ocispec.MediaTypeImageIndex }},
		{"a subject", func(m *ocispec.Manifest) { m.Subject = &other }},
		{"a config", func(m *ocispec.Manifest) { m.Config = other }},
		{"no layer", func(m *ocispec.Manifest) { m.Layers = nil }},
		{"two layers", func(m *ocispec.Manifest) { m.Layers = append(m.Layers, EmptyConfig) }},
		{"a file layer", func(m *ocispec.Manifest) { m.Layers = []ocispec.Descriptor{other} }},
		{"a version's artifact type", func(m *ocispec.Manifest) { m.ArtifactType = ArtifactType("npm") }},
		{"schema 2", func(m *ocispec.Manifest) { m.Annotations[AnnotationSchema] = "2" }},
		{"format raw", func(m *ocispec.Manifest) { m.Annotations[AnnotationFormat] = "raw" }},
		{"no repo", func(m *ocispec.Manifest) { delete(m.Annotations, AnnotationRepo) }},
		{"no name", func(m *ocispec.Manifest) { delete(m.Annotations, AnnotationName) }},
		{"no seq", func(m *ocispec.Manifest) { delete(m.Annotations, AnnotationNpmSeq) }},
		{"seq 0", func(m *ocispec.Manifest) { m.Annotations[AnnotationNpmSeq] = "0" }},
		{"seq 07", func(m *ocispec.Manifest) { m.Annotations[AnnotationNpmSeq] = "07" }},
		{"no op", func(m *ocispec.Manifest) { delete(m.Annotations, AnnotationNpmOp) }},
		{"no tag", func(m *ocispec.Manifest) { delete(m.Annotations, AnnotationNpmTag) }},
		{"a message on an add", func(m *ocispec.Manifest) { m.Annotations[AnnotationNpmMessage] = "m" }},
		{"no created", func(m *ocispec.Manifest) { delete(m.Annotations, ocispec.AnnotationCreated) }},
		{"a version annotation", func(m *ocispec.Manifest) { m.Annotations[AnnotationVersion] = "1.3.0-beta.2" }},
		{"a variant annotation", func(m *ocispec.Manifest) { m.Annotations[AnnotationVariant] = "x" }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := good
			m.Annotations = maps.Clone(good.Annotations)
			m.Layers = append([]ocispec.Descriptor(nil), good.Layers...)
			tt.change(&m)
			if _, err := ParseJournal(m); !errors.Is(err, ErrJournal) {
				t.Errorf("ParseJournal = %v, want ErrJournal", err)
			}
		})
	}
}

// TestJournalState folds a journal: add sets a dist-tag, rm removes it, deprecate sets a
// version's message, undeprecate clears it; each key keeps the seq that set it.
func TestJournalState(t *testing.T) {
	changes := []JournalChange{
		{ArtifactTypeNpmDistTags, JournalAdd, "latest", "1.0.0", ""},
		{ArtifactTypeNpmDistTags, JournalAdd, "beta", "1.1.0-beta.1", ""},
		{ArtifactTypeNpmDeprecation, JournalDeprecate, "", "1.0.0", "use 1.1"},
		{ArtifactTypeNpmDistTags, JournalAdd, "latest", "1.1.0", ""},
		{ArtifactTypeNpmDistTags, JournalRm, "beta", "", ""},
		{ArtifactTypeNpmDeprecation, JournalDeprecate, "", "0.9.0", "old"},
		{ArtifactTypeNpmDeprecation, JournalUndeprecate, "", "0.9.0", ""},
		{ArtifactTypeNpmDistTags, JournalRm, "next", "", ""}, // a tag the package does not have
	}
	s := JournalState{}
	for i, c := range changes {
		s.Apply(int64(i+1), c)
	}
	want := JournalState{
		{JournalKindDeprecated, "1.0.0"}: {"use 1.1", 3},
		{JournalKindDistTag, "latest"}:   {"1.1.0", 4},
	}
	if d := cmp.Diff(want, s); d != "" {
		t.Errorf("the fold (-want +got):\n%s", d)
	}
	if d := cmp.Diff([]JournalKey{{JournalKindDeprecated, "1.0.0"}, {JournalKindDistTag, "latest"}}, s.Keys()); d != "" {
		t.Errorf("Keys (-want +got):\n%s", d)
	}
}
