package profile

import (
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"time"

	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

// The snapshots of the control plane (§9, #76).
const (
	// SnapshotRepository is the OCI repository of the snapshots below the instance prefix:
	// lajstrom is a reserved first segment (format.Reserved), so no repo's repositories are
	// below it.
	SnapshotRepository = "lajstrom/control-plane"
	// ArtifactTypeSnapshot is the artifactType of a snapshot's manifest.
	ArtifactTypeSnapshot = "application/vnd.lajstrom.snapshot.v1"
	// MediaTypeSnapshotDocument is the layer of an unsealed snapshot: the document, gzip'd.
	MediaTypeSnapshotDocument = "application/vnd.lajstrom.snapshot.control-plane.v1.json+gzip"
	// MediaTypeSnapshotSealed is the layer of a sealed snapshot: nonce ‖ AES-256-GCM of the
	// gzip'd document under the snapshot key of the master key, this media type as the
	// additional data.
	MediaTypeSnapshotSealed = "application/vnd.lajstrom.snapshot.control-plane.v1.sealed"

	// AnnotationSnapshotVersion is the version of the document, decimal.
	AnnotationSnapshotVersion = "lajstrom.snapshot.version"
	// AnnotationSnapshotSchema is the newest migration of the database it was read from, decimal.
	AnnotationSnapshotSchema = "lajstrom.snapshot.schema"
	// AnnotationSnapshotBinary is the version of the lajstrom that wrote it.
	AnnotationSnapshotBinary = "lajstrom.snapshot.binary"
)

// The titles of the snapshot's layer, by its media type.
var snapshotTitles = map[string]string{
	MediaTypeSnapshotDocument: "control-plane.json.gz",
	MediaTypeSnapshotSealed:   "control-plane.sealed",
}

// ErrSnapshot means a manifest is not a snapshot of §9.
var ErrSnapshot = fmt.Errorf("%w snapshot", ErrInvalid)

// snapshotTag is the grammar of a snapshot's tag: the time to the second, then the first 12 hex
// digits of the manifest digest.
var snapshotTag = regexp.MustCompile(`^[0-9]{8}T[0-9]{6}Z-[0-9a-f]{12}$`)

// snapshotTime is the layout of the time of a snapshot's tag.
const snapshotTime = "20060102T150405Z"

// Snapshot is what a snapshot's manifest says of it (§9).
type Snapshot struct {
	Version int       // lajstrom.snapshot.version
	Schema  int       // lajstrom.snapshot.schema
	Binary  string    // lajstrom.snapshot.binary
	Created time.Time // org.opencontainers.image.created, UTC, whole seconds
	Sealed  bool      // the layer is MediaTypeSnapshotSealed
	Layer   ocispec.Descriptor
}

// SnapshotTag returns the tag of a snapshot made at created whose manifest is d:
// 20261114T103000Z-3f2a9c0d1e4b.
func SnapshotTag(created time.Time, d digest.Digest) string {
	enc := d.Encoded()
	if len(enc) > 12 {
		enc = enc[:12]
	}
	return created.UTC().Format(snapshotTime) + "-" + enc
}

// IsSnapshotTag reports whether tag has the grammar of a snapshot's tag; the greatest such tag
// of the repository is its newest snapshot.
func IsSnapshotTag(tag string) bool { return snapshotTag.MatchString(tag) }

// BuildSnapshot returns the manifest of the snapshot s: its artifact type, the empty config, its
// layer titled by its media type, and the annotations. s is checked as ParseSnapshot checks a
// manifest; Created is cut to the second.
func BuildSnapshot(s Snapshot) (ocispec.Manifest, error) {
	layer := s.Layer
	layer.MediaType = MediaTypeSnapshotDocument
	if s.Sealed {
		layer.MediaType = MediaTypeSnapshotSealed
	}
	layer.Annotations = map[string]string{ocispec.AnnotationTitle: snapshotTitles[layer.MediaType]}
	ann := map[string]string{
		AnnotationSchema:          SchemaVersion,
		AnnotationSnapshotVersion: strconv.Itoa(s.Version),
		AnnotationSnapshotSchema:  strconv.Itoa(s.Schema),
		ocispec.AnnotationCreated: s.Created.UTC().Truncate(time.Second).Format(time.RFC3339),
	}
	if s.Binary != "" {
		ann[AnnotationSnapshotBinary] = s.Binary
	}
	var m ocispec.Manifest
	m.SchemaVersion = 2
	m.MediaType = ocispec.MediaTypeImageManifest
	m.ArtifactType = ArtifactTypeSnapshot
	m.Config = EmptyConfig
	m.Layers = []ocispec.Descriptor{layer}
	m.Annotations = ann
	if _, err := ParseSnapshot(m); err != nil {
		return ocispec.Manifest{}, err
	}
	return m, nil
}

// ParseSnapshot reads a snapshot from its manifest and checks the rules of §9 that the manifest
// alone can answer: an OCI image manifest without a subject, the snapshot's artifact type, the
// empty config, one layer of the two media types with a digest and a size, lajstrom.schema 1, a
// version and a schema from 1, and an RFC 3339 created. Whether the version is one the reader
// knows is the caller's to check. Errors wrap ErrSnapshot and are joined.
func ParseSnapshot(m ocispec.Manifest) (Snapshot, error) {
	var errs []error
	bad := func(f string, args ...any) {
		errs = append(errs, fmt.Errorf("%w: "+f, append([]any{ErrSnapshot}, args...)...))
	}
	if m.SchemaVersion != 2 || m.MediaType != ocispec.MediaTypeImageManifest {
		bad("not an OCI image manifest")
	}
	if m.ArtifactType != ArtifactTypeSnapshot {
		bad("artifactType is %q, want %s", m.ArtifactType, ArtifactTypeSnapshot)
	}
	if m.Subject != nil {
		bad("a snapshot has no subject")
	}
	if !sameDescriptor(m.Config, EmptyConfig) {
		bad("config is not the empty descriptor")
	}
	var s Snapshot
	if len(m.Layers) != 1 {
		bad("%d layers, want one", len(m.Layers))
	} else {
		s.Layer = m.Layers[0]
		if _, ok := snapshotTitles[s.Layer.MediaType]; !ok {
			bad("the layer's media type is %q, want %s or %s", s.Layer.MediaType, MediaTypeSnapshotDocument, MediaTypeSnapshotSealed)
		}
		if s.Layer.Digest.Validate() != nil || s.Layer.Size < 0 {
			bad("the layer's descriptor is malformed")
		}
		s.Sealed = s.Layer.MediaType == MediaTypeSnapshotSealed
	}
	a := m.Annotations
	if a[AnnotationSchema] != SchemaVersion {
		bad("%s is %q, want %q", AnnotationSchema, a[AnnotationSchema], SchemaVersion)
	}
	var err error
	if s.Version, err = positive(a[AnnotationSnapshotVersion]); err != nil {
		bad("%s: %v", AnnotationSnapshotVersion, err)
	}
	if s.Schema, err = positive(a[AnnotationSnapshotSchema]); err != nil {
		bad("%s: %v", AnnotationSnapshotSchema, err)
	}
	if s.Created, err = parseCreated(a[ocispec.AnnotationCreated]); err != nil {
		bad("%s: %v", ocispec.AnnotationCreated, err)
	}
	s.Binary = a[AnnotationSnapshotBinary]
	if err := errors.Join(errs...); err != nil {
		return Snapshot{}, err
	}
	return s, nil
}

// positive parses a decimal from 1 without leading zeros.
func positive(v string) (int, error) {
	n, err := strconv.Atoi(v)
	if err != nil || n < 1 || strconv.Itoa(n) != v {
		return 0, fmt.Errorf("%q is not a decimal from 1", v)
	}
	return n, nil
}
