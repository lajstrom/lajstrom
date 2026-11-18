package reindex

import (
	"context"
	"errors"
	"slices"
	"sync"

	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/lajstrom/lajstrom/internal/index"
	"github.com/lajstrom/lajstrom/internal/ingest"
	"github.com/lajstrom/lajstrom/internal/policy"
	"github.com/lajstrom/lajstrom/internal/provider"
)

// Attacher reads the attachments of a referrer as ingest's PutReferrer indexes them, and names
// the trust that labels them (#89; docs/spec/index-schema.md, Attachments): the ingest pipeline.
type Attacher interface {
	InspectAttachments(ctx context.Context, repo index.Repo, v index.Version, r ingest.Referrer) ([]index.Attachment, error)
	AttachmentTrust() string
}

var _ Attacher = (*ingest.Pipeline)(nil)

// legacyRef is a manifest under cosign's legacy tag sha256-<hex>.sig, .att or .sbom that the
// scan read: it has no subject, and the tag names the version it attaches to.
type legacyRef struct {
	tag     string
	desc    ocispec.Descriptor
	raw     []byte
	subject digest.Digest
}

// keepLegacy records a manifest under cosign's legacy tag of pkgPath for the attachments pass.
func (s *scan) keepLegacy(pkgPath string, r legacyRef) {
	if s.w.attacher == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.legacy[pkgPath] = append(s.legacy[pkgPath], r)
}

// attachments indexes the attachments of every version whose manifest is in the OCI repository
// rr, at pkgPath, once its tags are read: the versions of the package whose versions may be there
// (index.PackageOf) whose manifest is there (index.ManifestPath), as known takes them. The scan
// of the other form of a split (#642) lists the referrers of the versions found there, and the
// scan of the indexed form leaves them to it, neither listing them where they are not nor
// sweeping their rows (#1133). Each version at a time under the scan's semaphore
// (versionAttachments). Nothing without an Attacher, on an oci repo, or for a repository the
// index has no package at.
func (s *scan) attachments(ctx context.Context, rr provider.Repository, cache *manifests, pkgPath string) {
	s.mu.Lock()
	legacy := s.legacy[pkgPath]
	delete(s.legacy, pkgPath)
	s.mu.Unlock()
	if s.w.attacher == nil || s.repo.Format == ociFormat || s.stopped(ctx) {
		return
	}
	pkg, err := index.PackageOf(ctx, s.w.index, s.repo.ID, pkgPath)
	if errors.Is(err, index.ErrNotFound) || s.dbErr(err) {
		return
	}
	versions, err := s.w.index.ListVersions(ctx, pkg.ID)
	if s.dbErr(err) {
		return
	}
	trust := s.w.attacher.AttachmentTrust()
	var wg sync.WaitGroup
	for _, v := range versions {
		if s.stopped(ctx) {
			break
		}
		if index.ManifestPath(*pkg, v) != pkgPath {
			continue // at another form of a split: that form's scan lists its referrers
		}
		s.sem <- struct{}{}
		wg.Go(func() {
			defer func() { <-s.sem }()
			s.versionAttachments(ctx, rr, cache, pkgPath, v, legacy, trust)
		})
	}
	wg.Wait()
}

// attachmentSource is a referrer of a version: listed by the registry, or under cosign's legacy
// tag with the bytes the scan read.
type attachmentSource struct {
	tag  string
	desc ocispec.Descriptor
	raw  []byte
}

// versionAttachments marks and sweeps the attachments of the version v: its referrers as the
// provider lists them and the legacy manifests of its digest. A referrer whose rows have the
// trust hash trust is seen without a read, unless the run verifies; another is read, inspected
// and its rows replaced. Then the version's rows not seen since the run started go. A listing
// that fails sweeps nothing; a referrer that cannot be read or inspected is an error of the
// report, and its rows are kept.
func (s *scan) versionAttachments(ctx context.Context, rr provider.Repository, cache *manifests, pkgPath string, v index.Version,
	legacy []legacyRef, trust string,
) {
	sources, ok := s.attachmentSources(ctx, rr, pkgPath, v, legacy)
	if !ok {
		return
	}
	rows, err := s.w.index.ListAttachments(ctx, []int64{v.ID})
	if s.dbErr(err) {
		return
	}
	have := map[digest.Digest][]index.Attachment{}
	for _, a := range rows {
		have[a.Digest] = append(have[a.Digest], a)
	}
	var touch []int64
	count := 0
	for _, src := range sources {
		known := have[src.desc.Digest]
		if len(known) > 0 && !s.req.Verify && !slices.ContainsFunc(known, func(a index.Attachment) bool { return a.Trust != trust }) {
			touch = append(touch, attachmentIDs(known)...)
			count += len(known)
			continue
		}
		written, ok := s.referrerAttachments(ctx, rr, cache, pkgPath, v, src)
		if !ok {
			touch = append(touch, attachmentIDs(known)...) // kept until a run reads it
			continue
		}
		count += written
	}
	if s.dbErr(s.w.index.TouchAttachments(ctx, touch, s.w.now())) {
		return
	}
	err = s.attachmentsTx(ctx, func(w index.Writer) (bool, error) {
		n, err := w.SweepAttachments(ctx, v.ID, s.start)
		return n > 0, err
	})
	if s.dbErr(err) {
		return
	}
	s.mu.Lock()
	s.rep.Attachments += count
	s.mu.Unlock()
}

