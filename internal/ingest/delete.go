package ingest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/lajstrom/lajstrom/internal/auth"
	"github.com/lajstrom/lajstrom/internal/format"
	"github.com/lajstrom/lajstrom/internal/index"
	"github.com/lajstrom/lajstrom/internal/provider"
)

// Delete deletes the manifest that del names in the OCI repository of del.Path below repo, what
// refers to it, and what the index holds of it (docs/spec/http-routing.md §2, DELETE). It
// implements format.Publisher. The backend goes first, because it is the truth: an index row of
// a deleted manifest is repaired by reindex, a manifest without its row would come back with it.
//
//  1. A backend that cannot delete manifests (Capabilities.DeleteManifest false) is
//     provider.ErrUnsupported, and nothing changes.
//  2. The reference, a tag or a digest, is resolved: on a managed repo to a version of the index
//     when it names one (by its tag, or by its digest in the package of del.Path), else in the
//     backend; one that names nothing is index.ErrNotFound.
//  3. The referrers go first, each with its own referrers before it: cosign's legacy manifests
//     under sha256-<hex>.sig, .att and .sbom, the tag-schema index sha256-<hex>, then the
//     referrers the backend lists. Without the referrers API this runs under the referrers lock
//     of the subject (Store.WithReferrersLock). Then the manifest, with every tag that names it.
//     The first request that changes anything is a plain delete, so a backend that refuses
//     deletes refuses it, provider.ErrUnsupported, and nothing has changed: a referrer deleted
//     on its own, without the referrers API, has its subject's sha256-<hex> index deleted first
//     and pushed again without it after. A manifest that is gone already is not an error.
//  4. For a version, one index transaction deletes its rows, its package's when it was the last
//     version, invalidates the repo's index_cache and appends the audit row; for any other
//     manifest the audit row alone is written. The row is a delete by principal with
//     del.ClientIP, the version's subject (or the manifest's subject digest), the digest, and
//     the number of referrers in the details.
//
// Errors wrap provider.ErrUnsupported (the backend does not delete manifests), index.ErrNotFound
// (nothing to delete) or provider.ErrConflict (the backend refused this one delete: Zot keeps a
// manifest that an image index lists), or none. Blobs are left to the backend's garbage
// collection.
func (p *Pipeline) Delete(ctx context.Context, repo index.Repo, del format.Deletion, principal auth.Principal) (*format.Deleted, error) {
	if del.Path == "" || del.Reference == "" {
		return nil, invalid(fmt.Errorf("ingest: delete in %s/%s: a package path and a reference are required", repo.Format, repo.Name))
	}
	prov, err := p.registry(repo)
	if err != nil {
		return nil, err
	}
	caps, err := prov.Capabilities(ctx)
	if err != nil {
		return nil, fmt.Errorf("ingest: capabilities of registry %s: %w", repo.Registry, err)
	}
	name := ociName(repo, del.Path)
	if !caps.DeleteManifest {
		return nil, fmt.Errorf("ingest: delete %s:%s: the registry %s does not delete manifests; nothing was deleted: %w",
			name, del.Reference, repo.Registry, provider.ErrUnsupported)
	}
	rr, err := prov.Repository(ctx, name)
	if err != nil {
		return nil, fmt.Errorf("ingest: repository %s: %w", name, err)
	}
	d := &deletion{p: p, repo: repo, del: del, name: name, rr: rr, api: caps.Referrers}
	if err := d.find(ctx); err != nil {
		return nil, err
	}
	if err := d.deleteInBackend(ctx); err != nil {
		return nil, err
	}
	if err := d.record(ctx, principal); err != nil {
		return nil, err
	}
	p.log.InfoContext(ctx, "ingest: deleted", "repo", repo.Format+"/"+repo.Name, "name", name, "digest", d.target.Digest.String(),
		"version", d.version != nil, "referrers", d.referrers, "principal", principal.String())
	return &format.Deleted{Digest: d.target.Digest, Version: d.version, Referrers: d.referrers}, nil
}

// deletion is one Delete on its way: what it found, then what it deleted.
type deletion struct {
	p    *Pipeline
	repo index.Repo
	del  format.Deletion
	name string              // the OCI repository, without the instance prefix
	rr   provider.Repository // its data path
	api  bool                // the backend has the referrers API

	target  ocispec.Descriptor // the manifest to delete
	gone    bool               // the backend does not have it any more: its rows and referrers go all the same
	version *index.Version     // the version it is; nil for a manifest that is none
	pkg     *index.Package     // the version's package

	referrers int // deleted before it, theirs included
}

