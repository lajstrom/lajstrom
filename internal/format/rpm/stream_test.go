package rpm

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/opencontainers/go-digest"

	"github.com/lajstrom/lajstrom/internal/format/formattest"
	"github.com/lajstrom/lajstrom/internal/testutil"
)

// TestUploadStreams1GiB uploads an rpm with a payload of 1 GiB through the upload route over a
// real connection, without a temporary directory, and checks that the heap in use stays under
// formattest.HeapLimit: the route holds the head, the inspector counts the payload, and the
// provider holds one chunk (ADR 0014, INV-25). The head is that of a package without SIZE and
// a payload digest, so that the payload may be any bytes.
func TestUploadStreams1GiB(t *testing.T) {
	if testing.Short() {
		t.Skip("moves 1 GiB; skipped under -short")
	}
	const size = 1 << 30
	formattest.NoTempDir(t)
	e := newEnv(t, envOptions{})
	e.prov.ChunkSize = 8 << 20
	srv := httptest.NewServer(e.h)
	defer srv.Close()
	hello, info := testutil.RPM(testutil.RPMOptions{NoDigests: true})
	head := hello[:info.HeaderEnd:info.HeaderEnd]
	peak := formattest.SampleHeap(t)

	h := sha256.New()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPut, srv.URL+"/rpm/el9/upload",
		io.TeeReader(io.MultiReader(bytes.NewReader(head), io.LimitReader(&formattest.Pattern{}, size)), h))
	must(t, err)
	req.ContentLength = int64(len(head)) + size
	req.Header.Set("X-Test-User", "alice")
	resp, err := http.DefaultClient.Do(req)
	must(t, err)
	defer func() { _ = resp.Body.Close() }()
	var got uploadResponse
	if resp.StatusCode != http.StatusCreated || json.NewDecoder(resp.Body).Decode(&got) != nil {
		t.Fatalf("PUT = %d", resp.StatusCode)
	}
	top := peak()
	files, err := e.st.FindFiles(t.Context(), e.repo.ID, info.Filename)
	must(t, err)
	if d := digest.NewDigest(digest.SHA256, h); len(files) != 1 || files[0].File.Digest != d || files[0].File.Size != req.ContentLength {
		t.Errorf("files %+v, want one of %d bytes with %s", files, req.ContentLength, d)
	}
	t.Logf("peak heap in use: %d MiB", top>>20)
	if top > formattest.HeapLimit {
		t.Errorf("peak heap in use %d MiB, want under %d MiB", top>>20, formattest.HeapLimit>>20)
	}
}
