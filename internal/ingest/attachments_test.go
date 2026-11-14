package ingest_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/opencontainers/go-digest"
	"github.com/opencontainers/image-spec/specs-go"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/lajstrom/lajstrom/internal/attest"
	"github.com/lajstrom/lajstrom/internal/attest/attesttest"
	"github.com/lajstrom/lajstrom/internal/format"
	"github.com/lajstrom/lajstrom/internal/index"
	"github.com/lajstrom/lajstrom/internal/ingest"
	"github.com/lajstrom/lajstrom/internal/metrics"
)

const (
	cyclonedx = "https://cyclonedx.org/bom"
	vulnType  = "https://cosign.sigstore.dev/attestation/vuln/v1"
)

// attachEnv is a repo with the version notes 1.0 and a pipeline that trusts scanner for
// CycloneDX and vulnerability reports; stranger is a key it does not trust.
type attachEnv struct {
	*env
	subject           ocispec.Descriptor
	scanner, stranger *attesttest.Key
}

func newAttachEnv(t *testing.T, maxSize int64) *attachEnv {
	t.Helper()
	e := &attachEnv{env: newEnv(t), scanner: attesttest.NewKey(t), stranger: attesttest.NewKey(t)}
	trust, err := attest.NewTrust([]attest.Entry{e.scanner.Entry(t, "scanner", cyclonedx, vulnType)})
	must(t, err)
	opts := e.options(e.prov)
	opts.Trust, opts.MaxAttachmentSize = trust, maxSize
	e.pipe = ingest.New(opts)
	built, raw, pkgPath := e.prepare(t, upload("notes", "1.0", file{"notes", "the subject"}))
	res, err := e.ingest(t, built, raw, pkgPath)
	must(t, err)
	e.subject = ocispec.Descriptor{MediaType: ocispec.MediaTypeImageManifest, Digest: res.Digest, Size: int64(len(raw))}
	e.push(t, ocispec.DescriptorEmptyJSON.Data)
	return e
}

// push stores b as a blob of the version's OCI repository and returns its descriptor.
func (e *attachEnv) push(t *testing.T, b []byte) ocispec.Descriptor {
	t.Helper()
	rr, err := e.prov.Repository(t.Context(), "raw/scratch/notes")
	must(t, err)
	d := ocispec.Descriptor{MediaType: "application/octet-stream", Digest: digest.FromBytes(b), Size: int64(len(b))}
	if ok, err := rr.Exists(t.Context(), d); err != nil || !ok {
		must(t, rr.Push(t.Context(), d, bytes.NewReader(b)))
	}
	return d
}

// referrer is the referrer of subject with these layers (their bytes pushed first), artifact type
// and manifest annotations, pushed by digest; with tag, a legacy manifest under it without a
// subject.
func (e *attachEnv) referrer(t *testing.T, subject digest.Digest, tag, artifactType string, annotations map[string]string,
	layers ...layerOf,
) ingest.Referrer {
	t.Helper()
	m := ocispec.Manifest{Versioned: specs.Versioned{SchemaVersion: 2}, MediaType: ocispec.MediaTypeImageManifest,
		ArtifactType: artifactType, Config: ocispec.DescriptorEmptyJSON, Annotations: annotations}
	if tag == "" {
		m.Subject = &ocispec.Descriptor{MediaType: ocispec.MediaTypeImageManifest, Digest: subject, Size: e.subject.Size}
	} else {
		m.Config = ocispec.Descriptor{MediaType: ocispec.MediaTypeImageConfig, Digest: ocispec.DescriptorEmptyJSON.Digest, Size: 2}
	}
	for _, l := range layers {
		d := e.push(t, l.content)
		d.MediaType, d.Annotations = l.mediaType, l.annotations
		m.Layers = append(m.Layers, d)
	}
	b, err := json.Marshal(m)
	must(t, err)
	return ingest.Referrer{Path: "notes", Tag: tag, Manifest: b, Subject: subject,
		Desc: ocispec.Descriptor{MediaType: m.MediaType, Digest: digest.FromBytes(b), Size: int64(len(b))}}
}

