package ingest_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/opencontainers/go-digest"
	"github.com/opencontainers/image-spec/specs-go"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/lajstrom/lajstrom/internal/format"
	"github.com/lajstrom/lajstrom/internal/index"
	"github.com/lajstrom/lajstrom/internal/ingest"
	"github.com/lajstrom/lajstrom/internal/provider"
)

// failingIndex is a registry whose pushes under tag fail while fail is above zero, as a registry
// that answers 500 after the provider's retries does; each failure counts fail down.
type failingIndex struct {
	provider.Provider
	tag  string
	fail *atomic.Int32
}

func (f failingIndex) Repository(ctx context.Context, name string) (provider.Repository, error) {
	rr, err := f.Provider.Repository(ctx, name)
	return failingIndexRepo{rr, f}, err
}

type failingIndexRepo struct {
	provider.Repository
	f failingIndex
}

func (r failingIndexRepo) PushReference(ctx context.Context, desc ocispec.Descriptor, content io.Reader, tag string) error {
	if tag == r.f.tag && r.f.fail.Add(-1) >= 0 {
		return errors.New("registry local answered 500 Internal Server Error")
	}
	return r.Repository.PushReference(ctx, desc, content, tag)
}

// threeReferrers pushes three signatures of subject through PutReferrer and the subject's
// tag-schema index listing them, which the fake does not write itself. It returns the
// signatures in the order of their pushes and the index.
func threeReferrers(t *testing.T, e *env, subject ocispec.Descriptor) ([]digest.Digest, ocispec.Descriptor) {
	t.Helper()
	ctx := t.Context()
	rr, err := e.prov.Repository(ctx, "raw/scratch/notes")
	must(t, err)
	var refs []digest.Digest
	var listed []ocispec.Descriptor
	for _, layer := range []string{"signature", "attestation", "sbom"} {
		must(t, rr.Push(ctx, ocispec.Descriptor{MediaType: "application/octet-stream", Digest: digest.FromString(layer), Size: int64(len(layer))},
			bytes.NewReader([]byte(layer))))
		r := signature(t, subject, layer)
		_, err := e.pipe.PutReferrer(ctx, e.repo, r, alice)
		must(t, err)
		refs, listed = append(refs, r.Desc.Digest), append(listed, r.Desc)
	}
	b, err := json.Marshal(ocispec.Index{Versioned: specs.Versioned{SchemaVersion: 2}, MediaType: ocispec.MediaTypeImageIndex, Manifests: listed})
	must(t, err)
	idx := ocispec.Descriptor{MediaType: ocispec.MediaTypeImageIndex, Digest: digest.FromBytes(b), Size: int64(len(b))}
	must(t, rr.PushReference(ctx, idx, bytes.NewReader(b), "sha256-"+subject.Digest.Encoded()))
	return refs, idx
}

// TestDeleteReferrerIndexNotPushed: without the referrers API, a delete of one of three
// signatures whose push of the subject's index without it fails (#711) pushes the index back as
// it was and deletes nothing: the three are listed, and the answer is a 503 with Retry-After.
// When the index cannot be pushed back either, it is a 503 all the same, and the error log names
// the subject and the signatures no index lists. Either way, the retry deletes the signature and
// leaves the index listing the other two: the index it lost is rebuilt from the audit log.
func TestDeleteReferrerIndexNotPushed(t *testing.T) {
	for _, tt := range []struct {
		name     string
		failures int32
		restored bool
	}{{"pushed back", 1, true}, {"not pushed back", 2, false}} {
		t.Run(tt.name, func(t *testing.T) {
			e, rec, subject := referrerEnv(t, false)
			refs, _ := threeReferrers(t, e, subject)
			tag := "sha256-" + subject.Digest.Encoded()
			var fail atomic.Int32
			fail.Store(tt.failures)
			var log bytes.Buffer
			opts := e.options(failingIndex{registry{Fake: e.prov, rec: rec}, tag, &fail})
			opts.Index, opts.Log = rec, slog.New(slog.NewTextHandler(&log, nil))
			pipe := ingest.New(opts)
			del := format.Deletion{Path: "notes", Reference: refs[0].String()}

			_, err := pipe.Delete(t.Context(), e.repo, del, alice)
			wantRetryLater(t, err)
			if d := e.tagged(t, "raw/scratch/notes", refs[0].String()); d != refs[0] {
				t.Errorf("the signature whose index was not pushed is deleted")
			}
			if ev := e.rows(t).Audit[0]; ev.Action != "delete" || ev.State != index.AuditFailed {
				t.Errorf("the audit row of the delete: %s %s", ev.Action, ev.State)
			}
			if tt.restored {
				same(t, "the referrers the index lists after the push back", refs, listedBy(t, e, tag))
				wantUnlistedLogged(t, &log, subject.Digest, nil)
			} else {
				same(t, "the referrers the index lists when it was not pushed back", []digest.Digest(nil), listedBy(t, e, tag))
				wantUnlistedLogged(t, &log, subject.Digest, refs)
			}

			fail.Store(0)
			got, err := pipe.Delete(t.Context(), e.repo, del, alice)
			must(t, err)
			if got.Digest != refs[0] {
				t.Errorf("the retry deleted %s, want %s", got.Digest, refs[0])
			}
			gone(t, e, refs[0].String())
			same(t, "the referrers the index lists after the retry", refs[1:], listedBy(t, e, tag))
			if ev := e.rows(t).Audit[0]; ev.Action != "delete" || ev.State != index.AuditOK || ev.Digest != refs[0] {
				t.Errorf("the audit row of the retry: %s %s %s", ev.Action, ev.State, ev.Digest)
			}
		})
	}
}

