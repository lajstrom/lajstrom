package providertest

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"slices"
	"strings"
	"testing"

	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/lajstrom/lajstrom/internal/provider"
)

const name = "raw/scratch/tool"

func descriptor(mediaType string, b []byte) ocispec.Descriptor {
	return ocispec.Descriptor{MediaType: mediaType, Digest: digest.FromBytes(b), Size: int64(len(b))}
}

// newRepo returns a new Fake and its repository name.
func newRepo(t *testing.T) (*Fake, provider.Repository) {
	t.Helper()
	f := &Fake{}
	rr, err := f.Repository(t.Context(), name)
	if err != nil {
		t.Fatal(err)
	}
	return f, rr
}

// TestFakeBlobs checks that pushes are verified, that a small blob reads back, and that a blob
// above MaxKept exists but is not kept.
func TestFakeBlobs(t *testing.T) {
	ctx := t.Context()
	f, rr := newRepo(t)
	small := []byte("hello")
	smallDesc := descriptor("application/octet-stream", small)
	if err := rr.Push(ctx, smallDesc, strings.NewReader("hellO")); err == nil {
		t.Error("Push of other bytes than the descriptor's succeeded")
	}
	if err := rr.Push(ctx, smallDesc, bytes.NewReader(small)); err != nil {
		t.Fatal(err)
	}
	if rc, err := rr.Fetch(ctx, smallDesc); err != nil {
		t.Error(err)
	} else if got, _ := io.ReadAll(rc); !bytes.Equal(got, small) {
		t.Errorf("Fetch = %q", got)
	}

	big := ocispec.Descriptor{MediaType: "application/octet-stream", Size: MaxKept + 1}
	d := digest.Canonical.Digester()
	_, _ = io.Copy(d.Hash(), io.LimitReader(zeros{}, big.Size))
	big.Digest = d.Digest()
	if err := rr.Push(ctx, big, io.LimitReader(zeros{}, big.Size)); err != nil {
		t.Fatal(err)
	}
	if ok, err := rr.Exists(ctx, big); !ok || err != nil {
		t.Errorf("Exists(big) = %v, %v", ok, err)
	}
	if _, err := rr.Fetch(ctx, big); !errors.Is(err, provider.ErrNotFound) {
		t.Errorf("Fetch of a discarded blob: %v, want ErrNotFound", err)
	}
	if got := len(f.Pushes()); got != 2 {
		t.Errorf("%d pushes recorded, want 2", got)
	}
	if names, _ := f.ListRepositories(ctx, "raw/"); !slices.Equal(names, []string{name}) {
		t.Errorf("ListRepositories = %v", names)
	}
}

// TestFakeManifests checks tags, referrers, the record of PushReference and Delete.
func TestFakeManifests(t *testing.T) {
	ctx := t.Context()
	f, rr := newRepo(t)
	m, _ := json.Marshal(ocispec.Manifest{MediaType: ocispec.MediaTypeImageManifest})
	mDesc := descriptor(ocispec.MediaTypeImageManifest, m)
	if err := rr.PushReference(ctx, mDesc, bytes.NewReader(m), "1.0.0"); err != nil {
		t.Fatal(err)
	}
	sig, _ := json.Marshal(ocispec.Manifest{MediaType: ocispec.MediaTypeImageManifest, ArtifactType: "application/vnd.test.sig", Subject: &mDesc})
	sigDesc := descriptor(ocispec.MediaTypeImageManifest, sig)
	if err := rr.Push(ctx, sigDesc, bytes.NewReader(sig)); err != nil {
		t.Fatal(err)
	}
	if got, err := rr.Resolve(ctx, "1.0.0"); err != nil || got.Digest != mDesc.Digest {
		t.Errorf("Resolve = %v, %v", got, err)
	}
	var refs []ocispec.Descriptor
	_ = rr.Referrers(ctx, mDesc, "", func(page []ocispec.Descriptor) error { refs = append(refs, page...); return nil })
	if len(refs) != 1 || refs[0].Digest != sigDesc.Digest || refs[0].ArtifactType != "application/vnd.test.sig" {
		t.Errorf("Referrers = %v", refs)
	}
	if got := f.PushReferences(); !slices.Equal(got, []PushRecord{{Name: name, Tag: "1.0.0", Digest: mDesc.Digest, Size: mDesc.Size}}) {
		t.Errorf("PushReferences = %v", got)
	}
	if err := rr.Delete(ctx, mDesc); err != nil {
		t.Fatal(err)
	}
	if _, err := rr.Resolve(ctx, "1.0.0"); !errors.Is(err, provider.ErrNotFound) {
		t.Errorf("Resolve after Delete: %v, want ErrNotFound", err)
	}
}

// TestFakeFaults checks the injected errors.
func TestFakeFaults(t *testing.T) {
	ctx := t.Context()
	f, rr := newRepo(t)
	boom := errors.New("boom")
	f.FailPush, f.FailPushReference, f.FailExists = boom, boom, boom
	b := []byte("x")
	desc := descriptor(ocispec.MediaTypeImageManifest, b)
	for what, err := range map[string]error{
		"Push":          rr.Push(ctx, desc, bytes.NewReader(b)),
		"PushReference": rr.PushReference(ctx, desc, bytes.NewReader(b), "1.0.0"),
		"Exists":        second(rr.Exists(ctx, desc)),
	} {
		if !errors.Is(err, boom) {
			t.Errorf("%s with its Fail field set: %v", what, err)
		}
	}
	if len(f.Pushes()) != 0 || len(f.PushReferences()) != 0 {
		t.Errorf("failed pushes were recorded: %v %v", f.Pushes(), f.PushReferences())
	}
}

func second[T any](_ T, err error) error { return err }

// zeros reads zero bytes forever.
type zeros struct{}

func (zeros) Read(p []byte) (int, error) {
	clear(p)
	return len(p), nil
}
