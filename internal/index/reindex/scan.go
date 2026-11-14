package reindex

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/lajstrom/lajstrom/internal/auth"
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
	splits      map[[2]string]*Split       // packages found at another form than the index has, by name and form
	listed      map[string]bool            // OCI repositories whose tag list was asked: false for a 404, true otherwise (repoUnknown)
	unknown     map[string]int             // the versions reported in RepositoryUnknown, by package path
	found       map[digest.Digest][]string // the manifests read, tagged or listed by a sha256-<hex> index, by digest: their package paths; nil unless repairing (prepareRepair)
	deleted     map[digest.Digest][]string // the manifests deleted through Lajstrom within the grace age, with the OCI repositories they were deleted from, "" for any (recentDeletes); nil on a strong registry
}

// repo reindexes one repo. The error is the run's own; everything about the registry and the
// manifests is in the report.
func (w *Worker) repo(ctx context.Context, repo index.Repo, start time.Time, req Request) (RepoReport, error) {
	s := &scan{w: w, repo: repo, base: repoBase(repo), req: req, start: start, began: w.now(),
		sem: make(chan struct{}, w.concurrency), rep: newRepoReport(repo.Format + "/" + repo.Name),
		referrers: map[digest.Digest]struct{}{}, splits: map[[2]string]*Split{}, listed: map[string]bool{}, unknown: map[string]int{}}
	prov, ok := w.providers.Provider(repo.Registry)
	if !ok || prov == nil {
		s.failure(Finding{Error: fmt.Sprintf("registry %q is not configured", repo.Registry)})
		return s.rep, nil
	}
	s.prov = prov
	s.prepareRepair(ctx)
	s.recentDeletes(ctx)
	names, listed := s.list(ctx)
	s.rep.OCIRepositories = len(names)
	for _, name := range newestFirst(s.base, names) {
		if s.stopped(ctx) {
			break
		}
		s.ociRepo(ctx, name)
	}
	s.orphans(ctx)
	s.repairReferrers(ctx)
	s.reportSplits(ctx)
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
	s.logDone(ctx)
	return s.rep, s.fatal
}

// logDone logs the counts of the scan, and once the manifests deleted that it did not index
// again.
func (s *scan) logDone(ctx context.Context) {
	r := s.rep
	if r.DeletedPending > 0 {
		s.w.log.InfoContext(ctx, "reindex: the registry still lists manifests deleted through Lajstrom less than reconcile.graceAge ago; "+
			"they are not indexed again", "repo", r.Repo, "tags", r.DeletedPending)
	}
	s.w.log.InfoContext(ctx, "reindex: repo done", "repo", r.Repo, "ociRepositories", r.OCIRepositories, "manifests", r.Manifests,
		"added", r.Added, "updated", r.Updated, "unchanged", r.Unchanged, "removed", r.Removed, "deletedPending", r.DeletedPending,
		"degraded", r.Degraded, "foreign", len(r.Foreign), "drift", len(r.Drift), "split", len(r.Split), "errors", len(r.Errors),
		"seconds", s.w.now().Sub(s.began).Seconds())
}

// auditPage is how many audit rows recentDeletes reads at a time.
const auditPage = 100

// recentDeletes loads, on a registry that is not strongly consistent, the digests of the
// manifests that the audit log has as deleted from the repo through Lajstrom, with success, less
// than the grace age before the run started (docs/spec/guarantees.md, INV-13), each with the OCI
// repository it was deleted from (deletedFrom). Such a registry may list and serve a manifest for
// a while after its delete, as it may miss one after its push; tag does not index those again
// (wasDeleted). A manifest pushed straight to the registry under such a digest is indexed by the
// first run past the grace age; one published again through Lajstrom has its rows from ingest.
// A strong registry lists nothing deleted, and the audit log is not read; one whose
// capabilities do not answer is read as if it were not strong.
func (s *scan) recentDeletes(ctx context.Context) {
	if caps, err := s.prov.Capabilities(ctx); err == nil && caps.Consistency == provider.ConsistencyStrong {
		return
	}
	s.deleted = map[digest.Digest][]string{}
	f := index.AuditFilter{Repo: s.rep.Repo, Action: "delete", State: index.AuditOK, Since: s.start.Add(-s.w.graceAge), Limit: auditPage}
	for {
		rows, err := s.w.index.ListAudit(ctx, f)
		if s.dbErr(err) {
			return
		}
		for _, ev := range rows {
			if ev.Digest != "" {
				s.deleted[ev.Digest] = append(s.deleted[ev.Digest], s.deletedFrom(ev))
			}
		}
		if len(rows) < auditPage {
			return
		}
		f.Offset += len(rows) // rows only ever join at the top: a page read again repeats, never skips
	}
}

