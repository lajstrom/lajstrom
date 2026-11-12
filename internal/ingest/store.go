package ingest

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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

// claimTag claims pub's package path and tag in one transaction and reports whether the index
// already has the manifest under the tag. The claims come first, the package path's before the
// tag's (claimPath): a concurrent claim of either holds the insert until it commits, so the
// package and the version read next see the other publish's outcome. Both are keyed by the
// package path, which names the OCI repository (docs/spec/index-schema.md, Coordination). A
// package of another spelling, or a fresh claim of another spelling, is ErrConflict naming it
// (#244); a version with another digest, or a fresh claim of another digest, is the
// *index.TagConflictError. Both are returned as they are. For an Upload (repeatOnFiles) a
// version with the same file layers is the repeat instead, and pub.existing names its manifest;
// and a fresh claim of another digest whose manifest the registry has, with the same files, is
// the publish that pub repeats: pub takes over that manifest and shares its claim (takeOver).
// A claim pub gets is the tag's in the index only: checkTag reads the tag in the backend next.
func (p *Pipeline) claimTag(ctx context.Context, pub *publish) (existed bool, err error) {
	existed, claimed, err := p.claimOnce(ctx, pub, 0)
	if claimed == "" {
		return existed, err
	}
	return p.takeOver(ctx, pub, claimed, 0, err)
}

// claimOnce is one claim transaction of claimTag. claimed is the digest of the fresh claim of
// another digest that refused an Upload's claim, which claimTag may adopt; err is the
// *index.TagConflictError then. replace, when not zero, is the id of pub's own claim on the tag,
// of the digest it built, which the transaction releases before it claims the tag for
// pub.digest, the manifest pub took over: another publish never finds the tag unclaimed in
// between. pub.claim is set when the transaction commits, so a claim that it did not move
// stays pub's.
func (p *Pipeline) claimOnce(ctx context.Context, pub *publish, replace int64) (existed bool, claimed digest.Digest, err error) {
	now := p.now()
	claim := index.TagClaim{RepoID: pub.repo.ID, Package: pub.pkgPath, Tag: pub.tag,
		Digest: pub.digest, Principal: pub.principal.String(), ClaimedAt: now}
	err = p.index.Tx(ctx, func(w index.Writer) error {
		if replace != 0 {
			if err := w.ReleaseTagClaim(ctx, replace); err != nil && !errors.Is(err, index.ErrNotFound) {
				return err
			}
		}
		if err := p.claimPath(ctx, w, pub, now); err != nil {
			return err
		}
		if err := w.ClaimTag(ctx, &claim, now.Add(-p.claimTTL)); err != nil {
			var conflict *index.TagConflictError
			if pub.repeatOnFiles && errors.As(err, &conflict) {
				claimed = conflict.Existing
			}
			return err
		}
		return pub.checkVersion(ctx, w)
	})
	switch {
	case err == nil:
		pub.claim = claim
		return false, "", nil
	case errors.Is(err, errExisted):
		return true, "", nil
	case errors.Is(err, ErrConflict):
		return false, claimed, err
	}
	return false, "", fmt.Errorf("ingest: claim %s:%s: %w", pub.name, pub.tag, err)
}

// checkVersion reads the version of pub's tag on w, under pub's claims: none is nil; the same
// digest, or for an Upload the same file layers (pub.existing names its manifest then), is
// errExisted; another digest is the *index.TagConflictError.
func (pub *publish) checkVersion(ctx context.Context, w index.Writer) error {
	v, err := versionByTag(ctx, w, pub.repo.ID, pub.pkgPath, pub.parsed.Annotations.Name, pub.tag)
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
}

// takeOver makes pub, an Upload, the publish of the manifest d (adopt) and claims pub's tag
// again, for d (claimOnce, which moves pub's own claim replace when there is one). refused is
// the conflict that named d, a claim's or the tag's in the backend; it stands when pub does not
// adopt d.
func (p *Pipeline) takeOver(ctx context.Context, pub *publish, d digest.Digest, replace int64, refused error) (existed bool, err error) {
	adopted, err := p.adopt(ctx, pub, d)
	switch {
	case err != nil:
		return false, err
	case !adopted:
		return false, refused
	}
	existed, _, err = p.claimOnce(ctx, pub, replace)
	return existed, err
}

