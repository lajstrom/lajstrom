package ingest

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/lajstrom/lajstrom/internal/attest"
	"github.com/lajstrom/lajstrom/internal/auth"
	"github.com/lajstrom/lajstrom/internal/format"
	"github.com/lajstrom/lajstrom/internal/index"
	"github.com/lajstrom/lajstrom/internal/metrics"
	"github.com/lajstrom/lajstrom/internal/policy"
	"github.com/lajstrom/lajstrom/internal/webhook"
)

// The verdicts of docs/spec/policy.md §2: the attachments of a referrer that verify as verdicts
// have their verdicts rows, written with the attachments' and, on an oci repo, which has none,
// on their own; each write and delete through ingest has its audit row and its event.

// verdictOf is the verdict that the attachment of the referrer r judged by res is, nil when it is
// none: it verifies (a trusted key, the policy key among them, signed it for the predicate type
// of verdicts), and its predicate is a verdict.
func verdictOf(r Referrer, subject digest.Digest, res attest.Result) *index.Verdict {
	if res.Verification != attest.Verified || res.PredicateType != attest.VerdictPredicateType {
		return nil
	}
	v, err := attest.ParseVerdict(res.Predicate)
	if err != nil {
		return nil
	}
	return &index.Verdict{Path: r.Path, Subject: subject, Verdict: v.Verdict, Reason: v.Reason, Source: v.Source, Scope: v.Scope,
		ValidUntil: v.ValidUntil, SignedBy: res.SignedBy}
}

// indexOCIVerdicts writes the verdicts rows of the referrer r of an oci repo, once the registry
// serves it back, with their audit rows and events: best effort, as attachments are indexed.
func (p *Pipeline) indexOCIVerdicts(ctx context.Context, repo index.Repo, r Referrer, principal auth.Principal) {
	verdicts, err := p.InspectVerdicts(ctx, repo, r)
	if err == nil && len(verdicts) > 0 {
		err = p.index.Tx(ctx, func(w index.Writer) error {
			if err := w.ReplaceOCIVerdicts(ctx, repo.ID, r.Desc.Digest, verdicts); err != nil {
				return err
			}
			return p.verdictEvents(ctx, w, repo, policy.ActionVerdictCreate, nil, verdicts, principal)
		})
	}
	if err != nil {
		metrics.AttachmentIndexFailures.Inc()
		p.log.WarnContext(ctx, "ingest: the verdicts of a referrer are not indexed; the next reindex of the repo indexes them",
			"repo", repo.Format+"/"+repo.Name, "digest", r.Desc.Digest.String(), "subject", r.Subject.String(), "error", err.Error())
	}
}

// verdictsWritten writes, on w, the audit rows and events of the verdicts among rows, the
// attachments of a referrer of the version v that ReplaceAttachments just wrote.
func (p *Pipeline) verdictsWritten(ctx context.Context, w index.Writer, repo index.Repo, v *index.Version, rows []index.Attachment,
	principal auth.Principal) error {
	var verdicts []index.Verdict
	for _, row := range rows {
		if row.Verdict != nil {
			verdicts = append(verdicts, *row.Verdict)
		}
	}
	if len(verdicts) == 0 {
		return nil
	}
	return p.verdictEvents(ctx, w, repo, policy.ActionVerdictCreate, v, verdicts, principal)
}

// referrerVerdicts returns the verdicts rows of the referrer d of repo.
func referrerVerdicts(ctx context.Context, r index.Reader, repo index.Repo, d digest.Digest) ([]index.Verdict, error) {
	rows, err := r.ListVerdicts(ctx, index.VerdictFilter{RepoID: repo.ID})
	if err != nil {
		return nil, err
	}
	var out []index.Verdict
	for _, v := range rows {
		if v.Referrer == d {
			out = append(out, v)
		}
	}
	return out, nil
}

