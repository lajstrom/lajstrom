package ingest

import (
	"context"
	"fmt"

	"github.com/lajstrom/lajstrom/internal/auth"
	"github.com/lajstrom/lajstrom/internal/format"
	"github.com/lajstrom/lajstrom/internal/index"
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
//
// This is the contract; the implementation comes in the next pull request of #61.
func (p *Pipeline) Delete(_ context.Context, _ index.Repo, _ format.Deletion, _ auth.Principal) (*format.Deleted, error) {
	return nil, fmt.Errorf("ingest: delete: %w", ErrNotImplemented)
}
