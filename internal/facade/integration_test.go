//go:build integration

package facade

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/lajstrom/lajstrom/internal/config"
	"github.com/lajstrom/lajstrom/internal/index"
	"github.com/lajstrom/lajstrom/internal/index/sqlite"
	"github.com/lajstrom/lajstrom/internal/provider"
	"github.com/lajstrom/lajstrom/internal/provider/generic"
)

// replicas is two facades over one SQLite database and the registry of
// LAJSTROM_TEST_REGISTRY, each with a provider of its own (its own connections and token
// cache), as two replicas of the server have; they share nothing else.
type replicas struct {
	st    *sqlite.Store
	a, b  *httptest.Server
	check provider.Repository // the backend, read directly
	name  string              // oci/images/it-<random>
	logs  *logBuffer
}

func newReplicas(t *testing.T) *replicas {
	t.Helper()
	addr := os.Getenv("LAJSTROM_TEST_REGISTRY")
	if addr == "" {
		t.Skip("set LAJSTROM_TEST_REGISTRY=host:port to run against a registry")
	}
	ctx := t.Context()
	st, err := sqlite.Open(ctx, fmt.Sprintf("file:facade-it-%d?mode=memory&cache=shared", memoryDBs.Add(1)))
	must(t, err)
	t.Cleanup(func() { _ = st.Close() })
	must(t, st.Migrate(ctx))
	repo := index.Repo{Format: "oci", Name: "images", Registry: "it", Prefix: "oci/images", Kind: "hosted"}
	must(t, st.CreateRepo(ctx, &repo))
	cfg := config.Registry{Name: "it", Type: generic.Type, URL: "http://" + addr, Prefix: os.Getenv("LAJSTROM_TEST_REGISTRY_PREFIX"),
		Auth: config.RegistryAuth{Username: os.Getenv("LAJSTROM_TEST_REGISTRY_USERNAME"), Password: os.Getenv("LAJSTROM_TEST_REGISTRY_PASSWORD")}}
	r := &replicas{st: st, logs: &logBuffer{}, name: "oci/images/it-" + randomHex(t)}
	server := func() *httptest.Server {
		p, err := generic.New(cfg, slog.New(slog.NewTextHandler(r.logs, nil)))
		must(t, err)
		srv := httptest.NewServer(New(Options{Index: st, Providers: provider.Map{"it": p}, Log: slog.New(slog.NewTextHandler(r.logs, nil))}))
		t.Cleanup(srv.Close)
		return srv
	}
	r.a, r.b = server(), server()
	p, err := generic.New(cfg, nil)
	must(t, err)
	r.check, err = p.Repository(ctx, r.name)
	must(t, err)
	return r
}

// request sends method to url with body and headers, and returns the answer with its body
// read.
func request(t *testing.T, method, url string, body io.Reader, size int64, hdr ...string) (*http.Response, []byte) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), method, url, body)
	must(t, err)
	if body != nil {
		req.ContentLength = size
	}
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	resp, err := http.DefaultClient.Do(req)
	must(t, err)
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	return resp, b
}

// wantBackendBlob checks that the backend holds b.
func (r *replicas) wantBackendBlob(t *testing.T, b []byte) {
	t.Helper()
	desc := ocispec.Descriptor{MediaType: octetStream, Digest: digest.FromBytes(b), Size: int64(len(b))}
	rc, err := r.check.Fetch(t.Context(), desc)
	if err != nil {
		t.Fatalf("the backend does not have %s: %v", desc.Digest, err)
	}
	defer func() { _ = rc.Close() }()
	if got, _ := io.ReadAll(rc); !bytes.Equal(got, b) {
		t.Errorf("the backend's %s is %q", desc.Digest, got)
	}
}

// TestIntegrationUploadAcrossReplicas is the lead's first criterion on #24 against a real
// registry: the session starts on replica A, the chunks go to replica B, the finish to A, and
// the blob exists in the backend with the right digest.
func TestIntegrationUploadAcrossReplicas(t *testing.T) {
	r := newReplicas(t)
	uploads := "/v2/" + r.name + "/blobs/uploads/"
	resp, _ := request(t, http.MethodPost, r.a.URL+uploads, nil, 0)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("POST on A: %d", resp.StatusCode)
	}
	loc := resp.Header.Get("Location")
	chunks := []string{"started on A, ", "chunked on B, ", "and on B, ", "finished on A " + randomHex(t)}
	off := 0
	for _, c := range chunks[:3] {
		resp, body := request(t, http.MethodPatch, r.b.URL+loc, strings.NewReader(c), int64(len(c)),
			"Content-Range", fmt.Sprintf("%d-%d", off, off+len(c)-1), "Content-Type", octetStream)
		off += len(c)
		if resp.StatusCode != http.StatusAccepted || resp.Header.Get("Range") != fmt.Sprintf("0-%d", off-1) {
			t.Fatalf("PATCH on B: %d, Range %q, %s", resp.StatusCode, resp.Header.Get("Range"), body)
		}
	}
	if resp, _ := request(t, http.MethodGet, r.b.URL+loc, nil, 0); resp.StatusCode != http.StatusNoContent ||
		resp.Header.Get("Range") != fmt.Sprintf("0-%d", off-1) {
		t.Fatalf("GET on B: %d, Range %q", resp.StatusCode, resp.Header.Get("Range"))
	}
	blob := strings.Join(chunks, "")
	d := digest.FromString(blob)
	last := chunks[3]
	resp, body := request(t, http.MethodPut, r.a.URL+loc+"?digest="+d.String(), strings.NewReader(last), int64(len(last)),
		"Content-Range", fmt.Sprintf("%d-%d", off, off+len(last)-1))
	if resp.StatusCode != http.StatusCreated || resp.Header.Get("Docker-Content-Digest") != d.String() {
		t.Fatalf("PUT on A: %d, %s", resp.StatusCode, body)
	}
	r.wantBackendBlob(t, []byte(blob))
}