// find resolves the reference to a version of the index.
func (d *deletion) find(ctx context.Context) error {
	if err := d.findVersion(ctx); err != nil || d.version != nil {
		return err
	}
	return fmt.Errorf("%w: %s in %s is no version", index.ErrNotFound, d.del.Reference, d.name)
}

// findVersion sets the version the reference names in the package of del.Path, by its digest
// or its tag, and the manifest the backend has of it. A repo of format oci has no versions.
func (d *deletion) findVersion(ctx context.Context) error {
	if d.repo.Format == ociFormat {
		return nil
	}
	pkg, err := d.p.index.GetPackageByNameOCI(ctx, d.repo.ID, d.del.Path)
	if errors.Is(err, index.ErrNotFound) {
		return nil
	} else if err != nil {
		return fmt.Errorf("ingest: package %s: %w", d.del.Path, err)
	}
	d.pkg = pkg
	var v *index.Version
	if strings.Contains(d.del.Reference, ":") {
		dg, perr := digest.Parse(d.del.Reference)
		if perr != nil {
			return invalid(fmt.Errorf("ingest: delete: %q is not a digest: %w", d.del.Reference, perr))
		}
		if v, err = d.p.index.GetVersionByDigest(ctx, d.repo.ID, dg); err == nil && v.PackageID != pkg.ID {
			return nil
		}
	} else {
		v, err = d.p.index.GetVersionByTag(ctx, pkg.ID, d.del.Reference)
	}
	if errors.Is(err, index.ErrNotFound) {
		return nil
	} else if err != nil {
		return fmt.Errorf("ingest: version %s of %s: %w", d.del.Reference, d.del.Path, err)
	}
	d.version = v
	d.target, err = d.rr.Resolve(ctx, v.Digest.String())
	if errors.Is(err, provider.ErrNotFound) {
		d.target, d.gone = ocispec.Descriptor{MediaType: ocispec.MediaTypeImageManifest, Digest: v.Digest}, true
		return nil
	}
	if err != nil {
		return fmt.Errorf("ingest: resolve %s in %s: %w", v.Digest, d.name, err)
	}
	return nil
}

// deleteInBackend deletes the target with its referrers, under the referrers lock of the target
// without the referrers API: its sha256-<hex> index goes.
func (d *deletion) deleteInBackend(ctx context.Context) error {
	if d.api {
		return d.deleteTarget(ctx)
	}
	return d.p.index.WithReferrersLock(ctx, d.name, d.target.Digest, d.deleteTarget)
}

// deleteTarget deletes the target with its referrers, the target itself unless it is gone.
func (d *deletion) deleteTarget(ctx context.Context) error {
	n, err := d.deleteTree(ctx, d.target, !d.gone, map[digest.Digest]bool{})
	d.referrers = n
	return err
}

// deleteTree deletes the referrers of desc, each with its own first, then desc itself unless
// self is false, and returns how many referrers it deleted. It reads before it changes anything:
// the referrers the backend lists, the tag-schema index and cosign's legacy manifests. Then the
// legacy manifests and the index go, which are plain deletes, then the listed referrers, whose
// deletes rewrite no index any more (oras-go rewrites the subject's sha256-<hex> index before it
// deletes a referrer, without the referrers API), then desc. So the first request that changes
// anything is a plain delete, which a backend that refuses deletes refuses.
func (d *deletion) deleteTree(ctx context.Context, desc ocispec.Descriptor, self bool, seen map[digest.Digest]bool) (int, error) {
	seen[desc.Digest] = true
	refs, err := d.listReferrers(ctx, desc)
	if err != nil {
		return 0, err
	}
	schema := "sha256-" + desc.Digest.Encoded()
	idx, err := d.resolveTag(ctx, schema)
	if err != nil {
		return 0, err
	}
	var legacy []ocispec.Descriptor
	for _, suffix := range []string{".sig", ".att", ".sbom"} {
		l, err := d.resolveTag(ctx, schema+suffix)
		if err != nil {
			return 0, err
		}
		if l != nil {
			legacy = append(legacy, *l)
		}
	}
	n := 0
	deleteAll := func(descs []ocispec.Descriptor) error {
		for _, r := range descs {
			if seen[r.Digest] {
				continue
			}
			k, err := d.deleteTree(ctx, r, true, seen)
			if n += k; err != nil {
				return err
			}
			n++
		}
		return nil
	}
	if err := deleteAll(legacy); err != nil {
		return n, err
	}
	if idx != nil {
		seen[idx.Digest] = true
		if err := d.deleteOne(ctx, *idx); err != nil {
			return n, err
		}
	}
	if err := deleteAll(refs); err != nil {
		return n, err
	}
	if !self {
		return n, nil
	}
	return n, d.deleteOne(ctx, desc)
}

