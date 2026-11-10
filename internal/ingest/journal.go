package ingest

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/lajstrom/lajstrom/internal/auth"
	"github.com/lajstrom/lajstrom/internal/format"
	"github.com/lajstrom/lajstrom/internal/index"
	"github.com/lajstrom/lajstrom/internal/oci/profile"
	"github.com/lajstrom/lajstrom/internal/provider"
)

// The audit actions of journal entries (docs/spec/http-routing.md, npm), by artifact type.
var journalActions = map[string]string{
	profile.ArtifactTypeNpmDistTags:    "npm.dist-tag",
	profile.ArtifactTypeNpmDeprecation: "npm.deprecate",
}

// Journal appends ops to the journal of the package pkgPath of repo, a hosted npm repo
// (docs/spec/manifest-profile.md §8, ADR 0012), and returns the seq each op got, in order. It
// implements format.Publisher.
//
//  1. The ops are checked (profile.JournalChange.Check): ErrInvalid. A repo of another format is
//     ErrInvalid too, one of another kind format.ErrMethodNotAllowed: a proxy journals nothing.
//  2. Under the package's journal lock, Store.WithReferrersLock of its OCI repository and the
//     empty blob's digest, which no manifest has: the package (index.ErrNotFound without one)
//     and n, the highest seq the index has of it; the tags _j<n+1>, _j<n+2>, … that the registry
//     has, each read, checked and adopted (an entry whose writer stopped before its index
//     write); then every version an op names must be one of the package (ErrInvalid); then each
//     op is pushed as the next entry.
//  3. One index transaction, still under the lock: the journal rows of the adopted and the new
//     entries, the package's journal_state folded from all its rows, the repo's index_cache
//     invalidated, and an audit row per op (npm.dist-tag or npm.deprecate, with the entry's
//     digest). When it fails the entries stay in the registry, and the next append or reindex
//     adopts them; the error is a plain one (500).
func (p *Pipeline) Journal(ctx context.Context, repo index.Repo, pkgPath string, ops []format.JournalOp, principal auth.Principal) ([]int64, error) {
	if err := checkJournal(repo, ops); err != nil || len(ops) == 0 {
		return []int64{}, err
	}
	prov, err := p.registry(repo)
	if err != nil {
		return nil, err
	}
	j := &journal{p: p, repo: repo, pkgPath: pkgPath, name: ociName(repo, pkgPath), ops: ops, principal: principal}
	if j.rr, err = prov.Repository(ctx, j.name); err != nil {
		return nil, fmt.Errorf("ingest: repository %s: %w", j.name, err)
	}
	if err := p.index.WithReferrersLock(ctx, j.name, profile.EmptyConfig.Digest, j.append); err != nil {
		return nil, err
	}
	for _, e := range j.written[len(j.adopted):] {
		p.log.InfoContext(ctx, "ingest: journal entry", "repo", repo.Format+"/"+repo.Name, "package", j.pkg.Name,
			"seq", e.Seq, "op", e.Op, "tag", e.Tag, "version", e.Version, "digest", j.digests[e.Seq].String(), "principal", principal.String())
	}
	seqs := make([]int64, len(ops))
	for i, e := range j.written[len(j.adopted):] {
		seqs[i] = e.Seq
	}
	return seqs, nil
}

// checkJournal checks the repo and every op before anything is read.
func checkJournal(repo index.Repo, ops []format.JournalOp) error {
	switch {
	case repo.Format != "npm":
		return fmt.Errorf("%w: ingest: repo %s/%s: only npm packages have a journal", ErrInvalid, repo.Format, repo.Name)
	case repo.Kind != "" && repo.Kind != index.KindHosted:
		return fmt.Errorf("%w: ingest: repo %s/%s is a %s repo, which journals nothing", format.ErrMethodNotAllowed, repo.Format, repo.Name, repo.Kind)
	}
	for i, op := range ops {
		if err := changeOf(op).Check(); err != nil {
			return fmt.Errorf("%w: ingest: journal op %d: %w", ErrInvalid, i+1, err)
		}
	}
	return nil
}

// changeOf is the change of an op.
func changeOf(op format.JournalOp) profile.JournalChange {
	return profile.JournalChange{ArtifactType: op.ArtifactType, Op: op.Op, Tag: op.Tag, Version: op.Version, Message: op.Message}
}

