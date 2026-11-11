package ingest

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"

	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"oras.land/oras-go/v2/content"

	"github.com/lajstrom/lajstrom/internal/format"
	"github.com/lajstrom/lajstrom/internal/index"
	"github.com/lajstrom/lajstrom/internal/oci/profile"
	"github.com/lajstrom/lajstrom/internal/provider"
)

// maxMetadata is the largest metadata layer ingest reads into versions.metadata when the plugin
// did not return the metadata itself: 4 MiB, the largest a plugin writes (the deb metadata
// layer, md5sums mostly, is bounded by the plugin's MaxControl, 4 MiB). A larger one is left
// out of the row: the column may be omitted for size (docs/spec/index-schema.md), and readers
// fetch the layer, as the apt index does for a row without metadata.
const maxMetadata = 4 << 20

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
	if err := pub.checkOrigin(); err != nil {
		return err
	}
	if err := pub.checkTarget(); err != nil {
		return err
	}
	if err := p.checkEarlier(ctx, pub); err != nil {
		return err
	}
	if !pub.readOnly {
		if err := pub.checkSizes(ctx); err != nil {
			return err
		}
	}
	// The blobs before the plugin: a missing layer is named here, rather than surfacing as
	// whatever the plugin makes of a failed fetch, and a HEAD is cheaper than the GETs of
	// Validate.
	if err := pub.checkBlobs(ctx, p.visibility); err != nil {
		return err
	}
	if pub.allowDegraded && pub.parsed.Metadata != nil {
		var err error
		if pub.degraded, err = pub.metadataUnreadable(ctx); err != nil || pub.degraded {
			return err
		}
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
// package path (§4.1) and tag (§4.2), and the artifactType with the plugin's. The package path
// may be in the form of before ADR 0017 (§4.3, profile.LegacyPackagePath), which sets
// pub.earlier: reindex reads both forms, and a publish writes to the one its package has.
func (pub *publish) checkTarget() error {
	a := pub.parsed.Annotations
	path, err := pub.plugin.EscapeName(a.Name)
	if err != nil {
		return invalid(fmt.Errorf("ingest: %s %q: %w", profile.AnnotationName, a.Name, err))
	}
	if pub.pkgPath != path && pub.pkgPath == profile.LegacyPackagePath(path) {
		path, pub.earlier = pub.pkgPath, true
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

// checkEarlier refuses a publish to the form of a package path of before ADR 0017 when the index
// has no package there: that form is kept for the packages stored in it, and a new package is
// written in the form of ADR 0017 (manifest-profile.md §4.3, §5).
func (p *Pipeline) checkEarlier(ctx context.Context, pub *publish) error {
	if !pub.earlier || pub.readOnly {
		return nil // the form of ADR 0017, or reindex, which reads both
	}
	_, err := p.index.GetPackageByNameOCI(ctx, pub.repo.ID, pub.pkgPath)
	switch {
	case errors.Is(err, index.ErrNotFound):
		path, _ := pub.plugin.EscapeName(pub.parsed.Annotations.Name)
		return fmt.Errorf("%w: %s is the package path of %q before ADR 0017, kept only for a package stored there; "+
			"a new package goes to %s", ErrInvalid, pub.pkgPath, pub.parsed.Annotations.Name, path)
	case err != nil:
		return fmt.Errorf("ingest: the package at %s: %w", pub.name, err)
	}
	return nil
}

// checkSizes refuses a file layer larger than its bound (manifest-profile.md §5): the repo's
// maxFileSize (format.MaxFileSize), which the frontends check as the bytes come and which a
// manifest pushed to /v2 meets here first, or the registry's capabilities.maxBlobSize when that
// is smaller, which no frontend reads. Inspect does not check: reindex keeps what the registry
// has, whatever the policy says now.
func (pub *publish) checkSizes(ctx context.Context) error {
	limit, err := format.MaxFileSize(pub.repo)
	if err != nil {
		return err
	}
	bound := fmt.Sprintf("the maxFileSize of %s/%s", pub.repo.Format, pub.repo.Name)
	if pub.prov != nil {
		if caps, err := pub.prov.Capabilities(ctx); err == nil && caps.MaxBlobSize > 0 && (limit < 0 || caps.MaxBlobSize < limit) {
			limit, bound = caps.MaxBlobSize, "the maxBlobSize of registry "+pub.repo.Registry
		}
	}
	if limit < 0 {
		return nil
	}
	for _, f := range pub.parsed.Files {
		if f.Size > limit {
			return &tooLarge{limit: limit, msg: fmt.Sprintf("the file %s of %d bytes is larger than %s, %d bytes",
				f.Title, f.Size, bound, limit)}
		}
	}
	return nil
}

// tooLarge is a file above the repo's maxFileSize: ErrInvalid, and an *http.MaxBytesError with
// the limit, which the facade's hook answers 413 SIZE_INVALID. The message is the file's alone.
type tooLarge struct {
	msg   string
	limit int64
}

func (e *tooLarge) Error() string { return e.msg }

func (e *tooLarge) Unwrap() []error {
	return []error{ErrInvalid, &http.MaxBytesError{Limit: e.limit}}
}

// checkBlobs checks that every layer is in the backend, and pushes the empty config, which is
// Lajstrom's own, when it is not; Inspect pushes nothing, and with allowDegraded leaves a missing
// metadata layer to metadataUnreadable. The streamed file of UploadStream is in its session, and
// its other blobs and the empty config are held, until the push commits them. On a publish a
// layer the backend does not serve is read again (readBlob) for the visibility timeout: it may
// have been pushed a moment ago.
func (pub *publish) checkBlobs(ctx context.Context, visibility time.Duration) error {
	var deadline time.Time
	for _, l := range pub.manifest.Layers {
		if pub.allowDegraded && pub.parsed.Metadata != nil && l.Digest == pub.parsed.Metadata.Digest || pub.streamed.later(l.Digest) {
			continue
		}
		ok, err := pub.rr.Exists(ctx, l)
		if err == nil && !ok && !pub.readOnly {
			ok, err = readBlob(ctx, pub.prov, pub.rr, l, visibility, &deadline)
		}
		if err != nil {
			return fmt.Errorf("ingest: layer %s in %s: %w", l.Digest, pub.name, err)
		}
		if !ok {
			return fmt.Errorf("%w: layer %s was not uploaded to %s", ErrInvalid, l.Digest, pub.name)
		}
	}
	if pub.streamed.later(profile.EmptyConfig.Digest) {
		return nil
	}
	return pub.checkConfig(ctx)
}

// checkConfig pushes the empty config, which is Lajstrom's own, when the backend does not have
// it; for Inspect, which pushes nothing, it is ErrInvalid.
func (pub *publish) checkConfig(ctx context.Context) error {
	ok, err := pub.rr.Exists(ctx, profile.EmptyConfig)
	switch {
	case err == nil && !ok && pub.readOnly:
		return fmt.Errorf("%w: the empty config is not in %s", ErrInvalid, pub.name)
	case err == nil && !ok:
		err = pub.rr.Push(ctx, profile.EmptyConfig, bytes.NewReader(ocispec.DescriptorEmptyJSON.Data))
	}
	if err != nil {
		return fmt.Errorf("ingest: empty config in %s: %w", pub.name, err)
	}
	return nil
}

// fetcher is the BlobFetcher of the plugin's Validate: it reads the config and the layers of
// the manifest from the backend, and nothing else. Its reader is a fetchedBlob: Read streams the
// whole blob checked against its descriptor, ReadAt reads parts with range requests. A blob that
// UploadStream holds is read from the upload, checked against its descriptor likewise.
func (pub *publish) fetcher() format.BlobFetcher {
	return func(ctx context.Context, d digest.Digest) (io.ReadCloser, error) {
		desc, ok := pub.descriptor(d)
		if !ok {
			return nil, fmt.Errorf("%w: the manifest names no blob %s", ErrInvalid, d)
		}
		if pub.streamed.holds(d) && !pub.streamed.done {
			return nil, fmt.Errorf("ingest: blob %s streams into %s and is committed after Validate: "+
				"a Streamer's Validate takes what it needs of it from the inspector", d, pub.name)
		}
		if _, ok := pub.streamed.heldBlob(d); ok {
			rc, err := pub.open(ctx, desc)
			if err != nil {
				return nil, err
			}
			return verifiedBlob{verifier: newVerifier(rc, desc), Closer: rc}, nil
		}
		return &fetchedBlob{ctx: ctx, pub: pub, desc: desc, blocks: map[int64][]byte{}}, nil
	}
}

// open returns the bytes of the blob desc, unchecked: from the upload when UploadStream holds
// it, else from the backend.
func (pub *publish) open(ctx context.Context, desc ocispec.Descriptor) (io.ReadCloser, error) {
	if b, ok := pub.streamed.heldBlob(desc.Digest); ok {
		rc, err := b.Open(ctx)
		if err != nil {
			return nil, fmt.Errorf("ingest: open blob %s: %w", desc.Digest, err)
		}
		return rc, nil
	}
	return pub.rr.Fetch(ctx, desc)
}

// verifiedBlob is a blob read through its verifier.
type verifiedBlob struct {
	*verifier
	io.Closer
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

// metadataUnreadable reports whether the metadata layer is missing from the backend, or its
// bytes are not its digest's or not JSON: the rows are then degraded (Inspect with
// allowDegraded). Any other failure, a backend that does not answer, is an error.
func (pub *publish) metadataUnreadable(ctx context.Context) (bool, error) {
	l := pub.parsed.Metadata
	desc := ocispec.Descriptor{MediaType: l.MediaType, Digest: l.Digest, Size: l.Size}
	rc, err := pub.rr.Fetch(ctx, desc)
	if errors.Is(err, provider.ErrNotFound) {
		return true, nil
	}
	if err != nil {
		return false, fmt.Errorf("ingest: fetch the metadata layer %s from %s: %w", l.Digest, pub.name, err)
	}
	defer func() { _ = rc.Close() }()
	v := newVerifier(rc, desc)
	md, err := io.ReadAll(io.LimitReader(v, maxMetadata+1))
	switch {
	case v.failure() != nil:
		return true, nil
	case err != nil:
		return false, fmt.Errorf("ingest: read the metadata layer %s: %w", l.Digest, err)
	case l.Size > maxMetadata: // not kept in versions.metadata; the plugin reads it
		return false, nil
	}
	return !json.Valid(md), nil
}

// readMetadata returns the metadata for versions.metadata: the plugin's, else the metadata
// layer read from the backend, or the upload that holds it, when it is at most maxMetadata
// bytes. It must be JSON.
func (pub *publish) readMetadata(ctx context.Context) (json.RawMessage, error) {
	md := pub.entry.Metadata
	if md == nil {
		l := pub.parsed.Metadata
		if l == nil || l.Size > maxMetadata {
			return nil, nil
		}
		desc := ocispec.Descriptor{MediaType: l.MediaType, Digest: l.Digest, Size: l.Size}
		rc, err := pub.open(ctx, desc)
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
// neither among them nor in the backend is ErrInvalid, but for the streamed file of
// UploadStream, which the push commits. UploadStream's blobs are held instead, and pushed with
// the file (commit): a refused upload stores none of them.
func (t *target) pushBlobs(ctx context.Context, built *format.Built) error {
	have := map[digest.Digest]bool{}
	for _, b := range built.Blobs {
		have[b.Descriptor.Digest] = true
		switch {
		case t.streamed.holds(b.Descriptor.Digest):
		case t.streamed != nil:
			t.streamed.held = append(t.streamed.held, b)
		default:
			if err := t.pushBlob(ctx, b); err != nil {
				return err
			}
		}
	}
	for _, l := range built.Manifest.Layers {
		if have[l.Digest] || t.streamed.holds(l.Digest) {
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
