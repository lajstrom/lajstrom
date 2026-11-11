package reindex

import (
	"context"
	"errors"
	"maps"
	"net/url"
	"slices"
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
	present     verdict = iota // the registry has the manifest: the row stays, seen
	pending                    // MANIFEST_UNKNOWN, but not yet unknownApart after the first, or the row is younger than the grace age
	gone                       // MANIFEST_UNKNOWN now and at least unknownApart before, the row older than the grace age: it goes
	unchecked                  // any other answer, or an error of the index: the row stays
	nameUnknown                // a 404 without MANIFEST_UNKNOWN in a repository whose tag list is a 404: the row stays, the caller reports it
)

// lookup asks the registry for the manifest of a row the run did not see, whose tag the
// registry did not resolve, by its digest (provider.Provider.StatManifest), in the OCI
// repository name; f is the row's finding and indexedAt when ingest wrote the row or a run
// last saw its manifest. Only provider.ErrManifestUnknown counts as unknown: it is recorded in
// prune_sightings, and the row goes once the first such answer is unknownApart old and the
// row graceAge old. A manifest the registry has keeps the row, and is reported with the tag
// it does not resolve. A 404 without the code in a repository the registry does not know
// (repoUnknown) keeps the row, for the caller to report (nameUnknown); every other answer is
// reported and keeps the row.
func (s *scan) lookup(ctx context.Context, name string, f Finding, indexedAt time.Time) verdict {
	_, err := s.prov.StatManifest(ctx, name, f.Digest)
	switch {
	case err == nil:
		f.Error = "the registry does not resolve the tag, but has the manifest: the row stays"
		s.failure(f)
		return present
	case errors.Is(err, provider.ErrNotFound) && !errors.Is(err, provider.ErrManifestUnknown) && s.repoUnknown(ctx, name):
		return nameUnknown
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

// errListed stops the read of a tag list at its first page: repoUnknown needs only its status.
var errListed = errors.New("reindex: the tag list answered")

// repoUnknown reports whether the registry does not know the OCI repository name: its tag list
// answered 404 (NAME_UNKNOWN on Zot and distribution's filesystem storage), in this run's
// ociRepo or now. Each repository is asked once a run; an answer that is neither a list nor a
// 404 is not kept and is not unknown.
func (s *scan) repoUnknown(ctx context.Context, name string) bool {
	s.mu.Lock()
	known, asked := s.listed[name]
	s.mu.Unlock()
	if asked {
		return !known
	}
	rr, err := s.prov.Repository(ctx, name)
	if err == nil {
		err = rr.Tags(ctx, "", func([]string) error { return errListed })
	}
	switch {
	case err == nil, errors.Is(err, errListed):
		s.tagList(name, true)
		return false
	case errors.Is(err, provider.ErrNotFound):
		s.tagList(name, false)
		return true
	}
	return false
}

// tagList records the answer of the tag list of the OCI repository name: known, or a 404.
func (s *scan) tagList(name string, known bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.listed[name] = known
}

// unknownRepository reports the version of m, whose OCI repository the registry does not know
// (repoUnknown), with the request of the management API that deletes it; its rows stay.
func (s *scan) unknownRepository(m index.VersionMatch) {
	call := "DELETE /api/v1/repos/" + s.repo.Format + "/" + s.repo.Name + "/packages/" + url.PathEscape(m.Package.Name) +
		"/versions/" + url.PathEscape(m.Version.Version)
	if m.Version.Variant != "" {
		call += "?variant=" + url.QueryEscape(m.Version.Variant)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	add(&s.rep, &s.rep.RepositoryUnknown, RepositoryUnknown{Repository: m.Package.NameOCI, Package: m.Package.Name,
		Version: m.Version.Version, Variant: m.Version.Variant, Tag: m.Version.Tag, Digest: m.Version.Digest, Delete: call})
	s.unknown[m.Package.NameOCI]++
}

// warnUnknown logs a warning for each OCI repository the registry does not know whose versions
// the sweep reported: prune never removes them.
func (s *scan) warnUnknown(ctx context.Context) {
	for _, name := range slices.Sorted(maps.Keys(s.unknown)) {
		s.w.log.WarnContext(ctx, "reindex: the registry does not know the OCI repository of versions in the index; prune keeps "+
			"their rows: delete them through the API (reindex --report lists the requests), or restore the repository",
			"repo", s.rep.Repo, "repository", name, "versions", s.unknown[name])
	}
}