// deleteVerdicts removes, on w, the verdicts rows of the referrer d of repo that a delete took (on
// a managed repo with its attachments, which deleteAttachments removes), and writes their audit
// rows and events.
func (p *Pipeline) deleteVerdicts(ctx context.Context, w index.Writer, repo index.Repo, d digest.Digest, principal auth.Principal,
	deleteAttachments func() error) error {
	gone, err := referrerVerdicts(ctx, w, repo, d)
	if err != nil {
		return err
	}
	if deleteAttachments != nil {
		if err := deleteAttachments(); err != nil {
			return err
		}
	} else if _, err := w.DeleteOCIVerdicts(ctx, repo.ID, d); err != nil {
		return err
	}
	if len(gone) == 0 {
		return nil
	}
	var v *index.Version
	if gone[0].AttachmentID != 0 {
		found, err := w.GetVersionByDigest(ctx, repo.ID, gone[0].Subject)
		if err != nil && !errors.Is(err, index.ErrNotFound) {
			return err
		}
		v = found
	}
	return p.verdictEvents(ctx, w, repo, policy.ActionVerdictDelete, v, gone, principal)
}

// verdictEvents writes, on w, for each verdict of rows, of the version v (nil on an oci repo or
// for a subject that is no version), the audit row of action by principal and its event, and
// invalidates the documents of the repos its scope matches (policy.Invalidate).
func (p *Pipeline) verdictEvents(ctx context.Context, w index.Writer, repo index.Repo, action string, v *index.Version,
	rows []index.Verdict, principal auth.Principal) error {
	typ := webhook.TypePolicyVerdictCreated
	if action == policy.ActionVerdictDelete {
		typ = webhook.TypePolicyVerdictDeleted
	}
	var pkg *index.Package
	if v != nil {
		found, err := index.PackageOf(ctx, w, repo.ID, rows[0].Path)
		if err != nil && !errors.Is(err, index.ErrNotFound) {
			return err
		}
		pkg = found
	}
	for _, row := range rows {
		s := policy.Subject{Repo: repo, Name: row.Path, Digest: row.Subject}
		if pkg != nil {
			s.Name, s.Version, s.Variant = pkg.Name, v.Version, v.Variant
		}
		ev := index.AuditEvent{At: p.now(), Principal: principal.String(), Action: action, Repo: repo.Format + "/" + repo.Name,
			Subject: s.String(), Digest: row.Subject, State: index.AuditOK}
		withOrigin(ctx, &ev, map[string]any{"verdict": row.Verdict, "reason": row.Reason, "source": row.Source, "scope": row.Scope,
			"validUntil": validUntil(row.ValidUntil), "attestation": row.Referrer.String()})
		if err := w.AppendAudit(ctx, &ev); err != nil {
			return err
		}
		data := webhook.VerdictData{Common: webhook.CommonOf(ev), Repo: webhook.RefOf(repo), Package: s.Name, Version: s.Version,
			Variant: s.Variant, OCIRepository: ociName(repo, row.Path), Digest: row.Subject,
			Verdict: webhook.Verdict{Verdict: row.Verdict, Reason: row.Reason, Source: row.Source, Scope: row.Scope,
				ValidUntil: row.ValidUntil, Attestation: row.Referrer}}
		if err := p.webhooks.Enqueue(ctx, w, webhook.Event{Type: typ, Repo: repo, Subject: ev.Subject, Data: data}); err != nil {
			return err
		}
		scope := row.Scope
		if scope == "" {
			scope = repo.Format + "/" + repo.Name
		}
		if err := policy.Invalidate(ctx, w, scope); err != nil {
			return fmt.Errorf("ingest: the documents of the scope %s of a verdict: %w", scope, err)
		}
	}
	return nil
}

