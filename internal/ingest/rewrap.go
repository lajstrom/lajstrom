package ingest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"time"

	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/lajstrom/lajstrom/internal/auth"
	"github.com/lajstrom/lajstrom/internal/index"
	"github.com/lajstrom/lajstrom/internal/oci/profile"
	"github.com/lajstrom/lajstrom/internal/provider"
)

// Rewrapper wraps a data key again for the repo's active key: keys.Encryption implements it, the
// Pipeline's Options.Encryption.
type Rewrapper interface {
	Rewrap(ctx context.Context, repo index.Repo, fingerprint, jwe string) (string, string, error)
}

// keyKind is the kind of an encrypted repo's keys in the keys table (keys.KindEnc).
const keyKind = "enc"

// Rewrap is the second half of the rotation of an encrypted repo's key
// (docs/spec/manifest-profile.md §11.1): for every stored manifest of the repo, a version's or a
// referrer's, whose encrypted layers are not all wrapped for the active key, it pushes a keys
// referrer that wraps their data keys for it, through PutReferrer, and updates their rows of
// blob_map. A manifest whose rows name the active key already is left: run again, Rewrap
// continues a rotation that stopped. It returns how many keys referrers it pushed.
func (p *Pipeline) Rewrap(ctx context.Context, repo index.Repo, principal auth.Principal) (int, error) {
	rw, ok := p.enc.(Rewrapper)
	if !ok || p.enc == nil {
		return 0, fmt.Errorf("%w: %w", ErrNoKeys, errNoMasterKey)
	}
	active, err := p.index.GetKey(ctx, &repo.ID, keyKind)
	if err != nil {
		return 0, fmt.Errorf("ingest: the encryption key of %s/%s: %w", repo.Format, repo.Name, err)
	}
	manifests, err := p.storedManifests(ctx, repo)
	if err != nil {
		return 0, err
	}
	pushed := 0
	for _, m := range manifests {
		done, err := p.rewrapOne(ctx, repo, rw, active.Fingerprint, m, principal)
		if err != nil {
			return pushed, err
		}
		if done {
			pushed++
		}
	}
	return pushed, nil
}

// storedManifest is a manifest of a repo by its package path and stored digest.
type storedManifest struct {
	path   string
	digest digest.Digest
}

// storedManifests returns every version of the repo and every referrer of one that the index has,
// by package path, each digest once.
func (p *Pipeline) storedManifests(ctx context.Context, repo index.Repo) ([]storedManifest, error) {
	pkgs, err := p.index.ListPackages(ctx, repo.ID, "")
	if err != nil {
		return nil, fmt.Errorf("ingest: the packages of %s/%s: %w", repo.Format, repo.Name, err)
	}
	var out []storedManifest
	for _, pkg := range pkgs {
		vs, err := p.index.ListVersions(ctx, pkg.ID)
		if err != nil {
			return nil, fmt.Errorf("ingest: the versions of %s: %w", pkg.Name, err)
		}
		ids := make([]int64, 0, len(vs))
		for _, v := range vs {
			out = append(out, storedManifest{path: index.ManifestPath(pkg, v), digest: v.Digest})
			ids = append(ids, v.ID)
		}
		atts, err := p.index.ListAttachments(ctx, ids)
		if err != nil {
			return nil, fmt.Errorf("ingest: the attachments of %s: %w", pkg.Name, err)
		}
		for _, a := range atts {
			sm := storedManifest{path: pkg.NameOCI, digest: a.Digest}
			if !slices.Contains(out, sm) {
				out = append(out, sm)
			}
		}
	}
	return out, nil
}

