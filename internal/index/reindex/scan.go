package reindex

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/lajstrom/lajstrom/internal/format"
	"github.com/lajstrom/lajstrom/internal/index"
	"github.com/lajstrom/lajstrom/internal/ingest"
	"github.com/lajstrom/lajstrom/internal/oci/profile"
	"github.com/lajstrom/lajstrom/internal/provider"
)

// maxManifest is the largest manifest a run reads, the limit registries apply to a manifest PUT.
const maxManifest = 4 << 20

// progressEvery is how many manifests a run reads between two progress lines in the log.
const progressEvery = 1000

// tagSchema matches the tags of the referrers tag schema (manifest-profile.md §6), sha256-<hex>,
// and the tags cosign gives its signatures, attestations and SBOMs on a registry without the
// referrers API, sha256-<hex>.sig, .att, .sbom. Both attach to the manifest sha256:<hex>.
var tagSchema = regexp.MustCompile(`^sha256-([0-9a-f]{64})(\.[a-z]+)?$`)

// scan is the reindex of one repo by one run.
type scan struct {
	w     *Worker
	repo  index.Repo
	prov  provider.Provider
	base  string // the repo's base path in the registry (manifest-profile.md §4.1)
	req   Request
	start time.Time // the run's start: rows not seen since are unseen
	began time.Time // when this repo's scan began, for the rate in the log
	sem   chan struct{}
	// recorded is the oci_repos rows the listing came from on a registry without a catalog.
	recorded map[string]bool

	mu          sync.Mutex
	rep         RepoReport
	seen        []int64                    // version rows seen without being written
	referrers   map[digest.Digest]struct{} // a referrer is listed under its own tag and in a tag-schema index
	subjects    []Finding                  // the referrers read, whose subjects orphans looks up
	fatal       error                      // the first error of the run's own: the database, the context
	unsupported bool                       // no plugin for the repo's format: the scan stops, and nothing is swept
}

// repo reindexes one repo. The error is the run's own; everything about the registry and the
// manifests is in the report.
func (w *Worker) repo(ctx context.Context, repo index.Repo, start time.Time, req Request) (RepoReport, error) {
	s := &scan{w: w, repo: repo, base: repoBase(repo), req: req, start: start, began: w.now(),
		sem: make(chan struct{}, w.concurrency), rep: newRepoReport(repo.Format + "/" + repo.Name),
		referrers: map[digest.Digest]struct{}{}}
	prov, ok := w.providers.Provider(repo.Registry)
	if !ok || prov == nil {
		s.failure(Finding{Error: fmt.Sprintf("registry %q is not configured", repo.Registry)})
		return s.rep, nil
	}
	s.prov = prov
	names, listed := s.list(ctx)
	s.rep.OCIRepositories = len(names)
	for _, name := range names {
		if s.stopped(ctx) {
			break
		}
		s.ociRepo(ctx, name)
	}
	s.orphans(ctx)
	if s.fatal == nil && ctx.Err() == nil {
		s.dbErr(w.index.TouchVersions(ctx, s.seen, w.now()))
	}
	if s.fatal == nil && ctx.Err() == nil && req.Prune && listed && !s.unsupported {
		s.sweep(ctx)
	}
	if s.fatal == nil && ctx.Err() == nil {
		s.dbErr(w.index.InvalidateIndexCache(ctx, repo.ID, ""))
	}
	if s.fatal == nil {
		s.fatal = ctx.Err()
	}
	r := s.rep
	w.log.InfoContext(ctx, "reindex: repo done", "repo", r.Repo, "ociRepositories", r.OCIRepositories, "manifests", r.Manifests,
		"added", r.Added, "updated", r.Updated, "unchanged", r.Unchanged, "removed", r.Removed, "degraded", r.Degraded,
		"foreign", len(r.Foreign), "drift", len(r.Drift), "errors", len(r.Errors), "seconds", w.now().Sub(s.began).Seconds())
	return r, s.fatal
}

