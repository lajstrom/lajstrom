package ingest

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/lajstrom/lajstrom/internal/format"
	"github.com/lajstrom/lajstrom/internal/index"
	"github.com/lajstrom/lajstrom/internal/oci/profile"
	"github.com/lajstrom/lajstrom/internal/provider"
)

// errExisted ends the claim transaction when the tag already holds the manifest: the claim is
// rolled back with it and nothing is written.
var errExisted = errors.New("ingest: the tag already holds this manifest")

// claimTag claims pub's tag in one transaction and reports whether the index already has the
// manifest under it. The claim comes first: a concurrent claim of the tag holds the insert
// until it commits, so the version read next sees the other publish's outcome. A version with
// another digest, or a fresh claim of another digest, is the *index.TagConflictError, which
// is returned as it is; for an Upload (repeatOnFiles) a version with the same file layers is
// the repeat instead, and pub.existing names its manifest.
func (p *Pipeline) claimTag(ctx context.Context, pub *publish) (existed bool, err error) {
	now := p.now()
	pub.claim = index.TagClaim{RepoID: pub.repo.ID, Package: pub.parsed.Annotations.Name, Tag: pub.tag,
		Digest: pub.digest, Principal: pub.principal.String(), ClaimedAt: now}
	err = p.index.Tx(ctx, func(w index.Writer) error {
		if err := w.ClaimTag(ctx, &pub.claim, now.Add(-p.claimTTL)); err != nil {
			return err
		}
		v, err := versionByTag(ctx, w, pub.repo.ID, pub.claim.Package, pub.tag)
		switch {
		case err != nil:
			return err
		case v == nil:
			return nil
		case v.Digest == pub.digest:
			return errExisted
		}
		if pub.repeatOnFiles {
			same, err := sameFiles(ctx, w, v.ID, pub.parsed.Files)
			if err != nil {
				return err
			}
			if same {
				pub.existing = v.Digest
				return errExisted
			}
		}
		return &index.TagConflictError{Tag: pub.tag, Existing: v.Digest}
	})
	var conflict *index.TagConflictError
	switch {
	case err == nil:
		return false, nil
	case errors.Is(err, errExisted):
		return true, nil
	case errors.As(err, &conflict):
		return false, err
	}
	return false, fmt.Errorf("ingest: claim %s:%s: %w", pub.name, pub.tag, err)
}