// journal is one Journal on its way.
type journal struct {
	p         *Pipeline
	repo      index.Repo
	pkgPath   string
	name      string // the OCI repository, without the instance prefix
	rr        provider.Repository
	ops       []format.JournalOp
	principal auth.Principal

	pkg     *index.Package
	adopted []profile.JournalEntry  // found in the registry after the index's last
	written []profile.JournalEntry  // adopted, then the ops' own
	digests map[int64]digest.Digest // of written, by seq
	events  []index.AuditEvent      // one per op
	at      time.Time               // the time of the entries and of their rows
	pushed  bool                    // the empty blob is in the repository
}

// append runs steps 2 and 3 of Journal under the journal lock.
func (j *journal) append(ctx context.Context) error {
	var err error
	if j.pkg, err = j.p.index.GetPackageByNameOCI(ctx, j.repo.ID, j.pkgPath); err != nil {
		return fmt.Errorf("ingest: journal of %s: %w", j.name, err)
	}
	rows, err := j.p.index.ListJournal(ctx, j.pkg.ID)
	if err != nil {
		return fmt.Errorf("ingest: journal of %s: %w", j.name, err)
	}
	var n int64
	if len(rows) > 0 {
		n = rows[len(rows)-1].Seq
	}
	j.at, j.digests = j.p.now().UTC().Truncate(time.Second), map[int64]digest.Digest{}
	if err := j.adopt(ctx, n); err != nil {
		return err
	}
	if err := j.checkVersions(ctx); err != nil {
		return err
	}
	n += int64(len(j.adopted))
	j.written = append(j.written, j.adopted...)
	for i, op := range j.ops {
		e := profile.JournalEntry{JournalChange: changeOf(op), Format: j.repo.Format, Repo: j.repo.Name, Name: j.pkg.Name,
			Seq: n + int64(i) + 1, Created: j.at}
		if j.principal.Kind != auth.KindAnonymous {
			e.UploadedBy = j.principal.String()
		}
		if err := j.push(ctx, e); err != nil {
			return err
		}
		j.written = append(j.written, e)
	}
	return j.record(ctx)
}

// adopt reads the entries the registry has after the index's last, n, and keeps them: their
// writer pushed them and stopped before its index write. An entry that breaks the rules of §8
// stops the journal, which cannot skip a seq nor re-point a tag: the operator removes it.
func (j *journal) adopt(ctx context.Context, n int64) error {
	for seq := n + 1; ; seq++ {
		tag := profile.JournalTag(seq)
		desc, err := j.rr.Resolve(ctx, tag)
		if errors.Is(err, provider.ErrNotFound) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("ingest: resolve %s:%s: %w", j.name, tag, err)
		}
		e, err := j.read(ctx, desc)
		switch {
		case err == nil && (e.Seq != seq || e.Repo != j.repo.Name || e.Name != j.pkg.Name):
			err = fmt.Errorf("it is entry %d of %s in %s", e.Seq, e.Name, e.Repo)
		case err == nil:
			j.p.log.WarnContext(ctx, "ingest: a journal entry without its index row adopted", "name", j.name, "tag", tag,
				"digest", desc.Digest.String())
			j.adopted, j.digests[seq] = append(j.adopted, e), desc.Digest
			continue
		}
		return fmt.Errorf("ingest: the journal of %s cannot go on: %s:%s is not entry %d of the package: %s", j.name, j.name, tag, seq, err.Error())
	}
}

// read fetches the manifest desc and parses it as a journal entry.
func (j *journal) read(ctx context.Context, desc ocispec.Descriptor) (profile.JournalEntry, error) {
	rc, err := j.rr.Fetch(ctx, desc)
	if err != nil {
		return profile.JournalEntry{}, err
	}
	defer func() { _ = rc.Close() }()
	raw, err := io.ReadAll(io.LimitReader(rc, maxManifestBytes))
	if err != nil {
		return profile.JournalEntry{}, err
	}
	if digest.FromBytes(raw) != desc.Digest {
		return profile.JournalEntry{}, fmt.Errorf("the manifest's bytes are not %s", desc.Digest)
	}
	var m ocispec.Manifest
	if err := json.Unmarshal(raw, &m); err != nil {
		return profile.JournalEntry{}, err
	}
	return profile.ParseJournal(m)
}

// checkVersions checks that every version an op names is one of the package's.
func (j *journal) checkVersions(ctx context.Context) error {
	versions, err := j.p.index.ListVersions(ctx, j.pkg.ID)
	if err != nil {
		return fmt.Errorf("ingest: versions of %s: %w", j.name, err)
	}
	has := map[string]bool{}
	for _, v := range versions {
		has[v.Version] = true
	}
	for _, op := range j.ops {
		if op.Version != "" && !has[op.Version] {
			return fmt.Errorf("%w: %s has no version %s", ErrInvalid, j.pkg.Name, op.Version)
		}
	}
	return nil
}