// list returns the OCI repositories under the repo's base: the registry's catalog, or the
// oci_repos rows of a backend without one (#37), and those of Request.Repositories; ociRepo
// checks each by its tag list. listed is false when the listing failed: the scan then sweeps
// nothing.
func (s *scan) list(ctx context.Context) (names []string, listed bool) {
	names, err := s.prov.ListRepositories(ctx, s.base+"/")
	switch {
	case errors.Is(err, provider.ErrUnsupported):
		names, listed = s.recordedRepos(ctx)
	case err != nil:
		s.failure(Finding{Error: "list the OCI repositories: " + err.Error()})
	default:
		listed = true
	}
	if len(s.req.Repositories) > 0 {
		names = append(names, s.req.Repositories...)
		slices.Sort(names)
		names = slices.Compact(names)
	}
	return names, listed
}

// recordedRepos returns the OCI repositories that oci_repos records for the repo: the listing of
// a registry without a catalog. Ingest records those it writes to and a run those it reads; one
// pushed straight to the registry is not there until a run attaches it (Request.Repositories).
func (s *scan) recordedRepos(ctx context.Context) ([]string, bool) {
	rows, err := s.w.index.ListOCIRepos(ctx, s.repo.ID)
	if s.dbErr(err) {
		return nil, false
	}
	s.recorded = make(map[string]bool, len(rows))
	names := make([]string, 0, len(rows))
	for _, r := range rows {
		names = append(names, r.Path)
		s.recorded[r.Path] = true
	}
	if len(names)+len(s.req.Repositories) == 0 && s.req.Source != SourceReconcile {
		s.w.log.WarnContext(ctx, "reindex: the registry has no catalog and the index records no OCI repository of the repo; "+
			"name them with lajstrom reindex --repo <repo> --attach <file>", "repo", s.rep.Repo)
	}
	return names, true
}

// ociRepo reads every tag of one OCI repository.
func (s *scan) ociRepo(ctx context.Context, name string) {
	pkgPath := strings.TrimPrefix(strings.TrimPrefix(name, s.base), "/")
	rr, err := s.prov.Repository(ctx, name)
	if err != nil {
		s.failure(Finding{Repository: pkgPath, Error: err.Error()})
		return
	}
	var tags []string
	err = rr.Tags(ctx, "", func(page []string) error {
		tags = append(tags, page...)
		return nil
	})
	if errors.Is(err, provider.ErrNotFound) {
		return // gone: not seen, so its oci_repos row and its versions are swept
	}
	if err != nil {
		if s.recorded[name] {
			// Without a catalog its row is the only record of it: it stays until the registry
			// says that the repository is gone.
			s.dbErr(s.w.index.UpsertOCIRepo(ctx, &index.OCIRepo{RepoID: s.repo.ID, Path: name, SeenAt: s.w.now()}))
		}
		s.failure(Finding{Repository: pkgPath, Error: "list the tags: " + err.Error()})
		return
	}
	if s.dbErr(s.w.index.UpsertOCIRepo(ctx, &index.OCIRepo{RepoID: s.repo.ID, Path: name, SeenAt: s.w.now()})) {
		return
	}
	cache := &manifests{byDigest: map[digest.Digest][]byte{}}
	var wg sync.WaitGroup
	for _, tag := range tags {
		if s.stopped(ctx) {
			break
		}
		s.sem <- struct{}{}
		wg.Go(func() {
			defer func() { <-s.sem }()
			s.tag(ctx, rr, cache, pkgPath, tag)
		})
	}
	wg.Wait()
}

// manifests caches the manifests of one OCI repository by digest: two tags may name one.
type manifests struct {
	mu       sync.Mutex
	byDigest map[digest.Digest][]byte
}

