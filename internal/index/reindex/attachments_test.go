package reindex_test

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/lajstrom/lajstrom/internal/attest"
	"github.com/lajstrom/lajstrom/internal/attest/attesttest"
	"github.com/lajstrom/lajstrom/internal/format"
	"github.com/lajstrom/lajstrom/internal/format/raw"
	"github.com/lajstrom/lajstrom/internal/index"
	"github.com/lajstrom/lajstrom/internal/index/reindex"
	"github.com/lajstrom/lajstrom/internal/ingest"
)

const cyclonedx = "https://cyclonedx.org/bom"

// countingAttacher counts the referrers the worker reads.
type countingAttacher struct {
	reindex.Attacher
	reads atomic.Int64
}

func (c *countingAttacher) InspectAttachments(ctx context.Context, repo index.Repo, v index.Version, r ingest.Referrer) ([]index.Attachment, error) {
	c.reads.Add(1)
	return c.Attacher.InspectAttachments(ctx, repo, v, r)
}

// attaching makes e's pipeline trust the entries and its worker read attachments through it.
func (e *env) attaching(t *testing.T, entries ...attest.Entry) *countingAttacher {
	t.Helper()
	return e.attachingWith(t, entries)
}

// attachingWith is attaching with the options of the trust too (attest.WithKeyless).
func (e *env) attachingWith(t *testing.T, entries []attest.Entry, opts ...attest.Option) *countingAttacher {
	t.Helper()
	trust, err := attest.NewTrust(entries, opts...)
	must(t, err)
	log := slog.New(slog.DiscardHandler)
	plugins := func(name string) (format.Plugin, bool) { return raw.Plugin{}, name == raw.Name }
	e.pipe = ingest.New(ingest.Options{Providers: e.providers, Index: e.st, Plugins: plugins, Log: log, Trust: trust})
	e.inspect = &counting{Inspector: e.pipe}
	attacher := &countingAttacher{Attacher: e.pipe}
	e.w = reindex.New(reindex.Options{Index: e.st, Providers: e.providers, Inspector: e.inspect, Repairer: e.pipe, Log: log,
		Now: e.clock.now, Attacher: attacher})
	return attacher
}

// attach pushes a referrer of pub with one layer of content and the predicate type annotated,
// through ingest when through, else straight to the registry; with tag, cosign's legacy manifest
// under it. It returns the referrer's digest.
func (e *env) attach(t *testing.T, pub *format.Published, tag, mediaType string, content []byte, predicateType string, through bool) digest.Digest {
	t.Helper()
	ctx := t.Context()
	rr, err := e.fake.Repository(ctx, pub.RepoPath)
	must(t, err)
	must(t, rr.Push(ctx, ocispec.DescriptorEmptyJSON, bytes.NewReader(ocispec.DescriptorEmptyJSON.Data)))
	layer := ocispec.Descriptor{MediaType: mediaType, Digest: digest.FromBytes(content), Size: int64(len(content))}
	if ok, _ := rr.Exists(ctx, layer); !ok {
		must(t, rr.Push(ctx, layer, bytes.NewReader(content)))
	}
	m := ocispec.Manifest{Versioned: specs2, MediaType: ocispec.MediaTypeImageManifest, Config: ocispec.DescriptorEmptyJSON,
		Layers: []ocispec.Descriptor{layer}}
	if tag == "" {
		subject, err := rr.Resolve(ctx, pub.Digest.String())
		must(t, err)
		m.ArtifactType, m.Subject = mediaType, &subject
		m.Annotations = map[string]string{"dev.sigstore.bundle.predicateType": predicateType}
	} else {
		m.Layers[0].Annotations = map[string]string{"predicateType": predicateType}
	}
	body, err := json.Marshal(m)
	must(t, err)
	desc := ocispec.Descriptor{MediaType: ocispec.MediaTypeImageManifest, Digest: digest.FromBytes(body), Size: int64(len(body))}
	if through {
		_, err = e.pipe.PutReferrer(ctx, e.repo, ingest.Referrer{Path: pub.RepoPath[len("raw/scratch/"):], Tag: tag, Desc: desc,
			Manifest: body, Subject: pub.Digest}, alice)
		must(t, err)
	} else if tag != "" {
		must(t, rr.PushReference(ctx, desc, bytes.NewReader(body), tag))
	} else {
		must(t, rr.Push(ctx, desc, bytes.NewReader(body)))
	}
	return desc.Digest
}

