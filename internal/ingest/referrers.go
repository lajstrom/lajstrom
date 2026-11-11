package ingest

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/lajstrom/lajstrom/internal/auth"
	"github.com/lajstrom/lajstrom/internal/index"
	"github.com/lajstrom/lajstrom/internal/oci/profile"
	"github.com/lajstrom/lajstrom/internal/provider"
)

// ociFormat is the format of the pass-through repos (format.Reserved), which have no plugin.
const ociFormat = "oci"

// Referrer is a manifest that refers to another, its subject: a signature, an SBOM or an
// attestation (docs/spec/manifest-profile.md §6). The facade decides that a manifest it got is
// one (profile.IsReferrer, or any manifest with a subject on an oci repo).
type Referrer struct {
	// Path is the package path below the repo: the OCI repository is the repo's prefix and Path.
	Path string
	// Tag is the tag it was pushed under, "" for a push by digest. On a managed repo only
	// cosign's legacy tag (profile.CosignTag) is one.
	Tag string
	// Desc is the manifest's media type, digest and size; Manifest its bytes as received.
	Desc     ocispec.Descriptor
	Manifest []byte
	// Subject is the manifest it refers to: its subject, or the manifest cosign's tag names.
	Subject digest.Digest
}

// PutReferrer stores the referrer r of a manifest of repo as the client pushed it, and indexes
// nothing: no derived table holds referrers before #89 (docs/spec/index-schema.md). On a
// managed repo the subject must be a manifest of the repository, the config and the layers
// must be in the backend, and a tag must be cosign's; an oci repo takes the referrer as it
// comes, as a registry does. When the backend has no referrers API (Capabilities.Referrers) and
// the manifest has a subject, the push, which also rewrites the sha256-<hex> index of the
// subject, runs under the referrers lock of the index (Store.WithReferrersLock); a push by tag to
// an oci repo runs under it with the API too, the tag lock that a delete holds. Its audit row,
// a push by principal whose subject is the subject's digest, with the client address, and the
// referrer's kind and artifact type and the request id in the details, is written pending
// before the push and completed after it, ok or failed; a repeat writes another. Before the
// row is completed the referrer reads back until the registry serves it, among its subject's
// referrers too (awaitVisible, for twice the visibility timeout). Errors wrap ErrInvalid (the
// checks), ErrConflict (a tag the backend will not re-point) or provider.ErrUnavailable (the
// registry did not serve the referrer in time: 503 with Retry-After; the push stays, and the
// same push again completes it).
func (p *Pipeline) PutReferrer(ctx context.Context, repo index.Repo, r Referrer, principal auth.Principal) (digest.Digest, error) {
	m, err := parseReferrer(r)
	if err != nil {
		return "", err
	}
	prov, err := p.registry(repo)
	if err != nil {
		return "", err
	}
	name := ociName(repo, r.Path)
	if repo.Format != ociFormat {
		if err := p.checkReferrer(ctx, prov, name, r, m); err != nil {
			return "", err
		}
	}

	kind := profile.ReferrerKind(r.Tag, m)
	artifactType := cmp.Or(m.ArtifactType, m.Config.MediaType)
	ev := index.AuditEvent{At: p.now(), Principal: principal.String(), Action: "push", Repo: repo.Format + "/" + repo.Name,
		Subject: r.Subject.String(), Digest: r.Desc.Digest}
	withOrigin(ctx, &ev, map[string]any{"kind": kind, "artifactType": artifactType})
	if err := p.intend(ctx, &ev); err != nil {
		return "", err
	}
	if err := p.pushReferrer(ctx, prov, name, r, m.Subject != nil, repo.Format == ociFormat && r.Tag != ""); err != nil {
		p.fail(ctx, &ev, err)
		return "", err
	}
	if err := p.awaitReferrer(ctx, prov, name, r, m); err != nil {
		p.fail(ctx, &ev, err)
		return "", err
	}
	p.log.InfoContext(ctx, "ingest: referrer stored", "repo", repo.Format+"/"+repo.Name, "name", name, "digest", r.Desc.Digest.String(),
		"subject", r.Subject.String(), "kind", kind, "artifactType", artifactType, "principal", principal.String())
	p.auditReferrer(ctx, &ev)
	return r.Desc.Digest, nil
}

// awaitReferrer reads the referrer r, whose manifest is m, back from the repository name of prov
// until the registry serves it, among its subject's referrers too (awaitVisible).
func (p *Pipeline) awaitReferrer(ctx context.Context, prov provider.Provider, name string, r Referrer, m ocispec.Manifest) error {
	rr, err := prov.Repository(ctx, name)
	if err != nil {
		return fmt.Errorf("ingest: repository %s: %w", name, err)
	}
	return p.awaitVisible(ctx, pushed{name: name, rr: rr, prov: prov, manifest: r.Desc, tag: r.Tag,
		blobs: append([]ocispec.Descriptor{m.Config}, m.Layers...), subject: m.Subject})
}