// tag reads the manifest under one tag and indexes, counts or reports it.
func (s *scan) tag(ctx context.Context, rr provider.Repository, cache *manifests, pkgPath, tag string) {
	s.counted(ctx)
	desc, err := rr.Resolve(ctx, tag)
	if err != nil {
		s.failure(Finding{Repository: pkgPath, Tag: tag, Error: err.Error()})
		return
	}
	var pkg *index.Package
	var known *index.Version
	if pkgPath != "" {
		var err error
		if pkg, known, err = s.known(ctx, pkgPath, tag); s.dbErr(err) {
			return
		}
	}
	switch {
	case known != nil && known.Digest != desc.Digest:
		s.drift(known, pkg.Name, tag, desc.Digest)
		return
	case known != nil && !known.Degraded && !s.req.Verify:
		s.see(known.ID, true)
		return
	}
	raw, err := s.fetch(ctx, rr, cache, desc)
	if err != nil {
		s.failure(Finding{Repository: pkgPath, Tag: tag, Digest: desc.Digest, Error: err.Error()})
		return
	}
	s.classify(ctx, pkgPath, tag, desc, raw, known != nil)
}

// known returns the package at pkgPath and its version under tag, nil when the index has none.
func (s *scan) known(ctx context.Context, pkgPath, tag string) (*index.Package, *index.Version, error) {
	pkg, err := s.w.index.GetPackageByNameOCI(ctx, s.repo.ID, pkgPath)
	if errors.Is(err, index.ErrNotFound) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	v, err := s.w.index.GetVersionByTag(ctx, pkg.ID, tag)
	if errors.Is(err, index.ErrNotFound) {
		return pkg, nil, nil
	}
	return pkg, v, err
}

// fetch returns the bytes of the manifest desc, checked against its digest.
func (s *scan) fetch(ctx context.Context, rr provider.Repository, cache *manifests, desc ocispec.Descriptor) ([]byte, error) {
	cache.mu.Lock()
	raw, ok := cache.byDigest[desc.Digest]
	cache.mu.Unlock()
	if ok {
		return raw, nil
	}
	rc, err := rr.Fetch(ctx, desc)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rc.Close() }()
	raw, err = io.ReadAll(io.LimitReader(rc, maxManifest+1))
	switch {
	case err != nil:
		return nil, fmt.Errorf("read the manifest: %w", err)
	case len(raw) > maxManifest:
		return nil, fmt.Errorf("the manifest is larger than %d bytes", maxManifest)
	case digest.FromBytes(raw) != desc.Digest:
		return nil, fmt.Errorf("the manifest's bytes are not %s", desc.Digest)
	}
	cache.mu.Lock()
	cache.byDigest[desc.Digest] = raw
	cache.mu.Unlock()
	return raw, nil
}

// probe is what classify reads of a manifest or an index.
type probe struct {
	MediaType    string               `json:"mediaType"`
	ArtifactType string               `json:"artifactType"`
	Subject      *ocispec.Descriptor  `json:"subject"`
	Manifests    []ocispec.Descriptor `json:"manifests"`
	Annotations  map[string]string    `json:"annotations"`
}