// rewrapOne pushes the keys referrer of the stored manifest m when one of its encrypted layers is
// not wrapped for the key active, and reports whether it did.
func (p *Pipeline) rewrapOne(ctx context.Context, repo index.Repo, rw Rewrapper, active string, m storedManifest, principal auth.Principal) (bool, error) {
	rr, err := p.repository(ctx, repo, m.path)
	if err != nil {
		return false, err
	}
	desc, err := rr.Resolve(ctx, m.digest.String())
	if errors.Is(err, provider.ErrNotFound) {
		return false, nil // gone from the registry: reindex removes its rows
	}
	if err != nil {
		return false, fmt.Errorf("ingest: resolve %s: %w", m.digest, err)
	}
	raw, err := readAll(ctx, rr, desc)
	if err != nil {
		return false, err
	}
	layers, _, err := profile.EncryptedLayers(raw)
	if err != nil || len(layers) == 0 {
		return false, nil //nolint:nilerr // no encrypted layer: nothing to wrap again
	}
	keys := profile.Keys{Key: active, Layers: map[digest.Digest]string{}}
	var rows []index.BlobMap
	for _, l := range layers {
		row, err := p.index.GetBlobMap(ctx, repo.ID, m.path, l.Stored)
		if err != nil {
			return false, fmt.Errorf("ingest: blob_map of %s at %s: %w (reindex writes it with the keys)", l.Stored, m.path, err)
		}
		if row.KeyFingerprint == active {
			keys.Layers[l.Stored] = row.Wrapped
			continue
		}
		jwe, fp, err := rw.Rewrap(ctx, repo, row.KeyFingerprint, row.Wrapped)
		if err != nil {
			return false, fmt.Errorf("ingest: wrap the data key of %s again: %w", l.Stored, err)
		}
		keys.Layers[l.Stored], keys.Key = jwe, fp
		row.KeyFingerprint, row.Wrapped, row.IndexedAt = fp, jwe, time.Time{}
		rows = append(rows, *row)
	}
	if len(rows) == 0 {
		return false, nil
	}
	ref, err := profile.KeysManifest(desc, keys, p.now().UTC().Format(time.RFC3339))
	if err != nil {
		return false, err
	}
	rd := ocispec.Descriptor{MediaType: ocispec.MediaTypeImageManifest, Digest: digest.FromBytes(ref), Size: int64(len(ref)),
		ArtifactType: profile.ArtifactTypeKeys}
	if _, err := p.PutReferrer(ctx, repo, Referrer{Path: m.path, Desc: rd, Manifest: ref, Subject: m.digest}, principal); err != nil {
		return false, fmt.Errorf("ingest: the keys referrer of %s: %w", m.digest, err)
	}
	if err := p.index.PutBlobMap(ctx, rows); err != nil {
		return false, fmt.Errorf("ingest: blob_map of %s after its keys referrer: %w", m.digest, err)
	}
	return true, nil
}

// readAll reads the manifest desc of rr, which must be its digest's bytes.
func readAll(ctx context.Context, rr provider.Repository, desc ocispec.Descriptor) ([]byte, error) {
	rc, err := rr.Fetch(ctx, desc)
	if err != nil {
		return nil, fmt.Errorf("ingest: fetch %s: %w", desc.Digest, err)
	}
	defer func() { _ = rc.Close() }()
	raw, err := io.ReadAll(io.LimitReader(rc, maxManifestBytes))
	if err != nil {
		return nil, fmt.Errorf("ingest: read %s: %w", desc.Digest, err)
	}
	if digest.FromBytes(raw) != desc.Digest {
		return nil, fmt.Errorf("ingest: the bytes of %s are not its digest's", desc.Digest)
	}
	return raw, nil
}

// newestKeys returns the newest keys referrer of the stored manifest d of t's repository, by
// created then digest, nil when it has none.
func (t *target) newestKeys(ctx context.Context, d digest.Digest) (*profile.Keys, error) {
	var refs []ocispec.Descriptor
	err := t.rr.Referrers(ctx, ocispec.Descriptor{MediaType: ocispec.MediaTypeImageManifest, Digest: d}, profile.ArtifactTypeKeys,
		func(page []ocispec.Descriptor) error { refs = append(refs, page...); return nil })
	if errors.Is(err, provider.ErrNotFound) || len(refs) == 0 {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("ingest: the keys referrers of %s: %w", d, err)
	}
	var newest *profile.Keys
	var at, by string
	for _, r := range refs {
		raw, err := readAll(ctx, t.rr, r)
		if err != nil {
			return nil, err
		}
		var m ocispec.Manifest
		if err := json.Unmarshal(raw, &m); err != nil || m.ArtifactType != profile.ArtifactTypeKeys {
			continue
		}
		k, err := profile.ParseKeys(m.Annotations[profile.AnnotationKeys])
		if err != nil {
			continue
		}
		created := m.Annotations[ocispec.AnnotationCreated]
		if newest == nil || created > at || created == at && r.Digest.String() > by {
			newest, at, by = &k, created, r.Digest.String()
		}
	}
	return newest, nil
}