// TestIntegrationMonolithicAbandoned is the lead's second criterion against a real registry:
// replica A goes away in the middle of a monolithic upload, the client's retry on B succeeds,
// and nothing is left on A's disk: there is no spool. A cancels its backend session after the
// client went; distribution refuses that (its session state in the location no longer matches
// the bytes the broken PUT wrote, 416), so there the partial upload is orphaned until
// distribution purges old uploads, which the test reports.
func TestIntegrationMonolithicAbandoned(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp)
	r := newReplicas(t)
	uploads := "/v2/" + r.name + "/blobs/uploads/"
	blob := bytes.Repeat([]byte("abandoned "+randomHex(t)+" "), 200_000)
	d := digest.FromBytes(blob)
	pr, pw := io.Pipe()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, r.a.URL+uploads+"?digest="+d.String(), pr)
	must(t, err)
	req.ContentLength = int64(len(blob))
	done := make(chan error, 1)
	go func() {
		resp, err := http.DefaultClient.Do(req)
		if resp != nil {
			_ = resp.Body.Close()
		}
		done <- err
	}()
	if _, err := pw.Write(blob[:len(blob)/3]); err != nil {
		t.Fatal(err)
	}
	time.Sleep(200 * time.Millisecond) // the body is on its way to the backend
	r.a.CloseClientConnections()
	pw.CloseWithError(errors.New("the client saw A go"))
	if err := <-done; err != nil {
		t.Logf("the client's request to A: %v", err)
	}
	resp, body := request(t, http.MethodPost, r.b.URL+uploads+"?digest="+d.String(), bytes.NewReader(blob), int64(len(blob)),
		"Content-Type", octetStream)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("retry on B: %d, %s", resp.StatusCode, body)
	}
	r.wantBackendBlob(t, blob)
	time.Sleep(200 * time.Millisecond) // A's cleanup runs after its client went
	if strings.Contains(r.logs.String(), "cancel backend upload session") {
		t.Logf("A's backend session is orphaned until the backend purges it: A could not cancel it")
	} else {
		t.Logf("A cancelled its backend session")
	}
	if left, _ := os.ReadDir(tmp); len(left) != 0 {
		t.Errorf("files left in the temporary directory: %v", left)
	}
}

// TestIntegrationPassThrough pushes an artifact to the oci repo through the facade, by
// replica, and reads it back: blobs, the manifest by tag and digest, tags/list, _catalog.
func TestIntegrationPassThrough(t *testing.T) {
	r := newReplicas(t)
	layer := []byte("layer " + randomHex(t))
	for _, b := range [][]byte{layer, ocispec.DescriptorEmptyJSON.Data} {
		resp, body := request(t, http.MethodPost, r.a.URL+"/v2/"+r.name+"/blobs/uploads/?digest="+digest.FromBytes(b).String(),
			bytes.NewReader(b), int64(len(b)))
		if resp.StatusCode != http.StatusCreated {
			t.Fatalf("blob: %d %s", resp.StatusCode, body)
		}
	}
	m, md := manifest(t, layer, "it")
	resp, body := request(t, http.MethodPut, r.b.URL+"/v2/"+r.name+"/manifests/v1", bytes.NewReader(m), int64(len(m)),
		"Content-Type", ocispecManifest)
	if resp.StatusCode != http.StatusCreated || resp.Header.Get("Docker-Content-Digest") != md.String() {
		t.Fatalf("manifest: %d %s", resp.StatusCode, body)
	}
	for _, ref := range []string{"v1", md.String()} {
		resp, body := request(t, http.MethodGet, r.a.URL+"/v2/"+r.name+"/manifests/"+ref, nil, 0)
		if resp.StatusCode != http.StatusOK || !bytes.Equal(body, m) || resp.Header.Get("Content-Type") != ocispecManifest {
			t.Errorf("GET manifest %s: %d %q", ref, resp.StatusCode, resp.Header.Get("Content-Type"))
		}
	}
	resp, body = request(t, http.MethodHead, r.b.URL+"/v2/"+r.name+"/blobs/"+digest.FromBytes(layer).String(), nil, 0)
	if resp.StatusCode != http.StatusOK || resp.ContentLength != int64(len(layer)) {
		t.Errorf("HEAD blob: %d, Content-Length %d", resp.StatusCode, resp.ContentLength)
	}
	resp, body = request(t, http.MethodGet, r.a.URL+"/v2/"+r.name+"/blobs/"+digest.FromBytes(layer).String(), nil, 0)
	if resp.StatusCode != http.StatusOK || !bytes.Equal(body, layer) {
		t.Errorf("GET blob: %d %q", resp.StatusCode, body)
	}
	resp, body = request(t, http.MethodGet, r.b.URL+"/v2/"+r.name+"/tags/list", nil, 0)
	if want := `{"name":"` + r.name + `","tags":["v1"]}` + "\n"; resp.StatusCode != http.StatusOK || string(body) != want {
		t.Errorf("tags/list: %d %s, want %s", resp.StatusCode, body, want)
	}
	resp, body = request(t, http.MethodGet, r.a.URL+"/v2/_catalog", nil, 0)
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), `"`+r.name+`"`) {
		t.Errorf("_catalog: %d %s", resp.StatusCode, body)
	}
}

// randomHex returns 8 random hex digits for repository names.
func randomHex(t *testing.T) string {
	t.Helper()
	b := make([]byte, 4)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(b)
}