type layerOf struct {
	mediaType   string
	content     []byte
	annotations map[string]string
}

// attachments returns the version's rows without their ids, versions and times of indexing.
func (e *attachEnv) attachments(t *testing.T) []index.Attachment {
	t.Helper()
	v, err := e.st.GetVersionByDigest(t.Context(), e.repo.ID, e.subject.Digest)
	must(t, err)
	rows, err := e.st.ListAttachments(t.Context(), []int64{v.ID})
	must(t, err)
	for i := range rows {
		rows[i].ID, rows[i].VersionID = 0, 0
	}
	return rows
}

// TestPutReferrerAttachments: a referrer of a version is indexed once the registry serves it,
// one row per attachment, labelled by the trust; a legacy tag moved takes the rows of the
// manifest it named; a referrer of a referrer has none; a delete takes a referrer's rows, and a
// version's delete all of them.
func TestPutReferrerAttachments(t *testing.T) {
	ctx := t.Context()
	e := newAttachEnv(t, 1<<20)
	trust := e.pipe.AttachmentTrust()
	sbomBundle := attesttest.Bundle(t, attesttest.Statement(e.subject.Digest, cyclonedx, `{"bomFormat":"CycloneDX"}`), e.scanner)
	created := "2026-11-14T12:20:33Z"
	signed := e.referrer(t, e.subject.Digest, "", attesttest.BundleMediaType, map[string]string{
		"dev.sigstore.bundle.predicateType": cyclonedx, "org.opencontainers.image.created": created,
		"lajstrom.attachment.tool": "trivy", "lajstrom.attachment.toolVersion": "0.60.0",
		"lajstrom.attachment.input": "not a digest",
	}, layerOf{attesttest.BundleMediaType, sbomBundle, nil})
	scanBundle := attesttest.Bundle(t, attesttest.Statement(e.subject.Digest, cyclonedx, `{"vulnerabilities":[{"id":"CVE-2026-1"}]}`), e.stranger)
	untrusted := e.referrer(t, e.subject.Digest, "", attesttest.BundleMediaType, map[string]string{
		"dev.sigstore.bundle.predicateType": cyclonedx, "lajstrom.attachment.database": "2026-11-14",
		"lajstrom.attachment.input": signed.Desc.Digest.String(),
	}, layerOf{attesttest.BundleMediaType, scanBundle, nil})
	spdx := e.referrer(t, e.subject.Digest, "", "application/spdx+json", nil, layerOf{"application/spdx+json", []byte(`{"spdxVersion":"SPDX-2.3"}`), nil})
	for _, r := range []ingest.Referrer{signed, untrusted, spdx} {
		_, err := e.pipe.PutReferrer(ctx, e.repo, r, alice)
		must(t, err)
	}
	at := now
	when, _ := time.Parse(time.RFC3339, created)
	want := []index.Attachment{
		{Digest: signed.Desc.Digest, Layer: digest.FromBytes(sbomBundle), MediaType: attesttest.BundleMediaType,
			ArtifactType: attesttest.BundleMediaType, Kind: index.AttachmentSBOM, PredicateType: cyclonedx, Size: int64(len(sbomBundle)),
			Annotations: map[string]string{"dev.sigstore.bundle.predicateType": cyclonedx, "org.opencontainers.image.created": created,
				"lajstrom.attachment.tool": "trivy", "lajstrom.attachment.toolVersion": "0.60.0", "lajstrom.attachment.input": "not a digest"},
			Tool: "trivy", ToolVersion: "0.60.0", Verification: index.AttachmentVerified, SignedBy: "scanner", Trust: trust,
			CreatedAt: &when, IndexedAt: at},
	}
	unordered := []index.Attachment{
		{Digest: untrusted.Desc.Digest, Layer: digest.FromBytes(scanBundle), MediaType: attesttest.BundleMediaType,
			ArtifactType: attesttest.BundleMediaType, Kind: index.AttachmentScan, PredicateType: cyclonedx, Size: int64(len(scanBundle)),
			Annotations: map[string]string{"dev.sigstore.bundle.predicateType": cyclonedx, "lajstrom.attachment.database": "2026-11-14",
				"lajstrom.attachment.input": signed.Desc.Digest.String()},
			Database: "2026-11-14", Input: signed.Desc.Digest, Verification: index.AttachmentUntrusted,
			Reason: "no trusted key verifies its signature", Trust: trust, IndexedAt: at},
		{Digest: spdx.Desc.Digest, Layer: digest.FromString(`{"spdxVersion":"SPDX-2.3"}`), MediaType: "application/spdx+json",
			ArtifactType: "application/spdx+json", Kind: index.AttachmentSBOM, Size: int64(len(`{"spdxVersion":"SPDX-2.3"}`)), Verification: index.AttachmentUnsigned,
			Reason: "it carries no signature", Trust: trust, IndexedAt: at},
	}
	if unordered[0].Digest > unordered[1].Digest {
		unordered[0], unordered[1] = unordered[1], unordered[0]
	}
	want = append(want, unordered...)
	same(t, "the rows of three referrers", want, e.attachments(t))

	// A layer above the bound is not read.
	small := newAttachEnv(t, 16)
	big := small.referrer(t, small.subject.Digest, "", attesttest.BundleMediaType, nil, layerOf{attesttest.BundleMediaType, sbomBundle, nil})
	_, err := small.pipe.PutReferrer(ctx, small.repo, big, alice)
	must(t, err)
	if got := small.attachments(t); len(got) != 1 || got[0].Verification != index.AttachmentUnsupported ||
		!strings.Contains(got[0].Reason, "attachments.maxBlobSize") {
		t.Errorf("a layer above the bound: %+v", got)
	}

	// A referrer of a referrer is no attachment of the version.
	counter := e.referrer(t, signed.Desc.Digest, "", "application/vnd.cncf.notary.signature", nil, layerOf{"application/jose+json", []byte("{}"), nil})
	_, err = e.pipe.PutReferrer(ctx, e.repo, counter, alice)
	must(t, err)
	if got := e.attachments(t); len(got) != 3 {
		t.Errorf("after a countersignature: %d rows, want 3", len(got))
	}

	// The delete of a referrer takes its rows.
	_, err = e.pipe.Delete(ctx, e.repo, format.Deletion{Path: "notes", Reference: untrusted.Desc.Digest.String()}, alice)
	must(t, err)
	if got := e.attachments(t); len(got) != 2 || got[0].Digest != signed.Desc.Digest {
		t.Errorf("after the delete of a referrer: %d rows", len(got))
	}
	// The delete of the version takes the rest.
	v, err := e.st.GetVersionByDigest(ctx, e.repo.ID, e.subject.Digest)
	must(t, err)
	_, err = e.pipe.Delete(ctx, e.repo, format.Deletion{Path: "notes", Reference: e.subject.Digest.String()}, alice)
	must(t, err)
	rows, err := e.st.ListAttachments(ctx, []int64{v.ID})
	must(t, err)
	if len(rows) != 0 {
		t.Errorf("after the delete of the version: %d rows", len(rows))
	}
}

