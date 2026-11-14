package reindex

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/lajstrom/lajstrom/internal/index"
	"github.com/lajstrom/lajstrom/internal/oci/profile"
	"github.com/lajstrom/lajstrom/internal/provider"
)

// splitJournal separates the journal tags of an npm or a pypi package's OCI repository,
// _j<seq>, from its other tags (docs/spec/manifest-profile.md §8); a repo of a format without a
// journal has none.
func (s *scan) splitJournal(tags []string) (others, journal []string) {
	if !profile.HasJournal(s.repo.Format) {
		return tags, nil
	}
	for _, tag := range tags {
		if _, ok := profile.JournalSeq(tag); ok {
			journal = append(journal, tag)
		} else {
			others = append(others, tag)
		}
	}
	return others, journal
}

// journal reads the journal tags of one package once its versions are read, and replays
// them into the package's state (manifest-profile.md §8, Reindex): a tag that names the digest
// of its row is seen without a read; another is read, checked and written; with prune, a row
// whose tag the registry does not know any more is deleted; then the state is the fold of the
// rows, written in one transaction. It runs under the package's journal lock, so that it never
// interleaves with an append. The entries of a repository without a package are orphans,
// reported and not indexed.
func (s *scan) journal(ctx context.Context, rr provider.Repository, cache *manifests, pkgPath string, tags []string) {
	pkg, err := s.w.index.GetPackageByNameOCI(ctx, s.repo.ID, pkgPath)
	if errors.Is(err, index.ErrNotFound) {
		s.orphanJournal(ctx, rr, pkgPath, tags)
		return
	}
	if s.dbErr(err) {
		return
	}
	j := &replay{s: s, rr: rr, cache: cache, pkg: pkg, pkgPath: pkgPath}
	err = s.w.index.WithReferrersLock(ctx, s.base+"/"+pkgPath, profile.EmptyConfig.Digest, func(ctx context.Context) error {
		return j.run(ctx, tags)
	})
	s.dbErr(err)
}

// orphanJournal reports the journal tags of a repository without a package.
func (s *scan) orphanJournal(ctx context.Context, rr provider.Repository, pkgPath string, tags []string) {
	for _, tag := range tags {
		s.counted(ctx)
		f := Finding{Repository: pkgPath, Tag: tag}
		desc, err := rr.Resolve(ctx, tag)
		if err != nil {
			f.Error = err.Error()
			s.failure(f)
			continue
		}
		f.Digest, f.MediaType = desc.Digest, desc.MediaType
		s.mu.Lock()
		add(&s.rep, &s.rep.OrphanJournal, f)
		s.mu.Unlock()
	}
}

// replay is the reindex of one package's journal.
type replay struct {
	s       *scan
	rr      provider.Repository
	cache   *manifests
	pkg     *index.Package
	pkgPath string
	seen    []int64 // rows seen without being written
}

// run reads the tags, sweeps the rows the registry lost and writes the state. Its error is the
// index's.
func (j *replay) run(ctx context.Context, tags []string) error {
	rows, err := j.s.w.index.ListJournal(ctx, j.pkg.ID)
	if err != nil {
		return err
	}
	byTag := make(map[string]index.JournalEntry, len(rows))
	for _, r := range rows {
		byTag[r.Tag] = r
	}
	listed := make(map[string]bool, len(tags))
	for _, tag := range tags {
		if j.s.stopped(ctx) {
			return nil
		}
		listed[tag] = true
		j.s.counted(ctx)
		row, known := byTag[tag]
		if err := j.tag(ctx, tag, row, known); err != nil {
			return err
		}
	}
	if j.s.req.Prune {
		for _, r := range rows {
			if !listed[r.Tag] {
				if err := j.sweep(ctx, r); err != nil {
					return err
				}
			}
		}
	}
	if err := j.s.w.index.TouchJournal(ctx, j.seen, j.s.w.now()); err != nil {
		return err
	}
	if rows, err = j.s.w.index.ListJournal(ctx, j.pkg.ID); err != nil {
		return err
	}
	return j.s.w.index.Tx(ctx, func(w index.Writer) error { return w.ReplaceJournalState(ctx, j.pkg.ID, j.fold(rows)) })
}

// tag reindexes one journal tag: seen when the index has its digest, else read and written.
func (j *replay) tag(ctx context.Context, tag string, row index.JournalEntry, known bool) error {
	f := Finding{Repository: j.pkgPath, Tag: tag}
	desc, err := j.rr.Resolve(ctx, tag)
	if err != nil {
		f.Error = err.Error()
		j.s.failure(f)
		return nil
	}
	f.Digest, f.MediaType = desc.Digest, desc.MediaType
	switch {
	case known && row.Digest != desc.Digest:
		j.drift(row, desc.Digest)
		return nil
	case known && !j.s.req.Verify:
		j.see(row.ID, true)
		return nil
	}
	raw, err := j.s.fetch(ctx, j.rr, j.cache, desc)
	if err != nil {
		f.Error = err.Error()
		j.s.failure(f)
		return nil
	}
	e, err := j.entry(raw, tag)
	if err != nil {
		f.ArtifactType, f.Error = e.ArtifactType, err.Error()
		j.s.failure(f)
		return nil
	}
	err = j.s.w.index.UpsertJournal(ctx, &index.JournalEntry{PackageID: j.pkg.ID, Seq: e.Seq, Tag: tag, Digest: desc.Digest,
		ArtifactType: e.ArtifactType, Attrs: e.Attrs(), CreatedAt: e.Created, IndexedAt: j.s.w.now()})
	var conflict *index.TagConflictError
	switch {
	case errors.As(err, &conflict):
		f.ArtifactType, f.Error = e.ArtifactType, err.Error()
		j.s.failure(f)
		return nil
	case err != nil:
		return err
	}
	j.s.mu.Lock()
	defer j.s.mu.Unlock()
	if known {
		j.s.rep.Updated++
	} else {
		j.s.rep.Added++
	}
	return nil
}

