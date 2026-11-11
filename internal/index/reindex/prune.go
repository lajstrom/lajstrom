package reindex

import (
	"context"
	"errors"
	"time"

	"github.com/lajstrom/lajstrom/internal/index"
	"github.com/lajstrom/lajstrom/internal/provider"
)

// unknownApart is how long after the registry's first answer MANIFEST_UNKNOWN for a manifest
// the answer that removes its row comes at the earliest (docs/spec/guarantees.md, INV-5): the
// runs of two replicas' loops that follow each other are not two answers.
const unknownApart = 10 * time.Minute

// defaultGraceAge is Options.GraceAge when it is zero: config.DefaultReconcileGraceAge.
const defaultGraceAge = time.Hour

// verdict is what a sweep does with a row whose tag the registry did not resolve.
type verdict int

const (
	present   verdict = iota // the registry has the manifest: the row stays, seen
	pending                  // MANIFEST_UNKNOWN, but not yet unknownApart after the first, or the row is younger than the grace age
	gone                     // MANIFEST_UNKNOWN now and at least unknownApart before, the row older than the grace age: it goes
	unchecked                // any other answer, or an error of the index: the row stays
)

// lookup asks the registry for the manifest of a row the run did not see, whose tag the
// registry did not resolve, by its digest (provider.Provider.StatManifest), in the OCI
// repository name; f is the row's finding and indexedAt when ingest wrote the row or a run
// last saw its manifest. Only provider.ErrManifestUnknown counts as unknown: it is recorded in
// prune_sightings, and the row goes once the first such answer is unknownApart old and the
// row graceAge old. A manifest the registry has keeps the row, and is reported with the tag
// it does not resolve; every other answer is reported and keeps the row.
func (s *scan) lookup(ctx context.Context, name string, f Finding, indexedAt time.Time) verdict {
	_, err := s.prov.StatManifest(ctx, name, f.Digest)
	switch {
	case err == nil:
		f.Error = "the registry does not resolve the tag, but has the manifest: the row stays"
		s.failure(f)
		return present
	case !errors.Is(err, provider.ErrManifestUnknown):
		f.Error = "look up the manifest before removing its rows: " + err.Error()
		s.failure(f)
		return unchecked
	}
	now := s.w.now()
	seen := index.Sighting{RepoID: s.repo.ID, Path: name, Digest: f.Digest, LastAt: now}
	if s.dbErr(s.w.index.RecordSighting(ctx, &seen)) {
		return unchecked
	}
	if now.Sub(seen.FirstAt) < unknownApart || now.Sub(indexedAt) < s.w.graceAge {
		s.mu.Lock()
		s.rep.Pending++
		s.mu.Unlock()
		return pending
	}
	return gone
}