// TestPutReferrerLegacyAttachments: cosign's legacy .att holds one attestation per layer, and
// cosign moves its tag to a manifest with one layer more: the rows follow the tag. A legacy .sig
// verifies as a signature.
func TestPutReferrerLegacyAttachments(t *testing.T) {
	ctx := t.Context()
	e := newAttachEnv(t, 0)
	att := "sha256-" + e.subject.Digest.Encoded() + ".att"
	layer := func(predicateType, predicate string) layerOf {
		return layerOf{attesttest.DSSEMediaType, attesttest.Envelope(t, attesttest.Statement(e.subject.Digest, predicateType, predicate), e.scanner),
			map[string]string{"predicateType": predicateType}}
	}
	first := e.referrer(t, e.subject.Digest, att, "", nil, layer(cyclonedx, `{}`))
	_, err := e.pipe.PutReferrer(ctx, e.repo, first, alice)
	must(t, err)
	second := e.referrer(t, e.subject.Digest, att, "", nil, layer(cyclonedx, `{}`), layer(vulnType, `{"scanner":{}}`))
	_, err = e.pipe.PutReferrer(ctx, e.repo, second, alice)
	must(t, err)
	got := e.attachments(t)
	if len(got) != 2 {
		t.Fatalf("after the tag moved: %d rows, want the 2 of the second manifest", len(got))
	}
	kinds := map[string]bool{}
	for _, a := range got {
		if a.Digest != second.Desc.Digest || a.Tag != att || a.Verification != index.AttachmentVerified {
			t.Errorf("row %+v", a)
		}
		kinds[a.Kind] = true
	}
	if !kinds[index.AttachmentSBOM] || !kinds[index.AttachmentScan] {
		t.Errorf("the kinds of the legacy layers: %v", kinds)
	}
}

