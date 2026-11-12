package ingest

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"slices"

	"github.com/opencontainers/go-digest"

	"github.com/lajstrom/lajstrom/internal/auth"
	"github.com/lajstrom/lajstrom/internal/index"
	"github.com/lajstrom/lajstrom/internal/provider"
)

// actionRepair is the audit action of a subject's sha256-<hex> index pushed again with the
// referrers it lacked (RepairReferrers).
const actionRepair = "referrers.repair"

// ReferrersRepair is a subject's sha256-<hex> index that RepairReferrers found lacking referrers,
// and what became of it.
type ReferrersRepair struct {
	Path    string          // the package path of the subject's OCI repository, below the repo's base
	Subject digest.Digest   // the subject
	Index   digest.Digest   // the index pushed with them; "" when Err is set
	Added   []digest.Digest // the referrers it lacked, in the order of their pushes
	Err     error           // a read or the push failed: the index is as it was
}

// RepairReferrers pushes again, on a registry without the referrers API, the sha256-<hex> index of
// each subject of found that lacks referrers the audit log has as pushed to repo and the
// registry still has with that subject, or that is missing while there are such: a delete whose
// push of the index failed (#711), a lost referrers lock or a registry's garbage collector can
// leave it so, and a collector of untagged manifests takes what no index lists (#628). found maps
// the digest of each manifest a reindex run found in repo to the package paths of the OCI
// repositories it found it in: the audit log names no OCI repository.
//
// The audit log is read once: the push rows whose subject is a digest, which PutReferrer writes.
// Each subject's index is compared with them first without a lock; one that lacks some is read
// again under the referrers lock of the subject (Store.WithReferrersLock), which PutReferrer and
// Delete hold, and, while the subject is still in the repository, pushed with them after those it
// lists (descriptors as Delete's repair writes them). Its audit row, referrers.repair by principal
// with the subject's digest, the digest of the index pushed and in the details the package path
// and the referrers added, is written pending before the push and completed after it, ok or
// failed. A registry with the referrers API lists referrers itself, and nothing is read; with an
// audit.Writer the log names no referrer, and nothing is repaired.
//
// The result has one entry per index found lacking, in the order of the subjects' digests: pushed,
// or with the error of a read or of the push. The error is the audit log's or the registry's
// capabilities', and then nothing was read, or the context's, with the entries before it.
func (p *Pipeline) RepairReferrers(ctx context.Context, repo index.Repo, found map[digest.Digest][]string, principal auth.Principal) ([]ReferrersRepair, error) {
	prov, err := p.registry(repo)
	if err != nil {
		return nil, err
	}
	caps, err := prov.Capabilities(ctx)
	if err != nil {
		return nil, fmt.Errorf("ingest: capabilities of registry %s: %w", repo.Registry, err)
	}
	if caps.Referrers || p.audit != nil || len(found) == 0 {
		return nil, nil
	}
	pushed, err := p.pushedReferrers(ctx, repo, func(s digest.Digest) bool { return len(found[s]) > 0 })
	if err != nil {
		return nil, fmt.Errorf("ingest: the audit rows of the referrers of %s/%s: %w", repo.Format, repo.Name, err)
	}
	var out []ReferrersRepair
	for _, subject := range slices.Sorted(maps.Keys(pushed)) {
		for _, pkgPath := range found[subject] {
			if err := ctx.Err(); err != nil {
				return out, err
			}
			if r := p.repairIndex(ctx, repo, prov, pkgPath, subject, pushed[subject], principal); r != nil {
				out = append(out, *r)
			}
		}
	}
	return out, nil
}

// repairIndex is RepairReferrers for the subject in the OCI repository of pkgPath, whose
// referrers the audit log has as pushed: nil when its index lacks none of them.
func (p *Pipeline) repairIndex(ctx context.Context, repo index.Repo, prov provider.Provider, pkgPath string, subject digest.Digest,
	pushed []digest.Digest, principal auth.Principal) *ReferrersRepair {
	r := &ReferrersRepair{Path: pkgPath, Subject: subject}
	name := ociName(repo, pkgPath)
	rr, err := prov.Repository(ctx, name)
	if err != nil {
		r.Err = fmt.Errorf("ingest: repository %s: %w", name, err)
		return r
	}
	d := &deletion{p: p, repo: repo, name: name, rr: rr, subject: subject, pushed: pushed}
	tag := "sha256-" + subject.Encoded()
	if _, missing, err := d.lacking(ctx, tag); err != nil || len(missing) == 0 {
		if r.Err = err; err != nil {
			return r
		}
		return nil
	}
	r.Err = p.index.WithReferrersLock(ctx, name, subject, func(ctx context.Context) error {
		return d.pushRepaired(ctx, tag, r, principal)
	})
	switch {
	case r.Err != nil:
		return r
	case r.Index == "":
		return nil // lacking none under the lock: a push or a delete meanwhile
	}
	p.log.WarnContext(ctx, "ingest: the referrers index of a subject lacked referrers the audit log has as pushed; pushed with them",
		"name", name, "subject", subject.String(), "added", len(r.Added), "index", r.Index.String())
	return r
}

// pushRepaired is the part of repairIndex under the referrers lock of the subject: the index under
// tag is read again and, while the subject is in the repository, pushed with the referrers it
// lacks, between its audit row pending and completed; r gets the index and the referrers added.
func (d *deletion) pushRepaired(ctx context.Context, tag string, r *ReferrersRepair, principal auth.Principal) error {
	if desc, err := d.resolveTag(ctx, d.subject.String()); err != nil || desc == nil {
		return err // the subject went meanwhile: its delete took the index
	}
	old, missing, err := d.lacking(ctx, tag)
	if err != nil || len(missing) == 0 {
		return err
	}
	s, err := encodeIndex(tag, withReferrers(old, missing))
	if err != nil {
		return err
	}
	added := make([]digest.Digest, 0, len(missing))
	for _, m := range missing {
		added = append(added, m.Digest)
	}
	p := d.p
	ev := index.AuditEvent{At: p.now(), Principal: principal.String(), Action: actionRepair, Repo: d.repo.Format + "/" + d.repo.Name,
		Subject: d.subject.String(), Digest: s.desc.Digest}
	ev.Details, _ = json.Marshal(map[string]any{"path": r.Path, "added": added}) // strings always marshal
	if err := p.intend(ctx, &ev); err != nil {
		return err
	}
	if err := d.rr.PushReference(ctx, s.desc, bytes.NewReader(s.raw), tag); err != nil {
		err = fmt.Errorf("ingest: push %s in %s: %w", tag, d.name, err)
		p.fail(ctx, &ev, err)
		return err
	}
	if err := p.complete(context.WithoutCancel(ctx), p.index, &ev); err != nil {
		p.log.ErrorContext(ctx, "ingest: the audit row of a referrers repair not completed; it is unknown after ingest.claimTTL",
			"repo", ev.Repo, "subject", ev.Subject, "digest", ev.Digest.String(), "error", err.Error())
	}
	r.Index, r.Added = s.desc.Digest, added
	return nil
}