// deletedFrom is the OCI repository that the delete ev names, "" when it names none. The audit
// row of the delete of a manifest that is neither a version nor a referrer has the repository as
// its subject, without the instance prefix (ingest's deletion.event); a version's subject is
// "<package> <version>", a referrer's its subject's digest, and those match by digest alone.
func (s *scan) deletedFrom(ev index.AuditEvent) string {
	if !strings.HasPrefix(ev.Subject, s.base+"/") || strings.ContainsRune(ev.Subject, ' ') {
		return ""
	}
	return ev.Subject
}

// wasDeleted reports whether recentDeletes found the manifest d deleted from the OCI repository
// name, or from a repository the audit row does not name.
func (s *scan) wasDeleted(d digest.Digest, name string) bool {
	for _, from := range s.deleted[d] {
		if from == "" || from == name {
			return true
		}
	}
	return false
}

// deletedPending counts a tag whose manifest recentDeletes found deleted, not indexed again.
func (s *scan) deletedPending() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rep.DeletedPending++
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

// newestFirst orders the OCI repositories names, below base, so that a package path comes before
// its earlier forms (profile.EarlierPackagePaths) when both are listed: of a package found at two
// forms, a run without its row writes the newer, as ingest writes a package the index does not
// have, and the other is a split (#609). The order is otherwise the listing's.
func newestFirst(base string, names []string) []string {
	listed := make(map[string]bool, len(names))
	for _, name := range names {
		listed[name] = true
	}
	newer := map[string]int{} // how many listed forms are newer than a name
	for _, name := range names {
		for _, earlier := range profile.EarlierPackagePaths(strings.TrimPrefix(name, base+"/")) {
			if listed[base+"/"+earlier] {
				newer[base+"/"+earlier]++
			}
		}
	}
	if len(newer) == 0 {
		return names
	}
	names = slices.Clone(names)
	slices.SortStableFunc(names, func(a, b string) int { return cmp.Compare(newer[a], newer[b]) })
	return names
}

