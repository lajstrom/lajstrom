package generic

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/lajstrom/lajstrom/internal/config"
	"github.com/lajstrom/lajstrom/internal/provider"
)

// TestUploadUnknownRetried: a registry not classified strong that answers BLOB_UPLOAD_UNKNOWN
// for a session it opened a moment before gets the write again, from its start, until it takes
// it: the monolithic PUT of a blob writer, the first PATCH of one, and the PUT of Push. A write
// to a session that holds bytes, a session older than freshSession and a strong registry's
// answer are ErrNotFound at once.
func TestUploadUnknownRetried(t *testing.T) {
	small, large := []byte("a blob of one chunk"), bytes.Repeat([]byte("x"), streamChunk+1)
	tests := []struct {
		name     string
		strong   bool
		method   string // the requests refused, the first refusals of them
		refusals int64
		blob     []byte
		write    func(ctx context.Context, rr *repository, blob []byte) error
		ok       bool
	}{
		{name: "the PUT of a writer", method: http.MethodPut, refusals: 2, blob: small, write: viaWriter, ok: true},
		{name: "the first PATCH of a writer", method: http.MethodPatch, refusals: 2, blob: large, write: viaWriter, ok: true},
		{name: "the PUT of Push", method: http.MethodPut, refusals: 2, blob: small, write: viaPush, ok: true},
		{name: "a session that holds bytes", method: http.MethodPut, refusals: 1, blob: large, write: viaWriter},
		{name: "an old session", method: http.MethodPut, refusals: 1, blob: small, write: viaOldSession},
		{name: "a strong registry", strong: true, method: http.MethodPut, refusals: 1, blob: small, write: viaWriter},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFakeRegistry(t)
			var refused atomic.Int64
			f.hook = func(w http.ResponseWriter, r *http.Request) bool {
				if r.Method != tt.method || !strings.Contains(r.URL.Path, "/blobs/uploads/") || refused.Load() >= tt.refusals {
					return false
				}
				refused.Add(1)
				_, _ = io.Copy(io.Discard, r.Body)
				writeErr(w, http.StatusNotFound, "BLOB_UPLOAD_UNKNOWN", "blob upload unknown to registry")
				return true
			}
			log, _ := testLogger()
			p := f.provider(t, log, func(c *config.Registry) {
				if tt.strong {
					c.Capabilities.Consistency = config.ConsistencyStrong
				}
			})
			rr, err := p.Repository(t.Context(), "raw/scratch/notes")
			if err != nil {
				t.Fatal(err)
			}
			err = tt.write(t.Context(), rr.(*repository), tt.blob)
			switch {
			case tt.ok && err != nil:
				t.Fatalf("write: %v", err)
			case tt.ok:
				f.mu.Lock()
				stored := bytes.Equal(f.blobs["art/raw/scratch/notes"][digest.FromBytes(tt.blob)], tt.blob)
				f.mu.Unlock()
				if !stored {
					t.Error("the registry does not hold the blob")
				}
			case !errors.Is(err, provider.ErrNotFound):
				t.Errorf("write: %v, want ErrNotFound", err)
			}
			if n := refused.Load(); n != tt.refusals {
				t.Errorf("%d writes refused, want %d", n, tt.refusals)
			}
		})
	}
}

// viaWriter writes blob through a blob writer.
func viaWriter(ctx context.Context, rr *repository, blob []byte) error {
	w, err := rr.OpenBlobWriter(ctx)
	if err != nil {
		return err
	}
	if _, err := w.Write(blob); err != nil {
		return err
	}
	_, err = w.Commit(ctx, digest.FromBytes(blob))
	return err
}

// viaPush writes blob with Push.
func viaPush(ctx context.Context, rr *repository, blob []byte) error {
	return rr.Push(ctx, ocispec.Descriptor{MediaType: octetStream, Digest: digest.FromBytes(blob), Size: int64(len(blob))},
		bytes.NewReader(blob))
}

// viaOldSession writes blob in one PUT to a session opened, as far as the writer knows, longer
// than freshSession ago.
func viaOldSession(ctx context.Context, rr *repository, blob []byte) error {
	u := &freshUploads{repository: rr}
	up, err := u.StartBlobUpload(ctx)
	if err != nil {
		return err
	}
	u.opened = time.Now().Add(-freshSession - time.Second)
	_, err = u.FinishBlobUpload(ctx, up, digest.FromBytes(blob), bytes.NewReader(blob), int64(len(blob)))
	return err
}
