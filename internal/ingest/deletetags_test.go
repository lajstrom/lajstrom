package ingest_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
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

// ociRepo creates the repo oci/images on the env's registry.
func ociRepo(t *testing.T, e *env) index.Repo {
	t.Helper()
	repo := index.Repo{Format: "oci", Name: "images", Registry: "local", Prefix: "oci/images", Kind: "hosted"}
	must(t, e.st.CreateRepo(t.Context(), &repo))
	return repo
}

// pushImage pushes a manifest of oci/images/app made of what to the fake, under each of tags.
func pushImage(t *testing.T, e *env, what string, tags ...string) ocispec.Descriptor {
	t.Helper()
	rr, err := e.prov.Repository(t.Context(), "oci/images/app")
	must(t, err)
	b := []byte(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json","config":` +
		`{"mediaType":"application/vnd.oci.empty.v1+json","digest":"` + ocispec.DescriptorEmptyJSON.Digest.String() + `","size":2},` +
		`"layers":[],"annotations":{"test":"` + what + `"}}`)
	desc := ocispec.Descriptor{MediaType: ocispec.MediaTypeImageManifest, Digest: digest.FromBytes(b), Size: int64(len(b))}
	for _, tag := range tags {
		must(t, rr.PushReference(t.Context(), desc, bytes.NewReader(b), tag))
	}
	return desc
}

// TestDeleteTagged: on an oci repo a delete by tag of a manifest that another tag also names is
// index.ErrConflict naming that tag, and nothing changes; by digest the manifest goes with both
// tags. The other manifests' tags, more than resolve at once, are not counted. Each delete holds
// one lock, the manifest's, with the referrers API or without.
func TestDeleteTagged(t *testing.T) {
	for _, api := range []bool{true, false} {
		e, rec, _ := referrerEnv(t, api)
		repo := ociRepo(t, e)
		img := pushImage(t, e, "image", "v1", "latest")
		for i := range 12 {
			pushImage(t, e, fmt.Sprint("other ", i), fmt.Sprint("o", i))
		}
		before := e.rows(t)
		rec.locks = nil
		_, err := e.pipe.Delete(t.Context(), repo, format.Deletion{Path: "app", Reference: "v1"}, alice)
		var tagged interface{ OtherTags() []string }
		if !errors.Is(err, index.ErrConflict) || !errors.As(err, &tagged) {
			t.Fatalf("api %t: Delete by tag = %v, want ErrConflict with the other tags", api, err)
		}
		same(t, "other tags", []string{"latest"}, tagged.OtherTags())
		same(t, "rows after the refusal", before, e.rows(t))
		for _, tag := range []string{"v1", "latest"} {
			if got := e.tagged(t, "oci/images/app", tag); got != img.Digest {
				t.Errorf("api %t: after the refusal %s names %q", api, tag, got)
			}
		}
		got, err := e.pipe.Delete(t.Context(), repo, format.Deletion{Path: "app", Reference: img.Digest.String()}, alice)
		must(t, err)
		if got.Digest != img.Digest || e.tagged(t, "oci/images/app", "v1") != "" || e.tagged(t, "oci/images/app", "latest") != "" {
			t.Errorf("api %t: the delete by digest left a tag: %+v", api, got)
		}
		if e.tagged(t, "oci/images/app", "o0") == "" {
			t.Errorf("api %t: the delete took another manifest's tag", api)
		}
		key := "oci/images/app@" + img.Digest.String()
		same(t, "locks", []string{key, key}, rec.locks)
	}
}

// stopper is the fake whose repositories stop in the first call of stopAt, "tags" or "push", until
// open is closed, having closed in; they log the pushes by tag and the deletes.
type stopper struct {
	*providertest.Fake
	stopAt   string
	in, open chan struct{}
	once     sync.Once
	mu       sync.Mutex
	log      []string
}

func (g *stopper) Repository(ctx context.Context, name string) (provider.Repository, error) {
	rr, err := g.Fake.Repository(ctx, name)
	return stopped{rr, g}, err
}

func (g *stopper) at(call, event string) {
	if call == g.stopAt {
		g.once.Do(func() { close(g.in); <-g.open })
	}
	if event != "" {
		g.mu.Lock()
		g.log = append(g.log, event)
		g.mu.Unlock()
	}
}