// attachmentSources returns the referrers of the version v, as the provider lists them in rr, and
// the legacy manifests of its digest, each digest once; ok is false when the listing failed, and
// the report has the error.
func (s *scan) attachmentSources(ctx context.Context, rr provider.Repository, pkgPath string, v index.Version, legacy []legacyRef,
) ([]attachmentSource, bool) {
	var listed []ocispec.Descriptor
	err := rr.Referrers(ctx, ocispec.Descriptor{MediaType: ocispec.MediaTypeImageManifest, Digest: v.Digest}, "",
		func(page []ocispec.Descriptor) error {
			listed = append(listed, page...)
			return nil
		})
	if err != nil && !errors.Is(err, provider.ErrNotFound) {
		s.failure(Finding{Repository: pkgPath, Tag: v.Tag, Digest: v.Digest, Error: "list the referrers: " + err.Error()})
		return nil, false
	}
	var sources []attachmentSource
	seen := map[digest.Digest]bool{}
	for _, d := range listed {
		if !seen[d.Digest] {
			seen[d.Digest] = true
			sources = append(sources, attachmentSource{desc: d})
		}
	}
	for _, l := range legacy {
		if l.subject == v.Digest && !seen[l.desc.Digest] {
			seen[l.desc.Digest] = true
			sources = append(sources, attachmentSource{tag: l.tag, desc: l.desc, raw: l.raw})
		}
	}
	return sources, true
}

// referrerAttachments reads the referrer src of the version v, inspects it and replaces its rows,
// and returns how many it wrote; ok is false when it could not, and the report has the error.
func (s *scan) referrerAttachments(ctx context.Context, rr provider.Repository, cache *manifests, pkgPath string, v index.Version,
	src attachmentSource,
) (int, bool) {
	f := Finding{Repository: pkgPath, Tag: src.tag, Digest: src.desc.Digest, Subject: v.Digest}
	raw := src.raw
	if raw == nil {
		var err error
		if raw, err = s.fetch(ctx, rr, cache, src.desc); err != nil {
			f.Error = "read the referrer: " + err.Error()
			s.failure(f)
			return 0, false
		}
	}
	rows, err := s.w.attacher.InspectAttachments(ctx, s.repo, v, ingest.Referrer{Path: pkgPath, Tag: src.tag, Desc: src.desc,
		Manifest: raw, Subject: v.Digest})
	if err != nil {
		f.Error = "the attachments of the referrer: " + err.Error()
		s.failure(f)
		return 0, false
	}
	at := s.w.now()
	for i := range rows {
		rows[i].IndexedAt = at
	}
	err = s.attachmentsTx(ctx, func(w index.Writer) (bool, error) {
		return true, w.ReplaceAttachments(ctx, v.ID, src.desc.Digest, rows)
	})
	if errors.Is(err, index.ErrNotFound) {
		return 0, true // the version went meanwhile, its rows with it
	} else if s.dbErr(err) {
		return 0, false
	}
	return len(rows), true
}

// attachmentsTx runs write, a change of a version's attachment rows that reports whether it
// changed any. On a repo with required attachments it runs in a transaction that also invalidates
// the repo's documents when it did, so that the next render decides on the rows (docs/spec/
// policy.md §11, index-schema.md, Attachments); elsewhere as it is, and the end of the scan
// invalidates them.
func (s *scan) attachmentsTx(ctx context.Context, write func(w index.Writer) (bool, error)) error {
	if len(policy.RequireAttachments(s.repo)) == 0 {
		_, err := write(s.w.index)
		return err
	}
	return s.w.index.Tx(ctx, func(w index.Writer) error {
		changed, err := write(w)
		if err != nil || !changed {
			return err
		}
		return w.InvalidateIndexCache(ctx, s.repo.ID, "")
	})
}

// attachmentIDs returns the ids of rows.
func attachmentIDs(rows []index.Attachment) []int64 {
	ids := make([]int64, 0, len(rows))
	for _, a := range rows {
		ids = append(ids, a.ID)
	}
	return ids
}
