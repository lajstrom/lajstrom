package ingest

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/lajstrom/lajstrom/internal/auth"
	"github.com/lajstrom/lajstrom/internal/index"
	"github.com/lajstrom/lajstrom/internal/oci/profile"
	"github.com/lajstrom/lajstrom/internal/provider"
)

// Mirror is a manifest or an index that an oci proxy repo fetched from its upstream, to keep as
// it came: no manifest of the profile and no annotation (ADR 0018,
// docs/spec/proxy-and-group.md §11.5).
type Mirror struct {
	// Path is the upstream's repository below the repo: the OCI repository is the repo's base
	// and Path.
	Path string
	// Desc is the media type, digest and size of Manifest, the exact bytes.
	Desc     ocispec.Descriptor
	Manifest []byte
	// Tag is the upstream's tag that names Desc, to push or move to it; "" for none.
	Tag string
	// Upstream is upstreams[].name, for the audit row.
	Upstream string
}

// MirrorManifest stores m in the backend of repo, an oci proxy repo, as it came, once every
// blob or child it lists is held (the caller's to know; the backend refuses it otherwise):
//
//  1. it is pushed under its keep-tag (profile.KeepTag), written once and never moved, under
//     the tag lock of its digest, which a delete holds while it counts the manifest's tags;
//  2. with m.Tag, the upstream's tag is pushed too, under the same lock, unless the backend's
//     tag names the digest already: this moves a tag the backend holds, the one exception to
//     "never re-point a tag" (§11.6), and only here. A push of the tag that the backend
//     refuses as a conflict (provider.ErrConflict: an immutable tag) leaves the manifest held
//     and is the error, wrapping ErrConflict, for the caller to remember;
//  3. what was pushed is read back until the registry serves it (awaitVisible);
//  4. oci_repos records the OCI repository, and the audit row proxy.fetch, written pending
//     before the push, is completed: the OCI name as its subject, the digest, and the upstream,
//     the reference and the tag in its details.
//
// The bytes must have m.Desc's digest and size (ErrInvalid). Other errors are the backend's
// and the index's; a failure after the push leaves it for the next call, which repeats it.
func (p *Pipeline) MirrorManifest(ctx context.Context, repo index.Repo, m Mirror, principal auth.Principal) error {
	listed, err := parseMirror(m)
	if err != nil {
		return err
	}
	prov, rr, name, err := p.mirrorTarget(ctx, repo, m.Path)
	if err != nil {
		return err
	}
	details := map[string]any{"upstream": m.Upstream, "ref": m.Desc.Digest.String()}
	if m.Tag != "" {
		details["tag"] = m.Tag
	}
	ev := index.AuditEvent{At: p.now(), Principal: principal.String(), Action: actionProxyFetch, Repo: repo.Format + "/" + repo.Name,
		Subject: name, Digest: m.Desc.Digest}
	withOrigin(ctx, &ev, details)
	if err := p.intend(ctx, &ev); err != nil {
		return err
	}
	tagErr, err := p.pushMirror(ctx, rr, name, m)
	if err != nil {
		p.fail(ctx, &ev, err)
		return err
	}
	tag := m.Tag
	if tagErr != nil {
		tag = ""
		setDetail(&ev, "tagRefused", true)
	}
	if err := p.awaitVisible(ctx, pushed{name: name, rr: rr, prov: prov, manifest: m.Desc, tag: tag, blobs: listed}); err != nil {
		p.fail(ctx, &ev, err)
		return err
	}
	p.recordMirror(ctx, repo, name, &ev)
	p.log.InfoContext(ctx, "ingest: mirrored from the upstream", "repo", ev.Repo, "name", name, "digest", m.Desc.Digest.String(),
		"tag", tag, "upstream", m.Upstream, "principal", principal.String())
	return tagErr
}

// parseMirror checks that m's bytes are its descriptor's and returns what it lists: a
// manifest's config and layers, an index's manifests.
func parseMirror(m Mirror) ([]ocispec.Descriptor, error) {
	d := m.Desc.Digest
	if err := d.Validate(); err != nil || d.Algorithm().FromBytes(m.Manifest) != d || int64(len(m.Manifest)) != m.Desc.Size {
		return nil, invalid(fmt.Errorf("the manifest's bytes are not %s of %d bytes", d, m.Desc.Size))
	}
	var doc struct {
		Config    *ocispec.Descriptor  `json:"config"`
		Layers    []ocispec.Descriptor `json:"layers"`
		Manifests []ocispec.Descriptor `json:"manifests"`
	}
	if err := json.Unmarshal(m.Manifest, &doc); err != nil {
		return nil, invalid(fmt.Errorf("the manifest %s is not JSON: %w", d, err))
	}
	listed := append(doc.Manifests, doc.Layers...)
	if doc.Config != nil {
		listed = append(listed, *doc.Config)
	}
	return listed, nil
}

