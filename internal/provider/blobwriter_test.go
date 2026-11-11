package provider_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strconv"
	"strings"
	"testing"

	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"oras.land/oras-go/v2/content"

	"github.com/lajstrom/lajstrom/internal/provider"
	"github.com/lajstrom/lajstrom/internal/provider/providertest"
)

// calls is a BlobUploader that records the requests a writer makes of the one under it, and
// fails AppendBlobChunk with failAppend.
type calls struct {
	provider.BlobUploader
	log        []string
	failAppend error
}

func (c *calls) StartBlobUpload(ctx context.Context) (provider.Upload, error) {
	c.log = append(c.log, "POST")
	return c.BlobUploader.StartBlobUpload(ctx)
}

func (c *calls) AppendBlobChunk(ctx context.Context, u provider.Upload, r io.Reader, size int64) (provider.Upload, error) {
	c.log = append(c.log, "PATCH "+itoa(size))
	if c.failAppend != nil {
		return u, c.failAppend
	}
	return c.BlobUploader.AppendBlobChunk(ctx, u, r, size)
}

func (c *calls) FinishBlobUpload(ctx context.Context, u provider.Upload, d digest.Digest, r io.Reader, size int64) (ocispec.Descriptor, error) {
	c.log = append(c.log, "PUT "+itoa(size))
	return c.BlobUploader.FinishBlobUpload(ctx, u, d, r, size)
}

func (c *calls) CancelBlobUpload(ctx context.Context, u provider.Upload) error {
	c.log = append(c.log, "DELETE")
	return c.BlobUploader.CancelBlobUpload(ctx, u)
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }

// TestChunkedWriter: the requests a chunked writer of 4-byte chunks makes for writes of several
// sizes, a commit and a cancel, and what the fake registry holds after them.
func TestChunkedWriter(t *testing.T) {
	for _, tt := range []struct {
		name   string
		writes []string
		cancel bool   // Cancel instead of Commit
		wrong  bool   // Commit with another digest, then Cancel
		want   string // the requests
	}{
		{name: "empty", want: "POST PUT 0"},
		{name: "one write below a chunk", writes: []string{"abc"}, want: "POST PUT 3"},
		{name: "exactly a chunk", writes: []string{"abcd"}, want: "POST PATCH 4 PUT 0"},
		{name: "writes across chunks", writes: []string{"a", "bcdef", "ghijk"}, want: "POST PATCH 4 PATCH 4 PUT 3"},
		{name: "one large write", writes: []string{"abcdefghij"}, want: "POST PATCH 4 PATCH 4 PUT 2"},
		{name: "cancel before a chunk", writes: []string{"abc"}, cancel: true, want: ""},
		{name: "cancel after chunks", writes: []string{"abcdefghij"}, cancel: true, want: "POST PATCH 4 PATCH 4 DELETE"},
		{name: "wrong digest", writes: []string{"abcdefghij"}, wrong: true, want: "POST PATCH 4 PATCH 4 PUT 2 DELETE"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			ctx := t.Context()
			f := &providertest.Fake{}
			rr, err := f.Repository(ctx, "raw/r/p")
			must(t, err)
			c := &calls{BlobUploader: rr}
			w := provider.NewChunkedWriter(ctx, c, 4)
			blob := strings.Join(tt.writes, "")
			for _, s := range tt.writes {
				if n, err := w.Write([]byte(s)); err != nil || n != len(s) {
					t.Fatalf("Write(%q) = %d, %v", s, n, err)
				}
			}
			if w.Size() != int64(len(blob)) {
				t.Errorf("Size = %d, want %d", w.Size(), len(blob))
			}
			d := digest.FromString(blob)
			switch {
			case tt.cancel:
				must(t, w.Cancel(ctx))
			case tt.wrong:
				if _, err := w.Commit(ctx, digest.FromString("other")); !errors.Is(err, content.ErrMismatchedDigest) {
					t.Fatalf("Commit with another digest = %v, want ErrMismatchedDigest", err)
				}
				must(t, w.Cancel(ctx))
			default:
				desc, err := w.Commit(ctx, d)
				if err != nil || desc.Digest != d || desc.Size != int64(len(blob)) {
					t.Fatalf("Commit = %+v, %v", desc, err)
				}
			}
			if got := strings.Join(c.log, " "); got != tt.want {
				t.Errorf("requests %q, want %q", got, tt.want)
			}
			if n := f.OpenUploads(); n != 0 {
				t.Errorf("%d sessions open, want none", n)
			}
			wantBlob(t, rr, blob, !tt.cancel && !tt.wrong)
		})
	}
}

