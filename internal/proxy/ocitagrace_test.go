package proxy_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/lajstrom/lajstrom/internal/oci/profile"
	"github.com/lajstrom/lajstrom/internal/provider"
	"github.com/lajstrom/lajstrom/internal/provider/providertest"
	"github.com/lajstrom/lajstrom/internal/proxy"
)

// lateTags is the backend as one replica reaches it: its push of an upstream's tag (not a
// keep-tag) calls before ahead of the push and after once the backend took it, before the
// answer goes back, so that a test makes the push land late or its answer come back late (#879).
// It counts the HEADs of tags.
type lateTags struct {
	*providertest.Fake
	before, after func()
	resolves      *atomic.Int64
}

func (l lateTags) Repository(ctx context.Context, name string) (provider.Repository, error) {
	rr, err := l.Fake.Repository(ctx, name)
	return lateRepo{rr, l}, err
}

type lateRepo struct {
	provider.Repository
	l lateTags
}

func (r lateRepo) PushReference(ctx context.Context, desc ocispec.Descriptor, content io.Reader, ref string) error {
	if profile.IsKeepTag(ref) {
		return r.Repository.PushReference(ctx, desc, content, ref)
	}
	if r.l.before != nil {
		r.l.before()
	}
	err := r.Repository.PushReference(ctx, desc, content, ref)
	if r.l.after != nil {
		r.l.after()
	}
	return err
}

func (r lateRepo) Resolve(ctx context.Context, ref string) (ocispec.Descriptor, error) {
	if _, err := digest.Parse(ref); err != nil && !profile.IsKeepTag(ref) && r.l.resolves != nil {
		r.l.resolves.Add(1)
	}
	return r.Repository.Resolve(ctx, ref)
}

// pullE is pull for another goroutine than the test's: the first error.
func pullE(ctx context.Context, e *ociEnv, c *proxy.Core, path, tag string, img image, i int) error {
	if _, _, err := c.OCIManifest(ctx, e.repo, path, tag); err != nil {
		return err
	}
	if _, _, err := c.OCIManifest(ctx, e.repo, path, img.manifests[i].Digest.String()); err != nil {
		return err
	}
	for _, b := range img.blobs[i] {
		if _, _, err := c.OCIBlob(ctx, e.repo, path, b.Digest, true, false); err != nil {
			return err
		}
	}
	return nil
}

// tagRow reads the oci.tag row of tag at path.
func (e *ociEnv) tagRow(t *testing.T, path, tag string) (d digest.Digest, pushed bool) {
	t.Helper()
	r := e.row(t, path, "oci.tag:"+tag)
	if r == nil {
		return "", false
	}
	var row struct {
		Digest digest.Digest `json:"digest"`
		Pushed bool          `json:"pushed"`
	}
	must(t, json.Unmarshal(r.Content, &row))
	return row.Digest, row.Pushed
}

