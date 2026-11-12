package reindex_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"testing"

	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/lajstrom/lajstrom/internal/format"
	"github.com/lajstrom/lajstrom/internal/index"
	"github.com/lajstrom/lajstrom/internal/index/reindex"
	"github.com/lajstrom/lajstrom/internal/ingest"
	"github.com/lajstrom/lajstrom/internal/provider"
)

// TestRepairReferrers: on a registry without the referrers API, a run of the reconcile loop or
// with verify has ingest push the sha256-<hex> index of a subject again when it is missing or
// lacks referrers that the audit log has as pushed and the registry still has (#628): four
// signatures pushed through ingest, the fourth then deleted in the registry. The index is pushed
// with the first three, in the order of their pushes after those it listed, the report lists the
// repair, and its audit row is referrers.repair, ok. The next run pushes nothing. A complete
// index, a run without verify and a registry with the API push nothing at all.
func TestRepairReferrers(t *testing.T) {
	reconcile := reindex.Request{Prune: true, Source: reindex.SourceReconcile, Principal: "anonymous"}
	verify := reindex.Request{Verify: true, Source: "cli", Principal: "user:admin"}
	for _, tt := range []struct {
		name   string
		api    bool
		req    reindex.Request
		listed int // how many of the signatures the index lists before the run; -1: there is none
		added  int // how many of the signatures the run adds after those
	}{
		{"missing, reconcile", false, reconcile, -1, 3},
		{"incomplete, verify", false, verify, 1, 2},
		{"complete, verify", false, verify, 3, 0},
		{"missing, a run without verify", false, reindex.Request{Source: "api", Principal: "user:admin"}, -1, 0},
		{"missing, with the referrers API", true, reconcile, -1, 0},
	} {
		t.Run(tt.name, func(t *testing.T) {
			e := newEnv(t)
			e.prov.noReferrersAPI = !tt.api
			pub := e.publish(t, "notes", "the subject")
			refs := e.signatures(t, pub, 4)
			e.deleteManifest(t, pub.RepoPath, refs[3])
			tag := "sha256-" + pub.Digest.Encoded()
			if tt.listed >= 0 {
				e.pushIndex(t, pub.RepoPath, tag, refs[:tt.listed])
			}
			before, _ := e.listedBy(t, pub.RepoPath, tag)

			req := tt.req
			req.Repo = &e.repo
			r := e.runAs(t, req)
			if len(r.Errors) != 0 {
				t.Fatalf("errors: %+v", r.Errors)
			}
			listed, idx := e.listedBy(t, pub.RepoPath, tag)
			rows, err := e.st.ListAudit(t.Context(), index.AuditFilter{Action: "referrers.repair"})
			must(t, err)
			if tt.added == 0 {
				same(t, "the referrers the index lists", before, listed)
				same(t, "repaired", []reindex.RepairedIndex{}, r.RepairedReferrers)
				same(t, "the audit rows of a repair", 0, len(rows))
				return
			}
			want := refs[:3]
			same(t, "the referrers the repaired index lists", want, listed)
			same(t, "repaired", []reindex.RepairedIndex{{Repository: "notes", Subject: pub.Digest, Index: idx,
				Added: want[max(tt.listed, 0):]}}, r.RepairedReferrers)
			if len(rows) != 1 || rows[0].State != index.AuditOK || rows[0].Principal != req.Principal || rows[0].Repo != "raw/scratch" ||
				rows[0].Subject != pub.Digest.String() || rows[0].Digest != idx {
				t.Errorf("the audit rows of the repair: %+v", rows)
			}

			again := e.runAs(t, req)
			same(t, "repaired by the next run", []reindex.RepairedIndex{}, again.RepairedReferrers)
			if _, d := e.listedBy(t, pub.RepoPath, tag); d != idx {
				t.Errorf("the next run pushed the index again: %s, want %s", d, idx)
			}
		})
	}
}

// runAs starts and runs req, a run of the env's repo, and returns its report.
func (e *env) runAs(t *testing.T, req reindex.Request) *reindex.RepoReport {
	t.Helper()
	run, err := e.w.Start(t.Context(), req)
	must(t, err)
	report, err := e.w.Run(t.Context(), run, req)
	must(t, err)
	return &report.Repos[0]
}