// classify sorts a manifest the index does not have under its digest: a referrer (counted, an
// orphan when its subject is unknown), a foreign manifest (reported, never indexed), or a
// profile manifest (checked and written). reread is true when the index has the row with this
// digest already (a degraded row, or a verifying run).
func (s *scan) classify(ctx context.Context, pkgPath, tag string, desc ocispec.Descriptor, raw []byte, reread bool) {
	var p probe
	if err := json.Unmarshal(raw, &p); err != nil {
		s.foreign(Finding{Repository: pkgPath, Tag: tag, Digest: desc.Digest, MediaType: desc.MediaType})
		return
	}
	mediaType := cmp.Or(p.MediaType, desc.MediaType)
	f := Finding{Repository: pkgPath, Tag: tag, Digest: desc.Digest, MediaType: mediaType, ArtifactType: p.ArtifactType}
	switch m := tagSchema.FindStringSubmatch(tag); {
	case m != nil:
		listed := []digest.Digest{desc.Digest}
		if mediaType == ocispec.MediaTypeImageIndex {
			listed = listed[:0]
			for _, d := range p.Manifests {
				listed = append(listed, d.Digest)
			}
		}
		f.Subject = digest.Digest("sha256:" + m[1])
		s.referrer(listed, f)
		return
	case p.Subject != nil:
		f.Subject = p.Subject.Digest
		s.referrer([]digest.Digest{desc.Digest}, f)
		return
	case mediaType != ocispec.MediaTypeImageManifest || p.Annotations[profile.AnnotationSchema] == "":
		s.foreign(f)
		return
	}
	var m ocispec.Manifest
	if err := json.Unmarshal(raw, &m); err != nil {
		f.Error = err.Error()
		s.failure(f)
		return
	}
	rows, err := s.w.inspector.Inspect(ctx, s.repo, pkgPath, tag, m, raw, ingest.InspectOptions{AllowDegraded: true})
	switch {
	case errors.Is(err, format.ErrUnsupported):
		s.unsupportedFormat(err)
		return
	case err != nil:
		f.Error = err.Error()
		s.failure(f)
		return
	}
	s.write(ctx, rows, f, reread)
}

// write writes the rows of a profile manifest in one transaction.
func (s *scan) write(ctx context.Context, rows *ingest.Rows, f Finding, reread bool) {
	err := s.w.index.Tx(ctx, func(w index.Writer) error { return ingest.WriteRows(ctx, w, rows, s.w.now()) })
	var conflict *index.TagConflictError
	switch {
	case errors.As(err, &conflict): // the tag was written meanwhile with another digest
		pkg, v, kerr := s.known(ctx, f.Repository, f.Tag)
		if s.dbErr(kerr) || v == nil {
			return
		}
		s.drift(v, pkg.Name, f.Tag, f.Digest)
		return
	case errors.Is(err, index.ErrConflict): // the package path of another spelling (#97)
		f.Error = err.Error()
		s.failure(f)
		return
	case s.dbErr(err):
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if reread {
		s.rep.Updated++
	} else {
		s.rep.Added++
	}
	if rows.Version.Degraded {
		s.rep.Degraded++
	}
}

// referrer counts the referrers listed, each digest once, and keeps f for orphans.
func (s *scan) referrer(listed []digest.Digest, f Finding) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, d := range listed {
		s.referrers[d] = struct{}{}
	}
	s.rep.Referrers = len(s.referrers)
	s.subjects = append(s.subjects, f)
}

// orphans reports the referrers read whose subject's digest no version of the repo has, once
// every tag is read: a rebuild may write the subject's row after it read the referrer, the
// sha256-<hex> index of the tag schema in particular.
func (s *scan) orphans(ctx context.Context) {
	for _, f := range s.subjects {
		if s.stopped(ctx) {
			return
		}
		_, err := s.w.index.GetVersionByDigest(ctx, s.repo.ID, f.Subject)
		switch {
		case errors.Is(err, index.ErrNotFound):
			s.mu.Lock()
			add(&s.rep, &s.rep.OrphanReferrers, f)
			s.mu.Unlock()
		case s.dbErr(err):
			return
		}
	}
}

