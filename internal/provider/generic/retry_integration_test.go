//go:build integration

package generic

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/lajstrom/lajstrom/internal/config"
	"github.com/lajstrom/lajstrom/internal/provider"
)

// TestIntegrationRetryNoAnswer runs the provider against the registry of LAJSTROM_TEST_REGISTRY
// through a proxy that closes the connection of one request of each step without an answer, on
// a fresh connection, and passes everything else through (#670). A blob pushed from bytes is
// pushed again from the start; a manifest PUT, a manifest HEAD and a tag list are sent again;
// each succeeds against the real registry. A chunk from a stream is sent once and is
// provider.ErrUnavailable. A registry with credentials or TLS is skipped: its token realm and
// its certificate name the registry's host, not the proxy's.
func TestIntegrationRetryNoAnswer(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if os.Getenv("LAJSTROM_TEST_REGISTRY_USERNAME") != "" || os.Getenv("LAJSTROM_TEST_REGISTRY_TLS") == "1" {
		t.Skip("the proxy speaks plain HTTP without credentials")
	}
	_, cfg := integrationProvider(ctx, t)
	target, err := url.Parse(cfg.URL)
	if err != nil {
		t.Fatal(err)
	}
	rp := &httputil.ReverseProxy{Rewrite: func(r *httputil.ProxyRequest) {
		r.SetURL(target)
		r.Out.Host = r.In.Host // so that an absolute Location (distribution) names the proxy
	}}
	var mu sync.Mutex
	var match func(*http.Request) bool // the requests of the step; the first one's connection closes
	matched := 0
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		hit := match != nil && match(r)
		if hit {
			matched++
		}
		first := hit && matched == 1
		mu.Unlock()
		if !first {
			rp.ServeHTTP(w, r)
			return
		}
		_, _ = io.Copy(io.Discard, r.Body)
		if c, _, err := http.NewResponseController(w).Hijack(); err == nil {
			_ = c.Close()
		}
	}))
	// Every request on a fresh connection, which net/http does not send again by itself.
	srv.Config.SetKeepAlivesEnabled(false)
	srv.Start()
	t.Cleanup(srv.Close)
	closeOnce := func(method, part string) {
		mu.Lock()
		defer mu.Unlock()
		matched = 0
		match = func(r *http.Request) bool { return r.Method == method && strings.Contains(r.URL.Path, part) }
	}
	wantSent := func(t *testing.T, want int) {
		t.Helper()
		mu.Lock()
		defer mu.Unlock()
		if matched != want {
			t.Errorf("the request was sent %d times, want %d", matched, want)
		}
	}

	p, _ := integrationProvider(ctx, t, func(c *config.Registry) { c.Name, c.URL = "integration-no-answer", srv.URL })
	repo, err := p.Repository(ctx, "raw/it-"+randomHex(t)+"/no-answer")
	if err != nil {
		t.Fatal(err)
	}
	blob := []byte("closed without an answer " + randomHex(t))
	blobDesc := descriptor("application/octet-stream", blob)
	step(t, "push of a blob from bytes", func(t *testing.T) {
		closeOnce(http.MethodPut, "/blobs/uploads/")
		if err := repo.Push(ctx, blobDesc, bytes.NewReader(blob)); err != nil {
			t.Fatal(err)
		}
		wantSent(t, 2)
		if ok, err := repo.Exists(ctx, blobDesc); err != nil || !ok {
			t.Fatalf("Exists(blob) = %v, %v", ok, err)
		}
	})
	var manifest ocispec.Descriptor
	t.Cleanup(func() {
		if manifest.Digest != "" {
			_ = repo.Delete(context.Background(), manifest) // where the registry deletes
		}
	})
	step(t, "manifest PUT", func(t *testing.T) {
		closeOnce(http.MethodPut, "/manifests/v1")
		_, manifest = pushTestManifest(ctx, t, repo, "v1", nil, blobDesc)
		wantSent(t, 2)

		closeOnce(http.MethodHead, "/manifests/")
		if ok, err := repo.Exists(ctx, manifest); err != nil || !ok {
			t.Fatalf("Exists(manifest) = %v, %v", ok, err)
		}
		wantSent(t, 2)
	})
	step(t, "tag list", func(t *testing.T) {
		closeOnce(http.MethodGet, "/tags/list")
		var tags []string
		err := repo.Tags(ctx, "", func(page []string) error { tags = append(tags, page...); return nil })
		if err != nil || !slices.Contains(tags, "v1") {
			t.Fatalf("Tags = %v, %v; want v1", tags, err)
		}
		wantSent(t, 2)
	})
	step(t, "chunk from a stream", func(t *testing.T) {
		u, err := repo.StartBlobUpload(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = repo.CancelBlobUpload(context.Background(), u) }()
		closeOnce(http.MethodPatch, "/blobs/uploads/")
		_, err = repo.AppendBlobChunk(ctx, u, struct{ io.Reader }{bytes.NewReader(blob)}, -1)
		if !errors.Is(err, provider.ErrUnavailable) {
			t.Errorf("AppendBlobChunk = %v, want provider.ErrUnavailable", err)
		}
		wantSent(t, 1)
	})
}
