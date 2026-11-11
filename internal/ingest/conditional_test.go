package ingest_test

import (
	"encoding/json"
	"errors"
	"math/rand/v2"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/opencontainers/go-digest"

	"github.com/lajstrom/lajstrom/internal/auth"
	"github.com/lajstrom/lajstrom/internal/format"
	"github.com/lajstrom/lajstrom/internal/index"
	"github.com/lajstrom/lajstrom/internal/ingest"
	"github.com/lajstrom/lajstrom/internal/oci/profile"
)

// publishRaw publishes the file name at version with content through the pipeline and returns
// the manifest's digest.
func publishRaw(t *testing.T, e *env, name, version, content string) digest.Digest {
	t.Helper()
	built, raw, pkgPath := e.prepare(t, upload(name, version, file{name, content}))
	res, err := e.ingest(t, built, raw, pkgPath)
	must(t, err)
	return res.Digest
}

// conditional is the conditional delete of the version and variant "" of the raw file name by
// its digest d.
func conditional(name, version string, d digest.Digest, c format.Condition) format.Deletion {
	c.Version = version
	return format.Deletion{Path: name, Reference: d.String(), Condition: &c}
}

// wantRefused fails the test unless err is a *format.RefusedError of reason.
func wantRefused(t *testing.T, err error, reason format.Refusal) *format.RefusedError {
	t.Helper()
	var refused *format.RefusedError
	if !errors.As(err, &refused) || refused.Reason != reason {
		t.Fatalf("err = %v, want a refusal %s", err, reason)
	}
	return refused
}

// TestConditionalDelete: a delete with a condition deletes the version when it still holds, and
// otherwise refuses it and changes nothing: published again under another digest (changed),
// published after the cut-off (young), gone (index.ErrNotFound). The audit row names the job
// and the token of the request. A conditional delete names a manifest by its digest.
func TestConditionalDelete(t *testing.T) {
	ctx := t.Context()
	e := newEnv(t)
	d1 := publishRaw(t, e, "notes", "1.0", "first")

	_, err := e.pipe.Delete(ctx, e.repo, conditional("notes", "1.0", d1, format.Condition{UnmodifiedSince: created.Add(-time.Second)}), alice)
	young := wantRefused(t, err, format.RefusedYoung)
	if !young.PublishedAt.Equal(created) {
		t.Errorf("PublishedAt %v, want %v", young.PublishedAt, created)
	}
	_, err = e.pipe.Delete(ctx, e.repo, conditional("notes", "1.0", digest.FromString("another manifest"), format.Condition{}), alice)
	changed := wantRefused(t, err, format.RefusedChanged)
	if !slices.Equal(changed.Digests, []digest.Digest{d1}) {
		t.Errorf("Digests %v, want %s", changed.Digests, d1)
	}
	_, err = e.pipe.Delete(ctx, e.repo, format.Deletion{Path: "notes", Reference: "1.0", Condition: &format.Condition{Version: "1.0"}}, alice)
	if !errors.Is(err, format.ErrInvalid) {
		t.Errorf("a conditional delete by tag: %v, want ErrInvalid", err)
	}
	if e.tagged(t, "raw/scratch/notes", "1.0") != d1 || len(e.rows(t).Versions) != 1 || len(e.rows(t).Audit) != 1 {
		t.Fatalf("a refused delete changed something: %+v", e.rows(t))
	}

	cred := auth.WithCredential(ctx, auth.Credential{Kind: auth.CredPAT, TokenID: 7, UserID: 1})
	del := conditional("notes", "1.0", d1, format.Condition{UnmodifiedSince: created})
	del.Job = "retention-1"
	got, err := e.pipe.Delete(cred, e.repo, del, alice)
	must(t, err)
	if got.Digest != d1 || got.Version == nil || got.Version.Version != "1.0" {
		t.Errorf("Deleted %+v", got)
	}
	rows := e.rows(t)
	if len(rows.Versions) != 0 || e.tagged(t, "raw/scratch/notes", "1.0") != "" {
		t.Errorf("left: %+v", rows)
	}
	var details map[string]any
	must(t, json.Unmarshal(rows.Audit[0].Details, &details))
	if rows.Audit[0].Action != "delete" || details["job"] != "retention-1" || details["token"] != "token:7" {
		t.Errorf("audit row %+v, details %v", rows.Audit[0], details)
	}
	if _, err := e.pipe.Delete(ctx, e.repo, conditional("notes", "1.0", d1, format.Condition{}), alice); !errors.Is(err, index.ErrNotFound) {
		t.Errorf("the same delete again: %v, want ErrNotFound", err)
	}
}

