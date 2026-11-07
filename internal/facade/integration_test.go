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
	"github.com/lajstrom/lajstrom/internal/ingest"
	"github.com/lajstrom/lajstrom/internal/oci/profile"
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

// TestIntegrationRawPush is the first criterion of #25 against a real registry: a raw file
// pushed as oras pushes it, the blob and then the manifest through the facade, is indexed by the
// pipeline and served by the raw route; another manifest under the tag is 409 with the digest
// the tag holds, a manifest without an annotation 400 naming it.
func TestIntegrationRawPush(t *testing.T) {
	addr := os.Getenv("LAJSTROM_TEST_REGISTRY")
	if addr == "" {
		t.Skip("set LAJSTROM_TEST_REGISTRY=host:port to run against a registry")
	}
	ctx := t.Context()
	st, err := sqlite.Open(ctx, fmt.Sprintf("file:facade-it-%d?mode=memory&cache=shared", memoryDBs.Add(1)))
	must(t, err)
	t.Cleanup(func() { _ = st.Close() })
	must(t, st.Migrate(ctx))
	repo := index.Repo{Format: "raw", Name: "assets", Registry: "it", Prefix: "raw/assets", Kind: "hosted"}
	must(t, st.CreateRepo(ctx, &repo))
	p, err := generic.New(config.Registry{Name: "it", Type: generic.Type, URL: "http://" + addr, Prefix: os.Getenv("LAJSTROM_TEST_REGISTRY_PREFIX"),
		Auth: config.RegistryAuth{Username: os.Getenv("LAJSTROM_TEST_REGISTRY_USERNAME"), Password: os.Getenv("LAJSTROM_TEST_REGISTRY_PASSWORD")}}, nil)
	must(t, err)
	providers := provider.Map{"it": p}
	logs := &logBuffer{}
	log := slog.New(slog.NewTextHandler(logs, nil))
	srv := httptest.NewServer(New(Options{Index: st, Providers: providers, Log: log,
		Ingest: ingest.New(ingest.Options{Providers: providers, Index: st, Log: log})}))
	t.Cleanup(srv.Close)

	f := rawFileOf("it-"+randomHex(t)+"/hello.txt", "hello lajstrom "+randomHex(t)+"\n")
	name := srv.URL + "/v2/raw/assets/" + f.path
	resp, body := request(t, http.MethodPost, name+"/blobs/uploads/?digest="+f.digest.String(), bytes.NewReader(f.content), int64(len(f.content)))
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("blob: %d %s", resp.StatusCode, body)
	}
	m, d := f.body(t)
	resp, body = request(t, http.MethodPut, name+"/manifests/"+f.tag(), bytes.NewReader(m), int64(len(m)), "Content-Type", ocispecManifest)
	if resp.StatusCode != http.StatusCreated || resp.Header.Get("Docker-Content-Digest") != d.String() {
		t.Fatalf("manifest: %d %s\n%s", resp.StatusCode, body, logs)
	}
	rr := httptest.NewRecorder()
	rawRoutes(t, st, providers).ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/raw/assets/"+f.path, nil))
	switch {
	case rr.Code == http.StatusTemporaryRedirect:
		t.Logf("the registry redirects blob downloads: %s", redacted(rr.Header().Get("Location")))
	case rr.Code != http.StatusOK || !bytes.Equal(rr.Body.Bytes(), f.content):
		t.Errorf("GET /raw/assets/%s: %d %q", f.path, rr.Code, rr.Body)
	}

	f.m.Annotations[ocispec.AnnotationCreated] = "2026-11-07T10:00:01Z"
	m, _ = f.body(t)
	resp, body = request(t, http.MethodPut, name+"/manifests/"+f.tag(), bytes.NewReader(m), int64(len(m)), "Content-Type", ocispecManifest)
	if resp.StatusCode != http.StatusConflict || !strings.Contains(string(body), `"existing":"`+d.String()+`"`) {
		t.Errorf("another manifest under the tag: %d %s", resp.StatusCode, body)
	}
	delete(f.m.Annotations, profile.AnnotationRepo)
	m, _ = f.body(t)
	resp, body = request(t, http.MethodPut, name+"/manifests/"+f.tag(), bytes.NewReader(m), int64(len(m)), "Content-Type", ocispecManifest)
	if resp.StatusCode != http.StatusBadRequest || !strings.Contains(string(body), "lajstrom.repo is missing") {
		t.Errorf("a missing annotation: %d %s", resp.StatusCode, body)
	}
}