// attachmentRows are the rows of every version of the repo by version tag, without their ids,
// versions and times of indexing: what reindex must restore.
func (e *env) attachmentRows(t *testing.T) map[string][]index.Attachment {
	t.Helper()
	ctx := t.Context()
	out := map[string][]index.Attachment{}
	pkgs, err := e.st.ListPackages(ctx, e.repo.ID, "")
	must(t, err)
	for _, p := range pkgs {
		vs, err := e.st.ListVersions(ctx, p.ID)
		must(t, err)
		for _, v := range vs {
			rows, err := e.st.ListAttachments(ctx, []int64{v.ID})
			must(t, err)
			for i := range rows {
				rows[i].ID, rows[i].VersionID, rows[i].IndexedAt = 0, 0, time.Time{}
			}
			out[p.Name+":"+v.Tag] = rows
		}
	}
	return out
}

// TestAttachmentsRebuild: reindex rebuilds the attachments ingest wrote, of the referrers API and
// of cosign's legacy tags, after the rows were lost and after the whole index was; it adds a
// referrer pushed past Lajstrom, sweeps one deleted past it, reads nothing it has under the
// same trust, and labels every row again when the trust changes.
func TestAttachmentsRebuild(t *testing.T) {
	ctx := t.Context()
	e := newEnv(t)
	scanner, stranger := attesttest.NewKey(t), attesttest.NewKey(t)
	attacher := e.attaching(t, scanner.Entry(t, "scanner"))
	pub := e.publish(t, "hello", "hello")
	other := e.publish(t, "other", "other")
	statement := attesttest.Statement(pub.Digest, cyclonedx, `{"bomFormat":"CycloneDX"}`)
	e.attach(t, pub, "", attesttest.BundleMediaType, attesttest.Bundle(t, statement, scanner), cyclonedx, true)
	strange := e.attach(t, pub, "", attesttest.BundleMediaType, attesttest.Bundle(t, statement, stranger), cyclonedx, true)
	legacy := "sha256-" + pub.Digest.Encoded() + ".att"
	e.attach(t, pub, legacy, attesttest.DSSEMediaType, attesttest.Envelope(t, statement, scanner), cyclonedx, true)
	e.attach(t, other, "", "application/spdx+json", []byte(`{"spdxVersion":"SPDX-2.3"}`), "", true)
	written := e.attachmentRows(t)
	if n := len(written["hello:"+pub.Tag]); n != 3 {
		t.Fatalf("ingest wrote %d rows of hello, want 3", n)
	}

	// Under the same trust, a run reads no referrer again and keeps every row.
	r := e.run(t, reindex.Request{})
	if r.Attachments != 4 || attacher.reads.Load() != 0 || len(r.Errors) != 0 {
		t.Errorf("a run over known rows: %d attachments, %d reads, errors %v", r.Attachments, attacher.reads.Load(), r.Errors)
	}
	same(t, "the rows after a run", written, e.attachmentRows(t))

	// The rows lost: the run reads the referrers again and writes the same rows.
	for _, p := range []*format.Published{pub, other} {
		v, err := e.st.GetVersionByDigest(ctx, e.repo.ID, p.Digest)
		must(t, err)
		_, err = e.st.SweepAttachments(ctx, v.ID, time.Now().Add(time.Hour))
		must(t, err)
	}
	r = e.run(t, reindex.Request{})
	if r.Attachments != 4 || attacher.reads.Load() != 4 {
		t.Errorf("a run after the rows were lost: %d attachments, %d reads", r.Attachments, attacher.reads.Load())
	}
	same(t, "the rows rebuilt", written, e.attachmentRows(t))

	// The whole index lost.
	must(t, e.st.ResetDerived(ctx, e.repo.ID))
	e.run(t, reindex.Request{Prune: true})
	same(t, "the rows rebuilt with the index", written, e.attachmentRows(t))

	e.pastLajstrom(t, attacher, pub, scanner, strange)
	e.newTrust(t, pub, scanner)
}