// ociRepo reads every tag of one OCI repository.
func (s *scan) ociRepo(ctx context.Context, name string) {
	pkgPath := strings.TrimPrefix(strings.TrimPrefix(name, s.base), "/")
	rr, err := s.prov.Repository(ctx, name)
	if err != nil {
		s.failure(Finding{Repository: pkgPath, Error: err.Error()})
		return
	}
	tags, err := listTags(ctx, rr)
	if errors.Is(err, provider.ErrNotFound) {
		s.tagList(name, false)
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
	s.tagList(name, true)
	if len(tags) == 0 && s.repo.Format == ociFormat {
		if tags = s.dropUntagged(ctx, rr, name, pkgPath); len(tags) == 0 {
			return
		}
	}
	if s.dbErr(s.w.index.UpsertOCIRepo(ctx, &index.OCIRepo{RepoID: s.repo.ID, Path: name, SeenAt: s.w.now()})) {
		return
	}
	cache := &manifests{byDigest: map[digest.Digest][]byte{}}
	tags, journal := s.splitJournal(tags)
	var wg sync.WaitGroup
	for _, tag := range tags {
		if s.stopped(ctx) {
			break
		}
		s.sem <- struct{}{}
		wg.Go(func() {
			defer func() { <-s.sem }()
			s.tag(ctx, rr, cache, name, pkgPath, tag)
		})
	}
	wg.Wait()
	if s.repo.Format == npmFormat && pkgPath != "" && !s.stopped(ctx) {
		s.journal(ctx, rr, cache, pkgPath, journal)
	}
}

// listTags returns every tag of the repository rr.
func listTags(ctx context.Context, rr provider.Repository) ([]string, error) {
	var tags []string
	err := rr.Tags(ctx, "", func(page []string) error {
		tags = append(tags, page...)
		return nil
	})
	return tags, err
}

// ociFormat is the format of pass-through repos, whose oci_repos rows the catalog lists.
const ociFormat = "oci"

// dropUntagged removes the oci_repos row of name, an OCI repository of an oci repo whose tag list
// has no tag, and returns its tags listed once more: an oci repo's oci_repos records the
// repositories that hold a tag, and Zot keeps an emptied repository, whose tag list has none
// (#748). A managed repo's rows are the registry's repositories, as its delete leaves them. A
// push records its repository after its manifest PUT, so the row goes first and the tags are
// listed after it: a tag pushed meanwhile is in that listing, and the scan goes on with it; a
// push after it records the row itself. A listing that fails records the row again, even when
// the run stops meanwhile, and is an error of the report.
func (s *scan) dropUntagged(ctx context.Context, rr provider.Repository, name, pkgPath string) []string {
	if err := s.w.index.DeleteOCIRepo(ctx, s.repo.ID, name); errors.Is(err, index.ErrNotFound) || s.dbErr(err) {
		return nil // no row, so none of a push to record again
	}
	tags, err := listTags(ctx, rr)
	switch {
	case errors.Is(err, provider.ErrNotFound):
		return nil
	case err != nil:
		s.dbErr(s.w.index.UpsertOCIRepo(context.WithoutCancel(ctx), &index.OCIRepo{RepoID: s.repo.ID, Path: name, SeenAt: s.w.now()}))
		s.failure(Finding{Repository: pkgPath, Error: "list the tags: " + err.Error()})
		return nil
	}
	return tags
}

// manifests caches the manifests of one OCI repository by digest: two tags may name one.
type manifests struct {
	mu       sync.Mutex
	byDigest map[digest.Digest][]byte
}

// tag reads the manifest under one tag of the OCI repository name, at pkgPath, and indexes,
// counts or reports it.
func (s *scan) tag(ctx context.Context, rr provider.Repository, cache *manifests, name, pkgPath, tag string) {
	s.counted(ctx)
	desc, err := rr.Resolve(ctx, tag)
	if err != nil {
		s.failure(Finding{Repository: pkgPath, Tag: tag, Error: err.Error()})
		return
	}
	s.locate(pkgPath, desc.Digest)
	var pkg *index.Package
	var known *index.Version
	if pkgPath != "" {
		var err error
		if pkg, known, err = s.known(ctx, pkgPath, tag); s.dbErr(err) {
			return
		}
		if pkg != nil && known == nil {
			if known, err = s.folded(ctx, rr, pkg, tag, desc.Digest); s.dbErr(err) {
				return
			}
		}
	}
	switch {
	case known != nil && known.Digest != desc.Digest:
		s.drift(known, pkg.Name, tag, desc.Digest)
		return
	case known != nil && !known.Degraded && !s.req.Verify:
		s.see(known.ID, true)
		return
	case known == nil && s.wasDeleted(desc.Digest, name): // a lagging listing of a delete (recentDeletes)
		s.deletedPending()
		return
	}
	raw, err := s.fetch(ctx, rr, cache, desc)
	if err != nil {
		s.failure(Finding{Repository: pkgPath, Tag: tag, Digest: desc.Digest, Error: err.Error()})
		return
	}
	s.classify(ctx, rr, pkgPath, tag, desc, raw, known != nil)
}

// folded returns the version of pkg whose manifest is d, listed under tag, when the index has it
// under a tag that differs from tag only in case and the registry resolves that tag to d too:
// a registry that folds the case of tags (Gitea, Forgejo) lists 1.4.0-m1 for the 1.4.0-M1 that
// ingest pushed (#844). Nil otherwise, and on a registry that keeps case, where the version's
// own tag is listed apart.
func (s *scan) folded(ctx context.Context, rr provider.Repository, pkg *index.Package, tag string, d digest.Digest) (*index.Version, error) {
	v, err := s.w.index.GetVersionByDigest(ctx, s.repo.ID, d)
	switch {
	case errors.Is(err, index.ErrNotFound):
		return nil, nil
	case err != nil:
		return nil, err
	case v.PackageID != pkg.ID || v.Tag == tag || !strings.EqualFold(v.Tag, tag):
		return nil, nil
	}
	if desc, err := rr.Resolve(ctx, v.Tag); err != nil || desc.Digest != d {
		return nil, nil //nolint:nilerr // not the same manifest: the tag is read as one the index lacks
	}
	return v, nil
}

// storedTag returns the tag to inspect the manifest of f, listed under tag, with: the tag its
// annotations a give (manifest-profile.md §4.2) when that differs from tag only in case and the
// registry resolves it to f.Digest as well, else tag. A registry that folds the case of tags (Gitea,
// Forgejo) lists the tag 1.4.0-M1 that ingest pushed as 1.4.0-m1 (#844); the version is then
// the annotations', and its row keeps the tag that ingest wrote. Nothing decodes the listed tag.
// On a registry that keeps case, a manifest pushed straight under a tag of another case is
// checked against that tag, as ingest checks it, and is an error of the report. ok is false
// when the registry did not answer: f, with the error, is in the report.
func (s *scan) storedTag(ctx context.Context, rr provider.Repository, tag string, a map[string]string, f Finding) (string, bool) {
	want := profile.Tag(a[profile.AnnotationVersion], a[profile.AnnotationVariant])
	if want == tag || !strings.EqualFold(want, tag) {
		return tag, true
	}
	desc, err := rr.Resolve(ctx, want)
	switch {
	case errors.Is(err, provider.ErrNotFound) || err == nil && desc.Digest != f.Digest:
		return tag, true
	case err != nil:
		f.Error = "resolve " + want + ", the tag of the annotations: " + err.Error()
		s.failure(f)
		return "", false
	}
	return want, true
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
func (s *scan) classify(ctx context.Context, rr provider.Repository, pkgPath, tag string, desc ocispec.Descriptor, raw []byte, reread bool) {
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
		s.locate(pkgPath, listed...)
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
	stored, ok := s.storedTag(ctx, rr, tag, p.Annotations, f)
	if !ok {
		return
	}
	rows, err := s.w.inspector.Inspect(ctx, s.repo, pkgPath, stored, m, raw, ingest.InspectOptions{AllowDegraded: true})
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

// splitError is the error of a manifest whose package the index has at another form of its OCI
// name (ADR 0017): its rows would name that form's OCI repository, which does not hold the
// manifest, so they are not written (#609).
type splitError struct{ indexed string }

func (e *splitError) Error() string {
	return "reindex: the package is stored at " + e.indexed + ", another form of its OCI name"
}

// write writes the rows of a profile manifest in one transaction, unless the index has its
// package at another form of the package path: a split, reported and not written.
func (s *scan) write(ctx context.Context, rows *ingest.Rows, f Finding, reread bool) {
	err := s.w.index.Tx(ctx, func(w index.Writer) error {
		pkg, err := w.GetPackage(ctx, rows.Package.RepoID, rows.Package.Name)
		switch {
		case err == nil && pkg.NameOCI != f.Repository:
			return &splitError{indexed: pkg.NameOCI}
		case err != nil && !errors.Is(err, index.ErrNotFound):
			return err
		}
		if err := ingest.WriteRows(ctx, w, rows, s.w.now()); err != nil {
			return err
		}
		if rows.Package.NameOCI != f.Repository { // a writer created the package at another form meanwhile
			return &splitError{indexed: rows.Package.NameOCI}
		}
		return nil
	})
	var conflict *index.TagConflictError
	var split *splitError
	switch {
	case errors.As(err, &split):
		s.split(rows.Package.Name, split.indexed, f.Repository, f.Tag)
		return
	case errors.As(err, &conflict): // the index has the tag with another digest: written meanwhile, or re-pointed past Lajstrom
		pkg, v, kerr := s.known(ctx, f.Repository, rows.Version.Tag)
		if s.dbErr(kerr) || v == nil {
			return
		}
		s.drift(v, pkg.Name, v.Tag, f.Digest)
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

// prepareRepair makes the scan record where it finds each manifest (locate) when it has ingest
// repair the sha256-<hex> indexes of the repo (#628): a run of the reconcile loop or with
// Verify, with a Repairer, where the provider keeps the tag schema (Capabilities.TagSchema): without
// the referrers API, which lists referrers itself, or with referrers.alwaysTag.
func (s *scan) prepareRepair(ctx context.Context) {
	if s.w.repairer == nil || !s.req.Verify && s.req.Source != SourceReconcile {
		return
	}
	caps, err := s.prov.Capabilities(ctx)
	switch {
	case err != nil:
		s.failure(Finding{Error: "the capabilities of the registry: " + err.Error()})
	case caps.TagSchema():
		s.found = map[digest.Digest][]string{}
	}
}

// locate records that the OCI repository of pkgPath has the manifests digests, when repairing.
func (s *scan) locate(pkgPath string, digests ...digest.Digest) {
	if s.found == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, d := range digests {
		if !slices.Contains(s.found[d], pkgPath) {
			s.found[d] = append(s.found[d], pkgPath)
		}
	}
}

// repairReferrers asks the Repairer, once every tag is read, to push again the sha256-<hex> index
// of each subject found that lacks referrers the audit log has as pushed and the registry still
// has, and reports what it pushed, and what failed among the errors.
func (s *scan) repairReferrers(ctx context.Context) {
	if len(s.found) == 0 || s.stopped(ctx) {
		return
	}
	principal, err := auth.ParsePrincipal(s.req.Principal)
	if err != nil {
		principal = auth.Anonymous
	}
	repairs, err := s.w.repairer.RepairReferrers(ctx, s.repo, s.found, principal)
	if err != nil {
		s.failure(Finding{Error: "repair the referrers indexes: " + err.Error()})
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, r := range repairs {
		if r.Err != nil {
			add(&s.rep, &s.rep.Errors, Finding{Repository: r.Path, Tag: "sha256-" + r.Subject.Encoded(), Subject: r.Subject,
				Error: "repair the referrers index: " + r.Err.Error()})
			continue
		}
		add(&s.rep, &s.rep.RepairedReferrers, RepairedIndex{Repository: r.Path, Subject: r.Subject, Index: r.Index, Added: r.Added})
	}
}

// sweep looks up, by its tag, every version of the repo the run did not see, keeps those whose
// tag names their digest (written after the listing) or another one (drift), and asks the
// registry for the manifest of those whose tag it does not know (lookup): only the second
// answer MANIFEST_UNKNOWN, unknownApart after the first, deletes a version older than the grace
// age. A manifest found records its OCI repository again. Then go the packages without
// versions, the OCI repositories not seen for the grace age, and the sightings the run did not
// record again. A registry that does not answer deletes nothing, and nor does one that does not
// know a version's OCI repository (RepositoryUnknown, logged once per repository).
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
		if s.recheck(ctx, m) {
			keep = append(keep, m.Version.ID)
		}
	}
	s.warnUnknown(ctx)
	if s.stopped(ctx) || s.dbErr(s.w.index.TouchVersions(ctx, keep, s.w.now())) {
		return
	}
	if _, err := s.w.index.DeleteEmptyPackages(ctx, s.repo.ID); s.dbErr(err) {
		return
	}
	if _, err := s.w.index.PruneOCIRepos(ctx, s.repo.ID, s.start.Add(-s.w.graceAge)); s.dbErr(err) {
		return
	}
	_, err = s.w.index.ClearSightings(ctx, s.repo.ID, s.start)
	s.dbErr(err)
}

// recheck looks up one version the run did not see, for sweep, and reports whether the
// registry has it: its tag names its digest, or another one (drift), or the tag does not resolve
// and lookup finds the manifest. Then its OCI repository is recorded again. Otherwise lookup
// decides: the version is removed, pending, reported as an error, or reported in
// RepositoryUnknown when the registry does not know its OCI repository.
func (s *scan) recheck(ctx context.Context, m index.VersionMatch) bool {
	f := Finding{Repository: m.Package.NameOCI, Tag: m.Version.Tag, Digest: m.Version.Digest}
	name := s.base + "/" + m.Package.NameOCI
	rr, err := s.prov.Repository(ctx, name)
	var desc ocispec.Descriptor
	if err == nil {
		desc, err = rr.Resolve(ctx, m.Version.Tag)
	}
	found := false
	switch {
	case err == nil:
		if desc.Digest != m.Version.Digest {
			s.drift(&m.Version, m.Package.Name, m.Version.Tag, desc.Digest)
		}
		found = true
	case errors.Is(err, provider.ErrNotFound):
		switch s.lookup(ctx, name, f, m.Version.IndexedAt) {
		case present:
			found = true
		case gone:
			err := s.w.index.DeleteVersion(ctx, m.Version.ID)
			if errors.Is(err, index.ErrNotFound) || !s.dbErr(err) {
				s.mu.Lock()
				s.rep.Removed++
				s.mu.Unlock()
			}
		case nameUnknown:
			s.unknownRepository(m)
		case pending, unchecked:
		}
	default:
		f.Error = "look up the tag before removing its rows: " + err.Error()
		s.failure(f)
	}
	if found {
		s.dbErr(s.w.index.UpsertOCIRepo(ctx, &index.OCIRepo{RepoID: s.repo.ID, Path: name, SeenAt: s.w.now()}))
	}
	return found
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

// split records a manifest at the package path other, not indexed because the index has its
// package at indexed, another form of it.
func (s *scan) split(pkgName, indexed, other, tag string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := [2]string{pkgName, other}
	if s.splits[key] == nil {
		s.splits[key] = &Split{Package: pkgName, Indexed: indexed, IndexedTags: []string{}, Other: other}
	}
	s.splits[key].OtherTags = append(s.splits[key].OtherTags, tag)
}

// reportSplits lists the splits of the scan, with the tags of the versions the index has at the
// indexed form, and logs a warning for each: the versions at the other form are not served
// until they are copied to the indexed one.
func (s *scan) reportSplits(ctx context.Context) {
	for _, key := range slices.SortedFunc(maps.Keys(s.splits), func(a, b [2]string) int {
		return cmp.Or(cmp.Compare(a[0], b[0]), cmp.Compare(a[1], b[1]))
	}) {
		if s.stopped(ctx) {
			return
		}
		sp := s.splits[key]
		pkg, err := s.w.index.GetPackage(ctx, s.repo.ID, sp.Package)
		if errors.Is(err, index.ErrNotFound) || err == nil && pkg.NameOCI != sp.Indexed {
			continue // deleted or written again meanwhile: the next run looks again
		}
		if s.dbErr(err) {
			return
		}
		versions, err := s.w.index.ListVersions(ctx, pkg.ID)
		if s.dbErr(err) {
			return
		}
		for _, v := range versions {
			sp.IndexedTags = append(sp.IndexedTags, v.Tag)
		}
		slices.Sort(sp.IndexedTags)
		slices.Sort(sp.OtherTags)
		s.mu.Lock()
		sp.IndexedTags, sp.OtherTags = s.bounded(sp.IndexedTags), s.bounded(sp.OtherTags)
		add(&s.rep, &s.rep.Split, *sp)
		s.mu.Unlock()
		s.w.log.WarnContext(ctx, "reindex: a package is stored at two forms of its OCI name (ADR 0017); the versions at the "+
			"other form are not indexed until they are copied to the indexed one", "repo", s.rep.Repo, "package", sp.Package,
			"indexed", sp.Indexed, "other", sp.Other, "otherTags", len(sp.OtherTags))
	}
}

// bounded returns the first maxFindings tags, and marks the report truncated when there are more;
// s.mu is held.
func (s *scan) bounded(tags []string) []string {
	if len(tags) <= maxFindings {
		return tags
	}
	s.rep.Truncated = true
	return tags[:maxFindings]
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
