package ingest_test

import (
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/lajstrom/lajstrom/internal/format"
	"github.com/lajstrom/lajstrom/internal/index"
	"github.com/lajstrom/lajstrom/internal/ingest"
	"github.com/lajstrom/lajstrom/internal/provider"
	"github.com/lajstrom/lajstrom/internal/provider/providertest"
)

// lagging makes the registry of e eventually consistent: the first reads of each manifest
// pushed, by digest and by tag, miss it (negative: every read), and a publish reads back for
// at most timeout.
func (e *env) lagging(manifests int, timeout time.Duration) *providertest.Lag {
	lag := &providertest.Lag{Fake: e.prov, Manifests: manifests}
	o := e.options(lag)
	o.VisibilityTimeout = timeout
	e.pipe = ingest.New(o)
	return lag
}

// wantNotVisible checks that err is the barrier's: 503 with Retry-After 30 s.
func wantNotVisible(t *testing.T, err error) {
	t.Helper()
	if !errors.Is(err, provider.ErrUnavailable) || format.StatusOf(err) != http.StatusServiceUnavailable {
		t.Fatalf("error = %v, want provider.ErrUnavailable, 503", err)
	}
	if after, ok := provider.RetryAfter(err); !ok || after != 30*time.Second {
		t.Errorf("Retry-After of %v = %v, %v; want 30s", err, after, ok)
	}
}

// TestVisibilityBarrier: on a registry that misses the first three reads of a manifest and of
// its tag, an upload and a manifest of /v2 are confirmed after they read back, not before.
func TestVisibilityBarrier(t *testing.T) {
	ctx := t.Context()
	e := newEnv(t)
	lag := e.lagging(3, 10*time.Second)
	pub, err := e.pipe.Upload(ctx, e.repo, upload("tool", "1.0.0", file{"tool", "linux binary"}), alice)
	must(t, err)
	if n := lag.Misses(); n != 6 {
		t.Errorf("%d reads missed, want 3 of the manifest and 3 of its tag before the confirm", n)
	}
	if r := e.rows(t); len(r.Versions) != 1 || r.Versions[0].Digest != pub.Digest {
		t.Errorf("versions %+v, want the one published", r.Versions)
	}
	e.noClaim(t, "tool", "1.0.0")

	built, raw, pkgPath := e.prepare(t, upload("tool", "2.0.0", file{"tool", "v2"}))
	res, err := e.ingest(t, built, raw, pkgPath)
	must(t, err)
	if n := lag.Misses(); n != 12 || res.Existed {
		t.Errorf("%d reads missed in all, Existed %v; want 12, false", n, res.Existed)
	}
}

// TestVisibilityTimeout: a manifest the registry does not serve within the visibility timeout
// is not confirmed: 503 with Retry-After, no version, and the claim stays, so that another
// digest is 409 and the same upload again, once the registry serves it, completes it.
func TestVisibilityTimeout(t *testing.T) {
	ctx := t.Context()
	e := newEnv(t)
	lag := e.lagging(-1, 200*time.Millisecond)
	up := upload("tool", "1.0.0", file{"tool", "linux binary"})
	_, err := e.pipe.Upload(ctx, e.repo, up, alice)
	wantNotVisible(t, err)
	refs := e.pushRefs("1.0.0")
	if len(refs) != 1 {
		t.Fatalf("pushes %v, want one", refs)
	}
	first := refs[0].Digest
	if r := e.rows(t); len(r.Versions) != 0 || len(r.Audit) != 1 || r.Audit[0].State != index.AuditFailed {
		t.Errorf("rows %+v, want no version before the registry serves the manifest, and the push's audit row failed", r)
	}
	if c, err := e.st.GetTagClaim(ctx, e.repo.ID, "tool", "1.0.0"); err != nil || c.Digest != first {
		t.Errorf("the claim = %+v, %v; want the first digest's, kept", c, err)
	}

	_, err = e.pipe.Upload(ctx, e.repo, upload("tool", "1.0.0", file{"tool", "other bytes"}), alice)
	wantTagConflict(t, err, first)

	lag.Serve()
	pub, err := e.pipe.Upload(ctx, e.repo, up, alice)
	must(t, err)
	if pub.Digest != first || pub.Existed {
		t.Errorf("the retry: %s, Existed %v; want the first manifest, %s, confirmed now", pub.Digest, pub.Existed, first)
	}
	if r := e.rows(t); len(r.Versions) != 1 || r.Versions[0].Digest != first || len(r.Audit) != 2 || r.Audit[0].State != index.AuditOK {
		t.Errorf("rows %+v, want one version of %s, and the retry's audit row ok beside the first one's", r, first)
	}
	if n := len(e.pushRefs("1.0.0")); n != 2 {
		t.Errorf("%d pushes of the tag, want the first manifest twice", n)
	}
	e.noClaim(t, "tool", "1.0.0")
	e.noClaim(t, "tool", "")
}

// TestVisibilityTimeoutIngest: a manifest of /v2 that the registry does not serve in time is
// 503, and the client's retry of the same bytes completes it.
func TestVisibilityTimeoutIngest(t *testing.T) {
	e := newEnv(t)
	lag := e.lagging(-1, 100*time.Millisecond)
	built, raw, pkgPath := e.prepare(t, upload("tool", "1.0.0", file{"tool", "linux binary"}))
	_, err := e.ingest(t, built, raw, pkgPath)
	wantNotVisible(t, err)
	lag.Serve()
	res, err := e.ingest(t, built, raw, pkgPath)
	must(t, err)
	if res.Existed || len(e.rows(t).Versions) != 1 {
		t.Errorf("the retry: %+v; want one version, written now", res)
	}
	e.noClaim(t, "tool", "1.0.0")
}