// parseReferrer returns the manifest of r, whose bytes must be its digest and whose subject,
// when it has one, r's.
func parseReferrer(r Referrer) (ocispec.Manifest, error) {
	var m ocispec.Manifest
	if err := json.Unmarshal(r.Manifest, &m); err != nil {
		return m, invalid(fmt.Errorf("the referrer is not JSON: %w", err))
	}
	if err := r.Desc.Digest.Validate(); err != nil || r.Desc.Digest.Algorithm().FromBytes(r.Manifest) != r.Desc.Digest {
		return m, invalid(fmt.Errorf("the referrer's bytes are not %s", r.Desc.Digest))
	}
	if m.Subject != nil && m.Subject.Digest != r.Subject {
		return m, invalid(fmt.Errorf("the subject of the manifest is %s, not %s", m.Subject.Digest, r.Subject))
	}
	return m, nil
}

// pushReferrer pushes r to the repository name, by digest or under its tag. When the manifest
// has a subject and the backend no referrers API, the provider's push also rewrites the
// sha256-<hex> index of the subject, and the whole push, the repository included, runs under the
// referrers lock of the subject. A push by tag to an oci repo (tagLock) holds the tag lock of the
// manifest with the API too, which a delete holds while it counts the tags: the lock of its
// subject, else of its own digest (docs/spec/index-schema.md, Coordination).
func (p *Pipeline) pushReferrer(ctx context.Context, prov provider.Provider, name string, r Referrer, hasSubject, tagLock bool) error {
	push := func(ctx context.Context) error {
		rr, err := prov.Repository(ctx, name)
		if err != nil {
			return fmt.Errorf("ingest: repository %s: %w", name, err)
		}
		if r.Tag != "" {
			return rr.PushReference(ctx, r.Desc, bytes.NewReader(r.Manifest), r.Tag)
		}
		return rr.Push(ctx, r.Desc, bytes.NewReader(r.Manifest))
	}
	caps, err := prov.Capabilities(ctx)
	if err != nil {
		return fmt.Errorf("ingest: capabilities of the registry of %s: %w", name, err)
	}
	key := r.Subject
	if !hasSubject {
		key = r.Desc.Digest
	}
	if (hasSubject && !caps.Referrers) || tagLock {
		err = p.index.WithReferrersLock(ctx, name, key, push)
	} else {
		err = push(ctx)
	}
	switch {
	case errors.Is(err, provider.ErrConflict):
		return fmt.Errorf("ingest: referrer %s under %s:%s: %w: %w", r.Desc.Digest, name, r.Tag, ErrConflict, err)
	case err != nil:
		return fmt.Errorf("ingest: referrer %s of %s: %w", r.Desc.Digest, name, err)
	}
	return nil
}

// checkReferrer runs the checks of a referrer of a managed repo: a tag must be cosign's and name
// the subject, the subject must be a manifest of the repository name, and the config and
// layers of m must be in the backend.
func (p *Pipeline) checkReferrer(ctx context.Context, prov provider.Provider, name string, r Referrer, m ocispec.Manifest) error {
	if r.Tag != "" {
		if subject, _, ok := profile.CosignTag(r.Tag); !ok || subject != r.Subject {
			return invalid(fmt.Errorf("a referrer of %s is pushed by digest, or under cosign's tag sha256-<hex>.sig, .att or .sbom of its subject, not under the tag %q", name, r.Tag))
		}
	}
	rr, err := prov.Repository(ctx, name)
	if err != nil {
		return fmt.Errorf("ingest: repository %s: %w", name, err)
	}
	if _, err := rr.Resolve(ctx, r.Subject.String()); errors.Is(err, provider.ErrNotFound) {
		return invalid(fmt.Errorf("the subject %s is not a manifest of %s: push the artifact before what refers to it", r.Subject, name))
	} else if err != nil {
		return fmt.Errorf("ingest: subject %s of %s: %w", r.Subject, name, err)
	}
	for _, b := range append([]ocispec.Descriptor{m.Config}, m.Layers...) {
		ok, err := rr.Exists(ctx, b)
		switch {
		case err != nil:
			return fmt.Errorf("ingest: blob %s of %s: %w", b.Digest, name, err)
		case !ok:
			return invalid(fmt.Errorf("blob %s was not uploaded to %s", b.Digest, name))
		}
	}
	return nil
}

// auditReferrer completes the audit row of a referrer push, ok, in a transaction of its own:
// no index row records a referrer. With an audit.Writer the event goes to it instead. The
// referrer is stored whatever happens here, so a failure is logged, and the row stays pending
// until it is unknown.
func (p *Pipeline) auditReferrer(ctx context.Context, ev *index.AuditEvent) {
	var err error
	if p.audit != nil {
		err = p.audit.Write(ctx, *ev)
	} else {
		err = p.complete(context.WithoutCancel(ctx), p.index, ev)
	}
	if err != nil {
		p.log.ErrorContext(ctx, "ingest: audit event of a referrer push not written", "repo", ev.Repo, "subject", ev.Subject,
			"digest", ev.Digest.String(), "error", err.Error())
	}
}

// registry returns the provider of repo's registry.
func (p *Pipeline) registry(repo index.Repo) (provider.Provider, error) {
	var prov provider.Provider
	ok := false
	if p.providers != nil {
		prov, ok = p.providers.Provider(repo.Registry)
	}
	if prov == nil || !ok {
		return nil, fmt.Errorf("ingest: repo %s/%s: registry %q is not configured", repo.Format, repo.Name, repo.Registry)
	}
	return prov, nil
}