// adopt makes pub the publish of the manifest d when the registry has it with the same name and
// the same file layers: a publish of the same files that pushed it and did not confirm it. d is
// what a fresh claim on pub's tag holds (claimTag): a frontend's upload answered 503 by the
// visibility barrier, which keeps its claims, or one still on its way. Or d is what pub's tag
// holds in the backend while the index has no version under it (checkTag): an upload whose
// confirm failed, the database out (503, #641), or whose claim went stale. A frontend builds the
// manifest again for a retry, with another org.opencontainers.image.created and so another
// digest, and would get 409 from its own first attempt. pub then validates d as it validated
// its own build, and pushes and confirms it; the tag keeps d. adopted is false when the
// registry does not serve d within the visibility timeout (a strongly consistent one is read
// once) or d is another artifact.
func (p *Pipeline) adopt(ctx context.Context, pub *publish, d digest.Digest) (adopted bool, err error) {
	caps, err := pub.prov.Capabilities(ctx)
	if err != nil {
		return false, fmt.Errorf("ingest: capabilities of the registry of %s: %w", pub.name, err)
	}
	timeout := max(p.visibility, caps.ConsistencyBound)
	if caps.Consistency == provider.ConsistencyStrong {
		timeout = 0
	}
	var desc ocispec.Descriptor
	found, err := provider.ReadBack{Timeout: timeout}.Await(ctx, func(ctx context.Context) (bool, error) {
		var rerr error
		desc, rerr = pub.rr.Resolve(ctx, d.String())
		if errors.Is(rerr, provider.ErrNotFound) {
			return false, nil
		}
		return rerr == nil, rerr
	})
	if err != nil || !found {
		return false, err
	}
	m, raw, err := fetchManifest(ctx, pub.rr, desc)
	if err != nil {
		return false, fmt.Errorf("ingest: the claimed manifest %s@%s: %w", pub.name, d, err)
	}
	parsed, err := profile.Validate(m)
	if err != nil || parsed.Annotations.Name != pub.parsed.Annotations.Name || !sameLayers(parsed.Files, pub.parsed.Files) {
		return false, nil //nolint:nilerr // another artifact: the claim's conflict stands
	}
	p.log.InfoContext(ctx, "ingest: an upload of the same files takes over the manifest of a publish not confirmed",
		"name", pub.name, "tag", pub.tag, "digest", d.String(), "built", pub.digest.String())
	pub.manifest, pub.raw, pub.digest = m, raw, d
	return true, p.validate(ctx, pub)
}

// fetchManifest reads the manifest desc of rr, which must be its digest's bytes.
func fetchManifest(ctx context.Context, rr provider.Repository, desc ocispec.Descriptor) (ocispec.Manifest, []byte, error) {
	var m ocispec.Manifest
	rc, err := rr.Fetch(ctx, desc)
	if err != nil {
		return m, nil, err
	}
	defer func() { _ = rc.Close() }()
	raw, err := io.ReadAll(io.LimitReader(rc, maxManifestBytes))
	switch {
	case err != nil:
		return m, nil, err
	case digest.FromBytes(raw) != desc.Digest:
		return m, nil, fmt.Errorf("the manifest's bytes are not %s", desc.Digest)
	}
	return m, raw, json.Unmarshal(raw, &m)
}

// sameLayers reports whether a and b are the same file layers, as a set: the same digest, size,
// media type and title each, in any order.
func sameLayers(a, b []profile.Layer) bool {
	if len(a) != len(b) {
		return false
	}
	type key struct {
		digest    digest.Digest
		size      int64
		mediaType string
		title     string
	}
	have := make(map[key]int, len(a))
	for _, l := range a {
		have[key{l.Digest, l.Size, l.MediaType, l.Title}]++
	}
	for _, l := range b {
		k := key{l.Digest, l.Size, l.MediaType, l.Title}
		if have[k] == 0 {
			return false
		}
		have[k]--
	}
	return true
}

