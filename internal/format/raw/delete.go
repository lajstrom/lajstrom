package raw

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/lajstrom/lajstrom/internal/auth"
	"github.com/lajstrom/lajstrom/internal/format"
	"github.com/lajstrom/lajstrom/internal/index"
	"github.com/lajstrom/lajstrom/internal/provider"
)

// actor is who deletes: the principal and the client address, for the audit rows.
type actor struct {
	principal auth.Principal
	ip        string
}

// deletePath removes every version of the file at path p from the registry and the index. A
// backend that cannot delete manifests (Capabilities.DeleteManifest false) is format.ErrUnsupported
// and nothing changes; a path without a version is index.ErrNotFound. Otherwise each version, in
// order: the manifest is deleted in the registry (one that is gone already is fine), then one
// index transaction deletes the version, the package with its last version, invalidates the
// repo's index_cache and records a delete audit row (through deps.Audit after the commit when it
// is set). The registry goes first because it is the truth: an index row of a deleted manifest
// is repaired by reindex, a manifest without its row would come back with it. Blobs are left to
// the registry's garbage collection.
//
// It is one function on its own so that #61, which generalises delete for /v2 and the
// management API, can move it.
func deletePath(ctx context.Context, deps format.Deps, repo index.Repo, p string, who actor) error {
	pkg, err := deps.Index.GetPackage(ctx, repo.ID, p)
	if errors.Is(err, index.ErrNotFound) {
		return fmt.Errorf("%w: raw/%s has no file %q", index.ErrNotFound, repo.Name, p)
	}
	if err != nil {
		return fmt.Errorf("raw: package %q: %w", p, err)
	}
	versions, err := deps.Index.ListVersions(ctx, pkg.ID)
	switch {
	case err != nil:
		return fmt.Errorf("raw: versions of %q: %w", p, err)
	case len(versions) == 0:
		return fmt.Errorf("%w: raw/%s has no version of %q", index.ErrNotFound, repo.Name, p)
	}
	prov, err := registryOf(deps, repo)
	if err != nil {
		return err
	}
	caps, err := prov.Capabilities(ctx)
	if err != nil {
		return fmt.Errorf("raw: capabilities of registry %s: %w", repo.Registry, err)
	}
	if !caps.DeleteManifest {
		return fmt.Errorf("%w: the backend registry cannot delete manifests; nothing was deleted", format.ErrUnsupported)
	}
	name := ociName(repo, pkg.NameOCI)
	rr, err := prov.Repository(ctx, name)
	if err != nil {
		return fmt.Errorf("raw: repository %s: %w", name, err)
	}
	for i, v := range versions {
		if err := deleteManifest(ctx, rr, v); err != nil {
			return err
		}
		if err := deleteRows(ctx, deps, repo, pkg, v, i == len(versions)-1, who); err != nil {
			return err
		}
	}
	return nil
}

// deleteManifest deletes the manifest of v in the registry. It is resolved first, for the size
// and media type a client needs to delete it; a manifest that is gone already is not an error.
func deleteManifest(ctx context.Context, rr provider.Repository, v index.Version) error {
	desc, err := rr.Resolve(ctx, v.Digest.String())
	if err == nil {
		err = rr.Delete(ctx, desc)
	}
	if err != nil && !errors.Is(err, provider.ErrNotFound) {
		return fmt.Errorf("raw: delete manifest %s: %w", v.Digest, err)
	}
	return nil
}

// deleteRows deletes the index rows of version v of pkg in one transaction, the package too when
// last is set, invalidates the repo's index_cache and records the audit row.
func deleteRows(ctx context.Context, deps format.Deps, repo index.Repo, pkg *index.Package, v index.Version, last bool, who actor) error {
	ev := index.AuditEvent{At: time.Now().UTC(), Principal: who.principal.String(), Action: "delete",
		Repo: repo.Format + "/" + repo.Name, Subject: pkg.Name + " " + v.Version, Digest: v.Digest, IP: who.ip}
	err := deps.Index.Tx(ctx, func(w index.Writer) error {
		if err := w.DeleteVersion(ctx, v.ID); err != nil {
			return err
		}
		if last {
			if err := w.DeletePackage(ctx, pkg.ID); err != nil {
				return err
			}
		}
		if err := w.InvalidateIndexCache(ctx, repo.ID, ""); err != nil {
			return err
		}
		if deps.Audit == nil {
			return w.AppendAudit(ctx, &ev)
		}
		return nil
	})
	if err != nil {
		// The cause is in the message only: the manifest is gone from the registry, and a
		// not-found of the index (a concurrent delete) must not turn the request into a 404.
		return fmt.Errorf("raw: index rows of %q %s after the registry deleted %s: %s", pkg.Name, v.Version, v.Digest, err.Error())
	}
	if deps.Audit != nil {
		if err := deps.Audit.Write(ctx, ev); err != nil {
			deps.Log.ErrorContext(ctx, "raw: audit event of a delete not written", "repo", ev.Repo, "subject", ev.Subject,
				"digest", v.Digest.String(), "error", err.Error())
		}
	}
	return nil
}
