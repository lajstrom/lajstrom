package facade

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/lajstrom/lajstrom/internal/metrics"
	"github.com/lajstrom/lajstrom/internal/provider"
	"github.com/lajstrom/lajstrom/internal/provider/providertest"
	"github.com/lajstrom/lajstrom/internal/server"
)

// corrupt is a backend that answers other bytes than it stores: for any blob, a pattern of
// size bytes, and a manifest with its last byte changed.
type corrupt struct {
	*providertest.Fake
	size int64
}

func (c corrupt) OpenBlob(_ context.Context, _ string, d digest.Digest, _ string, _ bool) (provider.Blob, error) {
	return provider.Blob{Status: http.StatusOK, Body: newPattern(c.size), Size: c.size, Digest: d}, nil
}

func (c corrupt) Repository(ctx context.Context, name string) (provider.Repository, error) {
	rr, err := c.Fake.Repository(ctx, name)
	return corruptRepository{rr}, err
}

type corruptRepository struct{ provider.Repository }

func (r corruptRepository) Fetch(ctx context.Context, desc ocispec.Descriptor) (io.ReadCloser, error) {
	rc, err := r.Repository.Fetch(ctx, desc)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rc.Close() }()
	b, err := io.ReadAll(rc)
	if err != nil || len(b) == 0 {
		return nil, err
	}
	b[len(b)-1] ^= 1
	return io.NopCloser(bytes.NewReader(b)), nil
}

// TestDownloadVerifiesDigest downloads through the server, over real connections, from a
// backend that answers other bytes than it stores: a blob of 256 MiB, and a manifest by tag
// and by digest. Each client gets a short body and an error, never all of it; each abort is
// counted under the registry and logged with the request id; and the heap stays under
// heapLimit while the blob is hashed (docs/spec/guarantees.md, INV-40).
func TestDownloadVerifiesDigest(t *testing.T) {
	size := int64(256 << 20)
	if testing.Short() {
		size = 4 << 20
	}
	e := newEnv(t, envOptions{registry: func(f *providertest.Fake) provider.Provider { return corrupt{Fake: f, size: size} }})
	body, d := manifest(t, []byte("layer"), "corrupt")
	wantStatus(t, "PUT", e.do(e.h, http.MethodPut, "/v2/oci/images/app/manifests/v1", bytes.NewReader(body),
		"Content-Type", ocispecManifest), http.StatusCreated)
	logs := &logBuffer{}
	srv, err := server.New(server.Options{Listen: "127.0.0.1:0", Facade: e.h, Log: slog.New(slog.NewJSONHandler(logs, nil))})
	must(t, err)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	client := &http.Client{Transport: &http.Transport{DisableKeepAlives: true}} // a fresh connection, no retry
	mismatches := func() float64 { return testutil.ToFloat64(metrics.DownloadDigestMismatches.WithLabelValues("local")) }
	before := mismatches()
	peak := sampleHeap(t)

	downloads := []struct {
		path string
		size int64
	}{
		{"/v2/oci/images/app/blobs/" + zeroDigest.String(), size},
		{"/v2/oci/images/app/manifests/v1", int64(len(body))},
		{"/v2/oci/images/app/manifests/" + d.String(), int64(len(body))},
	}
	for i, dl := range downloads {
		n, err := get(t, client, ts.URL+dl.path)
		if err == nil || n >= dl.size || (i == 0 && n == 0) {
			t.Errorf("GET %s: %d bytes of %d, %v; want a short body and an error", dl.path, n, dl.size, err)
		}
		t.Logf("GET %s: %d bytes of %d, then %v", dl.path, n, dl.size, err)
	}
	top := peak()
	t.Logf("peak heap in use: %d MiB", top>>20)
	if !testing.Short() && top > heapLimit {
		t.Errorf("peak heap in use %d MiB, want under %d MiB", top>>20, heapLimit>>20)
	}
	if n := mismatches() - before; n != float64(len(downloads)) {
		t.Errorf("%v more mismatches, want %d", n, len(downloads))
	}
	if n := abortLines(t, logs.String()); n != len(downloads) {
		t.Errorf("%d lines response aborted, want %d:\n%s", n, len(downloads), logs)
	}
}

// get downloads url with client and returns how many bytes of the body came, and the error
// that ended them: nil for a whole body.
func get(t *testing.T, client *http.Client, url string) (int64, error) {
	t.Helper()
	req, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, url, nil)
	resp, err := client.Do(req)
	if err != nil {
		return 0, err
	}
	defer func() { _ = resp.Body.Close() }()
	return io.Copy(io.Discard, resp.Body)
}

// abortLines counts the lines response aborted of the JSON log logs, and fails t for one that
// is not at error, has no request id or does not say that the bytes are not the digest.
func abortLines(t *testing.T, logs string) int {
	t.Helper()
	n := 0
	for l := range strings.SplitSeq(strings.TrimSpace(logs), "\n") {
		var m map[string]any
		if json.Unmarshal([]byte(l), &m) != nil || m["msg"] != "response aborted" {
			continue
		}
		n++
		if msg, _ := m["error"].(string); !strings.Contains(msg, "do not have the digest") || m["level"] != "ERROR" || m["requestId"] == "" {
			t.Errorf("abort log line %v", m)
		}
	}
	return n
}