// push pushes the entry e under its tag, the empty blob first when the repository lacks it.
func (j *journal) push(ctx context.Context, e profile.JournalEntry) error {
	m, err := profile.BuildJournal(e)
	if err != nil {
		return fmt.Errorf("ingest: journal entry %d of %s: %w", e.Seq, j.name, err)
	}
	raw, d, err := profile.Encode(m)
	if err != nil {
		return err
	}
	if !j.pushed {
		ok, err := j.rr.Exists(ctx, profile.EmptyConfig)
		if err == nil && !ok {
			err = j.rr.Push(ctx, profile.EmptyConfig, bytes.NewReader(ocispec.DescriptorEmptyJSON.Data))
		}
		if err != nil {
			return fmt.Errorf("ingest: empty blob in %s: %w", j.name, err)
		}
		j.pushed = true
	}
	tag := profile.JournalTag(e.Seq)
	desc := ocispec.Descriptor{MediaType: ocispec.MediaTypeImageManifest, Digest: d, Size: int64(len(raw))}
	if err := j.rr.PushReference(ctx, desc, bytes.NewReader(raw), tag); err != nil {
		return fmt.Errorf("ingest: push %s:%s: %w", j.name, tag, err)
	}
	j.digests[e.Seq] = d
	ev := index.AuditEvent{At: j.at, Principal: j.principal.String(), Action: journalActions[e.ArtifactType],
		Repo: j.repo.Format + "/" + j.repo.Name, Subject: j.pkg.Name + " " + e.Version, Digest: d}
	details := map[string]any{"seq": e.Seq, "op": e.Op}
	if e.Tag != "" {
		ev.Subject, details["tag"] = j.pkg.Name+" "+e.Tag, e.Tag
	}
	if e.Version != "" {
		details["version"] = e.Version
	}
	withOrigin(ctx, &ev, details)
	j.events = append(j.events, ev)
	return nil
}

// record writes the rows of the written entries, the package's state folded from all its rows,
// the invalidated index_cache and the audit rows in one transaction, and the audit rows to the
// audit.Writer after it when there is one.
func (j *journal) record(ctx context.Context) error {
	err := j.p.index.Tx(ctx, func(w index.Writer) error {
		for _, e := range j.written {
			row := index.JournalEntry{PackageID: j.pkg.ID, Seq: e.Seq, Tag: profile.JournalTag(e.Seq), Digest: j.digests[e.Seq],
				ArtifactType: e.ArtifactType, Attrs: e.Attrs(), CreatedAt: e.Created, IndexedAt: j.at}
			if err := w.UpsertJournal(ctx, &row); err != nil {
				return err
			}
		}
		rows, err := w.ListJournal(ctx, j.pkg.ID)
		if err != nil {
			return err
		}
		if err := w.ReplaceJournalState(ctx, j.pkg.ID, foldJournal(rows)); err != nil {
			return err
		}
		if err := w.InvalidateIndexCache(ctx, j.repo.ID, ""); err != nil {
			return err
		}
		for i := range j.events {
			if j.p.audit == nil {
				if err := w.AppendAudit(ctx, &j.events[i]); err != nil {
					return err
				}
			}
		}
		return nil
	})
	if err != nil {
		j.p.log.ErrorContext(ctx, "ingest: the registry has the journal entries, the index write failed; the next append or reindex adopts them",
			"name", j.name, "entries", len(j.written), "error", err.Error())
		return fmt.Errorf("ingest: index the journal of %s after the registry took it: %s", j.name, err.Error())
	}
	if j.p.audit != nil {
		for _, ev := range j.events {
			if err := j.p.audit.Write(ctx, ev); err != nil {
				j.p.log.ErrorContext(ctx, "ingest: audit event of a journal entry not written", "repo", ev.Repo, "subject", ev.Subject,
					"digest", ev.Digest.String(), "error", err.Error())
			}
		}
	}
	return nil
}

// foldJournal returns the state of a package from its journal rows in seq order (§8, Replay), as
// journal_state keeps it, ordered by kind and key. A row whose change cannot be read is left
// out: rows are written from entries that were checked.
func foldJournal(rows []index.JournalEntry) []index.JournalState {
	s := profile.JournalState{}
	for _, r := range rows {
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
