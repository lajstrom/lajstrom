//go:build integration

package generic

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"oras.land/oras-go/v2/content"

	"github.com/lajstrom/lajstrom/internal/provider"
)

// TestIntegrationUploads drives the BlobUploader against the registry of
// LAJSTROM_TEST_REGISTRY: a session that a second provider (another replica, with a token
// cache of its own) continues, the monolithic shape, a last chunk of unknown size, a wrong
// digest, a chunk out of order, the cancel, the mount, the empty blob, and the fetch of a blob
// by its digest alone.
func TestIntegrationUploads(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	pa, cfg := integrationProvider(ctx, t)
	pb, _ := integrationProvider(ctx, t)
	t.Logf("uploads against %s", cfg.URL)
	name := "raw/it-" + randomHex(t) + "/uploads"
	a, err := pa.Repository(ctx, name)
	if err != nil {
		t.Fatal(err)
	}
	b, err := pb.Repository(ctx, name)
	if err != nil {
		t.Fatal(err)
	}
	blob := []byte("chunk one chunk two end")
	d := digest.FromBytes(blob)

	step(t, "session across providers", func(t *testing.T) {
		u, err := a.StartBlobUpload(ctx)
		if err != nil || u.Location == "" || u.ID == "" || u.Offset != 0 {
			t.Fatalf("StartBlobUpload = %+v, %v", u, err)
		}
		if u, err = b.AppendBlobChunk(ctx, u, strings.NewReader("chunk one "), 10); err != nil || u.Offset != 10 {
			t.Fatalf("chunk on the second provider = %+v, %v", u, err)
		}
		if u, err = b.AppendBlobChunk(ctx, u, io.MultiReader(strings.NewReader("chunk two ")), -1); err != nil || u.Offset != 20 {
			t.Fatalf("chunk of unknown size = %+v, %v", u, err)
		}
		if got, err := b.BlobUploadStatus(ctx, u); err != nil || got.Offset != 20 {
			t.Fatalf("BlobUploadStatus = %+v, %v", got, err)
		}
		desc, err := a.FinishBlobUpload(ctx, u, d, strings.NewReader("end"), 3)
		if err != nil || desc.Digest != d || desc.Size != int64(len(blob)) {
			t.Fatalf("FinishBlobUpload = %+v, %v", desc, err)
		}
		wantBlob(ctx, t, a, blob)
	})
	step(t, "fetch by digest alone", func(t *testing.T) {
		rc, err := b.Fetch(ctx, ocispec.Descriptor{MediaType: octetStream, Digest: d, Size: -1})
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = rc.Close() }()
		if n, err := rc.(io.Seeker).Seek(0, io.SeekEnd); err != nil || n != int64(len(blob)) {
			t.Fatalf("size %d, %v", n, err)
		}
		if _, err := rc.(io.Seeker).Seek(0, io.SeekStart); err != nil {
			t.Fatal(err)
		}
		if got, _ := io.ReadAll(rc); !bytes.Equal(got, blob) {
			t.Errorf("content %q", got)
		}
	})
	step(t, "monolithic", func(t *testing.T) {
		mono := []byte("one PUT " + randomHex(t))
		u, err := a.StartBlobUpload(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := b.FinishBlobUpload(ctx, u, digest.FromBytes(mono), bytes.NewReader(mono), int64(len(mono))); err != nil {
			t.Fatal(err)
		}
		wantBlob(ctx, t, a, mono)
	})
	step(t, "last chunk of unknown size", func(t *testing.T) {
		data := []byte("known, then unknown " + randomHex(t))
		u, _ := a.StartBlobUpload(ctx)
		u, err := a.AppendBlobChunk(ctx, u, bytes.NewReader(data[:7]), 7)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := b.FinishBlobUpload(ctx, u, digest.FromBytes(data), io.MultiReader(bytes.NewReader(data[7:])), -1); err != nil {
			t.Fatal(err)
		}
		wantBlob(ctx, t, a, data)
	})
	step(t, "empty blob", func(t *testing.T) {
		u, _ := a.StartBlobUpload(ctx)
		if _, err := a.FinishBlobUpload(ctx, u, digest.FromBytes(nil), nil, 0); err != nil {
			t.Fatal(err)
		}
		wantBlob(ctx, t, a, []byte{})
	})
	step(t, "wrong digest", func(t *testing.T) {
		u, _ := a.StartBlobUpload(ctx)
		u, _ = a.AppendBlobChunk(ctx, u, strings.NewReader("abc"), 3)
		_, err := a.FinishBlobUpload(ctx, u, digest.FromString("other"), nil, 0)
		if !errors.Is(err, content.ErrMismatchedDigest) {
			t.Fatalf("wrong digest: %v, want content.ErrMismatchedDigest", err)
		}
		if strings.Contains(err.Error(), cfg.URL) {
			t.Errorf("error %q shows the backend", err)
		}
		if err := a.CancelBlobUpload(ctx, u); err != nil && !errors.Is(err, provider.ErrNotFound) {
			t.Errorf("cancel after the wrong digest: %v", err)
		}
	})
	step(t, "chunk out of order", func(t *testing.T) {
		u, _ := a.StartBlobUpload(ctx)
		u, _ = a.AppendBlobChunk(ctx, u, strings.NewReader("abc"), 3)
		late := u
		late.Offset = 1
		if _, err := b.AppendBlobChunk(ctx, late, strings.NewReader("x"), 1); err == nil {
			t.Error("a chunk out of order was accepted")
		}
		_ = a.CancelBlobUpload(ctx, u)
	})
	step(t, "cancel", func(t *testing.T) {
		u, _ := a.StartBlobUpload(ctx)
		u, _ = a.AppendBlobChunk(ctx, u, strings.NewReader("abc"), 3)
		if err := b.CancelBlobUpload(ctx, u); err != nil {
			t.Fatal(err)
		}
		if _, err := a.BlobUploadStatus(ctx, u); !errors.Is(err, provider.ErrNotFound) {
			t.Errorf("status after the cancel: %v, want ErrNotFound", err)
		}
		if _, err := a.AppendBlobChunk(ctx, u, strings.NewReader("x"), 1); !errors.Is(err, provider.ErrNotFound) {
			t.Errorf("chunk after the cancel: %v, want ErrNotFound", err)
		}
	})
	step(t, "mount", func(t *testing.T) {
		other, _ := pb.Repository(ctx, name+"-other")
		mounted, u, err := other.MountBlob(ctx, d, name)
		if err != nil {
			t.Fatal(err)
		}
		t.Logf("mounted: %v", mounted)
		if !mounted {
			if _, err := other.FinishBlobUpload(ctx, u, d, bytes.NewReader(blob), int64(len(blob))); err != nil {
				t.Fatal(err)
			}
		}
		wantBlob(ctx, t, other, blob)
		mounted, u, err = other.MountBlob(ctx, digest.FromString("absent "+randomHex(t)), name)
		if err != nil || mounted || u.Location == "" {
			t.Fatalf("mount of an absent blob = %v, %+v, %v; want a session", mounted, u, err)
		}
		if err := other.CancelBlobUpload(ctx, u); err != nil {
			t.Errorf("cancel the session of the mount: %v", err)
		}
	})
}

// wantBlob checks that repo holds want.
func wantBlob(ctx context.Context, t *testing.T, repo provider.Repository, want []byte) {
	t.Helper()
	desc := descriptor(octetStream, want)
	if ok, err := repo.Exists(ctx, desc); err != nil || !ok {
		t.Fatalf("Exists(%s) = %v, %v", desc.Digest, ok, err)
	}
	if got := fetchAll(ctx, t, repo, desc); !bytes.Equal(got, want) {
		t.Errorf("blob %s is %q, want %q", desc.Digest, got, want)
	}
}
