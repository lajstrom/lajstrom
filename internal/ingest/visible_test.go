package ingest_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

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

// TestNotVisibleNames: the error of a push the registry does not serve in time names what it
// does not serve, by what was pushed: the manifest by its digest, its tag and its blobs, sorted,
// in the order of Missing; nothing of the registry's own.
func TestNotVisibleNames(t *testing.T) {
	ctx := t.Context()
	e := newEnv(t)
	lag := &blobsLost{Lag: &providertest.Lag{Fake: e.prov, Manifests: -1}}
	o := e.options(lag)
	o.VisibilityTimeout = 100 * time.Millisecond
	e.pipe = ingest.New(o)
	_, published := e.pipe.Upload(ctx, e.repo, upload("tool", "1.0.0", file{"tool", "linux binary"}), alice)
	wantNotVisible(t, published)
	var nv *ingest.NotVisibleError
	if !errors.As(published, &nv) {
		t.Fatalf("error = %v, want an *ingest.NotVisibleError", published)
	}
	refs := e.pushRefs("1.0.0")
	if len(refs) != 1 {
		t.Fatalf("pushes %v, want one", refs)
	}
	lag.Serve()
	rr, err := e.prov.Repository(ctx, nv.Name)
	must(t, err)
	rc, err := rr.Fetch(ctx, ocispec.Descriptor{MediaType: ocispec.MediaTypeImageManifest, Digest: refs[0].Digest})
	must(t, err)
	var m ocispec.Manifest
	must(t, json.NewDecoder(rc).Decode(&m))
	_ = rc.Close()
	blobs := []digest.Digest{m.Config.Digest}
	for _, l := range m.Layers {
		blobs = append(blobs, l.Digest)
	}
	slices.Sort(blobs)
	want := &ingest.NotVisibleError{Name: "raw/scratch/tool", Digest: refs[0].Digest, Manifest: true, Tag: "1.0.0",
		Blobs: slices.Compact(blobs), Waited: 100 * time.Millisecond}
	if nv.Name != want.Name || nv.Digest != want.Digest || !nv.Manifest || nv.Tag != want.Tag ||
		!slices.Equal(nv.Blobs, want.Blobs) || nv.Subject != "" || nv.Waited != want.Waited {
		t.Errorf("NotVisibleError = %+v, want %+v", nv, want)
	}
	missing := []string{"the manifest", "the tag 1.0.0"}
	for _, b := range want.Blobs {
		missing = append(missing, "the blob "+b.String())
	}
	if got := nv.Missing(); !slices.Equal(got, missing) {
		t.Errorf("Missing = %q, want %q", got, missing)
	}
	if !strings.Contains(published.Error(), strings.Join(missing, ", ")) {
		t.Errorf("error %q does not name %q", published, missing)
	}
	// The frontends' answer names it too, after the status text (#667).
	if got, want := format.ClientMessage(published), "Service Unavailable; "+nv.Explain(); got != want {
		t.Errorf("ClientMessage = %q, want %q", got, want)
	}
	// One made outside the package, as a frontend's test fakes it, is the same 503.
	wantNotVisible(t, &ingest.NotVisibleError{Name: "raw/scratch/tool", Digest: refs[0].Digest, Tag: "1.0.0"})
}

// TestNotVisibleExplain: the words of a push the registry does not serve yet, for the
// frontends' 503 (#667), name the OCI name, the manifest's digest and what is missing, at most
// 20 blobs and a count of the rest, and nothing of the registry's.
func TestNotVisibleExplain(t *testing.T) {
	m := digest.FromString("manifest")
	blobs := make([]digest.Digest, 23)
	for i := range blobs {
		blobs[i] = digest.FromString(string(rune('a' + i)))
	}
	slices.Sort(blobs)
	named := func(ds []digest.Digest) string {
		var s []string
		for _, d := range ds {
			s = append(s, "the blob "+d.String())
		}
		return strings.Join(s, ", ")
	}
	const prefix, suffix = "the registry took raw/scratch/tool@", " yet; the same request again completes it"
	tests := []struct {
		name string
		nv   ingest.NotVisibleError
		want string // between prefix+digest+" and does not serve " and suffix
	}{
		{"the manifest", ingest.NotVisibleError{Manifest: true}, "the manifest"},
		{"the tag and a blob", ingest.NotVisibleError{Tag: "1.0.0", Blobs: blobs[:1]}, "the tag 1.0.0, " + named(blobs[:1])},
		{"20 blobs", ingest.NotVisibleError{Blobs: blobs[:20]}, named(blobs[:20])},
		{"21 blobs", ingest.NotVisibleError{Blobs: blobs[:21]}, named(blobs[:20]) + ", 1 more blob"},
		{"a referrer and 23 blobs", ingest.NotVisibleError{Blobs: blobs, Subject: m},
			named(blobs[:20]) + ", 3 more blobs, the referrers of " + m.String()},
		{"nothing named", ingest.NotVisibleError{}, "it"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			nv := tt.nv
			nv.Name, nv.Digest, nv.Waited = "raw/scratch/tool", m, 30*time.Second
			want := prefix + m.String() + " and does not serve " + tt.want + suffix
			if got := nv.Explain(); got != want {
				t.Errorf("Explain =\n %q, want\n %q", got, want)
			}
			if len(nv.Blobs) != len(tt.nv.Blobs) {
				t.Errorf("Explain changed the error's blobs")
			}
		})
	}
	var _ format.Explainer = (*ingest.NotVisibleError)(nil)
}

// blobsLost is a lagging registry that stops serving blobs once a manifest is pushed: the checks
// of the blobs before the push see them, the visibility barrier after it does not.
type blobsLost struct {
	*providertest.Lag
	pushed atomic.Bool
}

func (b *blobsLost) Repository(ctx context.Context, name string) (provider.Repository, error) {
	rr, err := b.Lag.Repository(ctx, name)
	return lostRepository{Repository: rr, b: b}, err
}

// lostRepository is a repository of blobsLost.
type lostRepository struct {
	provider.Repository
	b *blobsLost
}

func (r lostRepository) PushReference(ctx context.Context, desc ocispec.Descriptor, content io.Reader, ref string) error {
	defer r.b.pushed.Store(true)
	return r.Repository.PushReference(ctx, desc, content, ref)
}

func (r lostRepository) Exists(ctx context.Context, desc ocispec.Descriptor) (bool, error) {
	if r.b.pushed.Load() && desc.MediaType != ocispec.MediaTypeImageManifest {
		return false, nil
	}
	return r.Repository.Exists(ctx, desc)
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