// TestIntegrationBlobReads reads a blob through the facade from the registry of
// LAJSTROM_TEST_REGISTRY, which streams (Zot and distribution on the filesystem): GET with
// the whole blob, a Range answered 206 by the backend with those bytes, a range past the end
// 416, HEAD with the size and no body, If-None-Match 304. On a registry that redirects, GET is
// a 307 and the test follows it.
func TestIntegrationBlobReads(t *testing.T) {
	r := newReplicas(t)
	b := bytes.Repeat([]byte("blob read "+randomHex(t)+" "), 400)
	d := digest.FromBytes(b)
	resp, body := request(t, http.MethodPost, r.a.URL+"/v2/"+r.name+"/blobs/uploads/?digest="+d.String(), bytes.NewReader(b), int64(len(b)))
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("blob: %d %s", resp.StatusCode, body)
	}
	blob := r.b.URL + "/v2/" + r.name + "/blobs/" + d.String()
	resp, body = request(t, http.MethodGet, blob, nil, 0)
	if resp.StatusCode == http.StatusTemporaryRedirect {
		t.Logf("the registry redirects blob downloads to %s", redacted(resp.Header.Get("Location")))
		resp, body = request(t, http.MethodGet, resp.Header.Get("Location"), nil, 0)
	}
	if resp.StatusCode != http.StatusOK || !bytes.Equal(body, b) || resp.ContentLength != int64(len(b)) {
		t.Errorf("GET: %d, %d bytes, Content-Length %d", resp.StatusCode, len(body), resp.ContentLength)
	}
	if resp.Header.Get("Docker-Content-Digest") != d.String() && resp.Header.Get("Server") != "MinIO" {
		t.Errorf("GET: Docker-Content-Digest %q", resp.Header.Get("Docker-Content-Digest"))
	}

	resp, body = request(t, http.MethodGet, blob, nil, 0, "Range", "bytes=1000-1999")
	if resp.StatusCode == http.StatusTemporaryRedirect {
		resp, body = request(t, http.MethodGet, resp.Header.Get("Location"), nil, 0, "Range", "bytes=1000-1999")
	}
	if resp.StatusCode != http.StatusPartialContent || !bytes.Equal(body, b[1000:2000]) ||
		resp.Header.Get("Content-Range") != fmt.Sprintf("bytes 1000-1999/%d", len(b)) {
		t.Errorf("GET with a Range: %d, %d bytes, Content-Range %q", resp.StatusCode, len(body), resp.Header.Get("Content-Range"))
	}
	t.Logf("GET with a Range: %d, Content-Range %s, Accept-Ranges %q", resp.StatusCode, resp.Header.Get("Content-Range"), resp.Header.Get("Accept-Ranges"))
	resp, _ = request(t, http.MethodGet, blob, nil, 0, "Range", fmt.Sprintf("bytes=%d-", len(b)))
	if resp.StatusCode != http.StatusRequestedRangeNotSatisfiable && resp.StatusCode != http.StatusTemporaryRedirect {
		t.Errorf("GET with a range past the end: %d", resp.StatusCode)
	}

	resp, body = request(t, http.MethodHead, blob, nil, 0)
	if resp.StatusCode != http.StatusOK || len(body) != 0 || resp.ContentLength != int64(len(b)) || resp.Header.Get("Docker-Content-Digest") != d.String() {
		t.Errorf("HEAD: %d, Content-Length %d, %d bytes of body", resp.StatusCode, resp.ContentLength, len(body))
	}
	if resp, _ := request(t, http.MethodGet, blob, nil, 0, "If-None-Match", `"`+d.String()+`"`); resp.StatusCode != http.StatusNotModified {
		t.Errorf("GET with If-None-Match: %d", resp.StatusCode)
	}
}