// eventually waits up to 5 s for ok, which a push in the background makes true.
func eventually(t *testing.T, what string, ok func() bool) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); !ok(); {
		if time.Now().After(deadline) {
			t.Fatalf("after 5s: %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// tagRace is the race of #879 on two replicas of one index: the upstream's tag names first, a
// client pulls it through a, whose push of the tag waits in the backend's hook; then the
// upstream moves the tag to second and a client pulls it through b. A's push is let go once b's
// push landed (b's answer then held, with holdB), or after 200 ms when b waits for the tag's
// lock.
func tagRace(t *testing.T, e *ociEnv, holdB bool) (first, second image) {
	t.Helper()
	ctx := t.Context()
	arrivedA, releaseA, releaseB := make(chan struct{}), make(chan struct{}), make(chan struct{})
	landedB := make(chan struct{}, 1)
	var onceA sync.Once
	a := e.coreOn(lateTags{Fake: e.reg, before: func() { onceA.Do(func() { close(arrivedA); <-releaseA }) }}, "a", proxy.Options{})
	b := e.coreOn(lateTags{Fake: e.reg, after: func() {
		select {
		case landedB <- struct{}{}:
		default:
		}
		if holdB {
			<-releaseB
		}
	}}, "b", proxy.Options{})
	const path = "library/alpine"
	first = e.hub.image(t, path, "latest", "arm64")
	errA, errB := make(chan error, 1), make(chan error, 1)
	go func() { errA <- pullE(ctx, e, a, path, "latest", first, 0) }()
	select {
	case <-arrivedA:
	case err := <-errA:
		t.Fatalf("a's pull ended before its tag push: %v", err)
	}
	second = e.hub.image(t, path, "latest", "amd64")
	e.advance(11 * time.Minute)
	go func() { errB <- pullE(ctx, e, b, path, "latest", second, 0) }()
	select {
	case <-landedB:
	case <-time.After(200 * time.Millisecond):
	}
	close(releaseA)
	must(t, <-errA)
	close(releaseB)
	must(t, <-errB)
	return first, second
}

// TestOCITagRaceLatePush: replica a's push of the older index lands after b pushed the newer
// one. Without a lock on the tag, the backend's tag and the row went back to the older index,
// and with the upstream down both replicas served it after a client got the newer one (#879).
// Under the lock, b pushes after a, a records nothing over the row b's revalidation wrote, and
// the newer index stays in both.
func TestOCITagRaceLatePush(t *testing.T) {
	e := newOCIEnv(t, ociSettings)
	first, second := tagRace(t, e, false)
	const path = "library/alpine"
	if got := e.tagged(t, path, "latest"); got != second.index.Digest {
		t.Errorf("the backend's latest = %s, want the second index %s (the first is %s)", got, second.index.Digest, first.index.Digest)
	}
	if d, pushed := e.tagRow(t, path, "latest"); d != second.index.Digest || !pushed {
		t.Errorf("the row of latest = %s, pushed %v; want the second index, pushed", d, pushed)
	}
	e.hub.down.Store(true)
	e.advance(11 * time.Minute)
	for _, replica := range []string{"a", "b"} {
		desc, _, err := e.core(replica, proxy.Options{}).OCIManifest(t.Context(), e.repo, path, "latest")
		if err != nil || desc.Digest != second.index.Digest {
			t.Errorf("replica %s, the upstream down: latest = %s, %v; want the second index", replica, desc.Digest, err)
		}
	}
}

// TestOCITagRaceLateAnswer: a's push of the older index lands after b's, and b's answer comes
// back after a's push landed. Without a lock, the backend's tag stayed on the older index while
// the row said the newer one, pushed, and no read pushed it again (#879). Under the lock, b's
// push waits for a's, and the backend's tag is the newer index.
func TestOCITagRaceLateAnswer(t *testing.T) {
	e := newOCIEnv(t, ociSettings)
	_, second := tagRace(t, e, true)
	const path = "library/alpine"
	if d, pushed := e.tagRow(t, path, "latest"); d != second.index.Digest || !pushed {
		t.Errorf("the row of latest = %s, pushed %v; want the second index, pushed", d, pushed)
	}
	e.advance(11 * time.Minute)
	for _, replica := range []string{"a", "b"} {
		if _, _, err := e.core(replica, proxy.Options{}).OCIManifest(t.Context(), e.repo, path, "latest"); err != nil {
			t.Fatalf("replica %s: %v", replica, err)
		}
	}
	if got := e.tagged(t, path, "latest"); got != second.index.Digest {
		t.Errorf("the backend's latest = %s after reads through both replicas, want the second index %s", got, second.index.Digest)
	}
}

// TestOCITagRepair: a row marked pushed whose tag the backend names otherwise (a push that
// landed after its replica lost the lock, a replica that died between push and record) is
// pushed again by the first revalidation in a later window of ten minutes, with one HEAD of the
// backend's tag; a revalidation in the same window asks the backend nothing.
func TestOCITagRepair(t *testing.T) {
	e := newOCIEnv(t, `{"name": "hub", "metadataTTL": "0s"}`)
	const path = "library/alpine"
	img := e.hub.image(t, path, "latest", "arm64")
	resolves := new(atomic.Int64)
	c := e.coreOn(lateTags{Fake: e.reg, resolves: resolves}, "a", proxy.Options{})
	pull(t, e, c, path, "latest", img, 0)
	if d, pushed := e.tagRow(t, path, "latest"); d != img.index.Digest || !pushed {
		t.Fatalf("the row of latest = %s, pushed %v; want the index, pushed", d, pushed)
	}
	rr, err := e.reg.Repository(t.Context(), "oci/hub/"+path)
	must(t, err)
	must(t, rr.PushReference(t.Context(), img.manifests[0], bytes.NewReader(mustRaw(t, e, path, img.manifests[0])), "latest"))

	e.advance(time.Minute) // the same window as the row's checked_at
	before := resolves.Load()
	_, _, err = c.OCIManifest(t.Context(), e.repo, path, "latest")
	must(t, err)
	time.Sleep(50 * time.Millisecond)
	if n := resolves.Load() - before; n != 0 || e.tagged(t, path, "latest") != img.manifests[0].Digest {
		t.Fatalf("a revalidation in the row's window: %d HEADs of the backend's tag, latest = %s; want none, unchanged", n,
			e.tagged(t, path, "latest"))
	}
	e.advance(10 * time.Minute)
	_, _, err = c.OCIManifest(t.Context(), e.repo, path, "latest")
	must(t, err)
	eventually(t, "the backend's latest is not the index again", func() bool { return e.tagged(t, path, "latest") == img.index.Digest })
}

// TestOCITagPushAfterAnswer: the read of a tag whose push is not done (a completion lost to a
// concurrent write) is answered at once, and the tag is pushed in the background: a slow push
// of the backend's tag no longer adds its time to the client's request (#879).
func TestOCITagPushAfterAnswer(t *testing.T) {
	e := newOCIEnv(t, ociSettings)
	const path = "library/alpine"
	img := e.hub.image(t, path, "latest", "arm64")
	pull(t, e, e.core("a", proxy.Options{}), path, "latest", img, 0)
	rr, err := e.reg.Repository(t.Context(), "oci/hub/"+path)
	must(t, err)
	must(t, rr.PushReference(t.Context(), img.manifests[0], bytes.NewReader(mustRaw(t, e, path, img.manifests[0])), "latest"))
	row := e.row(t, path, "oci.tag:latest")
	row.Content = []byte(`{"digest":"` + img.index.Digest.String() + `","mediaType":"` + img.index.MediaType + `","size":` +
		strconv.FormatInt(img.index.Size, 10) + `}`)
	must(t, e.st.PutProxyMeta(t.Context(), row))

	arrived, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	c := e.coreOn(lateTags{Fake: e.reg, before: func() { once.Do(func() { close(arrived); <-release }) }}, "b", proxy.Options{})
	answered := make(chan error, 1)
	go func() {
		_, _, err := c.OCIManifest(t.Context(), e.repo, path, "latest")
		answered <- err
	}()
	select {
	case err := <-answered:
		must(t, err)
	case <-time.After(2 * time.Second):
		close(release)
		t.Fatal("the read of the tag waited for the push of the backend's tag")
	}
	<-arrived
	close(release)
	eventually(t, "the tag is not pushed after the answer", func() bool {
		_, pushed := e.tagRow(t, path, "latest")
		return pushed && e.tagged(t, path, "latest") == img.index.Digest
	})
}