// validUntil is a verdict's validUntil as an audit row's details have it: RFC 3339, "" for none.
func validUntil(t *time.Time) string {
	if t == nil {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

// Attach stores a referrer the server made itself, a verdict signed with the policy key
// (docs/spec/policy.md §4, §7): its blobs, each unless the backend has it, then the manifest
// through PutReferrer as a push of principal, which indexes it and writes its audit rows.
func (p *Pipeline) Attach(ctx context.Context, repo index.Repo, r Referrer, blobs []format.BlobSource, principal auth.Principal) (digest.Digest, error) {
	prov, err := p.registry(repo)
	if err != nil {
		return "", err
	}
	name := ociName(repo, r.Path)
	rr, err := prov.Repository(ctx, name)
	if err != nil {
		return "", fmt.Errorf("ingest: repository %s: %w", name, err)
	}
	t := &target{repo: repo, name: name, rr: rr, prov: prov}
	for _, b := range blobs {
		if err := t.pushBlob(ctx, b); err != nil {
			return "", err
		}
	}
	return p.PutReferrer(ctx, repo, r, principal)
}

// InspectVerdicts returns the verdicts rows of the referrer r of an oci repo, as PutReferrer
// writes them, and writes nothing (docs/spec/policy.md §10): reindex's read of a verdict.
func (p *Pipeline) InspectVerdicts(ctx context.Context, repo index.Repo, r Referrer) ([]index.Verdict, error) {
	rows, err := p.inspect(ctx, repo, r.Subject, r)
	if err != nil {
		return nil, err
	}
	var out []index.Verdict
	for _, row := range rows {
		if row.Verdict != nil {
			v := *row.Verdict
			v.Layer, v.CreatedAt = row.Layer, row.CreatedAt
			out = append(out, v)
		}
	}
	return out, nil
}

// VerdictSigner signs the verdicts the server makes: the policy key (keys.PolicyKey).
type VerdictSigner interface {
	Sign(ctx context.Context, msg []byte) ([]byte, error)
	PublicDER() []byte
}

// AttachVerdict makes the verdict v of the manifest subject at the package path path of repo,
// signed by key, and attaches it (Attach) as principal (docs/spec/policy.md §4, §7). It returns
// the referrer's digest.
func (p *Pipeline) AttachVerdict(ctx context.Context, repo index.Repo, path string, subject digest.Digest, v attest.Verdict,
	key VerdictSigner, principal auth.Principal) (digest.Digest, error) {
	rr, err := p.repository(ctx, repo, path)
	if err != nil {
		return "", err
	}
	desc, err := rr.Resolve(ctx, subject.String())
	if err != nil {
		return "", fmt.Errorf("ingest: the manifest %s of %s: %w", subject, ociName(repo, path), err)
	}
	ref, err := attest.BuildVerdict(ctx, ociName(repo, path), desc, v, key.PublicDER(), key.Sign, p.now())
	if err != nil {
		return "", err
	}
	r := Referrer{Path: path, Manifest: ref.Manifest, Desc: ref.Desc, Subject: subject}
	return p.Attach(ctx, repo, r, []format.BlobSource{bytesSource(ocispec.DescriptorEmptyJSON, ocispec.DescriptorEmptyJSON.Data),
		bytesSource(ref.BundleDesc, ref.Bundle)}, principal)
}

// LiftQuarantine attaches the allow verdict of a version fetched into a proxy repo with a
// quarantine after its quarantine is over (docs/spec/policy.md §4), once: nothing when its
// manifest has a verdict of source lajstrom:quarantine. Best effort: a failure is logged.
func (p *Pipeline) LiftQuarantine(ctx context.Context, repo index.Repo, m index.FileMatch, key VerdictSigner, principal auth.Principal) {
	have, err := p.index.ListVerdicts(ctx, index.VerdictFilter{RepoID: repo.ID, Subject: m.Version.Digest})
	if err == nil {
		for _, v := range have {
			if v.Source == policy.SourceQuarantine {
				return
			}
		}
		_, err = p.AttachVerdict(ctx, repo, index.ManifestPath(m.Package, m.Version), m.Version.Digest, attest.Verdict{
			Verdict: index.PolicyAllow, Reason: "quarantine of " + shortDuration(policy.Quarantine(repo)) + " over", Source: policy.SourceQuarantine,
			Scope: repo.Format + "/" + repo.Name}, key, principal)
	}
	if err != nil {
		p.log.WarnContext(ctx, "ingest: the verdict of a quarantine over is not attached", "repo", repo.Format+"/"+repo.Name,
			"package", m.Package.Name, "version", m.Version.Version, "error", err.Error())
	}
}

// shortDuration is d as Go writes it without its zero minutes and seconds: 72h, 90m, 1h30m.
func shortDuration(d time.Duration) string {
	s := d.String()
	if strings.HasSuffix(s, "m0s") {
		s = strings.TrimSuffix(s, "0s")
	}
	if strings.HasSuffix(s, "h0m") {
		s = strings.TrimSuffix(s, "0m")
	}
	return s
}

// bytesSource is a BlobSource of b.
func bytesSource(d ocispec.Descriptor, b []byte) format.BlobSource {
	return format.BlobSource{Descriptor: d, Open: func(context.Context) (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(b)), nil }}
}
