package generic

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"
	"testing/iotest"
	"time"

	"github.com/opencontainers/go-digest"

	"github.com/lajstrom/lajstrom/internal/config"
	"github.com/lajstrom/lajstrom/internal/provider"
)

// spoolRepo returns a fake registry that answers a streamed chunk after the first write with
// refuse, as Gitea (400) and JFrog (416) do, the repository raw/scratch/notes on a provider with
// registries[].uploads.spoolUnknownLength set to spool, and the TMPDIR of the test. seen gets
// the method, Content-Range and Content-Length of every PATCH and PUT.
func spoolRepo(t *testing.T, refuse int, spool bool, seen *[]string) (*fakeRegistry, provider.Repository, string) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("TMPDIR", dir)
	f := newFakeRegistry(t)
	f.refuseStreamed = refuse
	f.hook = func(_ http.ResponseWriter, r *http.Request) bool {
		if seen != nil && (r.Method == http.MethodPatch || r.Method == http.MethodPut) {
			*seen = append(*seen, fmt.Sprintf("%s range=%q length=%d", r.Method, r.Header.Get("Content-Range"), r.ContentLength))
		}
		return false
	}
	log, _ := testLogger()
	p := f.provider(t, log, func(c *config.Registry) { c.Uploads.SpoolUnknownLength = spool })
	repo, err := p.Repository(t.Context(), "raw/scratch/notes")
	if err != nil {
		t.Fatal(err)
	}
	return f, repo, dir
}

// noSpoolLeft fails t when a spooled chunk is still open or a file is left in dir.
func noSpoolLeft(t *testing.T, dir string) {
	t.Helper()
	if n := openChunks.Load(); n != 0 {
		t.Errorf("%d spooled chunks still open", n)
	}
	if left, err := os.ReadDir(dir); err != nil || len(left) != 0 {
		t.Errorf("TMPDIR holds %v, %v; want nothing", left, err)
	}
}

// refusals are the answers to a streamed chunk after the first write: Gitea's and JFrog's.
var refusals = []int{http.StatusBadRequest, http.StatusRequestedRangeNotSatisfiable}

// TestSpoolOff: against a registry that wants a known length after the first write, a chunk of
// unknown size fails with the registry's 4xx when the flag is off, and nothing is spooled.
func TestSpoolOff(t *testing.T) {
	for _, refuse := range refusals {
		t.Run(strconv.Itoa(refuse), func(t *testing.T) {
			ctx := t.Context()
			f, repo, dir := spoolRepo(t, refuse, false, nil)
			u, _ := repo.StartBlobUpload(ctx)
			u, _ = repo.AppendBlobChunk(ctx, u, strings.NewReader("abc"), 3)
			_, err := repo.AppendBlobChunk(ctx, u, io.MultiReader(strings.NewReader("de")), -1)
			wantErr(t, "a streamed chunk after the first", err, provider.ErrRejected)
			if rej := (*provider.RejectedError)(nil); !errors.As(err, &rej) || rej.Status != refuse || rej.Hint != "" {
				t.Errorf("error %v, want the registry's %d without a hint: the fake is no vendor of the table", err, refuse)
			}
			if n := f.count("PATCH "); n != 2 {
				t.Errorf("%d PATCHes, want the first write and the refused chunk", n)
			}
			_, err = repo.FinishBlobUpload(ctx, u, digest.FromString("abcde"), io.MultiReader(strings.NewReader("de")), -1)
			wantErr(t, "a streamed last chunk", err, errAny)
			if got, err := repo.BlobUploadStatus(ctx, u); err != nil || got.Offset != 3 {
				t.Errorf("BlobUploadStatus = %+v, %v; want the 3 bytes of the first write", got, err)
			}
			noSpoolLeft(t, dir)
		})
	}
}

