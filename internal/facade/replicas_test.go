package facade

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/opencontainers/go-digest"
)

// TestUploadAcrossReplicas is the lead's criterion on #24: two facades over one database and
// one backend, sharing nothing else; the session starts on A, the chunks go to B, the status
// is asked of B and the finish goes to A, and the blob is in the backend with its digest.
func TestUploadAcrossReplicas(t *testing.T) {
	e := newEnv(t, envOptions{})
	a, b := e.h, e.replica()
	id := e.start(a)
	chunks := []string{"started on A, ", "chunked on B, ", "and again on B, ", "finished on A"}
	blob := strings.Join(chunks, "")
	off := 0
	for i, c := range chunks[:3] {
		h := map[bool]http.Handler{true: b, false: a}[i > 0]
		rr := e.do(h, http.MethodPatch, uploads+id, strings.NewReader(c), "Content-Range", fmt.Sprintf("%d-%d", off, off+len(c)-1))
		wantStatus(t, fmt.Sprintf("chunk %d", i), rr, http.StatusAccepted)
		off += len(c)
		wantHeader(t, fmt.Sprintf("chunk %d", i), rr, "Range", fmt.Sprintf("0-%d", off-1))
	}
	rr := e.do(b, http.MethodGet, uploads+id, nil)
	wantHeader(t, "status on B", rr, "Range", fmt.Sprintf("0-%d", off-1))
	d := digest.FromString(blob)
	last := chunks[3]
	rr = e.do(a, http.MethodPut, uploads+id+"?digest="+d.String(), strings.NewReader(last),
		"Content-Range", fmt.Sprintf("%d-%d", off, off+len(last)-1))
	wantCreated(t, "finish on A", rr, d)
	e.wantBlob("oci/images/app", []byte(blob))
	if n := e.prov.OpenUploads(); n != 0 {
		t.Errorf("%d backend sessions open", n)
	}
}

// TestMonolithicUploadAbandoned is the lead's second criterion: replica A dies in the middle of
// a monolithic upload, the client's retry on B succeeds, and nothing is left behind. There is
// no spool to leave (the body streams to the backend, and the temporary directory stays empty);
// A's backend session is cancelled, not orphaned, which the backend's count shows.
func TestMonolithicUploadAbandoned(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp)
	e := newEnv(t, envOptions{})
	srvA := httptest.NewServer(e.h)
	defer srvA.Close()
	srvB := httptest.NewServer(e.replica())
	defer srvB.Close()

	blob := bytes.Repeat([]byte("lajstrom "), 100_000)
	d := digest.FromBytes(blob)
	pr, pw := io.Pipe()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, srvA.URL+uploads+"?digest="+d.String(), pr)
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
	if _, err := pw.Write(blob[:300_000]); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "A's backend session", func() bool { return e.prov.OpenUploads() == 1 })
	srvA.CloseClientConnections() // replica A goes away mid-body
	pw.CloseWithError(errors.New("the client saw A go"))
	if err := <-done; err != nil {
		t.Logf("the client's request to A: %v", err)
	}
	waitFor(t, "A's backend session cancelled", func() bool { return e.prov.OpenUploads() == 0 })

	resp, err := http.Post(srvB.URL+uploads+"?digest="+d.String(), octetStream, bytes.NewReader(blob))
	must(t, err)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("retry on B: status %d", resp.StatusCode)
	}
	e.wantBlob("oci/images/app", blob)
	if n := e.prov.OpenUploads(); n != 0 {
		t.Errorf("%d backend sessions open, want none", n)
	}
	if left, _ := os.ReadDir(tmp); len(left) != 0 {
		t.Errorf("files left in the temporary directory: %v", left)
	}
}

// waitFor waits up to 5 s for cond.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}
