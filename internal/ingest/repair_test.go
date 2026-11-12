package ingest_test

import (
	"encoding/json"
	"sync/atomic"
	"testing"

	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/lajstrom/lajstrom/internal/index"
	"github.com/lajstrom/lajstrom/internal/ingest"
)

// TestRepairReferrers: without the referrers API, RepairReferrers pushes a subject's lost
// sha256-<hex> index again with its three signatures (#628), under the referrers lock of the
// subject, with the descriptors oras-go writes and the audit row referrers.repair. A registry that
// does not take the push leaves the index lost: the repair has the error, and its audit row is
// failed. A subject that the run did not find is not looked at.
func TestRepairReferrers(t *testing.T) {
	e, rec, subject := referrerEnv(t, false)
	refs, lost := threeReferrers(t, e, subject)
	ctx := t.Context()
	rr, err := e.prov.Repository(ctx, "raw/scratch/notes")
	must(t, err)
	must(t, rr.Delete(ctx, lost))
	tag := "sha256-" + subject.Digest.Encoded()
	found := map[digest.Digest][]string{subject.Digest: {"notes"}}
	var fail atomic.Int32
	fail.Store(1)
	opts := e.options(failingIndex{registry{Fake: e.prov, rec: rec}, tag, &fail})
	opts.Index = rec
	pipe := ingest.New(opts)

	got, err := pipe.RepairReferrers(ctx, e.repo, found, alice)
	must(t, err)
	if len(got) != 1 || got[0].Err == nil || got[0].Index != "" || got[0].Subject != subject.Digest {
		t.Fatalf("RepairReferrers with the push failing: %+v", got)
	}
	same(t, "the referrers listed after the push failed", []digest.Digest(nil), listedBy(t, e, tag))
	wantRepairRow(t, e.rows(t).Audit[0], index.AuditFailed, subject.Digest, "", nil)

	rec.locks = nil
	got, err = pipe.RepairReferrers(ctx, e.repo, found, alice)
	must(t, err)
	idx, err := rr.Resolve(ctx, tag)
	must(t, err)
	same(t, "repairs", []ingest.ReferrersRepair{{Path: "notes", Subject: subject.Digest, Index: idx.Digest, Added: refs}}, got)
	same(t, "the referrers listed", refs, listedBy(t, e, tag))
	same(t, "locks", []string{"raw/scratch/notes@" + subject.Digest.String()}, rec.locks)
	rc, err := rr.Fetch(ctx, idx)
	must(t, err)
	var pushed ocispec.Index
	must(t, json.NewDecoder(rc).Decode(&pushed))
	_ = rc.Close()
	for _, m := range pushed.Manifests {
		if m.ArtifactType != bundleType || m.Annotations["dev.sigstore.bundle.predicateType"] == "" || m.Size == 0 {
			t.Errorf("a descriptor of the repaired index: %+v", m)
		}
	}
	wantRepairRow(t, e.rows(t).Audit[0], index.AuditOK, subject.Digest, idx.Digest, refs)

	other := map[digest.Digest][]string{digest.FromString("another subject"): {"notes"}}
	if got, err := pipe.RepairReferrers(ctx, e.repo, other, alice); err != nil || got != nil {
		t.Errorf("RepairReferrers of a subject without referrers: %+v, %v", got, err)
	}
}

// wantRepairRow checks ev, the audit row of a repair by alice of the index of subject in
// raw/scratch/notes: its state, the index pushed and, in the details, the path and the referrers
// added.
func wantRepairRow(t *testing.T, ev index.AuditEvent, state string, subject, idx digest.Digest, added []digest.Digest) {
	t.Helper()
	var details struct {
		Path  string          `json:"path"`
		Added []digest.Digest `json:"added"`
	}
	must(t, json.Unmarshal(ev.Details, &details))
	if ev.Action != "referrers.repair" || ev.State != state || ev.Principal != alice.String() || ev.Subject != subject.String() ||
		ev.Repo != "raw/scratch" || details.Path != "notes" {
		t.Errorf("the audit row of the repair: %+v, details %s", ev, ev.Details)
	}
	if idx != "" {
		same(t, "the index the audit row names", idx, ev.Digest)
		same(t, "the referrers the audit row names", added, details.Added)
	}
}