// pathTag is the tag of the claim on a package path; an OCI tag is never empty.
const pathTag = ""

// claimPath claims pub's package path on w for its spelling: the claim with pathTag and the
// digest of lajstrom.name, which the publishes of one spelling share. A fresh claim of another
// spelling is ErrConflict. A stale claim of the spelling, left by a publish that stopped, is
// made again rather than shared, so that another spelling cannot take it over while pub runs.
func (p *Pipeline) claimPath(ctx context.Context, w index.Writer, pub *publish, now time.Time) error {
	name, stale := pub.parsed.Annotations.Name, now.Add(-p.claimTTL)
	claim := func() (index.TagClaim, error) {
		c := index.TagClaim{RepoID: pub.repo.ID, Package: pub.pkgPath, Tag: pathTag, Digest: digest.FromString(name),
			Principal: pub.principal.String(), ClaimedAt: now}
		return c, w.ClaimTag(ctx, &c, stale)
	}
	c, err := claim()
	if err == nil && c.ClaimedAt.Before(stale) {
		if err = w.ReleaseTagClaim(ctx, c.ID); err == nil {
			c, err = claim()
		}
	}
	var conflict *index.TagConflictError
	if errors.As(err, &conflict) {
		return fmt.Errorf("%w: %q: another spelling of the name is being published to the package path %s; "+
			"names that differ only in case share it, and the first spelling published holds it", ErrConflict, name, pub.pkgPath)
	}
	pub.pathClaim = c.ID
	return err
}

// packageOf returns the package of a repo whose package path is pkgPath, or nil when there is
// none. A package of another name there is ErrConflict naming it: names that differ only in
// case share the package path, and the first spelling published holds it (#244). So is a
// package at an earlier form of pkgPath (ADR 0017, index.PackageAt), which keeps its OCI
// repository: a publish to pkgPath would split it.
func packageOf(ctx context.Context, r index.Reader, repoID int64, pkgPath, name string) (*index.Package, error) {
	pkg, err := index.PackageAt(ctx, r, repoID, pkgPath)
	switch {
	case errors.Is(err, index.ErrNotFound):
		return nil, nil
	case err != nil:
		return nil, err
	case pkg.Name != name:
		return nil, fmt.Errorf("%w: %q exists as %q: names that differ only in case share the package path %s, "+
			"which the first spelling published holds", ErrConflict, name, pkg.Name, pkg.NameOCI)
	case pkg.NameOCI != pkgPath:
		return nil, fmt.Errorf("%w: %q is stored at the package path %s, an earlier form (ADR 0017), and keeps it; "+
			"publish there, not to %s", ErrConflict, name, pkg.NameOCI, pkgPath)
	}
	return pkg, nil
}