// TestConditionalDeletePointers: an npm version that a dist-tag names is refused, naming the
// dist-tags, unless the delete is forced; the delete takes the package's journal lock, then the
// version's tag lock.
func TestConditionalDeletePointers(t *testing.T) {
	ctx := t.Context()
	e := newNpmEnv(t, "1.0.0", "2.0.0")
	rec := &lockRecorder{Store: e.st}
	opts := e.options(e.prov)
	opts.Index = rec
	e.pipe = ingest.New(opts)
	_, err := e.pipe.Journal(ctx, e.npm, widgetsPath, []format.JournalOp{opAdd("latest", "2.0.0"), opAdd("beta", "1.0.0"), opAdd("lts", "1.0.0")}, alice)
	must(t, err)
	rec.locks = nil
	del := format.Deletion{Path: widgetsPath, Reference: dg("1.0.0").String(), Condition: &format.Condition{Version: "1.0.0"}}
	_, err = e.pipe.Delete(ctx, e.npm, del, alice)
	refused := wantRefused(t, err, format.RefusedProtected)
	if want := []format.Pointer{{Kind: "dist-tag", Name: "beta"}, {Kind: "dist-tag", Name: "lts"}}; !slices.Equal(refused.Pointers, want) {
		t.Errorf("Pointers %v, want %v", refused.Pointers, want)
	}
	if !errors.Is(err, index.ErrConflict) {
		t.Errorf("a protected version is not ErrConflict (409): %v", err)
	}
	name := "npm/internal/" + widgetsPath
	same(t, "locks", []string{name + "@" + profile.EmptyConfig.Digest.String(), name + "@" + dg("1.0.0").String()}, rec.locks)
	del.Condition.Force = true
	_, err = e.pipe.Delete(ctx, e.npm, del, alice)
	must(t, err)
	if _, err := e.st.GetVersion(ctx, e.pkg.ID, "1.0.0", ""); !errors.Is(err, index.ErrNotFound) {
		t.Errorf("the forced delete left 1.0.0: %v", err)
	}
}

// jitter sleeps up to 3 ms, so that the goroutines of a race start in either order.
func jitter() { time.Sleep(time.Duration(rand.IntN(3000)) * time.Microsecond) }

