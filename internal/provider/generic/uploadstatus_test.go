package generic

import (
	"bytes"
	"errors"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/opencontainers/go-digest"

	"github.com/lajstrom/lajstrom/internal/provider"
)

// statusRepo returns the repository raw/scratch/notes of p, as a replica of the facade would
// have it.
func statusRepo(t *testing.T, p *Provider) provider.Repository {
	t.Helper()
	r, err := p.Repository(t.Context(), "raw/scratch/notes")
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// TestUploadStatusAhead continues a session on other provider instances, as the facade's
// replicas do (TestIntegrationUploadAcrossReplicas), against a registry whose upload status is
// one byte ahead (Quay 3.18.2, Forgejo 17.0.1: #804) and one that answers as the specification
// says. The status read on a third instance is where the next chunk starts, and the blob has
// its digest.
func TestUploadStatusAhead(t *testing.T) {
	tests := []struct {
		name    string
		ahead   string
		measure bool // a scratch session measures the status, once
	}{
		{name: "the specification's"},
		{name: "Forgejo", ahead: "0-%d", measure: true},
		{name: "Quay", ahead: "bytes=0-%d", measure: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFakeRegistry(t)
			f.statusAhead = tt.ahead
			log, buf := testLogger()
			a, b, c := statusRepo(t, f.provider(t, log, nil)), statusRepo(t, f.provider(t, log, nil)), statusRepo(t, f.provider(t, log, nil))
			blob, d := uploadAcross(t, a, b, c)
			f.mu.Lock()
			stored := f.blobs["art/raw/scratch/notes"][d]
			f.mu.Unlock()
			if !bytes.Equal(stored, blob) {
				t.Errorf("the registry holds %q, want %q", stored, blob)
			}
			if n := f.openUploads(); n != 0 {
				t.Errorf("%d sessions open: the scratch session was not cancelled", n)
			}
			wantPOSTs := map[bool]int{false: 1, true: 2}[tt.measure]
			if n := f.count("POST "); n != wantPOSTs {
				t.Errorf("%d sessions opened, want %d", n, wantPOSTs)
			}
			if got := strings.Contains(buf.String(), "one byte ahead"); got != tt.measure {
				t.Errorf("logged the status one byte ahead: %v, want %v\n%s", got, tt.measure, buf)
			}
			if strings.Contains(buf.String(), "disagrees") {
				t.Errorf("a status logged as disagreeing:\n%s", buf)
			}
		})
	}
}

// uploadAcross starts a session on a, sends a chunk on b, asks the status twice on c and sends
// the next chunk on c from it, and finishes on a; it returns the blob and its digest.
func uploadAcross(t *testing.T, a, b, c provider.Repository) ([]byte, digest.Digest) {
	t.Helper()
	ctx := t.Context()
	u, err := a.StartBlobUpload(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if u, err = b.AppendBlobChunk(ctx, u, strings.NewReader("chunk one "), 10); err != nil || u.Offset != 10 {
		t.Fatalf("chunk on b = %+v, %v", u, err)
	}
	for i := range 2 {
		if u, err = c.BlobUploadStatus(ctx, u); err != nil || u.Offset != 10 {
			t.Fatalf("status %d on c = %+v, %v", i, u, err)
		}
	}
	if u, err = c.AppendBlobChunk(ctx, u, strings.NewReader("chunk two "), 10); err != nil || u.Offset != 20 {
		t.Fatalf("chunk on c from its status = %+v, %v", u, err)
	}
	blob := []byte("chunk one chunk two end")
	d := digest.FromBytes(blob)
	if desc, err := a.FinishBlobUpload(ctx, u, d, strings.NewReader("end"), 3); err != nil || desc.Digest != d || desc.Size != int64(len(blob)) {
		t.Fatalf("finish on a = %+v, %v", desc, err)
	}
	return blob, d
}

// TestUploadStatusDisagrees checks a status that disagrees with the offset Lajstrom knows,
// because the session holds bytes it did not count (a chunk that failed on the way): the
// status is taken, read one byte back on a registry measured ahead, and logged. One byte more
// on a registry that answers as the specification says is one byte more, not a quirk.
func TestUploadStatusDisagrees(t *testing.T) {
	ctx := t.Context()
	tests := []struct {
		name  string
		ahead string
		known int64 // the offset Lajstrom has; the session holds 10 bytes
	}{
		{name: "one byte more, the specification's", known: 9},
		{name: "bytes not counted, the specification's", known: 4},
		{name: "bytes not counted, one byte ahead", ahead: "bytes=0-%d", known: 4},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFakeRegistry(t)
			f.statusAhead = tt.ahead
			log, buf := testLogger()
			repo := statusRepo(t, f.provider(t, log, nil))
			u, err := repo.StartBlobUpload(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if u, err = repo.AppendBlobChunk(ctx, u, strings.NewReader("0123456789"), 10); err != nil {
				t.Fatal(err)
			}
			u.Offset = tt.known
			if got, err := repo.BlobUploadStatus(ctx, u); err != nil || got.Offset != 10 {
				t.Errorf("BlobUploadStatus = %+v, %v; want the offset 10", got, err)
			}
			if !strings.Contains(buf.String(), "an upload status disagrees") {
				t.Errorf("the disagreement was not logged:\n%s", buf)
			}
		})
	}
}

