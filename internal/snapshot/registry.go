package snapshot

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"time"

	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/lajstrom/lajstrom/internal/index"
	"github.com/lajstrom/lajstrom/internal/keys"
	"github.com/lajstrom/lajstrom/internal/oci/profile"
	"github.com/lajstrom/lajstrom/internal/provider"
)

// ErrConflict means the tag of a snapshot names another manifest in the registry: no tag is
// ever moved (docs/spec/manifest-profile.md §9, Tags).
var ErrConflict = errors.New("snapshot: the tag names another manifest")

// ErrNoMasterKey means a sealed snapshot was asked for, to write or to read, without the
// master key.
var ErrNoMasterKey = errors.New("snapshot: a sealed snapshot needs the master key")

// DefaultReadBack bounds the read-back of a written snapshot's tag on a registry that serves a
// write late (Capabilities.Consistency).
const DefaultReadBack = time.Minute

// Registry is the snapshots of one registry: the repository profile.SnapshotRepository below its
// instance prefix, written and read through its provider.
type Registry struct {
	// Provider is the registry's.
	Provider provider.Provider
	// Master seals the snapshots Write seals and opens the sealed ones Read reads; nil writes
	// unsealed snapshots only, and opens none.
	Master *keys.Master
	// ReadBack bounds the read-back of a written tag; zero is DefaultReadBack.
	ReadBack time.Duration
}

// Info is what a snapshot's manifest says of it, with its tag and digest.
type Info struct {
	Tag    string        `json:"tag"`
	Digest digest.Digest `json:"digest"`
	// Created is the time of the read of the control plane, to the second.
	Created time.Time `json:"created"`
	Version int       `json:"version"`
	Schema  int       `json:"schema"`
	Binary  string    `json:"binary,omitempty"`
	Sealed  bool      `json:"sealed"`
	// Size is the size of the layer, sealed or not.
	Size int64 `json:"size"`
}

// infoOf is the Info of the snapshot s whose manifest is d under tag.
func infoOf(tag string, d digest.Digest, s profile.Snapshot) Info {
	return Info{Tag: tag, Digest: d, Created: s.Created, Version: s.Version, Schema: s.Schema, Binary: s.Binary, Sealed: s.Sealed,
		Size: s.Layer.Size}
}

// Write writes cp as a snapshot, sealed with Master when sealed, written by binary: the empty
// blob and the layer, then the manifest under its tag, which it reads back before it returns.
// A tag that names another manifest wraps ErrConflict and nothing is pushed; the same manifest
// under it is written again as it is. Sealed without Master wraps ErrNoMasterKey.
func (r Registry) Write(ctx context.Context, cp *index.ControlPlane, sealed bool, binary string) (Info, error) {
	b, err := r.build(cp, sealed, binary)
	if err != nil {
		return Info{}, err
	}
	rr, err := r.repository(ctx, true)
	if err != nil {
		return Info{}, err
	}
	if err := b.push(ctx, rr); err != nil {
		return Info{}, err
	}
	if err := r.readBack(ctx, rr, b.info); err != nil {
		return Info{}, err
	}
	return b.info, nil
}

// built is a snapshot ready to push: its layer, its manifest and what they say of it.
type built struct {
	layer    []byte
	manifest ocispec.Manifest
	raw      []byte // the manifest's bytes
	info     Info
}

// build encodes cp, seals it when sealed, and builds the manifest and the tag.
func (r Registry) build(cp *index.ControlPlane, sealed bool, binary string) (*built, error) {
	if sealed && r.Master == nil {
		return nil, ErrNoMasterKey
	}
	layer, err := encode(cp, binary)
	if err != nil {
		return nil, err
	}
	if sealed {
		layer = r.Master.SealSnapshot(layer, []byte(profile.MediaTypeSnapshotSealed))
	}
	s := profile.Snapshot{Version: Version, Schema: cp.Schema, Binary: binary, Created: cp.ReadAt, Sealed: sealed,
		Layer: ocispec.Descriptor{Digest: digest.FromBytes(layer), Size: int64(len(layer))}}
	m, err := profile.BuildSnapshot(s)
	if err != nil {
		return nil, fmt.Errorf("snapshot: %w", err)
	}
	raw, d, err := profile.Encode(m)
	if err != nil {
		return nil, err
	}
	s.Layer, s.Created = m.Layers[0], s.Created.UTC().Truncate(time.Second)
	return &built{layer: layer, manifest: m, raw: raw, info: infoOf(profile.SnapshotTag(s.Created, d), d, s)}, nil
}

// push pushes the snapshot into rr: the empty blob and the layer where rr lacks them, then the
// manifest under its tag, unless the tag names another manifest (ErrConflict).
func (b *built) push(ctx context.Context, rr provider.Repository) error {
	switch desc, err := rr.Resolve(ctx, b.info.Tag); {
	case err == nil && desc.Digest != b.info.Digest:
		return fmt.Errorf("%w: %s:%s is %s, not %s", ErrConflict, profile.SnapshotRepository, b.info.Tag, desc.Digest, b.info.Digest)
	case err != nil && !errors.Is(err, provider.ErrNotFound):
		return fmt.Errorf("snapshot: resolve %s:%s: %w", profile.SnapshotRepository, b.info.Tag, err)
	}
	blobs := []struct {
		desc ocispec.Descriptor
		data []byte
	}{{profile.EmptyConfig, ocispec.DescriptorEmptyJSON.Data}, {b.manifest.Layers[0], b.layer}}
	for _, blob := range blobs {
		ok, err := rr.Exists(ctx, blob.desc)
		if err == nil && !ok {
			err = rr.Push(ctx, blob.desc, bytes.NewReader(blob.data))
		}
		if err != nil {
			return fmt.Errorf("snapshot: push %s to %s: %w", blob.desc.Digest, profile.SnapshotRepository, err)
		}
	}
	desc := ocispec.Descriptor{MediaType: ocispec.MediaTypeImageManifest, Digest: b.info.Digest, Size: int64(len(b.raw))}
	if err := rr.PushReference(ctx, desc, bytes.NewReader(b.raw), b.info.Tag); err != nil {
		return fmt.Errorf("snapshot: push %s:%s: %w", profile.SnapshotRepository, b.info.Tag, err)
	}
	return nil
}

