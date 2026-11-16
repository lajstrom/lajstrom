package reindex

import (
	"context"
	"errors"
	"slices"

	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/lajstrom/lajstrom/internal/attest"
	"github.com/lajstrom/lajstrom/internal/index"
	"github.com/lajstrom/lajstrom/internal/ingest"
	"github.com/lajstrom/lajstrom/internal/provider"
)

// VerdictInspector reads the verdicts of a referrer of an oci repo as ingest's PutReferrer
// indexes them (docs/spec/policy.md §10): the ingest pipeline. An Attacher without it leaves an
// oci repo's verdicts as they are.
type VerdictInspector interface {
	InspectVerdicts(ctx context.Context, repo index.Repo, r ingest.Referrer) ([]index.Verdict, error)
}

var _ VerdictInspector = (*ingest.Pipeline)(nil)

// annotationPredicateType is cosign's manifest annotation of a bundle's predicate type, which a
// referrers listing carries.
const annotationPredicateType = "dev.sigstore.bundle.predicateType"

// keepTagged records, on an oci repo, the manifest d that a tag of pkgPath names.
func (s *scan) keepTagged(pkgPath string, d digest.Digest) {
	if s.repo.Format != ociFormat {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.tagged == nil {
		s.tagged = map[string][]digest.Digest{}
	}
	if !slices.Contains(s.tagged[pkgPath], d) {
		s.tagged[pkgPath] = append(s.tagged[pkgPath], d)
	}
}

// ociVerdicts marks and sweeps the verdicts of an oci repo's OCI repository pkgPath: the
// referrers of each manifest its tags name, one listing per manifest, whose predicate type the
// listing says is a verdict's, are read and inspected; a referrer whose rows come out as they
// were is touched, another's rows replaced; then the rows of pkgPath not seen since the run
// started go. A listing or a read that fails is an error of the report, and nothing is swept.
func (s *scan) ociVerdicts(ctx context.Context, rr provider.Repository, cache *manifests, pkgPath string) {
	vi, ok := s.w.attacher.(VerdictInspector)
	if !ok || s.repo.Format != ociFormat || s.stopped(ctx) {
		return
	}
	s.mu.Lock()
	subjects := slices.Clone(s.tagged[pkgPath])
	s.mu.Unlock()
	have, err := s.w.index.ListVerdicts(ctx, index.VerdictFilter{RepoID: s.repo.ID})
	if s.dbErr(err) {
		return
	}
	complete := true
	for _, subject := range subjects {
		complete = s.subjectVerdicts(ctx, vi, rr, cache, pkgPath, subject, have) && complete
	}
	if !complete || s.stopped(ctx) {
		return
	}
	_, err = s.w.index.SweepOCIVerdicts(ctx, s.repo.ID, pkgPath, s.start)
	s.dbErr(err)
}

// subjectVerdicts indexes the verdicts among the referrers of subject; false when the listing or
// a read failed.
func (s *scan) subjectVerdicts(ctx context.Context, vi VerdictInspector, rr provider.Repository, cache *manifests, pkgPath string,
	subject digest.Digest, have []index.Verdict) bool {
	var listed []ocispec.Descriptor
	err := rr.Referrers(ctx, ocispec.Descriptor{MediaType: ocispec.MediaTypeImageManifest, Digest: subject}, "",
		func(page []ocispec.Descriptor) error {
			listed = append(listed, page...)
			return nil
		})
	if err != nil && !errors.Is(err, provider.ErrNotFound) {
		s.failure(Finding{Repository: pkgPath, Digest: subject, Error: "list the referrers: " + err.Error()})
		return false
	}
	ok := true
	for _, d := range listed {
		if d.Annotations[annotationPredicateType] != attest.VerdictPredicateType {
			continue
		}
		f := Finding{Repository: pkgPath, Digest: d.Digest, Subject: subject}
		raw, err := s.fetch(ctx, rr, cache, d)
		if err != nil {
			f.Error = "read the referrer: " + err.Error()
			s.failure(f)
			ok = false
			continue
		}
		rows, err := vi.InspectVerdicts(ctx, s.repo, ingest.Referrer{Path: pkgPath, Desc: d, Manifest: raw, Subject: subject})
		if err != nil {
			f.Error = "the verdicts of the referrer: " + err.Error()
			s.failure(f)
			ok = false
			continue
		}
		ok = s.writeVerdicts(ctx, d.Digest, rows, have) && ok
	}
	return ok
}

// writeVerdicts touches the rows of the referrer d when rows are what the index has of it, and
// replaces them otherwise.
func (s *scan) writeVerdicts(ctx context.Context, d digest.Digest, rows, have []index.Verdict) bool {
	var known []index.Verdict
	for _, v := range have {
		if v.Referrer == d && v.AttachmentID == 0 {
			known = append(known, v)
		}
	}
	if sameVerdicts(known, rows) && !s.req.Verify {
		ids := make([]int64, 0, len(known))
		for _, v := range known {
			ids = append(ids, v.ID)
		}
		return !s.dbErr(s.w.index.TouchOCIVerdicts(ctx, ids, s.w.now()))
	}
	at := s.w.now()
	for i := range rows {
		rows[i].IndexedAt = at
	}
	return !s.dbErr(s.w.index.ReplaceOCIVerdicts(ctx, s.repo.ID, d, rows))
}

// sameVerdicts reports whether the rows a and b say the same, layer by layer.
func sameVerdicts(a, b []index.Verdict) bool {
	if len(a) != len(b) || len(a) == 0 {
		return false
	}
	key := func(v index.Verdict) string {
		until := ""
		if v.ValidUntil != nil {
			until = v.ValidUntil.UTC().String()
		}
		return string(v.Layer) + "\x00" + v.Verdict + "\x00" + v.Reason + "\x00" + v.Source + "\x00" + v.Scope + "\x00" + until +
			"\x00" + v.SignedBy + "\x00" + string(v.Subject) + "\x00" + v.Path
	}
	ka, kb := make([]string, 0, len(a)), make([]string, 0, len(b))
	for i := range a {
		ka, kb = append(ka, key(a[i])), append(kb, key(b[i]))
	}
	slices.Sort(ka)
	slices.Sort(kb)
	return slices.Equal(ka, kb)
}
