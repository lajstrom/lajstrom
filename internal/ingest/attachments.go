package ingest

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/lajstrom/lajstrom/internal/attest"
	"github.com/lajstrom/lajstrom/internal/index"
	"github.com/lajstrom/lajstrom/internal/metrics"
	"github.com/lajstrom/lajstrom/internal/oci/profile"
	"github.com/lajstrom/lajstrom/internal/provider"
)

// defaultMaxAttachmentSize is the largest layer read to judge an attachment when
// attachments.maxBlobSize sets none: a bundle is kilobytes, an SBOM of a large image megabytes.
const defaultMaxAttachmentSize = 16 << 20

// AttachmentTrust is the hash of the trust that labels attachments (attest.Trust.Hash), the trust
// column of their rows: reindex labels a row of another hash again.
func (p *Pipeline) AttachmentTrust() string { return p.trust.Hash() }

// InspectAttachments returns the rows of the referrer r of the version v of repo, as PutReferrer
// writes them, and writes nothing (docs/spec/index-schema.md, Attachments): one per attachment of
// its manifest (profile.Attachments), each layer that attest reads fetched through the provider,
// at most the configured size (a larger one is unsupported, unread), and judged by the trust. The
// predicate type and the kind are those of the statement when the layer holds one. IndexedAt is
// the pipeline's clock. A layer the registry does not serve, or whose bytes are not its digest,
// is an error.
func (p *Pipeline) InspectAttachments(ctx context.Context, repo index.Repo, v index.Version, r Referrer) ([]index.Attachment, error) {
	var m ocispec.Manifest
	if err := json.Unmarshal(r.Manifest, &m); err != nil {
		return nil, invalid(fmt.Errorf("the referrer %s is not JSON: %w", r.Desc.Digest, err))
	}
	var rr provider.Repository
	trust, at := p.trust.Hash(), p.now()
	var rows []index.Attachment
	for _, a := range profile.Attachments(r.Tag, m) {
		row := attachmentRow(r, m, a, trust, at)
		in := attest.Input{Subject: v.Digest, Layer: a.Layer, Annotated: a.PredicateType}
		if attest.Reads(a.Layer.MediaType) && a.Layer.Digest != "" {
			if a.Layer.Size > p.maxAttachment {
				row.Verification = index.AttachmentUnsupported
				row.Reason = fmt.Sprintf("the layer is %d bytes, more than attachments.maxBlobSize (%d)", a.Layer.Size, p.maxAttachment)
				rows = append(rows, row)
				continue
			}
			if rr == nil {
				var err error
				if rr, err = p.repository(ctx, repo, r.Path); err != nil {
					return nil, err
				}
			}
			content, err := readLayer(ctx, rr, a.Layer, p.maxAttachment)
			if err != nil {
				return nil, fmt.Errorf("ingest: the layer %s of the referrer %s: %w", a.Layer.Digest, r.Desc.Digest, err)
			}
			in.Content = content
		}
		res := p.trust.Verify(in)
		row.Verification, row.SignedBy, row.Reason = res.Verification, res.SignedBy, res.Reason
		if res.PredicateType != "" {
			row.PredicateType, row.Kind = res.PredicateType, profile.StatementKind(res.PredicateType, res.Predicate)
		}
		fillTool(&row, res)
		rows = append(rows, row)
	}
	return rows, nil
}

// attachmentRow is the row of the attachment a of the referrer r, whose manifest is m, before it
// is judged.
func attachmentRow(r Referrer, m ocispec.Manifest, a profile.Attachment, trust string, at time.Time) index.Attachment {
	row := index.Attachment{Tag: r.Tag, Layer: a.Layer.Digest, MediaType: a.Layer.MediaType,
		ArtifactType: cmp.Or(m.ArtifactType, m.Config.MediaType), Kind: a.Kind, PredicateType: a.PredicateType, Size: a.Layer.Size,
		Annotations: m.Annotations, Tool: m.Annotations[profile.AnnotationAttachmentTool],
		ToolVersion: m.Annotations[profile.AnnotationAttachmentToolVersion], Database: m.Annotations[profile.AnnotationAttachmentDatabase],
		Trust: trust, IndexedAt: at}
	if in, err := digest.Parse(m.Annotations[profile.AnnotationAttachmentInput]); err == nil {
		row.Input = in
	}
	if t, err := time.Parse(time.RFC3339, m.Annotations[ocispec.AnnotationCreated]); err == nil {
		t = t.UTC()
		row.CreatedAt = &t
	}
	return row
}

