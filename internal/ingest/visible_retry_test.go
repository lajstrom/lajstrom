package ingest_test

import (
	"testing"
	"time"

	"github.com/lajstrom/lajstrom/internal/format"
	"github.com/lajstrom/lajstrom/internal/index"
	"github.com/lajstrom/lajstrom/internal/ingest"
	"github.com/lajstrom/lajstrom/internal/oci/profile"
	"github.com/lajstrom/lajstrom/internal/provider"
)

// TestVisibilityRetryRebuilt: a frontend's retry after the 503 builds its manifest again, with
// another org.opencontainers.image.created and so another digest; it takes over the manifest
// of its first attempt, which the claim holds, instead of a 409 from that claim. An upload of
// other files is still 409.
func TestVisibilityRetryRebuilt(t *testing.T) {
	ctx := t.Context()
	e := newEnv(t)
	lag := e.lagging(-1, 100*time.Millisecond)
	up := upload("tool", "1.0.0", file{"tool", "linux binary"})
	_, err := e.pipe.Upload(ctx, e.repo, up, alice)
	wantNotVisible(t, err)
	first := e.pushRefs("1.0.0")[0].Digest

	_, err = e.pipe.Upload(ctx, e.repo, upload("tool", "1.0.0", file{"tool", "other bytes"}), alice)
	wantTagConflict(t, err, first)

	lag.Serve()
	e.plugin.BuildFunc = buildRawAt(created.Add(time.Minute))
	pub, err := e.pipe.Upload(ctx, e.repo, up, alice)
	must(t, err)
	if pub.Digest != first || pub.Existed {
		t.Errorf("the retry: %s, Existed %v; want the first manifest, %s, confirmed now", pub.Digest, pub.Existed, first)
	}
	if r := e.rows(t); len(r.Versions) != 1 || r.Versions[0].Digest != first || len(r.Audit) != 2 || r.Audit[0].State != index.AuditOK {
		t.Errorf("rows %+v, want one version of %s, and the retry's audit row ok beside the first one's", r, first)
	}
	e.noClaim(t, "tool", "1.0.0")
	e.noClaim(t, "tool", "")
}

// TestJournalVisibility: the journal records its entries once the registry serves them, and
// answers 503 when it does not; the next append adopts what the first pushed.
func TestJournalVisibility(t *testing.T) {
	ctx := t.Context()
	e := newNpmEnv(t, "1.0.0")
	lag := e.lagging(2, 10*time.Second)
	seqs, err := e.pipe.Journal(ctx, e.npm, widgetsPath, []format.JournalOp{opAdd("latest", "1.0.0")}, alice)
	must(t, err)
	if len(seqs) != 1 || lag.Misses() == 0 || len(e.entries(t)) != 1 {
		t.Errorf("seqs %v, %d misses, %d rows; want entry 1 recorded after the reads missed", seqs, lag.Misses(), len(e.entries(t)))
	}

	lag = e.lagging(-1, 100*time.Millisecond)
	_, err = e.pipe.Journal(ctx, e.npm, widgetsPath, []format.JournalOp{opAdd("next", "1.0.0")}, alice)
	wantNotVisible(t, err)
	if n := len(e.entries(t)); n != 1 {
		t.Errorf("%d journal rows, want entry 2 not recorded", n)
	}
	_, err = e.pipe.Journal(ctx, e.npm, widgetsPath, []format.JournalOp{opAdd("next", "1.0.0")}, alice)
	wantNotVisible(t, err) // entry 2 is claimed and not served: the journal waits for it

	lag.Serve()
	seqs, err = e.pipe.Journal(ctx, e.npm, widgetsPath, []format.JournalOp{opAdd("next", "1.0.0")}, alice)
	must(t, err)
	same(t, "seqs of the retry", []int64{3}, seqs)
	if rows := e.entries(t); len(rows) != 3 || rows[1].Tag != profile.JournalTag(2) {
		t.Errorf("rows %+v, want entry 2 adopted and 3", rows)
	}
	for _, seq := range []int64{2, 3} {
		if c, err := e.st.GetTagClaim(ctx, e.npm.ID, widgetsPath, profile.JournalTag(seq)); err == nil {
			t.Errorf("the claim of entry %d is left: %+v", seq, c)
		}
	}
}

// TestJournalClaimNotPushed: an append that claimed its entry and stopped before the push. On
// a strongly consistent registry the next append takes the claim over at once; on another one
// it waits until the claim is stale.
func TestJournalClaimNotPushed(t *testing.T) {
	ctx := t.Context()
	for _, strong := range []bool{true, false} {
		e := newNpmEnv(t, "1.0.0")
		var reg provider.Provider = e.prov
		if !strong {
			reg = e.lagging(0, 100*time.Millisecond)
		}
		dead := index.TagClaim{RepoID: e.npm.ID, Package: widgetsPath, Tag: profile.JournalTag(1), Digest: dg("lost"),
			Principal: alice.String(), ClaimedAt: now}
		must(t, e.st.ClaimTag(ctx, &dead, now.Add(-time.Hour)))
		seqs, err := e.pipe.Journal(ctx, e.npm, widgetsPath, []format.JournalOp{opAdd("latest", "1.0.0")}, alice)
		if !strong {
			wantNotVisible(t, err)
			o := e.options(reg)
			o.Now = func() time.Time { return now.Add(time.Hour) }
			e.pipe = ingest.New(o)
			seqs, err = e.pipe.Journal(ctx, e.npm, widgetsPath, []format.JournalOp{opAdd("latest", "1.0.0")}, alice)
		}
		must(t, err)
		same(t, "seqs", []int64{1}, seqs)
	}
}

// TestReferrerVisibility: a referrer is stored once the registry serves it, and is 503 when it
// does not within twice the visibility timeout.
func TestReferrerVisibility(t *testing.T) {
	ctx := t.Context()
	e, _, subject := referrerEnv(t, true)
	lag := e.lagging(2, 10*time.Second)
	r := signature(t, subject, "signature")
	_, err := e.pipe.PutReferrer(ctx, e.repo, r, alice)
	must(t, err)
	if n := lag.Misses(); n != 2 {
		t.Errorf("%d reads missed, want the referrer's first two", n)
	}
	e.lagging(-1, 50*time.Millisecond)
	start := time.Now()
	_, err = e.pipe.PutReferrer(ctx, e.repo, r, alice)
	wantNotVisible(t, err)
	if d := time.Since(start); d < 100*time.Millisecond {
		t.Errorf("gave up after %s, want twice the visibility timeout at least", d)
	}
}
