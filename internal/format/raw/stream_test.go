package raw

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/lajstrom/lajstrom/internal/format/formattest"
	"github.com/lajstrom/lajstrom/internal/oci/profile"
	"github.com/lajstrom/lajstrom/internal/provider"
	"github.com/lajstrom/lajstrom/internal/provider/providertest"
)

// TestPutWritesNoTempFile: with a TMPDIR that takes no file, a PUT of several chunks, with its
// length and without, is 201 and serves the bytes back: the body streams into the registry and
// nothing is spooled (ADR 0014). No upload session is left open.
func TestPutWritesNoTempFile(t *testing.T) {
	dir := formattest.NoTempDir(t)
	e := newEnv(t, envOptions{})
	content := strings.Repeat("streamed, never spooled. ", 20_000) // 500 KB: 8 chunks of the fake
	e.put("big/known.txt", content)
	rr := e.do(http.MethodPut, target("big/chunked.txt"), io.MultiReader(strings.NewReader(content)))
	if rr.Code != http.StatusCreated {
		t.Fatalf("PUT without a length = %d %s", rr.Code, rr.Body)
	}
	for _, p := range []string{"big/known.txt", "big/chunked.txt"} {
		if got := e.do(http.MethodGet, target(p), nil); got.Code != http.StatusOK || got.Body.String() != content {
			t.Errorf("GET %s = %d with %d bytes, want 200 with the %d uploaded", p, got.Code, got.Body.Len(), len(content))
		}
	}
	if n := e.prov.OpenUploads(); n != 0 {
		t.Errorf("%d upload sessions open", n)
	}
	if entries, err := os.ReadDir(dir); err != nil || len(entries) != 0 {
		t.Errorf("TMPDIR holds %v, %v", entries, err)
	}
}

// TestPutRefusedStoresNothing: a PUT refused while its body streams or after it, by the
// policy's maxFileSize, a short body, the registry getting other bytes than were hashed, or
// the tag being taken in the registry, stores nothing: no blob in the registry, no session left
// open, no manifest, no version. The idempotent repeat stores nothing new either.
func TestPutRefusedStoresNothing(t *testing.T) {
	const content = "a file that does not make it into the registry, at least eight chunks long. "
	body := strings.Repeat(content, 8)
	for _, tt := range []struct {
		name     string
		policy   string
		registry func(*providertest.Fake) provider.Provider // over the env's fake; nil: the fake
		setup    func(t *testing.T, e *env)                 // before the PUT
		req      func(r *http.Request)
		status   int
	}{
		{name: "maxFileSize, cut while the body streams", policy: `{"maxFileSize":200}`,
			req: func(r *http.Request) { r.ContentLength = -1 }, status: http.StatusRequestEntityTooLarge},
		{name: "maxFileSize, a Content-Length above it", policy: `{"maxFileSize":200}`, status: http.StatusRequestEntityTooLarge},
		{name: "a body shorter than its Content-Length", req: func(r *http.Request) { r.ContentLength += 10 }, status: http.StatusBadRequest},
		{name: "the registry gets other bytes than were hashed",
			registry: providertest.Corrupting, status: http.StatusInternalServerError},
		{name: "the tag is taken in the registry", setup: func(t *testing.T, e *env) {
			e.squat(t, "big/refused.txt", profile.Tag(digest.FromString(body).Encoded()[:versionLen], ""))
		}, status: http.StatusConflict},
	} {
		t.Run(tt.name, func(t *testing.T) {
			f := &providertest.Fake{RegistryName: "local", ChunkSize: 64}
			var registry provider.Provider = f
			if tt.registry != nil {
				registry = tt.registry(f)
			}
			e := newEnv(t, envOptions{policy: tt.policy, registry: registry})
			e.prov = f
			if tt.setup != nil {
				tt.setup(t, e)
			}
			pushed := len(e.prov.PushReferences())
			req := httptest.NewRequest(http.MethodPut, target("big/refused.txt"), strings.NewReader(body))
			if tt.req != nil {
				tt.req(req)
			}
			rr := httptest.NewRecorder()
			e.h.ServeHTTP(rr, req)
			if rr.Code != tt.status {
				t.Fatalf("PUT = %d %s, want %d", rr.Code, rr.Body, tt.status)
			}
			e.nothingStored(t, body, pushed)
		})
	}
	t.Run("the idempotent repeat", func(t *testing.T) {
		e := newEnv(t, envOptions{})
		e.prov.ChunkSize = 64
		first := e.put("big/refused.txt", body)
		if again := e.put("big/refused.txt", body); again.Digest != first.Digest {
			t.Errorf("repeat = %+v, want the digest %s", again, first.Digest)
		}
		if n, open := len(e.prov.PushReferences()), e.prov.OpenUploads(); n != 1 || open != 0 {
			t.Errorf("%d manifests pushed, %d sessions open; want 1 and none", n, open)
		}
	})
}