// TestUploadStatusMeasureFails checks that a measurement that fails leaves the status as it
// reads and is tried again on the next status that disagrees.
func TestUploadStatusMeasureFails(t *testing.T) {
	ctx := t.Context()
	f := newFakeRegistry(t)
	f.statusAhead = "0-%d"
	var posts atomic.Int32
	f.hook = func(w http.ResponseWriter, r *http.Request) bool {
		if r.Method != http.MethodPost || posts.Add(1) != 2 {
			return false
		}
		writeErr(w, http.StatusForbidden, "DENIED", "no second session")
		return true
	}
	log, buf := testLogger()
	repo := statusRepo(t, f.provider(t, log, nil))
	u, err := repo.StartBlobUpload(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if u, err = repo.AppendBlobChunk(ctx, u, strings.NewReader("12345"), 5); err != nil {
		t.Fatal(err)
	}
	if got, err := repo.BlobUploadStatus(ctx, u); err != nil || got.Offset != 6 {
		t.Errorf("status while the measurement fails = %+v, %v; want 6, as it reads", got, err)
	}
	if !strings.Contains(buf.String(), "could not measure") {
		t.Errorf("the failed measurement was not logged:\n%s", buf)
	}
	if got, err := repo.BlobUploadStatus(ctx, u); err != nil || got.Offset != 5 {
		t.Errorf("status once measured = %+v, %v; want 5", got, err)
	}
}

// emptyStatusRepo returns the repository raw/scratch/notes of a provider that has not probed, on a
// fake registry that answers the status of a session without bytes 404 BLOB_UNKNOWN and keeps the
// session, as JFrog does; jfrog makes it say so in its answer to GET /v2/, as JFrog does.
func emptyStatusRepo(t *testing.T, jfrog bool) provider.Repository {
	t.Helper()
	f := newFakeRegistry(t)
	f.emptyStatus404 = true
	if jfrog {
		f.hook = func(w http.ResponseWriter, r *http.Request) bool {
			if r.URL.Path == "/v2/" {
				for k, v := range jfrogV2 {
					w.Header()[k] = v
				}
			}
			return false
		}
	}
	log, _ := testLogger()
	return statusRepo(t, f.provider(t, log, nil))
}

// TestUploadStatusEmpty404 is #1045: JFrog answers the status of a session that holds no bytes
// 404 BLOB_UNKNOWN, and the session works. On vendor=jfrog, told by its header to a provider that
// has not probed yet (the status probes first, readQuirks), that 404 is the session as it was, at
// offset 0, and the upload goes on. A 404 at a later offset stays ErrNotFound.
func TestUploadStatusEmpty404(t *testing.T) {
	ctx := t.Context()
	repo := emptyStatusRepo(t, true)
	u, err := repo.StartBlobUpload(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := repo.BlobUploadStatus(ctx, u); err != nil || got != u {
		t.Fatalf("status of the empty session = %+v, %v; want %+v", got, err, u)
	}
	if u, err = repo.AppendBlobChunk(ctx, u, strings.NewReader("abc"), 3); err != nil || u.Offset != 3 {
		t.Fatalf("chunk after the status = %+v, %v; want offset 3", u, err)
	}
	if u, err = repo.BlobUploadStatus(ctx, u); err != nil || u.Offset != 3 {
		t.Fatalf("status after the chunk = %+v, %v; want offset 3", u, err)
	}
	if _, err := repo.FinishBlobUpload(ctx, u, digest.FromString("abc"), nil, 0); err != nil {
		t.Fatalf("finish: %v", err)
	}
	if _, err := repo.BlobUploadStatus(ctx, u); !errors.Is(err, provider.ErrNotFound) {
		t.Errorf("status of the finished session at offset 3 = %v, want ErrNotFound", err)
	}
}

// TestUploadStatusEmpty404Lost: a session JFrog no longer knows, at offset 0, cannot be told from
// one that works, so its status is offset 0 too; its 404 comes at the next PATCH. On a registry
// that the vendor table does not know for it, the 404 of an empty session's status stays
// ErrNotFound.
func TestUploadStatusEmpty404Lost(t *testing.T) {
	ctx := t.Context()
	repo := emptyStatusRepo(t, true)
	gone, err := repo.StartBlobUpload(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.CancelBlobUpload(ctx, gone); err != nil {
		t.Fatal(err)
	}
	if got, err := repo.BlobUploadStatus(ctx, gone); err != nil || got.Offset != 0 {
		t.Errorf("status of a cancelled session at offset 0 = %+v, %v; want offset 0", got, err)
	}
	if _, err := repo.AppendBlobChunk(ctx, gone, strings.NewReader("x"), 1); !errors.Is(err, provider.ErrNotFound) {
		t.Errorf("chunk to the cancelled session = %v, want ErrNotFound", err)
	}

	other := emptyStatusRepo(t, false)
	u, err := other.StartBlobUpload(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := other.BlobUploadStatus(ctx, u); !errors.Is(err, provider.ErrNotFound) {
		t.Errorf("status of the empty session on another registry = %+v, %v; want ErrNotFound", got, err)
	}
}
