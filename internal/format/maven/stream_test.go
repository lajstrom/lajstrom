package maven

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
	"github.com/lajstrom/lajstrom/internal/provider"
	"github.com/lajstrom/lajstrom/internal/provider/providertest"
)

// patterned returns n bytes of formattest.Pattern.
func patterned(t *testing.T, n int64) []byte {
	t.Helper()
	b, err := io.ReadAll(io.LimitReader(&formattest.Pattern{}, n))
	must(t, err)
	return b
}

// TestDeployWritesNoTempFile: with a TMPDIR that takes no file, a deploy whose jar has several
// chunks, sent once with its length and once without, publishes the release, and the jar is
// served back: every file streams into the registry and nothing is spooled (ADR 0014). No
// upload session is left open.
func TestDeployWritesNoTempFile(t *testing.T) {
	dir := formattest.NoTempDir(t)
	e := newDeployEnv(t, nil)
	e.fake.ChunkSize = 16 << 10
	files := release("1.2.0", string(patterned(t, 300<<10)))
	jar := files[0]
	if got := e.deploy("mvn", files, metadataDoc("1.2.0")); got != http.StatusCreated {
		t.Fatalf("the metadata PUT: %d, want 201", got)
	}
	again := release("1.3.0", string(patterned(t, 200<<10)))
	req := httptest.NewRequest(http.MethodPut, "/maven/releases/"+again[0].path, io.MultiReader(bytes.NewReader(again[0].body)))
	req.Header.Set("X-Test-User", "alice")
	rr := httptest.NewRecorder()
	e.h.ServeHTTP(rr, req)
	if rr.Code != http.StatusCreated {
		t.Fatalf("PUT without a length: %d %s", rr.Code, rr.Body)
	}
	e.wantPut(again[3].path, again[3].body, http.StatusCreated)
	if got := e.put("alice", metadataPath, metadataDoc("1.2.0", "1.3.0")).Code; got != http.StatusCreated {
		t.Fatalf("the metadata PUT of 1.3.0: %d, want 201", got)
	}
	for _, f := range []deployFile{jar, again[0]} {
		req := httptest.NewRequest(http.MethodGet, "/maven/releases/"+f.path, nil)
		req.Header.Set("X-Test-User", "alice")
		rr := httptest.NewRecorder()
		e.h.ServeHTTP(rr, req)
		if rr.Code != http.StatusOK || !bytes.Equal(rr.Body.Bytes(), f.body) {
			t.Errorf("GET %s: %d with %d bytes, want 200 with the %d deployed", f.path, rr.Code, rr.Body.Len(), len(f.body))
		}
	}
	if n := e.fake.OpenUploads(); n != 0 {
		t.Errorf("%d upload sessions open", n)
	}
	if entries, err := os.ReadDir(dir); err != nil || len(entries) != 0 {
		t.Errorf("TMPDIR holds %v, %v", entries, err)
	}
}