// TestPutStalled: a client that stops sending part way, whose body server.stallTimeout cuts, is
// 408 with a message that names neither connection, not a 500 at ERROR (#576); nothing is
// stored. With its length and without.
func TestPutStalled(t *testing.T) {
	body := strings.Repeat("a file whose client stops sending part way. ", 8)
	for _, length := range []int64{int64(len(body)), -1} {
		e := newEnv(t, envOptions{})
		e.prov.ChunkSize = 64
		req := httptest.NewRequest(http.MethodPut, target("big/refused.txt"), formattest.Stalled(strings.NewReader(body[:200])))
		req.ContentLength = length
		rr := httptest.NewRecorder()
		e.h.ServeHTTP(rr, req)
		formattest.CheckStalled(t, rr)
		if logs := e.logs.String(); strings.Contains(logs, "level=ERROR") {
			t.Errorf("Content-Length %d: logged %s", length, logs)
		}
		e.nothingStored(t, body, 0)
	}
}

// nothingStored fails the test when the registry holds content as a blob of big/refused.txt,
// or the empty config, or any blob pushed to it (the metadata layer, #559), a session is open,
// more manifests than pushed were pushed, or the path has a version.
func (e *env) nothingStored(t *testing.T, content string, pushed int) {
	t.Helper()
	const name = "raw/scratch/big/refused.txt"
	rr, err := e.prov.Repository(t.Context(), name)
	must(t, err)
	stored, err := rr.Exists(t.Context(), ocispec.Descriptor{Digest: digest.FromString(content), Size: int64(len(content))})
	must(t, err)
	config, err := rr.Exists(t.Context(), profile.EmptyConfig)
	must(t, err)
	var blobs []digest.Digest
	for _, p := range e.prov.Pushes() {
		if p.Name == name {
			blobs = append(blobs, p.Digest)
		}
	}
	if stored || config || len(blobs) != 0 || e.prov.OpenUploads() != 0 || len(e.prov.PushReferences()) != pushed ||
		len(e.versions("big/refused.txt")) != 0 {
		t.Errorf("file stored %v, empty config stored %v, blobs pushed %v, %d sessions open, %d manifests pushed (%d before), "+
			"%d versions; want nothing", stored, config, blobs, e.prov.OpenUploads(), len(e.prov.PushReferences()), pushed,
			len(e.versions("big/refused.txt")))
	}
}

// squat pushes a manifest of nothing under tag to the OCI repository of path p, straight to the
// registry: a write that bypassed Lajstrom.
func (e *env) squat(t *testing.T, p, tag string) {
	t.Helper()
	m := []byte(`{"schemaVersion":2,"mediaType":"` + ocispec.MediaTypeImageManifest + `",` +
		`"config":{"mediaType":"` + ocispec.MediaTypeEmptyJSON + `","digest":"` + ocispec.DescriptorEmptyJSON.Digest.String() + `","size":2},"layers":[]}`)
	rr, err := e.prov.Repository(t.Context(), "raw/scratch/"+p)
	must(t, err)
	d := ocispec.Descriptor{MediaType: ocispec.MediaTypeImageManifest, Digest: digest.FromBytes(m), Size: int64(len(m))}
	must(t, rr.PushReference(t.Context(), d, bytes.NewReader(m), tag))
}

// TestPutStreams1GiB uploads 1 GiB over a real connection, with a TMPDIR that takes no file and
// the generic provider's chunks of 8 MiB, and checks that it is 201 with the size and the sha256
// of the bytes, and that the heap in use stayed under formattest.HeapLimit: the body is neither
// spooled nor held. The fake hashes the chunks and keeps none of a blob above
// providertest.MaxKept.
func TestPutStreams1GiB(t *testing.T) {
	if testing.Short() {
		t.Skip("moves 1 GiB; skipped under -short")
	}
	const size = 1 << 30
	formattest.NoTempDir(t)
	e := newEnv(t, envOptions{})
	e.prov.ChunkSize = 8 << 20
	srv := httptest.NewServer(e.h)
	defer srv.Close()
	peak := formattest.SampleHeap(t)

	h := sha256.New()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPut, srv.URL+target("big/one.bin"),
		io.TeeReader(io.LimitReader(&formattest.Pattern{}, size), h))
	must(t, err)
	req.ContentLength = size
	resp, err := http.DefaultClient.Do(req)
	must(t, err)
	defer func() { _ = resp.Body.Close() }()
	var got putResponse
	if resp.StatusCode != http.StatusCreated || json.NewDecoder(resp.Body).Decode(&got) != nil {
		t.Fatalf("PUT = %d", resp.StatusCode)
	}
	d := digest.NewDigest(digest.SHA256, h)
	if got.Size != size || got.Version != d.Encoded()[:versionLen] {
		t.Errorf("PUT = %+v, want %d bytes, version of %s", got, size, d)
	}
	top := peak()
	t.Logf("peak heap in use: %d MiB", top>>20)
	if top > formattest.HeapLimit {
		t.Errorf("peak heap in use %d MiB, want under %d MiB", top>>20, formattest.HeapLimit>>20)
	}
}