// TestIntegrationS3Redirect is the criterion of #27 on distribution with the S3 driver, the s3
// profile of the dev stack (LAJSTROM_TEST_REGISTRY_S3=localhost:5002): a raw file pushed
// through the facade, as oras pushes it, is a 307 to a presigned URL of the object store on a
// GET through the facade and through /raw/assets/<path>, and following the Location, as curl
// -L does, gives bytes whose digest is the blob's, a Range too; HEAD answers the size; with
// policy.blobRedirect never both routes stream the bytes from the backend.
func TestIntegrationS3Redirect(t *testing.T) {
	addr := os.Getenv("LAJSTROM_TEST_REGISTRY_S3")
	if addr == "" {
		t.Skip("set LAJSTROM_TEST_REGISTRY_S3=host:port (the s3 profile of the dev stack: localhost:5002) to run against distribution on S3")
	}
	ctx := t.Context()
	st, err := sqlite.Open(ctx, fmt.Sprintf("file:facade-it-%d?mode=memory&cache=shared", memoryDBs.Add(1)))
	must(t, err)
	t.Cleanup(func() { _ = st.Close() })
	must(t, st.Migrate(ctx))
	repo := index.Repo{Format: "raw", Name: "assets", Registry: "s3", Prefix: "raw/assets", Kind: "hosted"}
	must(t, st.CreateRepo(ctx, &repo))
	p, err := generic.New(config.Registry{Name: "s3", Type: generic.Type, URL: "http://" + addr, Prefix: "lajstrom-it/"}, nil)
	must(t, err)
	providers := provider.Map{"s3": p}
	logs := &logBuffer{}
	log := slog.New(slog.NewTextHandler(logs, nil))
	srv := httptest.NewServer(New(Options{Index: st, Providers: providers, Log: log,
		Ingest: ingest.New(ingest.Options{Providers: providers, Index: st, Log: log})}))
	t.Cleanup(srv.Close)
	raw := httptest.NewServer(rawRoutes(t, st, providers))
	t.Cleanup(raw.Close)

	f := rawFileOf("it-"+randomHex(t)+"/big.bin", strings.Repeat("s3 redirect "+randomHex(t)+"\n", 3000))
	name := srv.URL + "/v2/raw/assets/" + f.path
	resp, body := request(t, http.MethodPost, name+"/blobs/uploads/?digest="+f.digest.String(), bytes.NewReader(f.content), int64(len(f.content)))
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("blob: %d %s", resp.StatusCode, body)
	}
	m, md := f.body(t)
	resp, body = request(t, http.MethodPut, name+"/manifests/"+f.tag(), bytes.NewReader(m), int64(len(m)), "Content-Type", ocispecManifest)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("manifest: %d %s\n%s", resp.StatusCode, body, logs)
	}
	routes := []blobRoute{
		{name: "facade", url: name + "/blobs/" + f.digest.String(), digest: f.digest},
		{name: "raw", url: raw.URL + "/raw/assets/" + f.path, digest: md},
	}
	for _, rt := range routes {
		rt.checkRedirected(t, f)
	}
	repo.Policy = []byte(`{"blobRedirect": "never"}`)
	must(t, st.UpdateRepo(ctx, &repo))
	for _, rt := range routes {
		rt.checkStreamed(t, f)
	}
	t.Logf("policy.blobRedirect never: both routes stream, a Range is answered 206 by the object store through the provider")
}

// blobRoute is a URL that answers the blob of a raw file: the facade's, or the raw route's.
type blobRoute struct {
	name   string
	url    string
	digest digest.Digest // the Docker-Content-Digest it answers: the blob's, or the manifest's
}

// noFollow is a client that returns a redirect as the answer.
var noFollow = &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}