// pastLajstrom pushes a referrer of pub straight to the registry and deletes strange there: the
// next run adds the one, reading it alone, and sweeps the other.
func (e *env) pastLajstrom(t *testing.T, attacher *countingAttacher, pub *format.Published, scanner *attesttest.Key, strange digest.Digest) {
	t.Helper()
	attacher.reads.Store(0)
	const slsa = "https://slsa.dev/provenance/v1"
	added := e.attach(t, pub, "", attesttest.BundleMediaType, attesttest.Bundle(t, attesttest.Statement(pub.Digest, slsa, `{}`), scanner), slsa, false)
	e.deleteManifest(t, pub.RepoPath, strange)
	e.clock.later(time.Second)
	e.run(t, reindex.Request{})
	rows := e.attachmentRows(t)["hello:"+pub.Tag]
	digests := map[digest.Digest]index.Attachment{}
	for _, a := range rows {
		digests[a.Digest] = a
	}
	if _, ok := digests[strange]; ok || len(rows) != 3 || digests[added].Kind != index.AttachmentProvenance || attacher.reads.Load() != 1 {
		t.Errorf("after a push and a delete past Lajstrom: %d rows, %d reads: %+v", len(rows), attacher.reads.Load(), rows)
	}
}

// newTrust runs under a trust that trusts scanner for provenance only: every row of pub is read
// and labelled again; then a verifying run reads them all again.
func (e *env) newTrust(t *testing.T, pub *format.Published, scanner *attesttest.Key) {
	t.Helper()
	attacher := e.attaching(t, scanner.Entry(t, "scanner", "https://slsa.dev/provenance/v1"))
	e.run(t, reindex.Request{})
	if attacher.reads.Load() != 4 {
		t.Errorf("a run under another trust read %d referrers, want 4", attacher.reads.Load())
	}
	for _, a := range e.attachmentRows(t)["hello:"+pub.Tag] {
		want := index.AttachmentUntrusted
		if a.Kind == index.AttachmentProvenance {
			want = index.AttachmentVerified
		}
		if a.Verification != want || a.Trust != e.pipe.AttachmentTrust() {
			t.Errorf("%s under the new trust: %s, trust %s", a.Kind, a.Verification, a.Trust)
		}
	}
	attacher.reads.Store(0)
	e.run(t, reindex.Request{Verify: true})
	if attacher.reads.Load() != 4 {
		t.Errorf("a verifying run read %d referrers, want 4", attacher.reads.Load())
	}
}

// TestAttachmentsKeylessRelabel: a keyless bundle pushed through ingest is untrusted while no
// identity matches its certificate, and a reindex labels it verified once the trust has the
// identity, reading it again because the trust's hash moved.
func TestAttachmentsKeylessRelabel(t *testing.T) {
	e := newEnv(t)
	sg := attesttest.NewSigstore(t)
	tr := sg.TrustedRoot(t)
	const issuer = "https://token.actions.githubusercontent.com"
	keyless := func(subject string) attest.Option {
		return attest.WithKeyless(attest.Keyless{TrustedRoot: tr, Identities: []attest.Identity{{Name: "ci", Issuer: issuer, Subject: subject}}})
	}
	e.attachingWith(t, nil, keyless("someone@example.com"))
	pub := e.publish(t, "hello", "hello")
	e.attach(t, pub, "", attesttest.BundleMediaType,
		sg.Bundle(t, issuer, "ci@example.com", attesttest.Statement(pub.Digest, cyclonedx, `{}`)), cyclonedx, true)
	rows := e.attachmentRows(t)["hello:"+pub.Tag]
	if len(rows) != 1 || rows[0].Verification != index.AttachmentUntrusted || !strings.Contains(rows[0].Reason, `subject "ci@example.com"`) {
		t.Fatalf("pushed under another identity: %+v", rows)
	}
	attacher := e.attachingWith(t, nil, keyless("ci@example.com"))
	e.run(t, reindex.Request{})
	rows = e.attachmentRows(t)["hello:"+pub.Tag]
	if attacher.reads.Load() != 1 || len(rows) != 1 || rows[0].Verification != index.AttachmentVerified || rows[0].SignedBy != "ci" {
		t.Errorf("after the identity was trusted: %d reads, %+v", attacher.reads.Load(), rows)
	}
}