// fillTool fills the tool, its version and the database of row that its annotations leave out
// from the statement's predicate (profile.PredicateTool; manifest-profile.md §6.2): an annotation
// that is set wins, each on its own.
func fillTool(row *index.Attachment, res attest.Result) {
	if row.Tool != "" && row.ToolVersion != "" && row.Database != "" {
		return
	}
	tool, version, database := profile.PredicateTool(res.PredicateType, res.Predicate)
	row.Tool, row.ToolVersion, row.Database = cmp.Or(row.Tool, tool), cmp.Or(row.ToolVersion, version), cmp.Or(row.Database, database)
}

// repository returns the data path of the OCI repository of path below repo.
func (p *Pipeline) repository(ctx context.Context, repo index.Repo, path string) (provider.Repository, error) {
	prov, err := p.registry(repo)
	if err != nil {
		return nil, err
	}
	name := ociName(repo, path)
	rr, err := prov.Repository(ctx, name)
	if err != nil {
		return nil, fmt.Errorf("ingest: repository %s: %w", name, err)
	}
	return rr, nil
}

// readLayer reads the blob desc of rr, at most limit bytes, checked against its digest.
func readLayer(ctx context.Context, rr provider.Repository, desc ocispec.Descriptor, limit int64) ([]byte, error) {
	if err := desc.Digest.Validate(); err != nil {
		return nil, err
	}
	rc, err := rr.Fetch(ctx, desc)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rc.Close() }()
	b, err := io.ReadAll(io.LimitReader(rc, limit+1))
	switch {
	case err != nil:
		return nil, err
	case int64(len(b)) > limit:
		return nil, fmt.Errorf("more than %d bytes", limit)
	case desc.Digest.Algorithm().FromBytes(b) != desc.Digest:
		return nil, fmt.Errorf("the bytes are not %s", desc.Digest)
	}
	return b, nil
}

// indexAttachments writes the rows of the referrer r of repo once the registry serves it back,
// when its subject is a version of the package of r.Path: the rows of its manifest replace those
// it had, and for a legacy tag those of the version under the tag (Store.ReplaceAttachments).
// Best effort: a failure is logged and counted, and the push stays a success; the next reindex
// of the repo writes the rows (docs/spec/index-schema.md, Attachments).
func (p *Pipeline) indexAttachments(ctx context.Context, repo index.Repo, r Referrer) {
	if repo.Format == ociFormat {
		return
	}
	rows, v, err := p.attachmentsOf(ctx, repo, r)
	if err == nil && v != nil {
		err = p.index.ReplaceAttachments(ctx, v.ID, r.Desc.Digest, rows)
	}
	switch {
	case v == nil && err == nil:
		return // the subject is no version: a referrer of a referrer, of a foreign manifest
	case err != nil:
		metrics.AttachmentIndexFailures.Inc()
		p.log.WarnContext(ctx, "ingest: the attachments of a referrer are not indexed; the next reindex of the repo indexes them",
			"repo", repo.Format+"/"+repo.Name, "digest", r.Desc.Digest.String(), "subject", r.Subject.String(), "error", err.Error())
		return
	}
	for _, a := range rows {
		metrics.AttachmentsIndexed.WithLabelValues(a.Kind, a.Verification).Inc()
	}
}

// attachmentsOf returns the version of the package of r.Path whose manifest is r's subject, and
// the rows of r's attachments; a nil version when there is none.
func (p *Pipeline) attachmentsOf(ctx context.Context, repo index.Repo, r Referrer) ([]index.Attachment, *index.Version, error) {
	pkg, err := p.index.GetPackageByNameOCI(ctx, repo.ID, r.Path)
	if errors.Is(err, index.ErrNotFound) {
		return nil, nil, nil
	} else if err != nil {
		return nil, nil, err
	}
	v, err := p.index.GetVersionByDigest(ctx, repo.ID, r.Subject)
	switch {
	case errors.Is(err, index.ErrNotFound), err == nil && v.PackageID != pkg.ID:
		return nil, nil, nil
	case err != nil:
		return nil, nil, err
	}
	rows, err := p.InspectAttachments(ctx, repo, *v, r)
	return rows, v, err
}