// versionByTag returns the version of package name in a repo that has tag, or nil when there is
// none.
func versionByTag(ctx context.Context, r index.Reader, repoID int64, name, tag string) (*index.Version, error) {
	pkg, err := r.GetPackage(ctx, repoID, name)
	if errors.Is(err, index.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	v, err := r.GetVersionByTag(ctx, pkg.ID, tag)
	if errors.Is(err, index.ErrNotFound) {
		return nil, nil
	}
	return v, err
}

// repeat returns the version that built's tag already holds when it has the same file layers,
// which Upload treats as the idempotent repeat: two builds of the same files differ by
// org.opencontainers.image.created, so the digest cannot tell a retry from a new publish and
// the files can. It runs before the blobs are pushed, so a repeat touches the registry not at
// all; claimTag applies the rule again under the claim. A manifest that fails the profile is
// left to the pipeline, which reports why.
func (p *Pipeline) repeat(ctx context.Context, repo index.Repo, built *format.Built) (*index.Version, error) {
	parsed, err := profile.Validate(built.Manifest)
	if err != nil {
		return nil, nil //nolint:nilerr // not a repeat; validate reports the error
	}
	v, err := versionByTag(ctx, p.index, repo.ID, parsed.Annotations.Name, built.Tag)
	if err != nil {
		return nil, fmt.Errorf("ingest: version %s:%s: %w", built.RepoPath, built.Tag, err)
	}
	if v == nil {
		return nil, nil
	}
	same, err := sameFiles(ctx, p.index, v.ID, parsed.Files)
	if err != nil || !same {
		return nil, err
	}
	return v, nil
}

// sameFiles reports whether the files of a version are the file layers, as a set: the same
// digest, size, media type and title each, in any order. The metadata layer is not compared:
// it is built from the files.
func sameFiles(ctx context.Context, r index.Reader, versionID int64, layers []profile.Layer) (bool, error) {
	files, err := r.ListFiles(ctx, versionID)
	if err != nil {
		return false, fmt.Errorf("ingest: files of version %d: %w", versionID, err)
	}
	if len(files) != len(layers) {
		return false, nil
	}
	type key struct {
		digest    digest.Digest
		size      int64
		mediaType string
		title     string
	}
	have := make(map[key]int, len(files))
	for _, f := range files {
		have[key{f.Digest, f.Size, f.MediaType, f.Name}]++
	}
	for _, l := range layers {
		k := key{l.Digest, l.Size, l.MediaType, l.Title}
		if have[k] == 0 {
			return false, nil
		}
		have[k]--
	}
	return true, nil
}

// push puts the manifest and its tag in one request. On any error the claim is released.
func (p *Pipeline) push(ctx context.Context, pub *publish) error {
	err := pub.pushManifest(ctx)
	if err != nil {
		p.release(ctx, pub)
	}
	return err
}

// pushManifest checks the tag in the backend and pushes the manifest with it. The tag rule is
// Lajstrom's, so a tag in the backend with another digest is a conflict even when the index
// does not know it: a write that bypassed Lajstrom, or an index not yet rebuilt by reindex.
// The same digest there is pushed again, which changes nothing, and confirmed.
func (pub *publish) pushManifest(ctx context.Context) error {
	existing, err := pub.rr.Resolve(ctx, pub.tag)
	switch {
	case errors.Is(err, provider.ErrNotFound): // the tag is free in the backend
	case err != nil:
		return fmt.Errorf("ingest: resolve %s:%s: %w", pub.name, pub.tag, err)
	case existing.Digest != pub.digest:
		return &index.TagConflictError{Tag: pub.tag, Existing: existing.Digest}
	}
	desc := ocispec.Descriptor{MediaType: pub.manifest.MediaType, Digest: pub.digest, Size: int64(len(pub.raw))}
	err = pub.rr.PushReference(ctx, desc, bytes.NewReader(pub.raw), pub.tag)
	switch {
	case err == nil:
		return nil
	case errors.Is(err, provider.ErrConflict):
		// A backend with immutable tags refused: the tag is taken there, by this manifest (a
		// replica that shared the claim pushed it first) or by another.
		existing, rerr := pub.rr.Resolve(ctx, pub.tag)
		switch {
		case rerr == nil && existing.Digest == pub.digest:
			return nil
		case rerr == nil:
			return fmt.Errorf("ingest: push %s:%s: %w (%w)", pub.name, pub.tag, &index.TagConflictError{Tag: pub.tag, Existing: existing.Digest}, err)
		}
		return fmt.Errorf("%w: ingest: push %s:%s: the registry refused the tag: %w", ErrConflict, pub.name, pub.tag, err)
	}
	return fmt.Errorf("ingest: push %s:%s: %w", pub.name, pub.tag, err)
}

// release deletes the claim of a publish that does not go on, also when ctx is done: a claim
// left behind holds the tag until it is stale. A claim that is gone already is fine.
func (p *Pipeline) release(ctx context.Context, pub *publish) {
	err := p.index.ReleaseTagClaim(context.WithoutCancel(ctx), pub.claim.ID)
	if err != nil && !errors.Is(err, index.ErrNotFound) {
		p.log.WarnContext(ctx, "ingest: tag claim not released; it holds the tag until it is stale",
			"repo", pub.repo.Format+"/"+pub.repo.Name, "package", pub.claim.Package, "tag", pub.tag, "error", err.Error())
	}
}

// confirm writes the index rows of a manifest the registry has accepted, in one transaction,
// and releases the claim. existed reports that the version was there already with the same
// digest: another replica published the same bytes under the shared claim and confirmed
// first, and nothing is written. A failure is logged and returned as a plain error (500),
// except a conflict: a *index.TagConflictError is returned as it is, and another
// index.ErrConflict, a package path that another spelling holds (#97), stays wrapped (409). The
// manifest stays in the registry and reindex picks it up.
func (p *Pipeline) confirm(ctx context.Context, pub *publish) (existed bool, err error) {
	now := p.now()
	ev := pub.event(now)
	withOrigin(ctx, &ev, nil)
	err = p.index.Tx(ctx, func(w index.Writer) error {
		var err error
		existed, err = p.writeRows(ctx, w, pub, &ev, now)
		return err
	})
	if err != nil {
		p.log.ErrorContext(ctx, "ingest: the registry accepted the manifest, the index write failed; reindex repairs the index",
			"repo", pub.repo.Format+"/"+pub.repo.Name, "package", pub.claim.Package, "tag", pub.tag,
			"digest", pub.digest.String(), "error", err.Error())
		p.release(ctx, pub)
		var conflict *index.TagConflictError
		switch {
		case errors.As(err, &conflict):
			return false, err
		case errors.Is(err, index.ErrConflict):
			return false, fmt.Errorf("ingest: index %s:%s after the registry accepted %s: %w", pub.name, pub.tag, pub.digest, err)
		}
		// The cause is in the message only: an index.ErrNotFound (the repo was deleted
		// meanwhile) must not turn a half-done publish into a 404.
		return false, fmt.Errorf("ingest: index %s:%s after the registry accepted %s: %s", pub.name, pub.tag, pub.digest, err.Error())
	}
	// Post-ingest hooks go here, after the commit. apt signing needs none: the transaction
	// dropped the repo's documents, signed ones included, and the next GET signs again (#41).
	if !existed && p.audit != nil {
		if err := p.audit.Write(ctx, ev); err != nil {
			p.log.ErrorContext(ctx, "ingest: audit event of a publish not written", "repo", ev.Repo, "subject", ev.Subject,
				"digest", pub.digest.String(), "error", err.Error())
		}
	}
	return existed, nil
}

// writeRows writes the rows of confirm on w: package, version, files, OCI repository, the
// invalidated index_cache, the audit event when there is no audit.Writer, and the released
// claim.
func (p *Pipeline) writeRows(ctx context.Context, w index.Writer, pub *publish, ev *index.AuditEvent, now time.Time) (existed bool, err error) {
	rows := pub.rows(now)
	pkg := rows.Package
	if err := w.UpsertPackage(ctx, &pkg); err != nil {
		return false, err
	}
	v, err := w.GetVersionByTag(ctx, pkg.ID, pub.tag)
	switch {
	case err == nil && v.Digest == pub.digest:
		return true, releaseClaim(ctx, w, pub.claim.ID)
	case err != nil && !errors.Is(err, index.ErrNotFound):
		return false, err
	}
	if err := WriteRows(ctx, w, &rows, now); err != nil {
		return false, err
	}
	// A plugin cannot name the index_cache scopes a version touches yet (format.Entry has no
	// field for them), so every scope of the repo is invalidated and rendered again on demand.
	if err := w.InvalidateIndexCache(ctx, pub.repo.ID, ""); err != nil {
		return false, err
	}
	if p.audit == nil {
		if err := w.AppendAudit(ctx, ev); err != nil {
			return false, err
		}
	}
	return false, releaseClaim(ctx, w, pub.claim.ID)
}

// releaseClaim deletes a claim on w; one that is gone already (released by a replica that
// shared it) is fine.
func releaseClaim(ctx context.Context, w index.Writer, id int64) error {
	if err := w.ReleaseTagClaim(ctx, id); err != nil && !errors.Is(err, index.ErrNotFound) {
		return err
	}
	return nil
}

// version is the versions row of pub. CreatedAt is org.opencontainers.image.created, or now
// when the annotation is zero.
func (pub *publish) version(packageID int64, now time.Time) index.Version {
	a := pub.parsed.Annotations
	created := a.Created
	if created.IsZero() {
		created = now
	}
	return index.Version{PackageID: packageID, Version: a.Version, Variant: a.Variant, Tag: pub.tag, Digest: pub.digest,
		ArtifactType: pub.manifest.ArtifactType, Attrs: a.Attrs, Metadata: pub.metadata, CreatedAt: created, IndexedAt: now,
		Degraded: pub.degraded}
}

// files are the files rows of pub, one per file layer in manifest order.
func (pub *publish) files() []index.File {
	files := make([]index.File, len(pub.parsed.Files))
	for i, l := range pub.parsed.Files {
		files[i] = index.File{Name: l.Title, MediaType: l.MediaType, Digest: l.Digest, Size: l.Size, Attrs: l.Annotations}
	}
	return files
}

// event is the audit event of pub: a push by its principal, the subject name, version and
// variant. confirm adds the client address and the request id of the request (withOrigin).
func (pub *publish) event(now time.Time) index.AuditEvent {
	a := pub.parsed.Annotations
	subject := a.Name + " " + a.Version
	if a.Variant != "" {
		subject += " " + a.Variant
	}
	return index.AuditEvent{At: now, Principal: pub.principal.String(), Action: "push",
		Repo: pub.repo.Format + "/" + pub.repo.Name, Subject: subject, Digest: pub.digest}
}