// mirrorTarget returns the registry of repo, which must be an oci proxy repo, and its
// repository of path, whose name it checks against the registry's limits and ensures.
func (p *Pipeline) mirrorTarget(ctx context.Context, repo index.Repo, path string) (provider.Provider, provider.Repository, string, error) {
	if repo.Format != ociFormat || repo.Kind != index.KindProxy {
		return nil, nil, "", invalid(fmt.Errorf("%s/%s is a %s repo, not an oci proxy", repo.Format, repo.Name, repo.Kind))
	}
	prov, err := p.registry(repo)
	if err != nil {
		return nil, nil, "", err
	}
	name := ociName(repo, path)
	if err := checkName(ctx, prov, name); err != nil {
		return nil, nil, "", err
	}
	if err := prov.EnsureRepository(ctx, name); err != nil {
		return nil, nil, "", fmt.Errorf("ingest: ensure repository %s: %w", name, err)
	}
	rr, err := prov.Repository(ctx, name)
	if err != nil {
		return nil, nil, "", fmt.Errorf("ingest: repository %s: %w", name, err)
	}
	return prov, rr, name, nil
}

// pushMirror pushes m under its keep-tag and, with m.Tag, under the upstream's tag, under the
// tag lock of its digest. tagErr is the backend's refusal of m.Tag, which leaves the manifest
// held; err is any other failure.
func (p *Pipeline) pushMirror(ctx context.Context, rr provider.Repository, name string, m Mirror) (tagErr, err error) {
	err = p.index.WithReferrersLock(ctx, name, m.Desc.Digest, func(ctx context.Context) error {
		if err := pushTag(ctx, rr, m, profile.KeepTag(m.Desc.Digest)); err != nil {
			return fmt.Errorf("ingest: %s@%s under its keep-tag: %w", name, m.Desc.Digest, err)
		}
		if m.Tag == "" {
			return nil
		}
		err := pushTag(ctx, rr, m, m.Tag)
		if errors.Is(err, provider.ErrConflict) {
			tagErr = fmt.Errorf("ingest: the registry refused to point the tag %s of %s at %s: %w: %w", m.Tag, name, m.Desc.Digest,
				ErrConflict, err)
			return nil
		}
		if err != nil {
			return fmt.Errorf("ingest: %s@%s under the tag %s: %w", name, m.Desc.Digest, m.Tag, err)
		}
		return nil
	})
	return tagErr, err
}

// pushTag points tag at m in rr, unless it names m already: ECR refuses the push of an image
// it has under the tag as a conflict.
func pushTag(ctx context.Context, rr provider.Repository, m Mirror, tag string) error {
	if desc, err := rr.Resolve(ctx, tag); err == nil && desc.Digest == m.Desc.Digest {
		return nil
	} else if err != nil && !errors.Is(err, provider.ErrNotFound) {
		return err
	}
	return rr.PushReference(ctx, m.Desc, bytes.NewReader(m.Manifest), tag)
}

// recordMirror records the OCI repository name, the full OCI name as reindex and the facade write
// it (index-schema.md, oci_repos), in oci_repos and completes the audit row ev, in one
// transaction. The backend holds the manifest and is the truth: a failure is logged, the
// row completed on its own, and reindex records the repository.
func (p *Pipeline) recordMirror(ctx context.Context, repo index.Repo, name string, ev *index.AuditEvent) {
	err := p.index.Tx(ctx, func(w index.Writer) error {
		if err := w.UpsertOCIRepo(ctx, &index.OCIRepo{RepoID: repo.ID, Path: name, SeenAt: p.now()}); err != nil {
			return err
		}
		return p.complete(ctx, w, ev)
	})
	if err != nil {
		p.log.ErrorContext(ctx, "ingest: record a mirrored manifest in oci_repos and the audit log", "name", ev.Subject,
			"digest", ev.Digest.String(), "error", err.Error())
		p.auditReferrer(ctx, ev)
	}
}