// TestConditionalDeleteDistTagRace races a dist-tag move onto a version against its
// conditional delete, on fresh packages, with -race: one of the two wins, whole. Either the
// delete is refused naming the dist-tag and the version stays, or the move is refused (the
// version is gone) and no dist-tag names the deleted version. Never both, never a dist-tag that
// names a version the index does not have.
func TestConditionalDeleteDistTagRace(t *testing.T) {
	wins := map[string]int{}
	for range 40 {
		e := newNpmEnv(t, "1.0.0", "2.0.0")
		ctx := t.Context()
		_, err := e.pipe.Journal(ctx, e.npm, widgetsPath, []format.JournalOp{opAdd("latest", "2.0.0")}, alice)
		must(t, err)
		start := make(chan struct{})
		var wg sync.WaitGroup
		var moveErr, delErr error
		wg.Go(func() {
			<-start
			jitter()
			_, moveErr = e.pipe.Journal(ctx, e.npm, widgetsPath, []format.JournalOp{opAdd("old", "1.0.0")}, alice)
		})
		wg.Go(func() {
			<-start
			jitter()
			_, delErr = e.pipe.Delete(ctx, e.npm, format.Deletion{Path: widgetsPath, Reference: dg("1.0.0").String(),
				Condition: &format.Condition{Version: "1.0.0"}}, alice)
		})
		close(start)
		wg.Wait()
		_, verr := e.st.GetVersion(ctx, e.pkg.ID, "1.0.0", "")
		pointed := slices.ContainsFunc(e.state(t), func(s index.JournalState) bool { return s.Kind == index.JournalDistTag && s.Value == "1.0.0" })
		switch {
		case moveErr == nil && delErr != nil:
			wantRefused(t, delErr, format.RefusedProtected)
			if verr != nil || !pointed {
				t.Fatalf("the move won, but the version is %v and named %v", verr, pointed)
			}
			wins["move"]++
		case delErr == nil && moveErr != nil:
			if !errors.Is(moveErr, format.ErrInvalid) || !errors.Is(verr, index.ErrNotFound) || pointed {
				t.Fatalf("the delete won, but the move failed with %v, the version is %v, named %v", moveErr, verr, pointed)
			}
			wins["delete"]++
		default:
			t.Fatalf("move %v, delete %v: want exactly one to succeed", moveErr, delErr)
		}
	}
	t.Logf("wins: %v", wins)
}

// TestConditionalDeleteRepublishRace races a conditional delete of a version, by the digest a
// client read, against another client that deletes the version and publishes it again with
// other bytes, with -race. The conditional delete deletes the version it read or is refused
// (changed, or gone): the version published again always survives, in the registry and in the
// index.
func TestConditionalDeleteRepublishRace(t *testing.T) {
	outcomes := map[string]int{}
	for i := range 40 {
		e := newEnv(t)
		ctx := t.Context()
		d1 := publishRaw(t, e, "notes", "1.0", "first")
		start := make(chan struct{})
		var wg sync.WaitGroup
		var condErr, delErr, pubErr error
		var d2 digest.Digest
		built, raw, pkgPath := e.prepare(t, upload("notes", "1.0", file{"notes", "second"}))
		wg.Go(func() {
			<-start
			jitter()
			if _, delErr = e.pipe.Delete(ctx, e.repo, format.Deletion{Path: "notes", Reference: d1.String()}, alice); errors.Is(delErr, index.ErrNotFound) {
				delErr = nil // the conditional delete was first
			}
			if delErr == nil {
				var res *ingest.Result
				if res, pubErr = e.pipe.Ingest(ctx, e.repo, pkgPath, built.Tag, built.Manifest, raw, alice); pubErr == nil {
					d2 = res.Digest
				}
			}
		})
		wg.Go(func() {
			<-start
			jitter()
			_, condErr = e.pipe.Delete(ctx, e.repo, conditional("notes", "1.0", d1, format.Condition{}), alice)
		})
		close(start)
		wg.Wait()
		if delErr != nil || pubErr != nil {
			t.Fatalf("round %d: the other client's delete %v, publish %v", i, delErr, pubErr)
		}
		var refused *format.RefusedError
		switch {
		case condErr == nil:
			outcomes["deleted"]++
		case errors.As(condErr, &refused) && refused.Reason == format.RefusedChanged:
			outcomes["changed"]++
		case errors.Is(condErr, index.ErrNotFound):
			outcomes["gone"]++
		default:
			t.Fatalf("round %d: the conditional delete: %v", i, condErr)
		}
		if got := e.tagged(t, "raw/scratch/notes", "1.0"); got != d2 {
			t.Fatalf("round %d: the registry has %q under 1.0, want the version published again, %s", i, got, d2)
		}
		if rows := e.rows(t); len(rows.Versions) != 1 || rows.Versions[0].Digest != d2 {
			t.Fatalf("round %d: the index has %+v, want %s", i, rows.Versions, d2)
		}
	}
	t.Logf("outcomes: %v", outcomes)
}