// entry parses the manifest raw under tag as an entry of the package's journal: the rules of
// §8, its tag the tag of its seq, its format the repo's, its repo and name the package's.
func (j *replay) entry(raw []byte, tag string) (profile.JournalEntry, error) {
	var m ocispec.Manifest
	if err := json.Unmarshal(raw, &m); err != nil {
		return profile.JournalEntry{}, err
	}
	e, err := profile.ParseJournal(m)
	switch {
	case err != nil:
		e.ArtifactType = m.ArtifactType
		return e, err
	case profile.JournalTag(e.Seq) != tag:
		return e, fmt.Errorf("the journal entry %d is under the tag %s, not %s", e.Seq, tag, profile.JournalTag(e.Seq))
	case e.Format != j.s.repo.Format:
		return e, fmt.Errorf("the journal entry is of the %s journal, in a %s repo", e.Format, j.s.repo.Format)
	case e.Repo != j.s.repo.Name || e.Name != j.pkg.Name:
		return e, fmt.Errorf("the journal entry is of %s in the repo %s, not of the package %s of the repository", e.Name, e.Repo, j.pkg.Name)
	}
	return e, nil
}

// sweep looks up the tag of a row the listing did not have: its digest keeps it, another digest
// is drift, and a tag the registry does not know asks for the manifest (scan.lookup), whose
// second answer MANIFEST_UNKNOWN deletes a row older than the grace age.
func (j *replay) sweep(ctx context.Context, r index.JournalEntry) error {
	desc, err := j.rr.Resolve(ctx, r.Tag)
	switch {
	case err == nil && desc.Digest == r.Digest:
		j.see(r.ID, false)
	case err == nil:
		j.drift(r, desc.Digest)
	case errors.Is(err, provider.ErrNotFound):
		switch j.s.lookup(ctx, j.s.base+"/"+j.pkgPath, Finding{Repository: j.pkgPath, Tag: r.Tag, Digest: r.Digest}, r.IndexedAt) {
		case present:
			j.see(r.ID, false)
		case gone:
			if err := j.s.w.index.DeleteJournal(ctx, r.ID); err != nil && !errors.Is(err, index.ErrNotFound) {
				return err
			}
			j.s.mu.Lock()
			j.s.rep.Removed++
			j.s.mu.Unlock()
		case nameUnknown: // the tag list answered a moment before: a repository deleted meanwhile
			j.s.failure(Finding{Repository: j.pkgPath, Tag: r.Tag, Digest: r.Digest,
				Error: "the registry does not know the OCI repository of the journal entry: the row stays"})
		case pending, unchecked:
		}
	default:
		j.s.failure(Finding{Repository: j.pkgPath, Tag: r.Tag, Digest: r.Digest, Error: "look up the tag before removing its row: " + err.Error()})
	}
	return nil
}

// see records a row seen without being written; unchanged counts it.
func (j *replay) see(id int64, unchanged bool) {
	j.seen = append(j.seen, id)
	if unchanged {
		j.s.mu.Lock()
		j.s.rep.Unchanged++
		j.s.mu.Unlock()
	}
}

// drift reports a journal tag whose digest in the registry is not its row's, and keeps the row.
func (j *replay) drift(r index.JournalEntry, registry digest.Digest) {
	j.see(r.ID, false)
	j.s.mu.Lock()
	defer j.s.mu.Unlock()
	add(&j.s.rep, &j.s.rep.Drift, Drift{Package: j.pkg.Name, Tag: r.Tag, Index: r.Digest, Registry: registry})
}

// fold returns the state of the rows in seq order (§8, Replay). A seq missing before a row is
// reported, and the replay goes on over the rest.
func (j *replay) fold(rows []index.JournalEntry) []index.JournalState {
	s := profile.JournalState{}
	var last int64
	for _, r := range rows {
		if r.Seq != last+1 {
			j.s.failure(Finding{Repository: j.pkgPath, Tag: profile.JournalTag(last + 1),
				Error: fmt.Sprintf("the journal has no entry %d before %d; the replay goes on over the rest", last+1, r.Seq)})
		}
		last = r.Seq
		if c, seq, err := profile.JournalChangeOf(r.ArtifactType, r.Attrs); err == nil {
			s.Apply(seq, c)
		}
	}
	out := make([]index.JournalState, 0, len(s))
	for _, k := range s.Keys() {
		out = append(out, index.JournalState{Kind: k.Kind, Key: k.Key, Value: s[k].Value, Seq: s[k].Seq})
	}
	return out
}