// checkRedirected checks that GET is a 307 whose Location, followed without Lajstrom's
// credentials as curl -L does, gives the bytes of f, a Range too, and that HEAD is 200.
func (rt blobRoute) checkRedirected(t *testing.T, f *rawFile) {
	t.Helper()
	resp := answer(t, noFollow, http.MethodGet, rt.url)
	loc := resp.Header.Get("Location")
	if resp.StatusCode != http.StatusTemporaryRedirect || loc == "" {
		t.Fatalf("%s: GET %d, Location %q; want 307", rt.name, resp.StatusCode, loc)
	}
	if got := resp.Header.Get("Docker-Content-Digest"); got != rt.digest.String() {
		t.Errorf("%s: Docker-Content-Digest %q, want %s", rt.name, got, rt.digest)
	}
	t.Logf("%s: GET 307, Location %s", rt.name, redacted(loc))
	resp, body := request(t, http.MethodGet, rt.url, nil, 0)
	if got := digest.FromBytes(body); resp.StatusCode != http.StatusOK || got != f.digest {
		t.Errorf("%s: following the Location gave %d, %d bytes with digest %s; want %s", rt.name, resp.StatusCode, len(body), got, f.digest)
	} else {
		t.Logf("%s: following the Location gave %d bytes with digest %s, the blob's (served by %s)", rt.name, len(body), got, resp.Header.Get("Server"))
	}
	resp, body = request(t, http.MethodGet, rt.url, nil, 0, "Range", "bytes=100-199")
	if resp.StatusCode != http.StatusPartialContent || !bytes.Equal(body, f.content[100:200]) {
		t.Errorf("%s: a Range after the redirect gave %d, %d bytes", rt.name, resp.StatusCode, len(body))
	}
	resp, body = request(t, http.MethodHead, rt.url, nil, 0)
	if resp.StatusCode != http.StatusOK || len(body) != 0 || resp.ContentLength != int64(len(f.content)) || resp.Header.Get("Location") != "" {
		t.Errorf("%s: HEAD %d, Content-Length %d, Location %q", rt.name, resp.StatusCode, resp.ContentLength, resp.Header.Get("Location"))
	}
}

// checkStreamed checks that GET streams the bytes of f with policy.blobRedirect never, and a
// Range is answered 206 through the provider.
func (rt blobRoute) checkStreamed(t *testing.T, f *rawFile) {
	t.Helper()
	what := rt.name + " with policy.blobRedirect never"
	resp := answer(t, noFollow, http.MethodGet, rt.url)
	if resp.StatusCode != http.StatusOK || resp.Header.Get("Location") != "" || resp.ContentLength != int64(len(f.content)) {
		t.Errorf("%s: GET %d, Location %q, Content-Length %d; want the stream", what, resp.StatusCode, resp.Header.Get("Location"), resp.ContentLength)
	}
	if _, body := request(t, http.MethodGet, rt.url, nil, 0); digest.FromBytes(body) != f.digest {
		t.Errorf("%s: %d bytes with digest %s", what, len(body), digest.FromBytes(body))
	}
	resp, body := request(t, http.MethodGet, rt.url, nil, 0, "Range", "bytes=100-199")
	if resp.StatusCode != http.StatusPartialContent || !bytes.Equal(body, f.content[100:200]) ||
		resp.Header.Get("Content-Range") != fmt.Sprintf("bytes 100-199/%d", len(f.content)) {
		t.Errorf("%s: a Range gave %d, %d bytes, Content-Range %q", what, resp.StatusCode, len(body), resp.Header.Get("Content-Range"))
	}
}

// answer sends method to u with client and returns the answer, its body read and closed.
func answer(t *testing.T, client *http.Client, method, u string) *http.Response {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), method, u, nil)
	must(t, err)
	resp, err := client.Do(req)
	must(t, err)
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	return resp
}

// redacted is a URL without its query, which carries a signature.
func redacted(u string) string {
	base, query, ok := strings.Cut(u, "?")
	if !ok {
		return u
	}
	var names []string
	for _, kv := range strings.Split(query, "&") {
		k, _, _ := strings.Cut(kv, "=")
		names = append(names, k)
	}
	return base + "?" + strings.Join(names, "&") + " (values left out)"
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