// MirrorBlob streams the blob desc from r into the backend of repo, an oci proxy repo, at
// path: into an upload session (ADR 0014; nothing is spooled), hashed on the way, and
// committed with its digest only when the bytes have desc's size and digest; then it is read
// back until the registry serves it. Other bytes are ErrInvalid, and nothing is committed. A
// manifest held or cached at path lists the blob, which is the caller's to know. MirrorBlob
// writes no index row and no audit row: the push of the manifest that lists the blob does.
func (p *Pipeline) MirrorBlob(ctx context.Context, repo index.Repo, path string, desc ocispec.Descriptor, r io.Reader) error {
	if err := desc.Digest.Validate(); err != nil || desc.Size < 0 {
		return invalid(fmt.Errorf("the blob %q of %d bytes is not a descriptor", desc.Digest, desc.Size))
	}
	prov, rr, name, err := p.mirrorTarget(ctx, repo, path)
	if err != nil {
		return err
	}
	w, err := rr.OpenBlobWriter(ctx)
	if err != nil {
		return fmt.Errorf("ingest: blob %s of %s: %w", desc.Digest, name, err)
	}
	if err := writeBlob(ctx, w, desc, r); err != nil {
		_ = w.Cancel(context.WithoutCancel(ctx))
		return fmt.Errorf("ingest: blob %s of %s: %w", desc.Digest, name, err)
	}
	return p.awaitBlob(ctx, prov, rr, name, desc)
}

// MirrorMount mounts the blob desc into path of repo, an oci proxy repo, from the path from of
// the same repo, where the backend holds it: POST …/blobs/uploads/?mount=&from=, and no byte
// moves (proxy-and-group.md §11.5). mounted is false, with no error, when the backend opened a
// session instead (202: Nexus's docker format, JFrog, ECR without BLOB_MOUNTING, a source that
// does not hold the blob), which is cancelled; then the caller fetches the blob. A mounted blob
// is read back as MirrorBlob reads one back. MirrorMount writes no index row and no audit row.
func (p *Pipeline) MirrorMount(ctx context.Context, repo index.Repo, path, from string, desc ocispec.Descriptor) (mounted bool, err error) {
	if err := desc.Digest.Validate(); err != nil || desc.Size < 0 || from == path {
		return false, invalid(fmt.Errorf("a mount of %q of %d bytes from %q into %q", desc.Digest, desc.Size, from, path))
	}
	prov, rr, name, err := p.mirrorTarget(ctx, repo, path)
	if err != nil {
		return false, err
	}
	mounted, u, err := rr.MountBlob(ctx, desc.Digest, ociName(repo, from))
	switch {
	case err != nil:
		return false, fmt.Errorf("ingest: mount the blob %s into %s: %w", desc.Digest, name, err)
	case !mounted:
		if err := rr.CancelBlobUpload(context.WithoutCancel(ctx), u); err != nil && !errors.Is(err, provider.ErrUnsupported) {
			p.log.DebugContext(ctx, "ingest: the session the registry opened instead of a mount not cancelled; it expires", "name", name,
				"error", err.Error())
		}
		return false, nil
	}
	return true, p.awaitBlob(ctx, prov, rr, name, desc)
}

// awaitBlob reads the blob desc, committed or mounted at name, back until the registry serves
// it, on a registry that is not strongly consistent.
func (p *Pipeline) awaitBlob(ctx context.Context, prov provider.Provider, rr provider.Repository, name string, desc ocispec.Descriptor) error {
	caps, err := prov.Capabilities(ctx)
	if err != nil || caps.Consistency == provider.ConsistencyStrong {
		return nil //nolint:nilerr // committed; the push of the manifest meets a registry that does not answer
	}
	ok, err := provider.ReadBack{Timeout: max(p.visibility, caps.ConsistencyBound)}.Await(ctx, func(ctx context.Context) (bool, error) {
		return rr.Exists(ctx, desc)
	})
	switch {
	case err != nil:
		return fmt.Errorf("ingest: read back the blob %s of %s: %w", desc.Digest, name, err)
	case !ok:
		return &NotVisibleError{Name: name, Digest: desc.Digest, Blobs: []digest.Digest{desc.Digest}, Waited: max(p.visibility, caps.ConsistencyBound)}
	}
	return nil
}

// writeBlob copies r through w, hashed, and commits w when the bytes are desc's; their size and
// digest otherwise are ErrInvalid, and w is the caller's to cancel.
func writeBlob(ctx context.Context, w provider.BlobWriter, desc ocispec.Descriptor, r io.Reader) error {
	h := desc.Digest.Algorithm().Hash()
	n, err := io.Copy(w, io.TeeReader(io.LimitReader(r, desc.Size+1), h))
	switch {
	case err != nil:
		return err
	case n != desc.Size:
		return invalid(fmt.Errorf("%d bytes, not the %d its manifest says", min(n, desc.Size+1), desc.Size))
	}
	if got := digest.NewDigest(desc.Digest.Algorithm(), h); got != desc.Digest {
		return invalid(fmt.Errorf("the bytes are %s", got))
	}
	_, err = w.Commit(ctx, desc.Digest)
	return err
}