// TestDeleteReferrerRepairsIndex: without the referrers API, the delete of a signature whose
// subject's index lacks a referrer that the audit log has as pushed, and the registry has, adds
// it back: the index then lists every other referrer (#711).
func TestDeleteReferrerRepairsIndex(t *testing.T) {
	e, rec, subject := referrerEnv(t, false)
	refs, _ := threeReferrers(t, e, subject)
	tag := "sha256-" + subject.Digest.Encoded()
	rr, err := e.prov.Repository(t.Context(), "raw/scratch/notes")
	must(t, err)
	lacking := ocispec.Index{Versioned: specs.Versioned{SchemaVersion: 2}, MediaType: ocispec.MediaTypeImageIndex}
	for _, d := range refs[:2] {
		desc, err := rr.Resolve(t.Context(), d.String())
		must(t, err)
		lacking.Manifests = append(lacking.Manifests, desc)
	}
	b, err := json.Marshal(lacking)
	must(t, err)
	must(t, rr.PushReference(t.Context(), ocispec.Descriptor{MediaType: ocispec.MediaTypeImageIndex, Digest: digest.FromBytes(b), Size: int64(len(b))},
		bytes.NewReader(b), tag))
	opts := e.options(registry{Fake: e.prov, rec: rec})
	opts.Index = rec
	_, err = ingest.New(opts).Delete(t.Context(), e.repo, format.Deletion{Path: "notes", Reference: refs[0].String()}, alice)
	must(t, err)
	gone(t, e, refs[0].String())
	same(t, "the referrers the index lists", refs[1:], listedBy(t, e, tag))
}

// TestDeleteReferrerGone: without the referrers API, a signature that a delete before #711's fix
// took, after the subject's index, whose push without it then failed, is deleted again: the
// retry rebuilds the index from the audit log with the other two, records the delete and
// answers success. A delete of it after that, and of a digest the repo never had, is 404.
func TestDeleteReferrerGone(t *testing.T) {
	e, rec, subject := referrerEnv(t, false)
	refs, idx := threeReferrers(t, e, subject)
	tag := "sha256-" + subject.Digest.Encoded()
	ctx := t.Context()
	rr, err := e.prov.Repository(ctx, "raw/scratch/notes")
	must(t, err)
	sig, err := rr.Resolve(ctx, refs[0].String())
	must(t, err)
	must(t, rr.Delete(ctx, idx))
	must(t, rr.Delete(ctx, sig))
	must(t, e.st.AppendAudit(ctx, &index.AuditEvent{At: now, Principal: alice.String(), Action: "delete", Repo: "raw/scratch",
		Subject: subject.Digest.String(), Digest: refs[0], State: index.AuditFailed, Error: "ingest: push " + tag + ": 500"}))
	opts := e.options(registry{Fake: e.prov, rec: rec})
	opts.Index = rec
	pipe := ingest.New(opts)

	never := digest.FromString("never pushed")
	if _, err := pipe.Delete(ctx, e.repo, format.Deletion{Path: "notes", Reference: never.String()}, alice); !errors.Is(err, index.ErrNotFound) {
		t.Errorf("Delete of a digest the repo never had: %v, want index.ErrNotFound", err)
	}
	got, err := pipe.Delete(ctx, e.repo, format.Deletion{Path: "notes", Reference: refs[0].String()}, alice)
	must(t, err)
	if got.Digest != refs[0] || got.Version != nil {
		t.Errorf("Deleted %+v, want %s and no version", got, refs[0])
	}
	same(t, "the referrers the index lists", refs[1:], listedBy(t, e, tag))
	if ev := e.rows(t).Audit[0]; ev.Action != "delete" || ev.State != index.AuditOK || ev.Digest != refs[0] || ev.Subject != subject.Digest.String() {
		t.Errorf("the audit row of the delete: %+v", ev)
	}
	if _, err := pipe.Delete(ctx, e.repo, format.Deletion{Path: "notes", Reference: refs[0].String()}, alice); !errors.Is(err, index.ErrNotFound) {
		t.Errorf("Delete of the signature once more: %v, want index.ErrNotFound", err)
	}
}

// wantRetryLater fails the test unless err is provider.ErrUnavailable: a 503 with Retry-After.
func wantRetryLater(t *testing.T, err error) {
	t.Helper()
	if !errors.Is(err, provider.ErrUnavailable) || format.StatusOf(err) != http.StatusServiceUnavailable {
		t.Fatalf("Delete with the index push failing: %v (status %d), want provider.ErrUnavailable, 503", err, format.StatusOf(err))
	}
	if after, ok := provider.RetryAfter(err); !ok || after <= 0 {
		t.Errorf("Retry-After of the 503: %s, %t", after, ok)
	}
}

// wantUnlistedLogged checks the error lines of log: one that names the subject and each of
// unlisted, or none when unlisted is empty.
func wantUnlistedLogged(t *testing.T, log *bytes.Buffer, subject digest.Digest, unlisted []digest.Digest) {
	t.Helper()
	var lines []string
	for line := range strings.Lines(log.String()) {
		if strings.Contains(line, "level=ERROR ") {
			lines = append(lines, line)
		}
	}
	if len(unlisted) == 0 {
		same(t, "error lines", []string(nil), lines)
		return
	}
	if len(lines) != 1 || !strings.Contains(lines[0], subject.String()) {
		t.Fatalf("error lines %q: want one naming the subject %s", lines, subject)
	}
	for _, d := range unlisted {
		if !strings.Contains(lines[0], d.String()) {
			t.Errorf("the error line %q does not name %s", lines[0], d)
		}
	}
}