// wantBlob fails the test unless rr holds blob exactly when stored is true.
func wantBlob(t *testing.T, rr provider.Repository, blob string, stored bool) {
	t.Helper()
	ctx := t.Context()
	desc := ocispec.Descriptor{Digest: digest.FromString(blob), Size: int64(len(blob))}
	ok, err := rr.Exists(ctx, desc)
	must(t, err)
	if ok != stored {
		t.Fatalf("the blob is stored: %v, want %v", ok, stored)
	}
	if !ok || blob == "" {
		return
	}
	rc, err := rr.Fetch(ctx, desc)
	must(t, err)
	got, _ := io.ReadAll(rc)
	_ = rc.Close()
	if !bytes.Equal(got, []byte(blob)) {
		t.Errorf("stored %q, want %q", got, blob)
	}
}

// TestChunkedWriterEnds: a writer that Commit or Cancel ended takes no Write and no second
// Commit, and Cancel does nothing after Commit; a failed chunk fails the Writes and the Commit
// after it and leaves the session to Cancel.
func TestChunkedWriterEnds(t *testing.T) {
	ctx := t.Context()
	f := &providertest.Fake{}
	rr, err := f.Repository(ctx, "raw/r/p")
	must(t, err)
	c := &calls{BlobUploader: rr}

	w := provider.NewChunkedWriter(ctx, c, 4)
	_, _ = w.Write([]byte("abcdef"))
	if _, err := w.Commit(ctx, digest.FromString("abcdef")); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte("g")); !errors.Is(err, provider.ErrWriterClosed) {
		t.Errorf("Write after Commit = %v, want ErrWriterClosed", err)
	}
	if _, err := w.Commit(ctx, digest.FromString("abcdef")); !errors.Is(err, provider.ErrWriterClosed) {
		t.Errorf("a second Commit = %v, want ErrWriterClosed", err)
	}
	must(t, w.Cancel(ctx))
	if ok, _ := rr.Exists(ctx, ocispec.Descriptor{Digest: digest.FromString("abcdef"), Size: 6}); !ok {
		t.Error("Cancel after Commit dropped the blob")
	}

	boom := errors.New("boom")
	c.log, c.failAppend = nil, boom
	w = provider.NewChunkedWriter(ctx, c, 4)
	if _, err := w.Write([]byte("abcdef")); !errors.Is(err, boom) {
		t.Fatalf("Write over a failing chunk = %v, want boom", err)
	}
	if _, err := w.Write([]byte("g")); !errors.Is(err, boom) {
		t.Errorf("the Write after it = %v, want boom", err)
	}
	if _, err := w.Commit(ctx, digest.FromString("abcdefg")); !errors.Is(err, boom) {
		t.Errorf("Commit after it = %v, want boom", err)
	}
	if f.OpenUploads() != 1 {
		t.Errorf("%d sessions open before Cancel, want 1", f.OpenUploads())
	}
	must(t, w.Cancel(ctx))
	must(t, w.Cancel(ctx))
	if got, want := strings.Join(c.log, " "), "POST PATCH 4 DELETE"; got != want || f.OpenUploads() != 0 {
		t.Errorf("requests %q, %d sessions open; want %q and none", got, f.OpenUploads(), want)
	}
}

// must stops the test on an error.
func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
