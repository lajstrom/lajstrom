package proxy_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/lajstrom/lajstrom/internal/format"
	"github.com/lajstrom/lajstrom/internal/provider"
	"github.com/lajstrom/lajstrom/internal/provider/providertest"
	"github.com/lajstrom/lajstrom/internal/proxy"
)

// logBuffer is a log destination that the fetch's goroutine and the test share.
type logBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *logBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *logBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// TestOCIBlobBroken: a blob whose upstream connection breaks for good, the resume answered with
// the whole blob, is a 502 to a request that waits, aborts the claim holder's tee, counts both
// requests as error and none as fetched, and is logged once at warn (proxy-and-group.md §6,
// #884).
func TestOCIBlobBroken(t *testing.T) {
	e := newOCIEnv(t, ociSettings)
	img := e.hub.image(t, "library/nginx", "1.27", "amd64")
	var logs logBuffer
	c := e.core("a", proxy.Options{Log: slog.New(slog.NewTextHandler(&logs, nil))})
	_, _, err := c.OCIManifest(t.Context(), e.repo, "library/nginx", img.manifests[0].Digest.String())
	must(t, err)
	layer := img.blobs[0][1]
	blob := e.hub.bytesOf("library/nginx", layer.Digest)
	f := &breaking{cut: func(int) int { return len(blob) / 2 }, whole: true, hold: make(chan struct{})}
	e.hub.breakBlob(layer.Digest, blob, f)
	failed, fetched := fetches("error"), fetches("fetched")

	_, rc, err := c.OCIBlob(t.Context(), e.repo, "library/nginx", layer.Digest, true, true)
	if err != nil || rc == nil {
		t.Fatalf("the claim holder: %v, %v; want the tee", rc, err)
	}
	defer func() { _ = rc.Close() }()
	waited := make(chan error, 1)
	go func() {
		_, _, err := c.OCIBlob(t.Context(), e.repo, "library/nginx", layer.Digest, true, false)
		waited <- err
	}()
	time.Sleep(100 * time.Millisecond) // the request joins the fetch, which the resume holds
	close(f.hold)
	if got, err := io.ReadAll(rc); err == nil || len(got) >= len(blob) {
		t.Errorf("the tee of a broken body: %d bytes, %v; want an error before the end", len(got), err)
	}
	err = <-waited
	if !errors.Is(err, format.ErrBadGateway) || errors.Is(err, format.ErrGatewayTimeout) {
		t.Fatalf("the request that waited: %v, want format.ErrBadGateway", err)
	}
	if n := fetches("error") - failed; n != 2 {
		t.Errorf("%v requests counted as error, want 2: the claim holder and the one that waited", n)
	}
	if n := fetches("fetched") - fetched; n != 0 {
		t.Errorf("%v requests counted as fetched, want none", n)
	}
	out := logs.String()
	if n := strings.Count(out, `msg="proxy: a blob not fetched from the upstream"`); n != 1 || !strings.Contains(out, "level=WARN") {
		t.Errorf("%d warn lines of the failed fetch, want 1:\n%s", n, out)
	}
}

// TestOCIBlobFetchedCount: the claim holder of a blob fetched whole counts as fetched once its
// copy is committed, and never as error.
func TestOCIBlobFetchedCount(t *testing.T) {
	e := newOCIEnv(t, ociSettings)
	img := e.hub.image(t, "library/nginx", "1.27", "amd64")
	c := e.core("a", proxy.Options{})
	_, _, err := c.OCIManifest(t.Context(), e.repo, "library/nginx", img.manifests[0].Digest.String())
	must(t, err)
	failed, fetched := fetches("error"), fetches("fetched")
	_, rc, err := c.OCIBlob(t.Context(), e.repo, "library/nginx", img.blobs[0][1].Digest, true, true)
	if err != nil || rc == nil {
		t.Fatalf("the claim holder: %v, %v; want the tee", rc, err)
	}
	if _, err := io.ReadAll(rc); err != nil {
		t.Fatal(err)
	}
	_ = rc.Close()
	if fetches("fetched")-fetched != 1 || fetches("error") != failed {
		t.Errorf("fetched +%v, error +%v; want fetched +1", fetches("fetched")-fetched, fetches("error")-failed)
	}
}

// failingWrites is a backend whose blob writes fail with err: a backend that fails mid-upload.
type failingWrites struct {
	*providertest.Fake
	err error
}

func (f failingWrites) Repository(ctx context.Context, name string) (provider.Repository, error) {
	rr, err := f.Fake.Repository(ctx, name)
	return failingWritesRepository{rr, f.err}, err
}

type failingWritesRepository struct {
	provider.Repository
	err error
}

func (r failingWritesRepository) OpenBlobWriter(ctx context.Context) (provider.BlobWriter, error) {
	w, err := r.Repository.OpenBlobWriter(ctx)
	return failingWriter{w, r.err}, err
}

type failingWriter struct {
	provider.BlobWriter
	err error
}

func (w failingWriter) Write([]byte) (int, error) { return 0, w.err }

// TestOCIBlobBackendFails: a backend that fails mid-upload keeps its own error, not the
// upstream's 502.
func TestOCIBlobBackendFails(t *testing.T) {
	e := newOCIEnv(t, ociSettings)
	img := e.hub.image(t, "library/nginx", "1.27", "amd64")
	c := e.coreOn(failingWrites{e.reg, provider.ErrUnavailable}, "a", proxy.Options{})
	_, _, err := c.OCIManifest(t.Context(), e.repo, "library/nginx", img.manifests[0].Digest.String())
	must(t, err)
	_, _, err = c.OCIBlob(t.Context(), e.repo, "library/nginx", img.blobs[0][1].Digest, true, false)
	if !errors.Is(err, provider.ErrUnavailable) || errors.Is(err, format.ErrBadGateway) {
		t.Fatalf("a backend that fails mid-upload: %v, want its provider.ErrUnavailable", err)
	}
}