// listReferrers returns the referrers of desc the backend lists: its referrers API, or the
// tag-schema index without it.
func (d *deletion) listReferrers(ctx context.Context, desc ocispec.Descriptor) ([]ocispec.Descriptor, error) {
	var refs []ocispec.Descriptor
	err := d.rr.Referrers(ctx, desc, "", func(page []ocispec.Descriptor) error {
		refs = append(refs, page...)
		return nil
	})
	if err != nil && !errors.Is(err, provider.ErrNotFound) {
		return nil, fmt.Errorf("ingest: referrers of %s in %s: %w", desc.Digest, d.name, err)
	}
	return refs, nil
}

// resolveTag returns the manifest tag names in the repository, or nil when there is none.
func (d *deletion) resolveTag(ctx context.Context, tag string) (*ocispec.Descriptor, error) {
	desc, err := d.rr.Resolve(ctx, tag)
	switch {
	case errors.Is(err, provider.ErrNotFound):
		return nil, nil
	case err != nil:
		return nil, fmt.Errorf("ingest: resolve %s in %s: %w", tag, d.name, err)
	}
	return &desc, nil
}

// deleteOne deletes the manifest desc; one that is gone already is not an error.
func (d *deletion) deleteOne(ctx context.Context, desc ocispec.Descriptor) error {
	if err := d.rr.Delete(ctx, desc); err != nil && !errors.Is(err, provider.ErrNotFound) {
		return fmt.Errorf("ingest: delete %s in %s: %w", desc.Digest, d.name, err)
	}
	return nil
}

// record writes what the index holds of the delete: for a version, its rows go in one
// transaction, the package's with its last version, the repo's index_cache is invalidated and
// the audit row appended; for any other manifest the audit row alone. The audit row goes to
// the audit.Writer after the commit when there is one. The backend has deleted already, so a
// failure of the transaction is an error without a sentinel (500): reindex repairs the rows.
func (d *deletion) record(ctx context.Context, principal auth.Principal) error {
	details, _ := json.Marshal(map[string]int{"referrers": d.referrers}) // an int map always marshals
	ev := index.AuditEvent{At: d.p.now(), Principal: principal.String(), Action: "delete",
		Repo: d.repo.Format + "/" + d.repo.Name, Digest: d.target.Digest, IP: d.del.ClientIP, Details: details}
	switch {
	case d.version != nil:
		ev.Subject = d.pkg.Name + " " + d.version.Version
		if d.version.Variant != "" {
			ev.Subject += " " + d.version.Variant
		}
	default:
		ev.Subject = d.name
	}
	err := d.p.index.Tx(ctx, func(w index.Writer) error {
		if d.version != nil {
			if err := d.deleteRows(ctx, w); err != nil {
				return err
			}
		}
		if d.p.audit == nil {
			return w.AppendAudit(ctx, &ev)
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("ingest: index rows of %s after the registry deleted it: %s", d.target.Digest, err.Error())
	}
	if d.p.audit != nil {
		if err := d.p.audit.Write(ctx, ev); err != nil {
			d.p.log.ErrorContext(ctx, "ingest: audit event of a delete not written", "repo", ev.Repo, "subject", ev.Subject,
				"digest", ev.Digest.String(), "error", err.Error())
		}
	}
	return nil
}

// deleteRows deletes the version's rows on w, its package's when no version is left, and
// invalidates the repo's index_cache. A row another delete took meanwhile is no error.
func (d *deletion) deleteRows(ctx context.Context, w index.Writer) error {
	if err := w.DeleteVersion(ctx, d.version.ID); err != nil && !errors.Is(err, index.ErrNotFound) {
		return err
	}
	rest, err := w.ListVersions(ctx, d.pkg.ID)
	if err != nil {
		return err
	}
	if len(rest) == 0 {
		if err := w.DeletePackage(ctx, d.pkg.ID); err != nil && !errors.Is(err, index.ErrNotFound) {
			return err
		}
	}
	return w.InvalidateIndexCache(ctx, d.repo.ID, "")
}