// readBack waits until rr serves the tag of info with its manifest, as a registry that serves a
// write late does after a while, for at most ReadBack.
func (r Registry) readBack(ctx context.Context, rr provider.Repository, info Info) error {
	timeout := cmp.Or(r.ReadBack, DefaultReadBack)
	seen, err := provider.ReadBack{Timeout: timeout}.Await(ctx, func(ctx context.Context) (bool, error) {
		desc, err := rr.Resolve(ctx, info.Tag)
		if errors.Is(err, provider.ErrNotFound) {
			return false, nil
		}
		return err == nil && desc.Digest == info.Digest, err
	})
	switch {
	case err != nil:
		return fmt.Errorf("snapshot: read back %s:%s: %w", profile.SnapshotRepository, info.Tag, err)
	case !seen:
		return fmt.Errorf("snapshot: the registry took %s:%s but does not serve it after %s", profile.SnapshotRepository, info.Tag,
			timeout)
	}
	return nil
}

// List returns the snapshots of the registry, the newest first: the tags of the repository that
// have the grammar of a snapshot's, each with what its manifest says. A registry without the
// repository has none. A tag whose manifest is no snapshot is an error.
func (r Registry) List(ctx context.Context) ([]Info, error) {
	tags, err := r.tags(ctx)
	if err != nil || len(tags) == 0 {
		return nil, err
	}
	rr, err := r.repository(ctx, false)
	if err != nil {
		return nil, err
	}
	out := make([]Info, 0, len(tags))
	for _, tag := range tags {
		desc, err := rr.Resolve(ctx, tag)
		if err != nil {
			return nil, fmt.Errorf("snapshot: resolve %s:%s: %w", profile.SnapshotRepository, tag, err)
		}
		_, s, err := r.manifest(ctx, rr, desc)
		if err != nil {
			return nil, fmt.Errorf("snapshot: %s:%s: %w", profile.SnapshotRepository, tag, err)
		}
		out = append(out, infoOf(tag, desc.Digest, s))
	}
	return out, nil
}

// tags returns the snapshot tags of the repository, the greatest, the newest, first; none when
// the registry does not have it.
func (r Registry) tags(ctx context.Context) ([]string, error) {
	rr, err := r.repository(ctx, false)
	if err != nil {
		return nil, err
	}
	var tags []string
	err = rr.Tags(ctx, "", func(page []string) error {
		for _, t := range page {
			if profile.IsSnapshotTag(t) {
				tags = append(tags, t)
			}
		}
		return nil
	})
	if errors.Is(err, provider.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("snapshot: list the tags of %s: %w", profile.SnapshotRepository, err)
	}
	slices.SortFunc(tags, func(a, b string) int { return cmp.Compare(b, a) })
	return tags, nil
}

// repository returns the repository of the snapshots, made where the registry needs it made
// first when ensure.
func (r Registry) repository(ctx context.Context, ensure bool) (provider.Repository, error) {
	if ensure {
		if err := r.Provider.EnsureRepository(ctx, profile.SnapshotRepository); err != nil {
			return nil, fmt.Errorf("snapshot: make %s: %w", profile.SnapshotRepository, err)
		}
	}
	rr, err := r.Provider.Repository(ctx, profile.SnapshotRepository)
	if err != nil {
		return nil, fmt.Errorf("snapshot: %s: %w", profile.SnapshotRepository, err)
	}
	return rr, nil
}

// maxManifest bounds a snapshot's manifest: it has one layer and a few annotations.
const maxManifest = 64 << 10

// manifest fetches the manifest desc and parses it as a snapshot's.
func (r Registry) manifest(ctx context.Context, rr provider.Repository, desc ocispec.Descriptor) (ocispec.Manifest, profile.Snapshot, error) {
	if desc.Size > maxManifest {
		return ocispec.Manifest{}, profile.Snapshot{}, fmt.Errorf("the manifest is %d bytes, more than a snapshot's", desc.Size)
	}
	raw, err := fetch(ctx, rr, desc)
	if err != nil {
		return ocispec.Manifest{}, profile.Snapshot{}, err
	}
	var m ocispec.Manifest
	if err := json.Unmarshal(raw, &m); err != nil {
		return ocispec.Manifest{}, profile.Snapshot{}, fmt.Errorf("the manifest is not JSON: %w", err)
	}
	s, err := profile.ParseSnapshot(m)
	return m, s, err
}

// fetch reads the content desc names whole and checks its size and digest.
func fetch(ctx context.Context, rr provider.Repository, desc ocispec.Descriptor) ([]byte, error) {
	rc, err := rr.Fetch(ctx, desc)
	if err != nil {
		return nil, fmt.Errorf("fetch %s: %w", desc.Digest, err)
	}
	defer func() { _ = rc.Close() }()
	b, err := io.ReadAll(io.LimitReader(rc, desc.Size+1))
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", desc.Digest, err)
	}
	if int64(len(b)) != desc.Size || digest.FromBytes(b) != desc.Digest {
		return nil, fmt.Errorf("%s: the registry's bytes do not have that digest and size", desc.Digest)
	}
	return b, nil
}