// failingAttachments is the store whose ReplaceAttachments fails.
type failingAttachments struct{ index.Store }

func (failingAttachments) ReplaceAttachments(context.Context, int64, digest.Digest, []index.Attachment) error {
	return errors.New("the database refused")
}

// TestPutReferrerAttachmentsBestEffort: when the rows cannot be written, the push is a success
// all the same, and the failure is counted.
func TestPutReferrerAttachmentsBestEffort(t *testing.T) {
	e := newAttachEnv(t, 0)
	opts := e.options(e.prov)
	opts.Index = failingAttachments{e.st}
	e.pipe = ingest.New(opts)
	before := testutil.ToFloat64(metrics.AttachmentIndexFailures)
	r := e.referrer(t, e.subject.Digest, "", "application/spdx+json", nil, layerOf{"application/spdx+json", []byte("{}"), nil})
	if _, err := e.pipe.PutReferrer(t.Context(), e.repo, r, alice); err != nil {
		t.Fatalf("PutReferrer when the rows cannot be written: %v", err)
	}
	if got := testutil.ToFloat64(metrics.AttachmentIndexFailures) - before; got != 1 {
		t.Errorf("lajstrom_attachments_index_failures_total grew by %v, want 1", got)
	}
	if got := e.attachments(t); len(got) != 0 {
		t.Errorf("rows: %+v", got)
	}
}

// TestPutReferrerToolFromPredicate: a scan attested with cosign, which sets no annotation, gets
// its tool, version and database from the signed predicate (manifest-profile.md §6.2); an
// annotation that is set wins over the predicate, field by field.
func TestPutReferrerToolFromPredicate(t *testing.T) {
	e := newAttachEnv(t, 0)
	predicate := `{"scanner":{"uri":"pkg:github/aquasecurity/trivy@0.60.0","version":"0.60.0","db":{"version":"2026-11-14"}}}`
	scan := attesttest.Bundle(t, attesttest.Statement(e.subject.Digest, vulnType, predicate), e.scanner)
	for _, tt := range []struct {
		name                    string
		annotations             map[string]string
		tool, version, database string
	}{
		{"no annotations", map[string]string{"dev.sigstore.bundle.predicateType": vulnType}, "trivy", "0.60.0", "2026-11-14"},
		{"the database annotated", map[string]string{"dev.sigstore.bundle.predicateType": vulnType, "lajstrom.attachment.database": "db-7",
			"lajstrom.attachment.tool": "trivy-fork"}, "trivy-fork", "0.60.0", "db-7"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			r := e.referrer(t, e.subject.Digest, "", attesttest.BundleMediaType, tt.annotations, layerOf{attesttest.BundleMediaType, scan, nil})
			_, err := e.pipe.PutReferrer(t.Context(), e.repo, r, alice)
			must(t, err)
			for _, a := range e.attachments(t) {
				if a.Digest != r.Desc.Digest {
					continue
				}
				if a.Tool != tt.tool || a.ToolVersion != tt.version || a.Database != tt.database || a.Verification != index.AttachmentVerified {
					t.Errorf("row: %q %q %q %s, want %q %q %q verified", a.Tool, a.ToolVersion, a.Database, a.Verification, tt.tool, tt.version, tt.database)
				}
				return
			}
			t.Errorf("no row of %s", r.Desc.Digest)
		})
	}
}
