package providertest

import (
	"bytes"
	"crypto/sha512"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"oras.land/oras-go/v2/content"

	"github.com/lajstrom/lajstrom/internal/provider"
)

// TestFakeUploadChunks drives a session through start, chunks, status and finish, and checks
// that the blob is stored and the session ends.
func TestFakeUploadChunks(t *testing.T) {
	ctx := t.Context()
	f, rr := newRepo(t)
	u := fakeChunks(t, rr)
	want := []byte("hello chunked short" + "world")
	d := digest.FromBytes(want)
	if f.OpenUploads() != 1 {
		t.Errorf("OpenUploads = %d, want 1", f.OpenUploads())
	}
	desc, err := rr.FinishBlobUpload(ctx, u, d, strings.NewReader("world"), 5)
	if err != nil {
		t.Fatal(err)
	}
	if desc.Digest != d || desc.Size != int64(len(want)) {
		t.Errorf("FinishBlobUpload = %+v", desc)
	}
	if f.OpenUploads() != 0 {
		t.Errorf("OpenUploads = %d after the finish, want 0", f.OpenUploads())
	}
	rc, err := rr.Fetch(ctx, ocispec.Descriptor{Digest: d, Size: -1})
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := io.ReadAll(rc); !bytes.Equal(got, want) {
		t.Errorf("Fetch = %q, want %q", got, want)
	}
	if _, err := rr.AppendBlobChunk(ctx, u, strings.NewReader("more"), 4); !errors.Is(err, provider.ErrNotFound) {
		t.Errorf("chunk after the finish: %v, want ErrNotFound", err)
	}
}

