package ingest

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"

	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"oras.land/oras-go/v2/content"

	"github.com/lajstrom/lajstrom/internal/format"
	"github.com/lajstrom/lajstrom/internal/oci/profile"
)

// maxMetadata is the largest metadata layer ingest reads into versions.metadata when the plugin
// did not return the metadata itself. A larger one is left out of the row: the column may be
// omitted for size (docs/spec/index-schema.md), and readers fetch the layer.
const maxMetadata = 1 << 20

// invalid returns err wrapping ErrInvalid: as it is when it does already, else wrapped.
func invalid(err error) error {
	if errors.Is(err, ErrInvalid) {
		return err
	}
	return fmt.Errorf("%w: %w", ErrInvalid, err)
}

// validate runs the checks that come before the claim (docs/spec/manifest-profile.md §5): the
// profile, the target the manifest was pushed to, the blobs in the backend and the plugin's
// Validate. It fills pub.parsed, pub.entry and pub.metadata.
func (p *Pipeline) validate(ctx context.Context, pub *publish) error {
	parsed, err := profile.Validate(pub.manifest)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrInvalid, err)
	}
	pub.parsed = parsed
	if err := pub.checkTarget(); err != nil {
		return err
	}
	// The blobs before the plugin: a missing layer is named here, rather than surfacing as
	// whatever the plugin makes of a failed fetch, and a HEAD is cheaper than the GETs of
	// Validate.
	if err := pub.checkBlobs(ctx); err != nil {
		return err
	}
	entry, err := pub.plugin.Validate(ctx, pub.repo, pub.manifest, pub.fetcher())
	switch {
	case err != nil:
		return invalid(fmt.Errorf("ingest: %s plugin: %w", pub.repo.Format, err))
	case entry == nil:
		return fmt.Errorf("ingest: %s plugin: Validate returned no entry", pub.repo.Format)
	}
	pub.entry = entry
	pub.metadata, err = pub.readMetadata(ctx)
	return err
}

// checkTarget compares the annotations with where the manifest was pushed: format, repo,
// package path (§4.1) and tag (§4.2), and the artifactType with the plugin's.
func (pub *publish) checkTarget() error {
	a := pub.parsed.Annotations
	path, err := pub.plugin.EscapeName(a.Name)
	if err != nil {
		return invalid(fmt.Errorf("ingest: %s %q: %w", profile.AnnotationName, a.Name, err))
	}
	for _, c := range []struct{ what, got, want string }{
		{profile.AnnotationFormat + " does not match the repo's format", a.Format, pub.repo.Format},
		{profile.AnnotationRepo + " does not match the repo", a.Repo, pub.repo.Name},
		{"the package path of " + profile.AnnotationName + " does not match the path pushed to", path, pub.pkgPath},
		{"the tag of " + profile.AnnotationVersion + " and " + profile.AnnotationVariant + " does not match the tag pushed to",
			profile.Tag(a.Version, a.Variant), pub.tag},
		{"artifactType does not match the plugin's", pub.manifest.ArtifactType, pub.plugin.ArtifactType()},
	} {
		if c.got != c.want {
			return fmt.Errorf("%w: %s: %q, want %q", ErrInvalid, c.what, c.got, c.want)
		}
	}
	return nil
}

// checkBlobs checks that every layer is in the backend, and pushes the empty config, which is
// Lajstrom's own, when it is not.
func (pub *publish) checkBlobs(ctx context.Context) error {
	for _, l := range pub.manifest.Layers {
		ok, err := pub.rr.Exists(ctx, l)
		if err != nil {
			return fmt.Errorf("ingest: layer %s in %s: %w", l.Digest, pub.name, err)
		}
		if !ok {
			return fmt.Errorf("%w: layer %s was not uploaded to %s", ErrInvalid, l.Digest, pub.name)
		}
	}
	ok, err := pub.rr.Exists(ctx, profile.EmptyConfig)
	if err == nil && !ok {
		err = pub.rr.Push(ctx, profile.EmptyConfig, bytes.NewReader(ocispec.DescriptorEmptyJSON.Data))
	}
	if err != nil {
		return fmt.Errorf("ingest: empty config in %s: %w", pub.name, err)
	}
	return nil
}

// fetcher is the BlobFetcher of the plugin's Validate: it reads the config and the layers of
// the manifest from the backend, checked against their descriptors, and nothing else.
func (pub *publish) fetcher() format.BlobFetcher {
	return func(ctx context.Context, d digest.Digest) (io.ReadCloser, error) {
		desc, ok := pub.descriptor(d)
		if !ok {
			return nil, fmt.Errorf("%w: the manifest names no blob %s", ErrInvalid, d)
		}
		rc, err := pub.rr.Fetch(ctx, desc)
		if err != nil {
			return nil, fmt.Errorf("ingest: fetch %s from %s: %w", d, pub.name, err)
		}
		return struct {
			io.Reader
			io.Closer
		}{newVerifier(rc, desc), rc}, nil
	}
}