// TestSpoolOffHint: when the vendor table knows the registry for refusing a chunk of unknown
// size after the first write (JFrog here, told by its header) and the flag is off, the refusal
// of such a chunk carries the hint that names the flag (#449); the refusal of a chunk of known
// size out of order does not.
func TestSpoolOffHint(t *testing.T) {
	for _, refuse := range refusals {
		t.Run(strconv.Itoa(refuse), func(t *testing.T) {
			ctx := t.Context()
			f := newFakeRegistry(t)
			f.refuseStreamed = refuse
			f.hook = func(w http.ResponseWriter, _ *http.Request) bool {
				w.Header().Set("X-Jfrog-Version", "Artifactory/7.161.26 86126900")
				return false
			}
			log, _ := testLogger()
			p := f.provider(t, log, nil)
			if _, err := p.Capabilities(ctx); err != nil {
				t.Fatal(err)
			}
			repo, err := p.Repository(ctx, "raw/scratch/notes")
			if err != nil {
				t.Fatal(err)
			}
			u, _ := repo.StartBlobUpload(ctx)
			u, _ = repo.AppendBlobChunk(ctx, u, strings.NewReader("abc"), 3)
			_, err = repo.AppendBlobChunk(ctx, u, io.MultiReader(strings.NewReader("de")), -1)
			rej := (*provider.RejectedError)(nil)
			if !errors.As(err, &rej) || rej.Status != refuse || !strings.Contains(err.Error(), "hint: ") ||
				!strings.Contains(rej.Hint, "registries[].uploads.spoolUnknownLength") {
				t.Errorf("a streamed chunk after the first: %v, want the registry's %d with the hint", err, refuse)
			}
			_, err = repo.AppendBlobChunk(ctx, provider.Upload{Location: u.Location, ID: u.ID, Offset: 1}, strings.NewReader("x"), 1)
			if !errors.As(err, &rej) || rej.Status != http.StatusRequestedRangeNotSatisfiable || rej.Hint != "" {
				t.Errorf("a chunk out of order: %v, want the registry's 416 without a hint", err)
			}
		})
	}
}

// TestSpoolOn: with the flag on, a chunk of unknown size after the first write is spooled and
// sent with Content-Range, the last one too; the first chunk is never spooled.
func TestSpoolOn(t *testing.T) {
	for _, refuse := range refusals {
		t.Run(strconv.Itoa(refuse), func(t *testing.T) {
			ctx := t.Context()
			var seen []string
			f, repo, dir := spoolRepo(t, refuse, true, &seen)
			u, _ := repo.StartBlobUpload(ctx)
			u, err := repo.AppendBlobChunk(ctx, u, io.MultiReader(strings.NewReader("abc")), -1)
			if err != nil || u.Offset != 3 {
				t.Fatalf("first chunk of unknown size = %+v, %v", u, err)
			}
			if u, err = repo.AppendBlobChunk(ctx, u, iotest.HalfReader(strings.NewReader("defg")), -1); err != nil || u.Offset != 7 {
				t.Fatalf("chunk of unknown size = %+v, %v", u, err)
			}
			if same, err := repo.AppendBlobChunk(ctx, u, strings.NewReader(""), -1); err != nil || same != u {
				t.Errorf("empty chunk of unknown size = %+v, %v; want the session as it was", same, err)
			}
			d := digest.FromString("abcdefghi")
			if desc, err := repo.FinishBlobUpload(ctx, u, d, io.MultiReader(strings.NewReader("hi")), -1); err != nil || desc.Size != 9 {
				t.Fatalf("FinishBlobUpload = %+v, %v", desc, err)
			}
			wantStoredBlob(t, f, d, "abcdefghi")
			want := []string{
				`PATCH range="" length=-1`, // the first chunk streams
				`PATCH range="3-6" length=4`,
				`PATCH range="7-8" length=2`, // the last chunk goes first
				`PUT range="" length=0`,
			}
			if !slices.Equal(seen, want) {
				t.Errorf("chunks\n%q\nwant\n%q", seen, want)
			}
			noSpoolLeft(t, dir)
		})
	}
}

