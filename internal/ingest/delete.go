package ingest

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/lajstrom/lajstrom/internal/auth"
	"github.com/lajstrom/lajstrom/internal/format"
	"github.com/lajstrom/lajstrom/internal/index"
	"github.com/lajstrom/lajstrom/internal/oci/profile"
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
//     backend; one that names nothing is index.ErrNotFound. A version whose manifest the backend
//     does not have any more, or a journal tag of an npm package that the backend does not have
//     and the index does, is gone: its rows go all the same, so that the retry of a delete whose
//     index transaction failed after the backend deleted deletes them (#668).
//  3. Under the tag lock of the manifest (Store.WithReferrersLock, keyed by its subject when it
//     has one, else by its own digest; a journal tag _j<seq> of an npm package under the
//     package's journal lock instead, keyed by the empty blob's digest, docs/spec/
//     manifest-profile.md §8), with the referrers API or without: on an oci repo a
//     delete by tag lists the repository's tags and resolves each, and a manifest that other
//     tags also name is refused, index.ErrConflict naming them, and nothing changes (#284).
//     The referrers go first, each with its own referrers before it: cosign's legacy manifests
//     under sha256-<hex>.sig, .att and .sbom, the tag-schema index sha256-<hex>, then the
//     referrers the backend lists. Then the manifest, with every tag that names it.
//     The first request that changes anything is a plain delete, so a backend that refuses
//     deletes refuses it, provider.ErrUnsupported, and nothing has changed: a referrer deleted
//     on its own, without the referrers API, has its subject's sha256-<hex> index deleted first
//     and pushed again without it after. A manifest that is gone already is not an error.
//  4. Still under the lock, for a version, one index transaction deletes its rows (by its
//     digest), its package's when it was the last version, invalidates the repo's index_cache
//     and completes the audit row; for a journal entry of a package the index still has, its
//     row, the package's state folded again from the rows left and the invalidated index_cache
//     with the audit row; for any other manifest the audit row alone. The row is written pending
//     in step 3, in a transaction of its own after the tags are counted and before the first
//     request that changes anything, and failed with the error when the rest of step 3 or this
//     transaction fails. This transaction's failure is a plain error (500), but a database that
//     did not answer: index.ErrUnavailable stays wrapped (503 with Retry-After), for the retry
//     finds the rows gone in the backend (step 2) and deletes them. It is a delete by principal
//     with del.ClientIP (the request's client address when it is empty), the version's subject
//     (or the manifest's subject digest), the digest, and in the details the number of
//     referrers, the request id, del.Job as job when it is set, and the request's token as
//     token, token:<id>, when it came with one.
//
// A delete with del.Condition is a compare-and-delete of a version (format.Condition): the
// reference must be a digest, and a digest that is no version of the package's is refused, never
// deleted as another manifest. An npm version's delete takes the package's journal lock before
// the tag lock, so that no dist-tag moves onto it meanwhile (docs/spec/index-schema.md,
// Coordination: the journal lock before a tag lock). Under the locks the version is read again
// by its digest (deletion.check), and a condition that fails deletes nothing and writes no audit
// row: it is refused before the row, as a delete by tag of a manifest other tags name is.
//
// Errors wrap provider.ErrUnsupported (the backend does not delete manifests), index.ErrNotFound
// (nothing to delete), index.ErrConflict (other tags name the manifest: the error has a method
// OtherTags() []string, which returns them sorted) or provider.ErrConflict (the backend refused
// this one delete: Zot keeps a manifest that an image index lists), a *format.RefusedError (a
// condition failed), or none. Blobs are left to the backend's garbage collection.
func (p *Pipeline) Delete(ctx context.Context, repo index.Repo, del format.Deletion, principal auth.Principal) (*format.Deleted, error) {
	if del.Path == "" || del.Reference == "" {
		return nil, invalid(fmt.Errorf("ingest: delete in %s/%s: a package path and a reference are required", repo.Format, repo.Name))
	}
	if _, err := digest.Parse(del.Reference); del.Condition != nil && err != nil {
		return nil, invalid(fmt.Errorf("ingest: conditional delete in %s/%s: %q is not the digest of a manifest: %w", repo.Format, repo.Name,
			del.Reference, err))
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
	if _, entry := profile.JournalSeq(del.Reference); entry && repo.Format == "npm" {
		d.journal = true
	}
	if err := d.find(ctx); err != nil {
		return nil, err
	}
	d.ev = d.event(ctx, principal)
	err = d.locked(ctx, func(ctx context.Context) error {
		if err := d.check(ctx); err != nil {
			return err
		}
		if err := d.deleteInBackend(ctx); err != nil {
			setDetail(&d.ev, "referrers", d.referrers)
			p.fail(ctx, &d.ev, err)
			return err
		}
		return d.record(ctx)
	})
	if err != nil {
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
	gone    bool               // the backend does not have it any more (a version's, a journal entry's): its rows and referrers go all the same
	version *index.Version     // the version it is; nil for a manifest that is none
	pkg     *index.Package     // the version's package
	subject digest.Digest      // the subject of a manifest that is no version, when it has one
	journal bool               // the reference is a journal tag of an npm package

	referrers int              // deleted before it, theirs included
	ev        index.AuditEvent // the audit row of the delete, pending from before it (intend)
}

// find resolves the reference: to a version of the index when it names one, else in the
// backend, where a digest that a version holds still makes it that version; a journal tag the
// backend does not have, to the entry the index has under it (findEntry).
func (d *deletion) find(ctx context.Context) error {
	if err := d.findVersion(ctx); err != nil || d.version != nil {
		return err
	}
	if d.del.Condition != nil {
		return d.missing(ctx)
	}
	desc, err := d.rr.Resolve(ctx, d.del.Reference)
	switch {
	case errors.Is(err, provider.ErrNotFound) && d.journal && d.pkg != nil:
		return d.findEntry(ctx)
	case errors.Is(err, provider.ErrNotFound):
		return fmt.Errorf("%w: no manifest %s in %s", index.ErrNotFound, d.del.Reference, d.name)
	case err != nil:
		return fmt.Errorf("ingest: resolve %s in %s: %w", d.del.Reference, d.name, err)
	}
	d.target = desc
	if d.repo.Format != ociFormat {
		v, err := d.p.index.GetVersionByDigest(ctx, d.repo.ID, desc.Digest)
		switch {
		case err == nil && d.pkg != nil && v.PackageID == d.pkg.ID:
			d.version = v
			return nil
		case err != nil && !errors.Is(err, index.ErrNotFound):
			return fmt.Errorf("ingest: version of %s: %w", desc.Digest, err)
		}
	}
	d.subject, err = d.subjectOf(ctx)
	return err
}

// findEntry sets the target to the journal entry under the reference, which the backend does
// not have, when the index still has its row: a delete whose index transaction failed after the
// backend deleted the entry left it (#668), and its retry deletes the row and folds the state
// again, as the delete of a version that is gone does. Without a row it is index.ErrNotFound.
func (d *deletion) findEntry(ctx context.Context) error {
	rows, err := d.p.index.ListJournal(ctx, d.pkg.ID)
	if err != nil {
		return fmt.Errorf("ingest: journal of %s: %w", d.name, err)
	}
	i := slices.IndexFunc(rows, func(e index.JournalEntry) bool { return e.Tag == d.del.Reference })
	if i < 0 {
		return fmt.Errorf("%w: no manifest %s in %s", index.ErrNotFound, d.del.Reference, d.name)
	}
	d.target, d.gone = ocispec.Descriptor{MediaType: ocispec.MediaTypeImageManifest, Digest: rows[i].Digest}, true
	return nil
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

// subjectOf reads the subject of the target, a manifest that is no version: a referrer's
// delete rewrites the tag-schema index of its subject, which is locked for it.
func (d *deletion) subjectOf(ctx context.Context) (digest.Digest, error) {
	b, err := d.fetch(ctx, d.target)
	if err != nil {
		return "", err
	}
	var m struct {
		Subject *ocispec.Descriptor `json:"subject"`
	}
	if json.Unmarshal(b, &m) != nil || m.Subject == nil {
		return "", nil // no manifest with a subject: nothing to lock but the target's own index
	}
	return m.Subject.Digest, nil
}

// maxManifestBytes bounds the read of a manifest that is deleted, or of a referrers index: the
// facade takes manifests of 4 MiB by default.
const maxManifestBytes = 16 << 20

// locked calls fn under the locks of the delete (docs/spec/index-schema.md, Coordination): the
// tag lock of the target, one lock for the whole delete, its rows included. It is the referrers
// lock of the subject of a referrer, whose tag-schema index its delete rewrites without the
// referrers API, else of the target, whose own index goes; a journal entry's is the package's
// journal lock. A push by tag to an oci repo holds the same lock, so a tag pushed meanwhile is
// either counted by refuseTagged or pushed after the delete. A conditional delete of an npm
// version takes the package's journal lock first, which every move of a dist-tag holds: the
// journal lock before a tag lock, never the other way.
func (d *deletion) locked(ctx context.Context, fn func(context.Context) error) error {
	key := d.target.Digest
	switch {
	case d.journal:
		key = profile.EmptyConfig.Digest
	case d.subject != "":
		key = d.subject
	}
	tagLocked := func(ctx context.Context) error { return d.p.index.WithReferrersLock(ctx, d.name, key, fn) }
	if d.del.Condition != nil && d.repo.Format == "npm" && !d.journal {
		return d.p.index.WithReferrersLock(ctx, d.name, profile.EmptyConfig.Digest, tagLocked)
	}
	return tagLocked(ctx)
}

// check checks the condition of a conditional delete under its locks (format.Condition): the
// version is read again by its digest, which must still be the version and variant the client
// read, published no later than the cut-off, and, unforced, named by no pointer of its package.
// A delete without a condition passes.
func (d *deletion) check(ctx context.Context) error {
	c := d.del.Condition
	if c == nil {
		return nil
	}
	v, err := d.p.index.GetVersionByDigest(ctx, d.repo.ID, d.target.Digest)
	switch {
	case errors.Is(err, index.ErrNotFound):
		return d.missing(ctx)
	case err != nil:
		return fmt.Errorf("ingest: version of %s: %w", d.target.Digest, err)
	case v.PackageID != d.pkg.ID || v.Version != c.Version || v.Variant != c.Variant:
		return d.missing(ctx)
	}
	d.version = v
	if !c.UnmodifiedSince.IsZero() && v.CreatedAt.Truncate(time.Second).After(c.UnmodifiedSince) {
		return &format.RefusedError{Reason: format.RefusedYoung, Subject: d.subjectOfCondition(), PublishedAt: v.CreatedAt}
	}
	if c.Force {
		return nil
	}
	state, err := d.p.index.ListJournalState(ctx, d.pkg.ID)
	if err != nil {
		return fmt.Errorf("ingest: dist-tags of %s: %w", d.pkg.Name, err)
	}
	if pointers := format.PointersOf(state, v.Version); len(pointers) > 0 {
		return &format.RefusedError{Reason: format.RefusedProtected, Subject: d.subjectOfCondition(), Pointers: pointers}
	}
	return nil
}

// missing is the refusal of a conditional delete whose digest is no version of the package (any
// more): a *format.RefusedError, RefusedChanged, when the version and variant of the condition
// have another digest now, else index.ErrNotFound.
func (d *deletion) missing(ctx context.Context) error {
	c := d.del.Condition
	if d.pkg != nil {
		v, err := d.p.index.GetVersion(ctx, d.pkg.ID, c.Version, c.Variant)
		switch {
		case err == nil:
			return &format.RefusedError{Reason: format.RefusedChanged, Subject: d.subjectOfCondition(), Digests: []digest.Digest{v.Digest}}
		case !errors.Is(err, index.ErrNotFound):
			return fmt.Errorf("ingest: version %s of %s: %w", c.Version, d.pkg.Name, err)
		}
	}
	return fmt.Errorf("%w: %s is not in the index (deleted meanwhile)", index.ErrNotFound, d.subjectOfCondition())
}

// subjectOfCondition names the version of the condition in an error.
func (d *deletion) subjectOfCondition() string {
	c, name := d.del.Condition, d.del.Path
	if d.pkg != nil {
		name = d.pkg.Name
	}
	version := c.Version
	if c.Variant != "" {
		version += " (" + c.Variant + ")"
	}
	return fmt.Sprintf("version %s of %q in %s/%s", version, name, d.repo.Format, d.repo.Name)
}

// deleteInBackend deletes the target with its referrers, under the locks of the delete. Its audit
// row is written pending after the other tags are counted, before the first request that changes
// anything (#473).
func (d *deletion) deleteInBackend(ctx context.Context) error {
	if err := d.refuseTagged(ctx); err != nil {
		return err
	}
	if err := d.p.intend(ctx, &d.ev); err != nil {
		return err
	}
	if d.subject != "" && !d.api {
		return d.deleteReferrer(ctx)
	}
	return d.deleteTarget(ctx)
}

// tagResolvers bounds the resolves of refuseTagged that run at once.
const tagResolvers = 8

// refuseTagged refuses the delete by tag of a manifest of an oci repo that other tags also
// name, with a tagsError naming them. The distribution API cannot list the tags of a digest: it
// lists the repository's tags and resolves each but the reference, tagResolvers at a time. A
// delete by digest is not refused, nor one on a managed repo, where a manifest has one tag.
func (d *deletion) refuseTagged(ctx context.Context) error {
	if d.repo.Format != ociFormat || strings.Contains(d.del.Reference, ":") {
		return nil
	}
	var tags []string
	err := d.rr.Tags(ctx, "", func(page []string) error {
		tags = append(tags, page...)
		return nil
	})
	if err != nil && !errors.Is(err, provider.ErrNotFound) {
		return fmt.Errorf("ingest: tags of %s: %w", d.name, err)
	}
	tags = slices.DeleteFunc(tags, func(t string) bool { return t == d.del.Reference })
	names, errs := make([]bool, len(tags)), make([]error, len(tags))
	sem := make(chan struct{}, tagResolvers)
	var wg sync.WaitGroup
	for i, tag := range tags {
		sem <- struct{}{}
		wg.Go(func() {
			defer func() { <-sem }()
			desc, err := d.resolveTag(ctx, tag)
			names[i], errs[i] = desc != nil && desc.Digest == d.target.Digest, err
		})
	}
	wg.Wait()
	if err := cmp.Or(errs...); err != nil {
		return err
	}
	var others []string
	for i, tag := range tags {
		if names[i] {
			others = append(others, tag)
		}
	}
	if len(others) == 0 {
		return nil
	}
	slices.Sort(others)
	return &tagsError{name: d.name, ref: d.del.Reference, digest: d.target.Digest, tags: others}
}

// tagsError refuses a delete by tag of a manifest that other tags also name: index.ErrConflict,
// with the tags for the facade's answer (OtherTags).
type tagsError struct {
	name, ref string
	digest    digest.Digest
	tags      []string
}

func (e *tagsError) Error() string {
	return fmt.Sprintf("ingest: delete %s:%s: the manifest %s is also tagged %s, which the delete would take with it; "+
		"nothing was deleted (a delete by digest deletes every tag)", e.name, e.ref, e.digest, strings.Join(e.tags, ", "))
}

func (e *tagsError) Unwrap() error { return index.ErrConflict }

// OtherTags returns the other tags that name the manifest, sorted.
func (e *tagsError) OtherTags() []string { return slices.Clone(e.tags) }

// deleteTarget deletes the target with its referrers, the target itself unless it is gone.
func (d *deletion) deleteTarget(ctx context.Context) error {
	n, err := d.deleteTree(ctx, d.target, !d.gone, map[digest.Digest]bool{})
	d.referrers = n
	return err
}

// deleteReferrer deletes the target, a referrer, on a backend without the referrers API. Its
// subject's sha256-<hex> index goes first, a plain delete, so that oras-go rewrites no index
// before the delete of the referrer, which a backend that refuses deletes would leave out of the
// index; then the referrer with its own; then the index is pushed again with the referrers it
// listed but this one, or with all of them when the delete failed.
func (d *deletion) deleteReferrer(ctx context.Context) error {
	tag := "sha256-" + d.subject.Encoded()
	idx, err := d.resolveTag(ctx, tag)
	if err != nil {
		return err
	}
	if idx == nil {
		return d.deleteTarget(ctx)
	}
	listed, err := d.fetchIndex(ctx, *idx)
	if err != nil {
		return err
	}
	if err := d.deleteOne(ctx, *idx); err != nil {
		return err
	}
	err = d.deleteTarget(ctx)
	if err == nil {
		listed.Manifests = slices.DeleteFunc(listed.Manifests, func(m ocispec.Descriptor) bool { return m.Digest == d.target.Digest })
	}
	if len(listed.Manifests) == 0 {
		return err
	}
	if perr := d.pushIndex(ctx, tag, listed); perr != nil {
		d.p.log.ErrorContext(ctx, "ingest: the referrers index of a subject was not pushed again; its referrers are not listed",
			"name", d.name, "subject", d.subject.String(), "referrers", len(listed.Manifests), "error", perr.Error())
		return cmp.Or(err, perr)
	}
	return err
}

// fetchIndex reads the image index desc.
func (d *deletion) fetchIndex(ctx context.Context, desc ocispec.Descriptor) (ocispec.Index, error) {
	var idx ocispec.Index
	b, err := d.fetch(ctx, desc)
	if err == nil {
		err = json.Unmarshal(b, &idx)
	}
	if err != nil {
		return idx, fmt.Errorf("ingest: referrers index %s in %s: %w", desc.Digest, d.name, err)
	}
	return idx, nil
}

// pushIndex pushes idx under tag.
func (d *deletion) pushIndex(ctx context.Context, tag string, idx ocispec.Index) error {
	b, err := json.Marshal(idx)
	if err != nil {
		return fmt.Errorf("ingest: referrers index %s: %w", tag, err)
	}
	desc := ocispec.Descriptor{MediaType: ocispec.MediaTypeImageIndex, Digest: digest.FromBytes(b), Size: int64(len(b))}
	if err := d.rr.PushReference(ctx, desc, bytes.NewReader(b), tag); err != nil {
		return fmt.Errorf("ingest: push %s in %s: %w", tag, d.name, err)
	}
	return nil
}

// fetch reads the manifest desc, at most maxManifestBytes.
func (d *deletion) fetch(ctx context.Context, desc ocispec.Descriptor) ([]byte, error) {
	rc, err := d.rr.Fetch(ctx, desc)
	if err != nil {
		return nil, fmt.Errorf("ingest: fetch %s of %s: %w", desc.Digest, d.name, err)
	}
	defer func() { _ = rc.Close() }()
	b, err := io.ReadAll(io.LimitReader(rc, maxManifestBytes))
	if err != nil {
		return nil, fmt.Errorf("ingest: read %s of %s: %w", desc.Digest, d.name, err)
	}
	return b, nil
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

// event is the audit event of the delete, before it: a delete by principal with del.ClientIP
// (the request's client address when it is empty), the version's subject (or the manifest's
// subject digest, or the OCI repository), the digest and the request id. The number of
// referrers joins its details when the delete ends.
func (d *deletion) event(ctx context.Context, principal auth.Principal) index.AuditEvent {
	ev := index.AuditEvent{At: d.p.now(), Principal: principal.String(), Action: "delete",
		Repo: d.repo.Format + "/" + d.repo.Name, Digest: d.target.Digest, IP: d.del.ClientIP}
	withOrigin(ctx, &ev, d.details(ctx))
	switch {
	case d.version != nil:
		ev.Subject = d.pkg.Name + " " + d.version.Version
		if d.version.Variant != "" {
			ev.Subject += " " + d.version.Variant
		}
	case d.subject != "":
		ev.Subject = d.subject.String()
	default:
		ev.Subject = d.name
	}
	return ev
}

// record writes what the index holds of the delete: for a version, its rows go in one
// transaction, the package's with its last version, the repo's index_cache is invalidated and
// the audit row is completed ok; for any other manifest the audit row alone. The audit row goes
// to the audit.Writer after the commit when there is one. The backend has deleted already, so a
// failure of the transaction is an error without a sentinel (500), and the row is failed with
// it (#474): reindex repairs the rows. A database that did not answer is the exception:
// index.ErrUnavailable stays wrapped (503 with Retry-After), and the retry, which finds the rows
// with the manifest gone (find), deletes them (#668).
func (d *deletion) record(ctx context.Context) error {
	setDetail(&d.ev, "referrers", d.referrers)
	err := d.p.index.Tx(ctx, func(w index.Writer) error {
		switch {
		case d.version != nil:
			if err := d.deleteRows(ctx, w); err != nil {
				return err
			}
		case d.journal && d.pkg != nil:
			if err := d.deleteEntry(ctx, w); err != nil {
				return err
			}
		}
		return d.p.complete(ctx, w, &d.ev)
	})
	if err != nil {
		if errors.Is(err, index.ErrUnavailable) {
			err = fmt.Errorf("ingest: index rows of %s after the registry deleted it: %w", d.target.Digest, err)
		} else {
			err = fmt.Errorf("ingest: index rows of %s after the registry deleted it: %s", d.target.Digest, err.Error())
		}
		d.p.fail(ctx, &d.ev, err)
		return err
	}
	if d.p.audit != nil {
		if err := d.p.audit.Write(ctx, d.ev); err != nil {
			d.p.log.ErrorContext(ctx, "ingest: audit event of a delete not written", "repo", d.ev.Repo, "subject", d.ev.Subject,
				"digest", d.ev.Digest.String(), "error", err.Error())
		}
	}
	return nil
}

// details are the details of the audit row of the delete when it is written pending: the
// batch's job when there is one, and the request's token, token:<id>, when it came with one.
// The referrers join them when the delete ends.
func (d *deletion) details(ctx context.Context) map[string]any {
	details := map[string]any{}
	if d.del.Job != "" {
		details["job"] = d.del.Job
	}
	if c := auth.CredentialFrom(ctx); c.Kind == auth.CredPAT || c.Kind == auth.CredDeploy {
		details["token"] = "token:" + strconv.FormatInt(c.TokenID, 10)
	}
	return details
}

// deleteRows deletes the version's rows on w, found by the target's digest, its package's when
// no version is left, and invalidates the repo's index_cache. A row another delete took
// meanwhile is no error; the id read before the lock is not used, since a version published
// again after that delete may have a row of its own under it.
func (d *deletion) deleteRows(ctx context.Context, w index.Writer) error {
	v, err := w.GetVersionByDigest(ctx, d.repo.ID, d.target.Digest)
	switch {
	case err == nil && v.PackageID == d.pkg.ID:
		if err := w.DeleteVersion(ctx, v.ID); err != nil && !errors.Is(err, index.ErrNotFound) {
			return err
		}
	case err != nil && !errors.Is(err, index.ErrNotFound):
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

// deleteEntry deletes the journal row of the entry under the reference's tag, when the package
// has one, folds the package's state again from the rows left and invalidates the repo's
// index_cache.
func (d *deletion) deleteEntry(ctx context.Context, w index.Writer) error {
	rows, err := w.ListJournal(ctx, d.pkg.ID)
	if err != nil {
		return err
	}
	i := slices.IndexFunc(rows, func(e index.JournalEntry) bool { return e.Tag == d.del.Reference })
	if i < 0 {
		return nil
	}
	if err := w.DeleteJournal(ctx, rows[i].ID); err != nil && !errors.Is(err, index.ErrNotFound) {
		return err
	}
	if err := w.ReplaceJournalState(ctx, d.pkg.ID, foldJournal(slices.Delete(rows, i, i+1))); err != nil {
		return err
	}
	return w.InvalidateIndexCache(ctx, d.repo.ID, "")
}
