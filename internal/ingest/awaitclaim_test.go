package ingest_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/opencontainers/go-digest"

	"github.com/lajstrom/lajstrom/internal/format"
	"github.com/lajstrom/lajstrom/internal/index"
	"github.com/lajstrom/lajstrom/internal/ingest"
	"github.com/lajstrom/lajstrom/internal/provider"
)

// buildContentTag is buildRawAt(at) with format.Built.ContentTag set, as raw builds: the tag is
// taken to be made of the file's bytes.
func buildContentTag(at time.Time) func(context.Context, index.Repo, format.Upload) (*format.Built, error) {
	build := buildRawAt(at)
	return func(ctx context.Context, repo index.Repo, up format.Upload) (*format.Built, error) {
		b, err := build(ctx, repo, up)
		if b != nil {
			b.ContentTag = true
		}
		return b, err
	}
}

// holdClaim claims tag of package path pkg for another digest, as a publish of the same file
// on its way does before its push, and returns the claim.
func holdClaim(t *testing.T, e *env, pkg, tag string) index.TagClaim {
	t.Helper()
	c := index.TagClaim{RepoID: e.repo.ID, Package: pkg, Tag: tag, Digest: digest.FromString("another build of the same file"),
		Principal: "user:bob", ClaimedAt: now}
	must(t, e.st.ClaimTag(t.Context(), &c, now.Add(-time.Hour)))
	return c
}

// TestUploadAwaitsClaim: an upload whose tag is made of its file's bytes (format.Built.ContentTag)
// and that a fresh claim of another digest refuses, whose manifest the registry does not serve,
// waits for that claim (#1129): released while it waits, the upload stores its own manifest; held
// past the visibility timeout, it is 503 with Retry-After: 30 and nothing is pushed. An upload
// without ContentTag gets the 409 at once, as before.
func TestUploadAwaitsClaim(t *testing.T) {
	up := upload("tool", "1.0.0", file{"tool", "the same file"})
	t.Run("released", func(t *testing.T) {
		e := newEnv(t)
		e.plugin.BuildFunc = buildContentTag(created)
		c := holdClaim(t, e, "tool", "1.0.0")
		start := time.Now()
		go func() {
			time.Sleep(300 * time.Millisecond)
			if err := e.st.ReleaseTagClaim(context.Background(), c.ID); err != nil {
				t.Errorf("release the claim: %v", err)
			}
		}()
		pub, err := e.pipe.Upload(t.Context(), e.repo, up, alice)
		must(t, err)
		if waited := time.Since(start); pub.Existed || waited < 300*time.Millisecond {
			t.Errorf("Upload = %+v after %s; want a publish of its own after the claim went", pub, waited)
		}
		if refs := e.pushRefs("1.0.0"); len(refs) != 1 || refs[0].Digest != pub.Digest {
			t.Errorf("pushes %+v, want the upload's own %s", refs, pub.Digest)
		}
		e.noClaim(t, "tool", "1.0.0")
	})
	t.Run("held", func(t *testing.T) {
		e := newEnv(t)
		e.plugin.BuildFunc = buildContentTag(created)
		opts := e.options(e.prov)
		opts.VisibilityTimeout = 300 * time.Millisecond
		e.pipe = ingest.New(opts)
		holdClaim(t, e, "tool", "1.0.0")
		start := time.Now()
		_, err := e.pipe.Upload(t.Context(), e.repo, up, alice)
		h := http.Header{}
		format.SetRetryAfter(h, err)
		if !errors.Is(err, provider.ErrUnavailable) || format.StatusOf(err) != http.StatusServiceUnavailable || h.Get("Retry-After") != "30" {
			t.Errorf("Upload under a claim held = %v (status %d, Retry-After %q), want 503 with Retry-After: 30", err,
				format.StatusOf(err), h.Get("Retry-After"))
		}
		if waited := time.Since(start); waited < 300*time.Millisecond {
			t.Errorf("Upload answered after %s, want it to wait for the visibility timeout", waited)
		}
		if refs := e.pushRefs("1.0.0"); len(refs) != 0 {
			t.Errorf("pushes %+v, want none", refs)
		}
	})
	t.Run("no ContentTag", func(t *testing.T) {
		e := newEnv(t)
		e.plugin.BuildFunc = buildRawAt(created)
		holdClaim(t, e, "tool", "1.0.0")
		start := time.Now()
		_, err := e.pipe.Upload(t.Context(), e.repo, up, alice)
		var conflict *index.TagConflictError
		if !errors.As(err, &conflict) || time.Since(start) > 5*time.Second {
			t.Errorf("Upload without ContentTag under a claim = %v after %s, want the TagConflictError at once", err, time.Since(start))
		}
	})
}

// TestUploadRetryAfterIntendFailed is H-06 of ha.sh (#1129): the database drops out after an
// upload claimed its tag, so its pending audit row fails and nothing reaches the registry, and so
// does the release of its claim. Once the database answers, the pipeline releases the claim, and
// the client's retry, built a second later with another digest, stores the file: not the 409 of
// its own first attempt's claim.
func TestUploadRetryAfterIntendFailed(t *testing.T) {
	ctx := t.Context()
	e := newEnv(t)
	e.plugin.BuildFunc = buildContentTag(created)
	down := &intendDown{Store: e.st}
	opts := e.options(e.prov)
	opts.Index = down
	e.pipe = ingest.New(opts)
	up := upload("tool", "1.0.0", file{"tool", "the same file"})

	_, err := e.pipe.Upload(ctx, e.repo, up, alice)
	wantUnavailable(t, "Upload while the database drops out", err)
	if refs := e.pushRefs("1.0.0"); len(refs) != 0 {
		t.Fatalf("pushes %+v, want none", refs)
	}
	if _, err := e.st.GetTagClaim(ctx, e.repo.ID, "tool", "1.0.0"); err != nil {
		t.Fatalf("the claim of the failed upload: %v, want it held while the database is out", err)
	}

	down.up.Store(true)
	e.plugin.BuildFunc = buildContentTag(created.Add(time.Second))
	pub, err := e.pipe.Upload(ctx, e.repo, up, alice)
	must(t, err)
	if pub.Existed {
		t.Errorf("the retry = %+v, want a publish of its own", pub)
	}
	if r := e.rows(t); len(r.Versions) != 1 || r.Versions[0].Digest != pub.Digest {
		t.Errorf("versions after the retry: %+v, want one, %s", r.Versions, pub.Digest)
	}
	e.noClaim(t, "tool", "1.0.0")
}

// intendDown is a store whose database drops out after a publish claimed its tag: the first
// pending audit row fails, and every release of a claim fails until up is set, with
// index.ErrUnavailable as a database that dropped out fails them.
type intendDown struct {
	index.Store
	failed atomic.Bool
	up     atomic.Bool
}

func (s *intendDown) AppendAudit(ctx context.Context, ev *index.AuditEvent) error {
	if ev.State == index.AuditPending && s.failed.CompareAndSwap(false, true) {
		return fmt.Errorf("%w: audit: %w", index.ErrUnavailable, errDown)
	}
	return s.Store.AppendAudit(ctx, ev)
}

func (s *intendDown) ReleaseTagClaim(ctx context.Context, id int64) error {
	if !s.up.Load() {
		return fmt.Errorf("%w: tag_claims: %w", index.ErrUnavailable, errDown)
	}
	return s.Store.ReleaseTagClaim(ctx, id)
}