// fakeChunks starts a session of rr and appends "hello ", "chunked " and a chunk that says
// 9 bytes and has 5 ("short"), checking each step; the session holds the 19 bytes.
func fakeChunks(t *testing.T, rr provider.Repository) provider.Upload {
	t.Helper()
	ctx := t.Context()
	u, err := rr.StartBlobUpload(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if u.ID == "" || !strings.HasSuffix(u.Location, "/blobs/uploads/"+u.ID) || u.Offset != 0 {
		t.Fatalf("StartBlobUpload = %+v", u)
	}
	if u, err = rr.AppendBlobChunk(ctx, u, strings.NewReader("hello "), 6); err != nil || u.Offset != 6 {
		t.Fatalf("first chunk: %+v, %v", u, err)
	}
	if u, err = rr.AppendBlobChunk(ctx, u, strings.NewReader("chunked "), -1); err != nil || u.Offset != 14 {
		t.Fatalf("chunk of unknown size: %+v, %v", u, err)
	}
	if got, err := rr.BlobUploadStatus(ctx, provider.Upload{Location: u.Location, ID: u.ID}); err != nil || got.Offset != 14 {
		t.Fatalf("BlobUploadStatus = %+v, %v", got, err)
	}
	if _, err := rr.AppendBlobChunk(ctx, provider.Upload{ID: u.ID, Location: u.Location, Offset: 6}, strings.NewReader("x"), 1); err == nil {
		t.Error("a chunk at an offset the session does not hold was accepted")
	}
	if _, err := rr.AppendBlobChunk(ctx, u, strings.NewReader("short"), 9); err == nil {
		t.Error("a chunk shorter than its size was accepted")
	}
	if u, err = rr.BlobUploadStatus(ctx, u); err != nil || u.Offset != 19 {
		t.Fatalf("BlobUploadStatus after the short chunk = %+v, %v", u, err)
	}
	return u
}

// TestFakeUploadFinish covers a finish without a body, with another algorithm, with a wrong
// digest (the session stays for the caller to cancel) and the cancel.
func TestFakeUploadFinish(t *testing.T) {
	ctx := t.Context()
	f, rr := newRepo(t)
	u, _ := rr.StartBlobUpload(ctx)
	if _, err := rr.FinishBlobUpload(ctx, u, digest.FromString("other"), strings.NewReader("abc"), 3); !errors.Is(err, content.ErrMismatchedDigest) {
		t.Errorf("wrong digest: %v, want ErrMismatchedDigest", err)
	}
	if f.OpenUploads() != 1 {
		t.Errorf("OpenUploads = %d after a wrong digest, want 1", f.OpenUploads())
	}
	if err := rr.CancelBlobUpload(ctx, u); err != nil {
		t.Fatal(err)
	}
	if err := rr.CancelBlobUpload(ctx, u); !errors.Is(err, provider.ErrNotFound) {
		t.Errorf("second cancel: %v, want ErrNotFound", err)
	}

	u, _ = rr.StartBlobUpload(ctx)
	if u, _ = rr.AppendBlobChunk(ctx, u, strings.NewReader("sha512 bytes"), -1); u.Offset != 12 {
		t.Fatalf("offset %d", u.Offset)
	}
	sum := sha512.Sum512([]byte("sha512 bytes"))
	d := digest.NewDigestFromBytes(digest.SHA512, sum[:])
	if _, err := rr.FinishBlobUpload(ctx, u, d, nil, 0); err != nil {
		t.Fatalf("finish with a sha512 digest and no body: %v", err)
	}
	if ok, _ := rr.Exists(ctx, ocispec.Descriptor{Digest: d}); !ok {
		t.Error("the sha512 blob does not exist")
	}

	u, _ = rr.StartBlobUpload(ctx)
	if _, err := rr.FinishBlobUpload(ctx, u, digest.FromBytes(nil), nil, 0); err != nil {
		t.Fatalf("empty blob: %v", err)
	}
	rc, err := rr.Fetch(ctx, ocispec.Descriptor{Digest: digest.FromBytes(nil), Size: -1})
	if err != nil {
		t.Fatalf("Fetch of the empty blob: %v", err)
	}
	if n, _ := rc.(io.Seeker).Seek(0, io.SeekEnd); n != 0 {
		t.Errorf("empty blob size %d", n)
	}
}

// TestFakeUploadLarge checks that a session above MaxKept is digested but not kept, and that a
// body that fails leaves its bytes in the session.
func TestFakeUploadLarge(t *testing.T) {
	ctx := t.Context()
	f, rr := newRepo(t)
	size := int64(MaxKept + 3)
	h := digest.SHA256.Digester()
	_, _ = io.Copy(h.Hash(), io.LimitReader(zeros{}, size))
	u, _ := rr.StartBlobUpload(ctx)
	if u, _ = rr.AppendBlobChunk(ctx, u, io.LimitReader(zeros{}, MaxKept-1), MaxKept-1); u.Offset != MaxKept-1 {
		t.Fatalf("offset %d", u.Offset)
	}
	desc, err := rr.FinishBlobUpload(ctx, u, h.Digest(), io.LimitReader(zeros{}, 4), 4)
	if err != nil || desc.Size != size {
		t.Fatalf("FinishBlobUpload = %+v, %v", desc, err)
	}
	if ok, _ := rr.Exists(ctx, desc); !ok {
		t.Error("the large blob does not exist")
	}
	if _, err := rr.Fetch(ctx, desc); !errors.Is(err, provider.ErrNotFound) {
		t.Errorf("Fetch of a discarded blob: %v, want ErrNotFound", err)
	}

	u, _ = rr.StartBlobUpload(ctx)
	_, err = rr.FinishBlobUpload(ctx, u, digest.FromString("x"), io.MultiReader(strings.NewReader("abc"), failing{}), 10)
	if err == nil {
		t.Fatal("a body that fails finished the upload")
	}
	if got, _ := rr.BlobUploadStatus(ctx, u); got.Offset != 3 {
		t.Errorf("offset after a failed body %d, want 3", got.Offset)
	}
	if f.OpenUploads() != 1 {
		t.Errorf("OpenUploads = %d, want 1", f.OpenUploads())
	}
}

// TestFakeMount mounts a blob from another repository and falls back to a session.
func TestFakeMount(t *testing.T) {
	ctx := t.Context()
	f, rr := newRepo(t)
	b := []byte("mount me")
	if err := rr.Push(ctx, descriptor("application/octet-stream", b), bytes.NewReader(b)); err != nil {
		t.Fatal(err)
	}
	other, _ := f.Repository(ctx, "raw/scratch/other")
	mounted, u, err := other.MountBlob(ctx, digest.FromBytes(b), name)
	if err != nil || !mounted || u != (provider.Upload{}) {
		t.Fatalf("MountBlob = %v, %+v, %v", mounted, u, err)
	}
	if ok, _ := other.Exists(ctx, descriptor("application/octet-stream", b)); !ok {
		t.Error("the mounted blob does not exist")
	}
	for _, from := range []string{"", "raw/scratch/none", name} {
		mounted, u, err := other.MountBlob(ctx, digest.FromString("absent"), from)
		if err != nil || mounted || u.ID == "" {
			t.Errorf("MountBlob of an absent blob from %q = %v, %+v, %v; want a session", from, mounted, u, err)
		}
	}
	if _, err := rr.BlobUploadStatus(ctx, provider.Upload{ID: "upload-2"}); !errors.Is(err, provider.ErrNotFound) {
		t.Errorf("status of another repository's session: %v, want ErrNotFound", err)
	}
}

// failing is a reader that always fails.
type failing struct{}

func (failing) Read([]byte) (int, error) { return 0, errors.New("connection reset") }
