package raw

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/lajstrom/lajstrom/internal/index"
	"github.com/lajstrom/lajstrom/internal/oci/profile"
	"github.com/lajstrom/lajstrom/internal/provider"
	"github.com/lajstrom/lajstrom/internal/provider/providertest"
)

// TestPutSameBytesAtOnce sends the same bytes to one path in several PUTs at once, each by
// another user, so with manifests of other digests, on a registry whose manifest pushes take a
// while (#1129): one PUT stores the file, 201; each other waits for its claim on the tag and
// answers 200 with the same digest and body; never 409. One version, one manifest, no claim
// left.
func TestPutSameBytesAtOnce(t *testing.T) {
	f := &providertest.Fake{RegistryName: "local"}
	e := newEnv(t, envOptions{registry: slowPush{f}})
	e.prov = f
	const n = 6
	codes, digests := make([]int, n), make([]string, n)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := range n {
		wg.Go(func() {
			<-start
			rr := e.do(http.MethodPut, target(fileTxt), strings.NewReader("the same bytes"), principalHeader, "user"+strconv.Itoa(i))
			codes[i], digests[i] = rr.Code, rr.Header().Get("Docker-Content-Digest")
		})
	}
	close(start)
	wg.Wait()
	created := 0
	for i, code := range codes {
		switch {
		case code == http.StatusCreated:
			created++
		case code != http.StatusOK:
			t.Errorf("PUT %d = %d, want 201 or 200", i, code)
		}
		if digests[i] != digests[0] {
			t.Errorf("PUT %d named %s, PUT 0 %s; want one digest", i, digests[i], digests[0])
		}
	}
	if created != 1 {
		t.Errorf("statuses %v, want one 201", codes)
	}
	if vs, refs := e.versions(fileTxt), e.prov.PushReferences(); len(vs) != 1 || len(refs) != 1 || vs[0].Digest.String() != digests[0] {
		t.Errorf("%d versions, manifest pushes %+v; want one, of %s", len(vs), refs, digests[0])
	}
	tag := profile.Tag(tagOf("the same bytes"), "")
	if c, err := e.st.GetTagClaim(t.Context(), e.repo.ID, fileTxt, tag); !errors.Is(err, index.ErrNotFound) {
		t.Errorf("the claim on %s: %+v, %v; want none", tag, c, err)
	}
}

// TestPutSameBytesWhileFirstConfirms is the interleaving of #1369, made certain: a PUT whose
// manifest the registry serves already, and which has not confirmed it, holds its claim on the
// tag; a PUT of the same bytes meanwhile takes that manifest over and shares the claim. It
// indexes the manifest without pushing it again (201), and the first PUT then finds it indexed
// (200). One manifest push, one version, no claim left.
func TestPutSameBytesWhileFirstConfirms(t *testing.T) {
	f := &providertest.Fake{RegistryName: "local"}
	hold := &holdPush{pushed: make(chan struct{}), release: make(chan struct{})}
	e := newEnv(t, envOptions{registry: holdingPush{f, hold}})
	e.prov = f
	first := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		first <- e.do(http.MethodPut, target(fileTxt), strings.NewReader("the same bytes"), principalHeader, "alice")
	}()
	select {
	case <-hold.pushed:
	case rr := <-first:
		t.Fatalf("the first PUT answered %d before its push: %s", rr.Code, rr.Body)
	}
	second := e.do(http.MethodPut, target(fileTxt), strings.NewReader("the same bytes"), principalHeader, "bob")
	close(hold.release)
	rr := <-first
	d := rr.Header().Get("Docker-Content-Digest")
	if second.Code != http.StatusCreated || rr.Code != http.StatusOK || second.Header().Get("Docker-Content-Digest") != d {
		t.Errorf("the PUT while the first confirms = %d %s, the first = %d %s; want 201 and 200 with one digest",
			second.Code, second.Header().Get("Docker-Content-Digest"), rr.Code, d)
	}
	if vs, refs := e.versions(fileTxt), e.prov.PushReferences(); len(vs) != 1 || len(refs) != 1 || vs[0].Digest.String() != d {
		t.Errorf("%d versions, manifest pushes %+v; want one, of %s", len(vs), refs, d)
	}
	tag := profile.Tag(tagOf("the same bytes"), "")
	if c, err := e.st.GetTagClaim(t.Context(), e.repo.ID, fileTxt, tag); !errors.Is(err, index.ErrNotFound) {
		t.Errorf("the claim on %s: %+v, %v; want none", tag, c, err)
	}
}

// holdingPush is the fake whose first manifest push stores the manifest, then waits on hold:
// the registry serves it, and the PUT that pushed it has not confirmed it yet.
type holdingPush struct {
	*providertest.Fake
	hold *holdPush
}

// holdPush is the hook of holdingPush: pushed is closed once the first push stored its
// manifest, which then waits for release.
type holdPush struct {
	once    sync.Once
	pushed  chan struct{}
	release chan struct{}
}

func (s holdingPush) Repository(ctx context.Context, name string) (provider.Repository, error) {
	rr, err := s.Fake.Repository(ctx, name)
	if err != nil {
		return nil, err
	}
	return holdingRepository{rr, s.hold}, nil
}

type holdingRepository struct {
	provider.Repository
	hold *holdPush
}

func (r holdingRepository) PushReference(ctx context.Context, d ocispec.Descriptor, content io.Reader, reference string) error {
	err := r.Repository.PushReference(ctx, d, content, reference)
	first := false
	r.hold.once.Do(func() { first = true })
	if first {
		close(r.hold.pushed)
		<-r.hold.release
	}
	return err
}

// slowPush is the fake whose manifest pushes take 100 ms, so that the claim of the PUT that
// pushes is held while the others arrive.
type slowPush struct{ *providertest.Fake }

func (s slowPush) Repository(ctx context.Context, name string) (provider.Repository, error) {
	rr, err := s.Fake.Repository(ctx, name)
	if err != nil {
		return nil, err
	}
	return slowRepository{rr}, nil
}

type slowRepository struct{ provider.Repository }

func (r slowRepository) PushReference(ctx context.Context, d ocispec.Descriptor, content io.Reader, reference string) error {
	time.Sleep(100 * time.Millisecond)
	return r.Repository.PushReference(ctx, d, content, reference)
}