// wantStoredBlob checks that the fake holds blob as d in raw/scratch/notes.
func wantStoredBlob(t *testing.T, f *fakeRegistry, d digest.Digest, blob string) {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if got := string(f.blobs["art/raw/scratch/notes"][d]); got != blob {
		t.Errorf("the registry holds %q as %s, want %q", got, d, blob)
	}
}

// TestSpoolLeavesNothing: a spooled chunk whose body fails, is cut at the repo's maxFileSize, is
// refused by the registry or outlives its context leaves no file, and sends nothing when it
// never got its whole body.
func TestSpoolLeavesNothing(t *testing.T) {
	tests := []struct {
		name    string
		body    io.Reader
		refuse  bool // the registry answers the spooled PATCH with 500
		cancel  bool
		patches int // PATCHes after the first write
		want    error
	}{
		{name: "the body breaks off", body: io.MultiReader(strings.NewReader("de"), iotest.ErrReader(io.ErrUnexpectedEOF)), want: io.ErrUnexpectedEOF},
		{name: "cut at maxFileSize", body: http.MaxBytesReader(nil, io.NopCloser(strings.NewReader("0123456789")), 4), want: errAny},
		{name: "the registry refuses it", body: strings.NewReader("de"), refuse: true, patches: 1, want: errAny},
		{name: "the context ends", body: strings.NewReader("de"), cancel: true, want: context.Canceled},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f, repo, dir := spoolRepo(t, http.StatusBadRequest, true, nil)
			u, _ := repo.StartBlobUpload(t.Context())
			u, _ = repo.AppendBlobChunk(t.Context(), u, strings.NewReader("abc"), 3)
			before := f.count("PATCH ")
			if tt.refuse {
				f.hook = func(w http.ResponseWriter, r *http.Request) bool {
					if r.Method != http.MethodPatch {
						return false
					}
					writeErr(w, http.StatusInternalServerError, "UNKNOWN", "disk full")
					return true
				}
			}
			ctx, cancel := context.WithCancel(t.Context())
			if tt.cancel {
				cancel()
			}
			defer cancel()
			_, err := repo.AppendBlobChunk(ctx, u, tt.body, -1)
			wantErr(t, tt.name, err, tt.want)
			var tooLarge *http.MaxBytesError
			if tt.name == "cut at maxFileSize" && !errors.As(err, &tooLarge) {
				t.Errorf("error %v does not wrap the *http.MaxBytesError of the cut", err)
			}
			if n := f.count("PATCH ") - before; n != tt.patches {
				t.Errorf("%d PATCHes after the first write, want %d", n, tt.patches)
			}
			noSpoolLeft(t, dir)
		})
	}
}

// TestSpoolClientGone: a client that goes away while its chunk is spooled, through a real
// connection, leaves no file, and nothing reaches the registry.
func TestSpoolClientGone(t *testing.T) {
	ctx := t.Context()
	f, repo, dir := spoolRepo(t, http.StatusBadRequest, true, nil)
	u, _ := repo.StartBlobUpload(ctx)
	u, _ = repo.AppendBlobChunk(ctx, u, strings.NewReader("abc"), 3)
	before := f.count("PATCH ")
	done := make(chan error, 1)
	front := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		_, err := repo.AppendBlobChunk(r.Context(), u, r.Body, r.ContentLength)
		done <- err
	}))
	defer front.Close()
	conn, err := (&net.Dialer{}).DialContext(ctx, "tcp", front.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.WriteString(conn, "PATCH /chunk HTTP/1.1\r\nHost: lajstrom\r\nTransfer-Encoding: chunked\r\n\r\n5\r\nhello\r\n")
	for deadline := time.Now().Add(5 * time.Second); openChunks.Load() == 0; time.Sleep(time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the chunk was not spooled")
		}
	}
	_ = conn.Close()
	select {
	case err := <-done:
		wantErr(t, "a client gone", err, errAny)
	case <-time.After(5 * time.Second):
		t.Fatal("AppendBlobChunk did not return after the client went away")
	}
	if n := f.count("PATCH ") - before; n != 0 {
		t.Errorf("%d PATCHes reached the registry", n)
	}
	noSpoolLeft(t, dir)
}