// descriptor returns the descriptor of the config or a layer of the manifest with digest d.
func (pub *publish) descriptor(d digest.Digest) (ocispec.Descriptor, bool) {
	if pub.manifest.Config.Digest == d {
		return pub.manifest.Config, true
	}
	for _, l := range pub.manifest.Layers {
		if l.Digest == d {
			return l, true
		}
	}
	return ocispec.Descriptor{}, false
}

// readMetadata returns the metadata for versions.metadata: the plugin's, else the metadata
// layer read from the backend when it is at most maxMetadata bytes. It must be JSON.
func (pub *publish) readMetadata(ctx context.Context) (json.RawMessage, error) {
	md := pub.entry.Metadata
	if md == nil {
		l := pub.parsed.Metadata
		if l == nil || l.Size > maxMetadata {
			return nil, nil
		}
		desc := ocispec.Descriptor{MediaType: l.MediaType, Digest: l.Digest, Size: l.Size}
		rc, err := pub.rr.Fetch(ctx, desc)
		if err != nil {
			return nil, fmt.Errorf("ingest: fetch the metadata layer %s from %s: %w", l.Digest, pub.name, err)
		}
		defer func() { _ = rc.Close() }()
		v := newVerifier(rc, desc)
		if md, err = io.ReadAll(v); err != nil {
			if bad := v.failure(); bad != nil {
				return nil, fmt.Errorf("%w: the metadata layer %s: %w", ErrInvalid, l.Digest, bad)
			}
			return nil, fmt.Errorf("ingest: read the metadata layer %s: %w", l.Digest, err)
		}
	}
	if !json.Valid(md) {
		return nil, fmt.Errorf("%w: the metadata of %s is not JSON", ErrInvalid, pub.tag)
	}
	return md, nil
}

// pushBlobs pushes the blobs of a Build that the backend does not have, each streamed from its
// source and checked against its descriptor on the way. A layer of the manifest that is
// neither among them nor in the backend is ErrInvalid.
func (t *target) pushBlobs(ctx context.Context, built *format.Built) error {
	have := map[digest.Digest]bool{}
	for _, b := range built.Blobs {
		have[b.Descriptor.Digest] = true
		if err := t.pushBlob(ctx, b); err != nil {
			return err
		}
	}
	for _, l := range built.Manifest.Layers {
		if have[l.Digest] {
			continue
		}
		ok, err := t.rr.Exists(ctx, l)
		if err != nil {
			return fmt.Errorf("ingest: layer %s in %s: %w", l.Digest, t.name, err)
		}
		if !ok {
			return fmt.Errorf("%w: layer %s is neither in the upload nor in %s", ErrInvalid, l.Digest, t.name)
		}
	}
	return nil
}

// pushBlob pushes b unless the backend has it. The bytes stream from b.Open to the provider;
// nothing holds more than a buffer of them.
func (t *target) pushBlob(ctx context.Context, b format.BlobSource) error {
	d := b.Descriptor
	ok, err := t.rr.Exists(ctx, d)
	if err != nil {
		return fmt.Errorf("ingest: blob %s in %s: %w", d.Digest, t.name, err)
	}
	if ok {
		return nil
	}
	rc, err := b.Open(ctx)
	if err != nil {
		return fmt.Errorf("ingest: open blob %s: %w", d.Digest, err)
	}
	defer func() { _ = rc.Close() }()
	v := newVerifier(rc, d)
	if err := t.rr.Push(ctx, d, v); err != nil {
		if bad := v.failure(); bad != nil {
			return fmt.Errorf("%w: blob %s: the bytes are not the descriptor's: %w", ErrInvalid, d.Digest, bad)
		}
		return fmt.Errorf("ingest: push blob %s to %s: %w", d.Digest, t.name, err)
	}
	return nil
}

// verifier passes the bytes of a blob through and fails the read that would end them when they
// are not exactly the size and digest of the descriptor, so that a consumer never takes wrong
// bytes for the blob. failure tells that mismatch apart from an error of the source. The
// provider may read on another goroutine, hence the lock.
type verifier struct {
	vr *content.VerifyReader

	mu  sync.Mutex
	bad error
}

func newVerifier(r io.Reader, desc ocispec.Descriptor) *verifier {
	return &verifier{vr: content.NewVerifyReader(r, desc)}
}

// Read implements io.Reader.
func (v *verifier) Read(p []byte) (int, error) {
	n, err := v.vr.Read(p)
	switch {
	case errors.Is(err, io.ErrUnexpectedEOF): // shorter than the descriptor
		v.fail(err)
	case errors.Is(err, io.EOF):
		if verr := v.vr.Verify(); verr != nil { // longer, or another digest
			v.fail(verr)
			return n, verr
		}
	}
	return n, err
}

func (v *verifier) fail(err error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.bad = err
}

// failure returns the mismatch the verifier found, or nil.
func (v *verifier) failure() error {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.bad
}