// TestDeployRefusedStoresNothing: a file refused before its body streams or while it streams
// stores nothing: no blob in the registry, no session left open in it, no file in the
// publication session; and the release does not commit without it.
func TestDeployRefusedStoresNothing(t *testing.T) {
	files := release("1.2.0", string(patterned(t, 4<<10)))
	jar := files[0]
	dir := jar.path[:strings.LastIndex(jar.path, "/")+1]
	for _, tt := range []struct {
		name     string
		policy   string
		registry func(*providertest.Fake) provider.Provider // over the env's fake; nil: the fake
		req      func(r *http.Request)
		status   int
		want     string
	}{
		{name: "maxFileSize, cut while the body streams", policy: `{"maxFileSize":2000}`,
			req: func(r *http.Request) { r.ContentLength = -1 }, status: http.StatusRequestEntityTooLarge, want: "maxFileSize"},
		{name: "maxFileSize, a Content-Length above it", policy: `{"maxFileSize":2000}`, status: http.StatusRequestEntityTooLarge,
			want: "maxFileSize"},
		{name: "a body shorter than its Content-Length", req: func(r *http.Request) { r.ContentLength += 10 },
			status: http.StatusBadRequest, want: "Content-Length says"},
		{name: "the session is full", policy: `{"maven":{"maxSessionFiles":1}}`, status: http.StatusBadRequest,
			want: "the bound policy.maven.maxSessionFiles"},
		{name: "the registry gets other bytes than were hashed", registry: providertest.Corrupting, status: http.StatusInternalServerError},
	} {
		t.Run(tt.name, func(t *testing.T) {
			e := newDeployEnvOver(t, nil, tt.registry)
			e.fake.ChunkSize = 512
			if tt.policy != "" {
				e.repo.Policy = json.RawMessage(tt.policy)
				must(t, e.st.UpdateRepo(t.Context(), &e.repo))
			}
			if tt.registry == nil { // a session with a file; the corrupting registry refuses every file
				e.wantPut(dir+"lib-core-1.2.0.pom", files[3].body, http.StatusCreated)
			}
			req := httptest.NewRequest(http.MethodPut, "/maven/releases/"+jar.path, bytes.NewReader(jar.body))
			req.Header.Set("X-Test-User", "alice")
			if tt.req != nil {
				tt.req(req)
			}
			rr := httptest.NewRecorder()
			e.h.ServeHTTP(rr, req)
			if rr.Code != tt.status || !strings.Contains(rr.Body.String(), tt.want) {
				t.Fatalf("PUT = %d %s, want %d with %q", rr.Code, rr.Body, tt.status, tt.want)
			}
			repo, err := e.fake.Repository(t.Context(), "maven/releases/com.example/lib-core")
			must(t, err)
			stored, err := repo.Exists(t.Context(), ocispec.Descriptor{Digest: digest.FromBytes(jar.body), Size: int64(len(jar.body))})
			must(t, err)
			if rr := e.put("alice", jar.path+".sha1", []byte(sumsOf(jar.body)[SHA1])); stored || e.fake.OpenUploads() != 0 ||
				!strings.Contains(rr.Body.String(), "which this deploy has not uploaded") {
				t.Errorf("stored %v, %d sessions open, its checksum %d %s; want nothing stored and the jar not in the session",
					stored, e.fake.OpenUploads(), rr.Code, rr.Body)
			}
		})
	}
}

// TestDeployStreams1GiB deploys a jar of 1 GiB over a real connection, with a TMPDIR that takes
// no file and the generic provider's chunks of 8 MiB, then its POM and maven-metadata.xml, and
// checks that the version has the jar with the size and the sha256 of the bytes, and that the
// heap in use stayed under formattest.HeapLimit: the jar is neither spooled nor held. The fake
// hashes the chunks and keeps none of a blob above providertest.MaxKept.
func TestDeployStreams1GiB(t *testing.T) {
	if testing.Short() {
		t.Skip("moves 1 GiB; skipped under -short")
	}
	const size = 1 << 30
	formattest.NoTempDir(t)
	e := newDeployEnv(t, nil)
	e.fake.ChunkSize = 8 << 20
	srv := httptest.NewServer(e.h)
	defer srv.Close()
	files := release("1.2.0", "")
	jar, pom := files[0], files[3]
	peak := formattest.SampleHeap(t)

	h := sha256.New()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPut, srv.URL+"/maven/releases/"+jar.path,
		io.TeeReader(io.LimitReader(&formattest.Pattern{}, size), h))
	must(t, err)
	req.ContentLength = size
	req.Header.Set("X-Test-User", "alice")
	resp, err := http.DefaultClient.Do(req)
	must(t, err)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("PUT of the jar = %d", resp.StatusCode)
	}
	top := peak()
	e.wantPut(pom.path, pom.body, http.StatusCreated)
	if got := e.put("alice", metadataPath, metadataDoc("1.2.0")).Code; got != http.StatusCreated {
		t.Fatalf("the metadata PUT: %d, want 201", got)
	}
	vs := e.versions()
	if len(vs) != 1 {
		t.Fatalf("versions %+v, want 1.2.0", vs)
	}
	fs, err := e.st.ListFiles(t.Context(), vs[0].ID)
	must(t, err)
	d := digest.NewDigest(digest.SHA256, h)
	var found bool
	for _, f := range fs {
		found = found || (f.Digest == d && f.Size == size)
	}
	if !found {
		t.Errorf("files %+v, want one of %d bytes with %s", fs, int64(size), d)
	}
	t.Logf("peak heap in use: %d MiB", top>>20)
	if top > formattest.HeapLimit {
		t.Errorf("peak heap in use %d MiB, want under %d MiB", top>>20, formattest.HeapLimit>>20)
	}
}