// versionByTag returns the version that has tag in the package of name at pkgPath (packageOf),
// or nil when there is none.
func versionByTag(ctx context.Context, r index.Reader, repoID int64, pkgPath, name, tag string) (*index.Version, error) {
	pkg, err := packageOf(ctx, r, repoID, pkgPath, name)
	if err != nil || pkg == nil {
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
// all, and neither does an upload of another spelling than the package's at pkgPath, which is
// ErrConflict; claimTag applies both rules again under the claims. A manifest that fails the
// profile is left to the pipeline, which reports why.
func (p *Pipeline) repeat(ctx context.Context, repo index.Repo, pkgPath string, built *format.Built) (*index.Version, error) {
	parsed, err := profile.Validate(built.Manifest)
	if err != nil {
		return nil, nil //nolint:nilerr // not a repeat; validate reports the error
	}
	v, err := versionByTag(ctx, p.index, repo.ID, pkgPath, parsed.Annotations.Name, built.Tag)
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
	had := make([]profile.Layer, len(files))
	for i, f := range files {
		had[i] = profile.Layer{Digest: f.Digest, Size: f.Size, MediaType: f.MediaType, Title: f.Name}
	}
	return sameLayers(had, layers), nil
}

// push checks the tag in the backend (checkTag), then puts the manifest and its tag in one
// request, its audit row pending before the request (pushManifest). existed reports that
// checkTag found the manifest it took over in the index already: nothing is pushed. On any
// error, and when existed, the claim is released, and the row, when there is one, is failed.
func (p *Pipeline) push(ctx context.Context, pub *publish) (existed bool, err error) {
	existed, err = p.checkTag(ctx, pub)
	if err == nil && !existed {
		err = pub.pushManifest(ctx, func(ctx context.Context) error {
			pub.ev = pub.event(p.now())
			withOrigin(ctx, &pub.ev, pub.auditDetails())
			return p.intend(ctx, &pub.ev)
		})
	}
	if err != nil {
		p.fail(ctx, &pub.ev, err)
	}
	if err != nil || existed {
		p.release(ctx, pub)
	}
	return existed, err
}

// checkTag resolves pub's tag in the backend, under pub's claims and before anything is
// written. The tag rule is Lajstrom's, so a tag in the backend with another digest is a
// conflict even when the index does not know it: a write that bypassed Lajstrom, or an index
// not yet rebuilt by reindex. For an Upload (repeatOnFiles) that digest is first the publish
// that pub repeats when its manifest has the same files (adopt): one that pushed and did not
// confirm, so that the index, which claimTag read under the claim, has no version under the
// tag. pub takes it over and moves its claim to it (takeOver); existed reports that the index
// has it meanwhile. Nothing re-points the tag: pub pushes the digest the tag holds. A manifest
// of other files there is the conflict. The same digest there is pushed again too, which
// changes nothing, and confirmed.
func (p *Pipeline) checkTag(ctx context.Context, pub *publish) (existed bool, err error) {
	desc, err := pub.rr.Resolve(ctx, pub.tag)
	switch {
	case errors.Is(err, provider.ErrNotFound): // the tag is free in the backend
		return false, nil
	case err != nil:
		return false, fmt.Errorf("ingest: resolve %s:%s: %w", pub.name, pub.tag, err)
	case desc.Digest == pub.digest:
		return false, nil
	}
	conflict := &index.TagConflictError{Tag: pub.tag, Existing: desc.Digest}
	if !pub.repeatOnFiles {
		return false, conflict
	}
	return p.takeOver(ctx, pub, desc.Digest, pub.claim.ID, conflict)
}

// pushManifest calls intend and pushes the manifest with its tag in one request, after
// checkTag. The streamed file of UploadStream is committed after intend, the first write to the
// registry, and before the manifest.
func (pub *publish) pushManifest(ctx context.Context, intend func(context.Context) error) error {
	if err := intend(ctx); err != nil {
		return err
	}
	if err := pub.commit(ctx); err != nil {
		return err
	}
	desc := ocispec.Descriptor{MediaType: pub.manifest.MediaType, Digest: pub.digest, Size: int64(len(pub.raw))}
	err := pub.rr.PushReference(ctx, desc, bytes.NewReader(pub.raw), pub.tag)
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
// first, and nothing is written but the audit row, ok with existed in its details. A failure
// is logged, fails the audit row, and is returned as a plain error (500), except a conflict: a
// *index.TagConflictError is returned as it is, and another index.ErrConflict, a package path
// that another spelling holds (#97), stays wrapped (409); and a database that did not answer:
// index.ErrUnavailable stays wrapped (503 with Retry-After), for the same publish again
// completes (#641): the claim is released and the manifest stays under its tag, where a
// frontend's retry, built again with another digest, takes it over (checkTag). The manifest
// stays in the registry and reindex picks it up.
func (p *Pipeline) confirm(ctx context.Context, pub *publish) (existed bool, err error) {
	now := p.now()
	err = p.index.Tx(ctx, func(w index.Writer) error {
		var err error
		existed, err = p.writeRows(ctx, w, pub, now)
		return err
	})
	if err != nil {
		p.log.ErrorContext(ctx, "ingest: the registry accepted the manifest, the index write failed; reindex repairs the index",
			"repo", pub.repo.Format+"/"+pub.repo.Name, "package", pub.claim.Package, "tag", pub.tag,
			"digest", pub.digest.String(), "error", err.Error())
		p.fail(ctx, &pub.ev, fmt.Errorf("index the manifest after the registry accepted it: %w", err))
		p.release(ctx, pub)
		var conflict *index.TagConflictError
		switch {
		case errors.As(err, &conflict):
			return false, err
		case errors.Is(err, index.ErrConflict), errors.Is(err, index.ErrUnavailable):
			return false, fmt.Errorf("ingest: index %s:%s after the registry accepted %s: %w", pub.name, pub.tag, pub.digest, err)
		}
		// The cause is in the message only: an index.ErrNotFound (the repo was deleted
		// meanwhile) must not turn a half-done publish into a 404.
		return false, fmt.Errorf("ingest: index %s:%s after the registry accepted %s: %s", pub.name, pub.tag, pub.digest, err.Error())
	}
	// Post-ingest hooks go here, after the commit. apt signing needs none: the transaction
	// dropped the repo's documents, signed ones included, and the next GET signs again (#41).
	if !existed && p.audit != nil {
		if err := p.audit.Write(ctx, pub.ev); err != nil {
			p.log.ErrorContext(ctx, "ingest: audit event of a publish not written", "repo", pub.ev.Repo, "subject", pub.ev.Subject,
				"digest", pub.digest.String(), "error", err.Error())
		}
	}
	return existed, nil
}

// writeRows writes the rows of confirm on w: package, version, files, OCI repository, the
// invalidated index_cache, the completed audit row when there is no audit.Writer, and the
// released claim.
func (p *Pipeline) writeRows(ctx context.Context, w index.Writer, pub *publish, now time.Time) (existed bool, err error) {
	rows := pub.rows(now)
	pkg := rows.Package
	if err := w.UpsertPackage(ctx, &pkg); err != nil {
		return false, err
	}
	v, err := w.GetVersionByTag(ctx, pkg.ID, pub.tag)
	switch {
	case err == nil && v.Digest == pub.digest:
		if pub.ev.ID != 0 {
			setDetail(&pub.ev, "existed", true)
			if err := p.complete(ctx, w, &pub.ev); err != nil {
				return false, err
			}
		}
		return true, pub.releaseClaims(ctx, w)
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
	if err := p.complete(ctx, w, &pub.ev); err != nil {
		return false, err
	}
	return false, pub.releaseClaims(ctx, w)
}

// releaseClaims deletes pub's claims on its tag and on its package path on w, in the
// transaction that writes the package row, which holds the package path from then on. A claim
// that is gone already (released by a publish that shared it) is fine.
func (pub *publish) releaseClaims(ctx context.Context, w index.Writer) error {
	for _, id := range []int64{pub.claim.ID, pub.pathClaim} {
		if err := w.ReleaseTagClaim(ctx, id); err != nil && !errors.Is(err, index.ErrNotFound) {
			return err
		}
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

// event is the audit event of pub: a push by its principal, or the proxy.fetch of a proxied
// artifact by the principal whose request caused it, the subject name, version and variant.
// push adds the client address and the request id of the request (withOrigin).
func (pub *publish) event(now time.Time) index.AuditEvent {
	a := pub.parsed.Annotations
	subject := a.Name + " " + a.Version
	if a.Variant != "" {
		subject += " " + a.Variant
	}
	action := actionPush
	if pub.origin != nil {
		action = actionProxyFetch
	}
	return index.AuditEvent{At: now, Principal: pub.principal.String(), Action: action,
		Repo: pub.repo.Format + "/" + pub.repo.Name, Subject: subject, Digest: pub.digest}
}
