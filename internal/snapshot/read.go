package snapshot

import (
	"context"
	"errors"
	"fmt"

	"github.com/opencontainers/go-digest"

	"github.com/lajstrom/lajstrom/internal/index"
	"github.com/lajstrom/lajstrom/internal/oci/profile"
	"github.com/lajstrom/lajstrom/internal/provider"
)

// Latest is the reference of the newest snapshot of a registry: the greatest of its tags.
const Latest = "latest"

// ErrNotFound means the registry has no snapshot of the reference.
var ErrNotFound = errors.New("snapshot: no such snapshot")

// maxLayer bounds the layer of a snapshot as the registry has it, sealed or not: the document
// unpacked is bounded by maxDocument, and gzip does not make it larger.
const maxLayer = maxDocument

// Read reads the snapshot ref: one of its tags, the digest of its manifest (sha256:…), or Latest,
// the newest. It checks the manifest (docs/spec/manifest-profile.md §9, Reading), reads the layer
// and checks its digest and size, opens a sealed layer with Master, and decodes the document. A
// reference the registry does not have wraps ErrNotFound; a sealed snapshot without Master
// ErrNoMasterKey; a document of a version this binary does not read ErrVersion. The Info of a
// snapshot read by its digest has no tag.
func (r Registry) Read(ctx context.Context, ref string) (*index.ControlPlane, Info, error) {
	tag := ref
	if ref == Latest {
		tags, err := r.tags(ctx)
		if err != nil {
			return nil, Info{}, err
		}
		if len(tags) == 0 {
			return nil, Info{}, fmt.Errorf("%w: %s has none", ErrNotFound, profile.SnapshotRepository)
		}
		tag = tags[0]
	}
	key := tag
	if d, err := digest.Parse(ref); err == nil {
		key, tag = d.String(), ""
	} else if !profile.IsSnapshotTag(tag) {
		return nil, Info{}, fmt.Errorf("%w: %q is neither a snapshot's tag (20261114T103000Z-3f2a9c0d1e4b), a digest, nor %s",
			ErrNotFound, ref, Latest)
	}
	rr, err := r.repository(ctx, false)
	if err != nil {
		return nil, Info{}, err
	}
	desc, err := rr.Resolve(ctx, key)
	if errors.Is(err, provider.ErrNotFound) {
		return nil, Info{}, fmt.Errorf("%w: %s@%s", ErrNotFound, profile.SnapshotRepository, ref)
	}
	if err != nil {
		return nil, Info{}, fmt.Errorf("snapshot: resolve %s@%s: %w", profile.SnapshotRepository, ref, err)
	}
	_, s, err := r.manifest(ctx, rr, desc)
	if err != nil {
		return nil, Info{}, fmt.Errorf("snapshot: %s@%s: %w", profile.SnapshotRepository, ref, err)
	}
	info := infoOf(tag, desc.Digest, s)
	if s.Version > Version {
		return nil, info, fmt.Errorf("%w: %s is of version %d, this binary reads 1 to %d", ErrVersion, ref, s.Version, Version)
	}
	cp, err := r.open(ctx, rr, s)
	if err != nil {
		return nil, info, fmt.Errorf("%s: %w", ref, err)
	}
	if cp.Schema != s.Schema {
		return nil, info, fmt.Errorf("snapshot: %s: its document is of migration %d, its manifest says %d", ref, cp.Schema, s.Schema)
	}
	return cp, info, nil
}

// open reads the layer of the snapshot s, opens it when it is sealed, and decodes it.
func (r Registry) open(ctx context.Context, rr provider.Repository, s profile.Snapshot) (*index.ControlPlane, error) {
	if s.Sealed && r.Master == nil {
		return nil, ErrNoMasterKey
	}
	if s.Layer.Size > maxLayer {
		return nil, fmt.Errorf("snapshot: the layer is %d bytes, more than %d", s.Layer.Size, maxLayer)
	}
	layer, err := fetch(ctx, rr, s.Layer)
	if err != nil {
		return nil, fmt.Errorf("snapshot: %w", err)
	}
	if s.Sealed {
		if layer, err = r.Master.OpenSnapshot(layer, []byte(profile.MediaTypeSnapshotSealed)); err != nil {
			return nil, err
		}
	}
	cp, _, err := decode(layer)
	return cp, err
}
