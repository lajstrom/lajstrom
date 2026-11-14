package rpm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/lajstrom/lajstrom/internal/index"
	"github.com/lajstrom/lajstrom/internal/oci/profile"
	"github.com/lajstrom/lajstrom/internal/provider"
)

// maxManifest is the largest manifest read from the registry for a metadata layer the index
// does not hold: the limit of the facade for a manifest PUT.
const maxManifest = 4 << 20

// fetchMetadata reads the metadata layer of version v of pkg from the registry: the manifest by
// its digest (Resolve for its size, then Fetch), checked against the profile, and its metadata
// layer, each read up to its limit and checked against its digest. It runs inside the
// transaction of format.CachedDocument, so it is for the rows whose metadata the index left out
// (versions.metadata may be omitted for size, docs/spec/index-schema.md), not for every
// render. The errors of the provider are kept in the message only (onlyUnavailable): a manifest
// that is gone makes the render fail (500); a registry that is out is provider.ErrUnavailable,
// a 503 with Retry-After.
func (p Plugin) fetchMetadata(ctx context.Context, repo index.Repo, pkg index.Package, v index.Version) ([]byte, error) {
	var prov provider.Provider
	if p.svc != nil && p.svc.providers != nil {
		prov, _ = p.svc.providers.Provider(repo.Registry)
	}
	if prov == nil {
		return nil, fmt.Errorf("rpm: the registry %q of rpm/%s is not configured for the renderer", repo.Registry, repo.Name)
	}
	name := ociName(repo, index.ManifestPath(pkg, v))
	rr, err := prov.Repository(ctx, name)
	if err != nil {
		return nil, fmt.Errorf("rpm: repository %s: %w", name, onlyUnavailable(err))
	}
	desc, err := rr.Resolve(ctx, v.Digest.String())
	if err != nil {
		return nil, fmt.Errorf("rpm: resolve %s in %s: %w", v.Digest, name, onlyUnavailable(err))
	}
	b, err := readBlob(ctx, rr, ocispec.Descriptor{MediaType: desc.MediaType, Digest: v.Digest, Size: desc.Size}, maxManifest)
	if err != nil {
		return nil, fmt.Errorf("rpm: the manifest in %s: %w", name, err)
	}
	var m ocispec.Manifest
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, fmt.Errorf("rpm: the manifest %s in %s: %w", v.Digest, name, err)
	}
	parsed, err := profile.Validate(m)
	if err != nil {
		return nil, fmt.Errorf("rpm: the manifest %s in %s: %w", v.Digest, name, err)
	}
	l := parsed.Metadata
	if l == nil {
		return nil, fmt.Errorf("rpm: the manifest %s in %s has no metadata layer", v.Digest, name)
	}
	b, err = readBlob(ctx, rr, ocispec.Descriptor{MediaType: l.MediaType, Digest: l.Digest, Size: l.Size}, maxMetadata)
	if err != nil {
		return nil, fmt.Errorf("rpm: the metadata layer in %s: %w", name, err)
	}
	return b, nil
}

// readBlob fetches desc from rr, at most limit bytes, and checks the bytes against its size and
// digest.
func readBlob(ctx context.Context, rr provider.Repository, desc ocispec.Descriptor, limit int64) ([]byte, error) {
	if desc.Size > limit {
		return nil, fmt.Errorf("%s has %d bytes, at most %d", desc.Digest, desc.Size, limit)
	}
	rc, err := rr.Fetch(ctx, desc)
	if err != nil {
		return nil, fmt.Errorf("fetch %s: %w", desc.Digest, onlyUnavailable(err))
	}
	defer func() { _ = rc.Close() }()
	b, err := io.ReadAll(io.LimitReader(rc, limit+1))
	switch {
	case err != nil:
		return nil, fmt.Errorf("read %s: %s", desc.Digest, err.Error())
	case int64(len(b)) != desc.Size || desc.Digest.Validate() != nil || desc.Digest.Algorithm().FromBytes(b) != desc.Digest:
		return nil, fmt.Errorf("the %d bytes fetched are not %s of %d bytes", len(b), desc.Digest, desc.Size)
	}
	return b, nil
}

// onlyUnavailable returns an error with the message of err, an error of the provider, that
// wraps nothing of it, so that a manifest or a layer the registry does not have fails the
// render (500) instead of making repomd.xml a 404; but a registry that is out keeps
// provider.ErrUnavailable and its wait, for a 503 with Retry-After.
func onlyUnavailable(err error) error {
	switch after, ok := provider.RetryAfter(err); {
	case !errors.Is(err, provider.ErrUnavailable):
		return errors.New(err.Error())
	case ok:
		return &unavailableError{msg: err.Error(), kind: &provider.UnavailableError{RetryAfter: after}}
	}
	return &unavailableError{msg: err.Error(), kind: provider.ErrUnavailable}
}

// unavailableError is a provider.ErrUnavailable of fetchMetadata: the message of the provider's
// error, and kind, provider.ErrUnavailable or a *provider.UnavailableError with its wait.
type unavailableError struct {
	msg  string
	kind error
}

func (e *unavailableError) Error() string { return e.msg }

func (e *unavailableError) Unwrap() error { return e.kind }