type stopped struct {
	provider.Repository
	g *stopper
}

func (r stopped) Tags(ctx context.Context, last string, fn func([]string) error) error {
	r.g.at("tags", "")
	return r.Repository.Tags(ctx, last, fn)
}

func (r stopped) PushReference(ctx context.Context, d ocispec.Descriptor, c io.Reader, tag string) error {
	r.g.at("push", "push "+tag)
	return r.Repository.PushReference(ctx, d, c, tag)
}

func (r stopped) Delete(ctx context.Context, d ocispec.Descriptor) error {
	r.g.at("", "delete "+d.Digest.String())
	return r.Repository.Delete(ctx, d)
}

// arrivals is the store that closes second when a second caller asks for a referrers lock.
type arrivals struct {
	index.Store
	n      atomic.Int32
	second chan struct{}
}

func (s *arrivals) WithReferrersLock(ctx context.Context, name string, subject digest.Digest, fn func(context.Context) error) error {
	if s.n.Add(1) == 2 {
		close(s.second)
	}
	return s.Store.WithReferrersLock(ctx, name, subject, fn)
}

// waitFor fails the test when ch is not closed within ten seconds.
func waitFor(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(10 * time.Second):
		t.Fatalf("%s: not within ten seconds", what)
	}
}

// TestTagPushAndDelete: a push by tag and a delete of the same manifest never interleave, on a
// backend with the referrers API, where the tag lock alone orders them. The manifest is a
// referrer of an oci repo, pushed by tag through PutReferrer. A push that comes while the
// delete counts the tags waits and pushes after the delete; a delete that comes while the push
// runs waits and counts the pushed tag, and refuses.
func TestTagPushAndDelete(t *testing.T) {
	for _, first := range []string{"tags", "push"} {
		e := newEnv(t)
		repo := ociRepo(t, e)
		g := &stopper{Fake: e.prov, stopAt: first, in: make(chan struct{}), open: make(chan struct{})}
		st := &arrivals{Store: e.st, second: make(chan struct{})}
		opts := e.options(g)
		opts.Index = st
		e.pipe = ingest.New(opts)
		r := signature(t, ocispec.Descriptor{MediaType: ocispec.MediaTypeImageManifest, Digest: digest.FromString("image"), Size: 2}, "sig")
		r.Path = "app"
		rr, err := e.prov.Repository(t.Context(), "oci/images/app")
		must(t, err)
		must(t, rr.PushReference(t.Context(), r.Desc, bytes.NewReader(r.Manifest), "sig-1"))
		deleted, pushed := make(chan error, 1), make(chan error, 1)
		del := func() {
			_, err := e.pipe.Delete(context.WithoutCancel(t.Context()), repo, format.Deletion{Path: "app", Reference: "sig-1"}, alice)
			deleted <- err
		}
		push := func() {
			ref := r
			ref.Tag = "sig-2"
			_, err := e.pipe.PutReferrer(context.WithoutCancel(t.Context()), repo, ref, alice)
			pushed <- err
		}
		start, then, thenDone := del, push, pushed
		if first == "push" {
			start, then, thenDone = push, del, deleted
		}
		go start()
		waitFor(t, g.in, first+" of the first caller")
		go then()
		select {
		case err := <-thenDone:
			t.Fatalf("%s first: the second caller ran while the first held the lock: %v", first, err)
		case <-st.second:
		}
		close(g.open)
		errDel, errPush := <-deleted, <-pushed
		must(t, errPush)
		if first == "tags" {
			must(t, errDel)
			same(t, "the order", []string{"delete " + r.Desc.Digest.String(), "push sig-2"}, g.log)
			if e.tagged(t, "oci/images/app", "sig-1") != "" || e.tagged(t, "oci/images/app", "sig-2") != r.Desc.Digest {
				t.Errorf("tags first: sig-1 is left, or sig-2 is not pushed after the delete")
			}
			continue
		}
		if !errors.Is(errDel, index.ErrConflict) {
			t.Errorf("push first: Delete = %v, want ErrConflict naming sig-2", errDel)
		}
		same(t, "the order", []string{"push sig-2"}, g.log)
	}
}