// signatures pushes n signatures of the version pub through ingest, as the facade does for
// cosign sign, and returns their digests in the order of their pushes. The fake writes no
// sha256-<hex> index for them.
func (e *env) signatures(t *testing.T, pub *format.Published, n int) []digest.Digest {
	t.Helper()
	ctx := t.Context()
	rr, err := e.fake.Repository(ctx, pub.RepoPath)
	must(t, err)
	subject, err := rr.Resolve(ctx, pub.Digest.String())
	must(t, err)
	must(t, rr.Push(ctx, ocispec.DescriptorEmptyJSON, bytes.NewReader(ocispec.DescriptorEmptyJSON.Data)))
	var refs []digest.Digest
	for i := range n {
		layer := []byte(fmt.Sprintf(`{"signature":%d}`, i))
		desc := ocispec.Descriptor{MediaType: "application/vnd.dev.sigstore.bundle.v0.3+json", Digest: digest.FromBytes(layer), Size: int64(len(layer))}
		must(t, rr.Push(ctx, desc, bytes.NewReader(layer)))
		body, err := json.Marshal(ocispec.Manifest{Versioned: specs2, MediaType: ocispec.MediaTypeImageManifest,
			ArtifactType: desc.MediaType, Config: ocispec.DescriptorEmptyJSON, Subject: &subject, Layers: []ocispec.Descriptor{desc},
			Annotations: map[string]string{"dev.sigstore.bundle.predicateType": "https://sigstore.dev/cosign/sign/v1"}})
		must(t, err)
		m := ocispec.Descriptor{MediaType: ocispec.MediaTypeImageManifest, Digest: digest.FromBytes(body), Size: int64(len(body))}
		_, err = e.pipe.PutReferrer(ctx, e.repo, ingest.Referrer{Path: "notes", Desc: m, Manifest: body, Subject: pub.Digest}, alice)
		must(t, err)
		refs = append(refs, m.Digest)
	}
	return refs
}

// deleteManifest deletes the manifest dg in the OCI repository name, in the registry.
func (e *env) deleteManifest(t *testing.T, name string, dg digest.Digest) {
	t.Helper()
	rr, err := e.fake.Repository(t.Context(), name)
	must(t, err)
	desc, err := rr.Resolve(t.Context(), dg.String())
	must(t, err)
	must(t, rr.Delete(t.Context(), desc))
}

// pushIndex pushes under tag in the OCI repository name an image index that lists refs with the
// descriptors the registry resolves.
func (e *env) pushIndex(t *testing.T, name, tag string, refs []digest.Digest) {
	t.Helper()
	rr, err := e.fake.Repository(t.Context(), name)
	must(t, err)
	idx := ocispec.Index{Versioned: specs2, MediaType: ocispec.MediaTypeImageIndex, Manifests: []ocispec.Descriptor{}}
	for _, d := range refs {
		desc, err := rr.Resolve(t.Context(), d.String())
		must(t, err)
		idx.Manifests = append(idx.Manifests, desc)
	}
	e.pushManifest(t, name, tag, idx, ocispec.MediaTypeImageIndex)
}

// listedBy returns the digests that the image index under tag in the OCI repository name lists,
// nil when the tag names nothing, and the index's digest.
func (e *env) listedBy(t *testing.T, name, tag string) ([]digest.Digest, digest.Digest) {
	t.Helper()
	rr, err := e.fake.Repository(t.Context(), name)
	must(t, err)
	desc, err := rr.Resolve(t.Context(), tag)
	if errors.Is(err, provider.ErrNotFound) {
		return nil, ""
	}
	must(t, err)
	rc, err := rr.Fetch(t.Context(), desc)
	must(t, err)
	defer func() { _ = rc.Close() }()
	var idx ocispec.Index
	must(t, json.NewDecoder(rc).Decode(&idx))
	var out []digest.Digest
	for _, m := range idx.Manifests {
		out = append(out, m.Digest)
	}
	return out, desc.Digest
}