// sweep looks up, by its tag, every version of the repo the run did not see, keeps those whose
// tag names their digest (written after the listing) or another one (drift), and deletes those
// whose tag the registry does not know; then the packages without versions and the OCI
// repositories not seen. A registry that does not answer deletes nothing.
func (s *scan) sweep(ctx context.Context) {
	unseen, err := s.w.index.ListUnseenVersions(ctx, s.repo.ID, s.start)
	if s.dbErr(err) {
		return
	}
	var keep []int64
	for _, m := range unseen {
		if s.stopped(ctx) {
			return
		}
		f := Finding{Repository: m.Package.NameOCI, Tag: m.Version.Tag, Digest: m.Version.Digest}
		rr, err := s.prov.Repository(ctx, s.base+"/"+m.Package.NameOCI)
		var desc ocispec.Descriptor
		if err == nil {
			desc, err = rr.Resolve(ctx, m.Version.Tag)
		}
		switch {
		case err == nil && desc.Digest == m.Version.Digest:
			keep = append(keep, m.Version.ID)
		case err == nil:
			s.drift(&m.Version, m.Package.Name, m.Version.Tag, desc.Digest)
			keep = append(keep, m.Version.ID)
		case errors.Is(err, provider.ErrNotFound):
			err := s.w.index.DeleteVersion(ctx, m.Version.ID)
			if errors.Is(err, index.ErrNotFound) || !s.dbErr(err) {
				s.mu.Lock()
				s.rep.Removed++
				s.mu.Unlock()
			}
		default:
			f.Error = "look up the tag before removing its rows: " + err.Error()
			s.failure(f)
		}
	}
	if s.dbErr(s.w.index.TouchVersions(ctx, keep, s.w.now())) {
		return
	}
	if _, err := s.w.index.DeleteEmptyPackages(ctx, s.repo.ID); s.dbErr(err) {
		return
	}
	_, err = s.w.index.PruneOCIRepos(ctx, s.repo.ID, s.start)
	s.dbErr(err)
}

// see records a version row seen without being written; unchanged counts it.
func (s *scan) see(id int64, unchanged bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.seen = append(s.seen, id)
	if unchanged {
		s.rep.Unchanged++
	}
}

// drift reports a tag whose digest in the registry is not the index's, and keeps the row.
func (s *scan) drift(v *index.Version, pkgName, tag string, registry digest.Digest) {
	s.see(v.ID, false)
	s.mu.Lock()
	defer s.mu.Unlock()
	add(&s.rep, &s.rep.Drift, Drift{Package: pkgName, Tag: tag, Index: v.Digest, Registry: registry})
}

// foreign reports a manifest under a managed path that is not Lajstrom's.
func (s *scan) foreign(f Finding) {
	s.mu.Lock()
	defer s.mu.Unlock()
	add(&s.rep, &s.rep.Foreign, f)
}

// failure reports what could not be read or checked.
func (s *scan) failure(f Finding) {
	s.mu.Lock()
	defer s.mu.Unlock()
	add(&s.rep, &s.rep.Errors, f)
}

// unsupportedFormat stops the scan of a repo whose format has no plugin, once.
func (s *scan) unsupportedFormat(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.unsupported {
		s.unsupported = true
		add(&s.rep, &s.rep.Errors, Finding{Error: err.Error()})
	}
}

// dbErr records err, an error of the index, as the run's own and reports whether there was one.
func (s *scan) dbErr(err error) bool {
	if err == nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.fatal == nil {
		s.fatal = fmt.Errorf("reindex: repo %s: %w", s.rep.Repo, err)
	}
	return true
}

// stopped reports whether the scan should stop: an error of the run's own, a cancelled
// context, or a format without a plugin.
func (s *scan) stopped(ctx context.Context) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.fatal != nil || s.unsupported || ctx.Err() != nil
}

// counted counts a manifest read and logs the progress every progressEvery.
func (s *scan) counted(ctx context.Context) {
	s.mu.Lock()
	s.rep.Manifests++
	n := s.rep.Manifests
	s.mu.Unlock()
	if n%progressEvery == 0 {
		elapsed := s.w.now().Sub(s.began).Seconds()
		rate := 0.0
		if elapsed > 0 {
			rate = float64(n) / elapsed
		}
		s.w.log.InfoContext(ctx, "reindex: progress", "repo", s.rep.Repo, "manifests", n, "perSecond", rate)
	}
}

// repoBase is the base path of a repo in the registry, as ingest names it: its prefix, else
// <format>/<name> (manifest-profile.md §4.1).
func repoBase(repo index.Repo) string {
	if base := strings.Trim(repo.Prefix, "/"); base != "" {
		return base
	}
	return repo.Format + "/" + repo.Name
}
