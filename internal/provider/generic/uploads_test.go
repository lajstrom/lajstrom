package generic

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"testing"
	"testing/iotest"

	"github.com/opencontainers/go-digest"
	"oras.land/oras-go/v2/content"

	"github.com/lajstrom/lajstrom/internal/config"
	"github.com/lajstrom/lajstrom/internal/provider"
)

// uploadRepo returns the fake registry, set up by edit, and its repository raw/scratch/notes
// below the instance prefix art/.
func uploadRepo(t *testing.T, edit func(*fakeRegistry)) (*fakeRegistry, provider.Repository) {
	t.Helper()
	f := newFakeRegistry(t)
	if edit != nil {
		edit(f)
	}
	log, _ := testLogger()
	repo, err := f.provider(t, log, nil).Repository(t.Context(), "raw/scratch/notes")
	if err != nil {
		t.Fatal(err)
	}
	return f, repo
}

// noBackend fails t when err shows the backend URL or the instance prefix.
func noBackend(t *testing.T, f *fakeRegistry, err error) {
	t.Helper()
	if err != nil && (strings.Contains(err.Error(), f.srv.URL) || strings.Contains(err.Error(), "art/")) {
		t.Errorf("error %q shows the backend URL or the instance prefix", err)
	}
}

// TestUploadSession drives a session through every step, against a registry with relative
// Locations (Zot) and one with absolute Locations whose state changes with every chunk
// (distribution), and checks what reached the registry.
func TestUploadSession(t *testing.T) {
	for _, stateful := range []bool{false, true} {
		t.Run(fmt.Sprintf("stateful=%v", stateful), func(t *testing.T) {
			f, repo := uploadRepo(t, func(f *fakeRegistry) { f.stateful = stateful })
			u := uploadChunks(t, repo, stateful)
			blob := []byte("hello streamed world")
			d := digest.FromBytes(blob)
			desc, err := repo.FinishBlobUpload(t.Context(), u, d, strings.NewReader("world"), 5)
			if err != nil {
				t.Fatal(err)
			}
			if desc.MediaType != octetStream || desc.Digest != d || desc.Size != int64(len(blob)) {
				t.Errorf("FinishBlobUpload = %+v", desc)
			}
			wantStored(t, f, d, blob)
		})
	}
}

// uploadChunks starts a session and sends "hello " and "streamed " to it, checking each step.
func uploadChunks(t *testing.T, repo provider.Repository, stateful bool) provider.Upload {
	t.Helper()
	ctx := t.Context()
	u, err := repo.StartBlobUpload(ctx)
	if err != nil {
		t.Fatal(err)
	}
	wantID := map[bool]string{false: "1", true: "uuid-1"}[stateful]
	if u.ID != wantID || u.Offset != 0 || !strings.Contains(u.Location, "/v2/art/raw/scratch/notes/blobs/uploads/1") {
		t.Fatalf("StartBlobUpload = %+v", u)
	}
	first := u.Location
	if u, err = repo.AppendBlobChunk(ctx, u, strings.NewReader("hello "), 6); err != nil || u.Offset != 6 {
		t.Fatalf("chunk of known size: %+v, %v", u, err)
	}
	if stateful == (u.Location == first) {
		t.Errorf("Location after a chunk %q, before %q: stateful=%v", u.Location, first, stateful)
	}
	if u, err = repo.AppendBlobChunk(ctx, u, iotest.HalfReader(strings.NewReader("streamed ")), -1); err != nil || u.Offset != 15 {
		t.Fatalf("chunk of unknown size: %+v, %v", u, err)
	}
	if same, err := repo.AppendBlobChunk(ctx, u, strings.NewReader(""), 0); err != nil || same != u {
		t.Errorf("empty chunk = %+v, %v; want the session as it was", same, err)
	}
	if got, err := repo.BlobUploadStatus(ctx, u); err != nil || got.Offset != 15 {
		t.Errorf("BlobUploadStatus = %+v, %v", got, err)
	}
	return u
}

// wantStored checks that the registry holds blob as d, that no session is open, and that the
// chunks went as three PATCHes, the last chunk among them, and the PUT with the digest.
func wantStored(t *testing.T, f *fakeRegistry, d digest.Digest, blob []byte) {
	t.Helper()
	f.mu.Lock()
	stored := f.blobs["art/raw/scratch/notes"][d]
	f.mu.Unlock()
	if !bytes.Equal(stored, blob) {
		t.Errorf("the registry holds %q, want %q", stored, blob)
	}
	if f.openUploads() != 0 {
		t.Errorf("%d sessions open after the finish", f.openUploads())
	}
	var chunks []string
	for _, line := range f.log() {
		if strings.HasPrefix(line, "PATCH ") || strings.HasPrefix(line, "PUT ") {
			chunks = append(chunks, line)
		}
	}
	if len(chunks) != 4 || !strings.HasPrefix(chunks[2], "PATCH ") || !strings.Contains(chunks[3], "digest="+url.QueryEscape(d.String())) {
		t.Errorf("chunk requests %q, want three PATCHes and the PUT with the digest", chunks)
	}
}

// TestUploadECRSessions: ECR answers a new session with Range: bytes=0-10485760 and every PATCH
// with 201 (#19). The session starts at 0, each chunk moves it on, and the blob is stored.
func TestUploadECRSessions(t *testing.T) {
	ctx := t.Context()
	f, repo := uploadRepo(t, func(f *fakeRegistry) { f.ecrSessions = true })
	u, err := repo.StartBlobUpload(ctx)
	if err != nil || u.Offset != 0 {
		t.Fatalf("StartBlobUpload = %+v, %v; want offset 0", u, err)
	}
	if u, err = repo.AppendBlobChunk(ctx, u, strings.NewReader("hello "), 6); err != nil || u.Offset != 6 {
		t.Fatalf("a chunk answered 201: %+v, %v; want offset 6", u, err)
	}
	if u, err = repo.AppendBlobChunk(ctx, u, strings.NewReader("ecr"), 3); err != nil || u.Offset != 9 {
		t.Fatalf("the next chunk: %+v, %v; want offset 9", u, err)
	}
	blob := []byte("hello ecr")
	if _, err := repo.FinishBlobUpload(ctx, u, digest.FromBytes(blob), nil, 0); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	stored := f.blobs["art/raw/scratch/notes"][digest.FromBytes(blob)]
	f.mu.Unlock()
	if !bytes.Equal(stored, blob) {
		t.Errorf("the registry holds %q, want %q", stored, blob)
	}
	mounted, m, err := repo.MountBlob(ctx, digest.FromString("elsewhere"), "raw/other")
	if err != nil || mounted || m.Offset != 0 {
		t.Errorf("MountBlob answered with a session = %v, %+v, %v; want a session at 0", mounted, m, err)
	}
}

// TestUploadChunkMinLength checks that the session's ChunkMinLength is the registry's
// OCI-Chunk-Min-Length of the last answer, and 0 when that has none or one that is no size.
func TestUploadChunkMinLength(t *testing.T) {
	ctx := t.Context()
	answer := "10485760"
	_, repo := uploadRepo(t, func(f *fakeRegistry) {
		f.hook = func(w http.ResponseWriter, r *http.Request) bool {
			if answer != "" && (r.Method == http.MethodPost || r.Method == http.MethodPatch) {
				w.Header().Set("OCI-Chunk-Min-Length", answer)
			}
			return false
		}
	})
	u, err := repo.StartBlobUpload(ctx)
	if err != nil || u.ChunkMinLength != 10485760 {
		t.Fatalf("StartBlobUpload = %+v, %v; want ChunkMinLength 10485760", u, err)
	}
	for _, tc := range []struct {
		answer string
		want   int64
	}{{" 5242880 ", 5242880}, {"", 0}, {"ten", 0}, {"-1", 0}} {
		answer = tc.answer
		if u, err = repo.AppendBlobChunk(ctx, u, strings.NewReader("a"), 1); err != nil || u.ChunkMinLength != tc.want {
			t.Errorf("PATCH answered OCI-Chunk-Min-Length %q: %+v, %v; want ChunkMinLength %d", tc.answer, u, err, tc.want)
		}
	}
}

// TestUploadHeaders checks Content-Range and Content-Length of every chunk. The closing PUT
// carries the last chunk only when the session is empty and the chunk's size known; otherwise
// the chunk goes first as a PATCH (JFrog drops the body of a closing PUT after a PATCH).
func TestUploadHeaders(t *testing.T) {
	ctx := t.Context()
	var seen []string
	_, repo := uploadRepo(t, func(f *fakeRegistry) {
		f.hook = func(_ http.ResponseWriter, r *http.Request) bool {
			if r.Method == http.MethodPatch || r.Method == http.MethodPut {
				seen = append(seen, fmt.Sprintf("%s range=%q length=%d", r.Method, r.Header.Get("Content-Range"), r.ContentLength))
			}
			return false
		}
	})
	finish := func(u provider.Upload, blob, last string, size int64) {
		t.Helper()
		desc, err := repo.FinishBlobUpload(ctx, u, digest.FromString(blob), strings.NewReader(last), size)
		if err != nil || desc.Size != int64(len(blob)) {
			t.Fatalf("FinishBlobUpload of %q = %+v, %v", blob, desc, err)
		}
	}
	u, _ := repo.StartBlobUpload(ctx)
	u, _ = repo.AppendBlobChunk(ctx, u, strings.NewReader("abc"), 3)
	u, _ = repo.AppendBlobChunk(ctx, u, strings.NewReader("de"), -1)
	finish(u, "abcdefg", "fg", -1)
	u, _ = repo.StartBlobUpload(ctx)
	finish(u, "xyz", "xyz", 3)
	u, _ = repo.StartBlobUpload(ctx)
	u, _ = repo.AppendBlobChunk(ctx, u, strings.NewReader("streamed"), -1)
	finish(u, "streamed end", " end", 4)
	want := []string{
		`PATCH range="0-2" length=3`,
		`PATCH range="" length=-1`,
		`PATCH range="" length=-1`, // the last chunk of unknown size goes first
		`PUT range="" length=0`,
		`PUT range="0-2" length=3`, // into an empty session: the body of the PUT
		`PATCH range="" length=-1`,
		`PATCH range="8-11" length=4`, // of known size after a write: a PATCH, not the PUT's body
		`PUT range="" length=0`,
	}
	if !slices.Equal(seen, want) {
		t.Errorf("chunks\n%q\nwant\n%q", seen, want)
	}
}

// TestUploadLastChunkBreaks: a last chunk that breaks off while it streams, as the facade's
// digest check breaks one off (#368), fails the finish, and no PUT closes a session that holds
// bytes; in an empty one the PUT itself breaks off. The PUTs are counted as the client sent
// them, in the debug log of logTransport: a PUT that breaks off may end before the registry
// reads its headers, and then the registry never sees it (#566).
func TestUploadLastChunkBreaks(t *testing.T) {
	ctx := t.Context()
	f := newFakeRegistry(t)
	log, buf := testLogger()
	repo, err := f.provider(t, log, nil).Repository(ctx, "raw/scratch/notes")
	if err != nil {
		t.Fatal(err)
	}
	for _, tt := range []struct {
		offset int64
		puts   int
	}{{0, 1}, {3, 0}} {
		u, _ := repo.StartBlobUpload(ctx)
		if tt.offset > 0 {
			u, _ = repo.AppendBlobChunk(ctx, u, strings.NewReader("abc"), 3)
		}
		before := sentPUTs(buf.String())
		broken := io.MultiReader(strings.NewReader("de"), iotest.ErrReader(errors.New("not the digest")))
		if _, err := repo.FinishBlobUpload(ctx, u, digest.FromString("abcdef"), broken, 3); err == nil {
			t.Errorf("offset %d: FinishBlobUpload of a broken chunk succeeded", tt.offset)
		}
		if n := sentPUTs(buf.String()) - before; n != tt.puts {
			t.Errorf("offset %d: the client sent %d PUT requests, want %d", tt.offset, n, tt.puts)
		}
	}
	if n := f.count("PUT "); n > 1 {
		t.Errorf("the registry got %d PUT requests, want the monolithic one at most, which broke off", n)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if n := len(f.blobs["art/raw/scratch/notes"]); n != 0 {
		t.Errorf("the registry holds %d blobs, want none", n)
	}
}

// sentPUTs counts the PUT requests in logs that logTransport logged, answered or failed.
func sentPUTs(logs string) int {
	n := 0
	for line := range strings.Lines(logs) {
		if strings.Contains(line, " method=PUT ") &&
			(strings.Contains(line, `msg="registry request" `) || strings.Contains(line, `msg="registry request failed" `)) {
			n++
		}
	}
	return n
}

// TestUploadErrors covers the answers that are errors: a wrong digest, a chunk out of order, a
// session the registry does not know, a location on another host. None shows the backend.
func TestUploadErrors(t *testing.T) {
	ctx := t.Context()
	f, repo := uploadRepo(t, func(f *fakeRegistry) { f.stateful = true })
	u, _ := repo.StartBlobUpload(ctx)
	u, _ = repo.AppendBlobChunk(ctx, u, strings.NewReader("abc"), 3)

	_, err := repo.FinishBlobUpload(ctx, u, digest.FromString("other"), nil, 0)
	wantErr(t, "wrong digest", err, content.ErrMismatchedDigest)
	noBackend(t, f, err)

	_, err = repo.AppendBlobChunk(ctx, provider.Upload{Location: u.Location, ID: u.ID, Offset: 1}, strings.NewReader("x"), 1)
	wantErr(t, "chunk out of order", err, errAny)
	if errors.Is(err, provider.ErrNotFound) || strings.Contains(fmt.Sprint(err), "_state") {
		t.Errorf("chunk out of order: %v", err)
	}

	_, err = repo.FinishBlobUpload(ctx, u, "sha256:nothex", nil, 0)
	wantErr(t, "invalid digest", err, content.ErrMismatchedDigest)

	wantErr(t, "cancel", repo.CancelBlobUpload(ctx, u), nil)
	_, err = repo.BlobUploadStatus(ctx, u)
	wantErr(t, "status after the cancel", err, provider.ErrNotFound)
	noBackend(t, f, err)
	_, err = repo.AppendBlobChunk(ctx, u, strings.NewReader("x"), 1)
	wantErr(t, "chunk after the cancel", err, provider.ErrNotFound)
	wantErr(t, "second cancel", repo.CancelBlobUpload(ctx, u), provider.ErrNotFound)

	for _, loc := range []string{"http://elsewhere.example/v2/x/blobs/uploads/1", "", "http://[::1"} {
		before := len(f.log())
		_, err = repo.AppendBlobChunk(ctx, provider.Upload{Location: loc, ID: "1"}, strings.NewReader("x"), 1)
		wantErr(t, "location "+loc, err, errAny)
		if strings.Contains(fmt.Sprint(err), "elsewhere") || len(f.log()) != before {
			t.Errorf("location %q: %v, %d requests", loc, err, len(f.log())-before)
		}
	}
}

// TestUploadDowngrade refuses an http location from an https registry.
func TestUploadDowngrade(t *testing.T) {
	p, err := New(config.Registry{Name: "tls", Type: Type, URL: "https://registry.example"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.location("http://registry.example/v2/x/blobs/uploads/1"); err == nil {
		t.Error("a downgrade to http was accepted")
	}
	if u, err := p.location("/v2/x/blobs/uploads/1?_state=a"); err != nil || u.String() != "https://registry.example/v2/x/blobs/uploads/1?_state=a" {
		t.Errorf("relative location = %v, %v", u, err)
	}
	if _, err := p.location("https://registry.example:443/v2/x"); err != nil {
		t.Errorf("the default port spelt out: %v", err)
	}
}

// TestUploadMount mounts a blob the source has and gets a session for one it has not.
func TestUploadMount(t *testing.T) {
	ctx := t.Context()
	f, repo := uploadRepo(t, nil)
	log, _ := testLogger()
	src, _ := f.provider(t, log, nil).Repository(ctx, "raw/scratch/src")
	blob := pushBlob(t, src, "shared")
	mounted, u, err := repo.MountBlob(ctx, blob.Digest, "raw/scratch/src")
	if err != nil || !mounted || u != (provider.Upload{}) {
		t.Fatalf("MountBlob = %v, %+v, %v", mounted, u, err)
	}
	if !slices.Contains(f.log(), "POST /v2/art/raw/scratch/notes/blobs/uploads/?from=art%2Fraw%2Fscratch%2Fsrc&mount="+url.QueryEscape(blob.Digest.String())) {
		t.Errorf("no mount request with the prefixed source in %q", f.log())
	}
	mounted, u, err = repo.MountBlob(ctx, digest.FromString("absent"), "")
	if err != nil || mounted || u.ID == "" {
		t.Fatalf("MountBlob of an absent blob = %v, %+v, %v", mounted, u, err)
	}
	if _, err := repo.FinishBlobUpload(ctx, u, digest.FromString("absent"), strings.NewReader("absent"), 6); err != nil {
		t.Errorf("finish the session of the mount: %v", err)
	}
	_, _, err = repo.MountBlob(ctx, "sha256:bad", "")
	wantErr(t, "mount of an invalid digest", err, errAny)
}

// TestUploadAcrossProviders continues, with a provider whose token cache is empty (another
// replica), a session that the first provider started, against a registry with bearer
// tokens. The streamed chunk cannot be sent twice, so the provider takes the challenge with a
// bodyless GET first. The challenge may name scopes of its own, which the auth client adds to
// the key of the token it caches: Gitea's names "*" whatever the request (#120).
func TestUploadAcrossProviders(t *testing.T) {
	ctx := t.Context()
	for _, tc := range []struct{ name, scope string }{
		{"no scope in the challenge", ""},
		{"the repository's scope in the challenge", "repository:art/raw/scratch/notes:pull,push"},
		{"a scope of its own in the challenge", "*"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeRegistry(t)
			f.authMode, f.challengeScope = "bearer", tc.scope
			log, _ := testLogger()
			creds := func(c *config.Registry) {
				c.Auth = config.RegistryAuth{Username: f.user, Password: f.pass, PasswordEnv: "FAKE_PASSWORD"}
			}
			a, _ := f.provider(t, log, creds).Repository(ctx, "raw/scratch/notes")
			b, _ := f.provider(t, log, creds).Repository(ctx, "raw/scratch/notes")
			u, err := a.StartBlobUpload(ctx)
			if err != nil {
				t.Fatal(err)
			}
			before := len(f.log())
			// A reader without a GetBody: the request cannot be replayed after a 401.
			if u, err = b.AppendBlobChunk(ctx, u, io.MultiReader(strings.NewReader("chunk")), 5); err != nil {
				t.Fatalf("chunk on the second provider: %v", err)
			}
			after := f.log()[before:]
			if len(after) < 2 || !strings.HasPrefix(after[0], "GET /v2/art/raw/scratch/notes/blobs/uploads/") ||
				!strings.HasPrefix(after[len(after)-1], "PATCH ") {
				t.Errorf("requests of the chunk %q, want the priming GET first and one PATCH last", after)
			}
			// The last chunk streamed with the closing PUT, on the provider that started the
			// session: its token was cached by the POST.
			if _, err := a.FinishBlobUpload(ctx, u, digest.FromString("chunk-end"), io.MultiReader(strings.NewReader("-end")), 4); err != nil {
				t.Fatalf("finish on the first provider: %v", err)
			}
		})
	}

	// Without credentials nothing is primed.
	f2, repo := uploadRepo(t, nil)
	u, _ := repo.StartBlobUpload(ctx)
	if _, err := repo.AppendBlobChunk(ctx, u, strings.NewReader("x"), 1); err != nil {
		t.Fatal(err)
	}
	if n := f2.count("GET "); n != 0 {
		t.Errorf("%d GETs without credentials, want none", n)
	}
}

func TestOffsetOf(t *testing.T) {
	tests := []struct {
		rng     string
		counted int64
		want    int64
	}{
		{"0-9", 3, 10},
		{"bytes=0-9", 3, 10},
		{"0-0", 0, 0},
		{"0-0", 1, 1},
		{"0-0", 7, 1},
		{"", 4, 4},
		{"5-9", 4, 4},
		{"0-x", 4, 4},
		{"0--1", 4, 4},
	}
	for _, tt := range tests {
		if got := offsetOf(tt.rng, tt.counted); got != tt.want {
			t.Errorf("offsetOf(%q, %d) = %d, want %d", tt.rng, tt.counted, got, tt.want)
		}
	}
}

// TestUploadTransportError checks that a failed connection does not put the upload URL in the
// error.
func TestUploadTransportError(t *testing.T) {
	f, repo := uploadRepo(t, func(f *fakeRegistry) { f.stateful = true })
	u, err := repo.StartBlobUpload(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	f.srv.Close()
	_, err = repo.AppendBlobChunk(context.Background(), u, strings.NewReader("x"), 1)
	wantErr(t, "chunk to a closed registry", err, errAny)
	if strings.Contains(fmt.Sprint(err), "_state") || strings.Contains(fmt.Sprint(err), f.srv.URL) {
		t.Errorf("error %q carries the upload URL", err)
	}
}

// TestBlobChunk: the chunks of OpenBlobWriter are 4 MiB on GHCR, which refuses a larger PATCH,
// and streamChunk elsewhere; registries[].uploads.chunkSize replaces either (#558). The bound
// of a client's /v2 chunk is the same where the vendor table or the key says, and none
// elsewhere (#697).
func TestBlobChunk(t *testing.T) {
	for _, tt := range []struct {
		url        string
		configured int
		want       int
		bound      int
	}{
		{"https://ghcr.io", 0, ghcrMaxChunk, ghcrMaxChunk},
		{"https://ghcr.io:443", 0, ghcrMaxChunk, ghcrMaxChunk},
		{"https://GHCR.io.", 0, ghcrMaxChunk, ghcrMaxChunk},
		{"https://notghcr.io", 0, streamChunk, 0},
		{"https://ghcr.io.example.com", 0, streamChunk, 0},
		{"http://localhost:5000", 0, streamChunk, 0},
		{"https://registry.gitlab.com", 0, streamChunk, 0},
		{"https://containers.github.example", 4 << 20, 4 << 20, 4 << 20}, // GHCR on another host name
		{"https://ghcr.io", 1 << 20, 1 << 20, 1 << 20},
		{"http://localhost:5000", 32 << 20, 32 << 20, 32 << 20},
	} {
		p, err := New(config.Registry{Name: "r", Type: Type, URL: tt.url, Uploads: config.RegistryUploads{ChunkSize: tt.configured}}, nil)
		if err != nil {
			t.Fatal(err)
		}
		if p.chunk != tt.want || p.bound != tt.bound {
			t.Errorf("%s, chunkSize %d: chunk = %d, bound = %d; want %d, %d", tt.url, tt.configured, p.chunk, p.bound, tt.want, tt.bound)
		}
	}
}

// TestOpenBlobWriterGHCR: a streamed blob of two chunks and a bit goes to a registry of GHCR's
// chunk size as PATCHes of 4 MiB, each with its Content-Range, the rest as a PATCH of its own,
// and an empty PUT (#558). GHCR is told by its host, so the fake gets the size by
// registries[].uploads.chunkSize.
func TestOpenBlobWriterGHCR(t *testing.T) {
	ctx := t.Context()
	var seen []string
	f := newFakeRegistry(t)
	f.hook = func(_ http.ResponseWriter, r *http.Request) bool {
		seen = append(seen, fmt.Sprintf("%s range=%q length=%d", r.Method, r.Header.Get("Content-Range"), r.ContentLength))
		return false
	}
	log, _ := testLogger()
	repo, err := f.provider(t, log, func(c *config.Registry) { c.Uploads.ChunkSize = ghcrMaxChunk }).Repository(ctx, "raw/scratch/notes")
	if err != nil {
		t.Fatal(err)
	}
	blob := bytes.Repeat([]byte("0123456789abcdef"), (2*ghcrMaxChunk+16)/16)
	w, err := repo.OpenBlobWriter(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(w, bytes.NewReader(blob)); err != nil {
		t.Fatal(err)
	}
	d := digest.FromBytes(blob)
	if desc, err := w.Commit(ctx, d); err != nil || desc.Digest != d || desc.Size != int64(len(blob)) {
		t.Fatalf("Commit = %+v, %v", desc, err)
	}
	f.mu.Lock()
	stored := bytes.Equal(f.blobs["art/raw/scratch/notes"][d], blob)
	f.mu.Unlock()
	if !stored {
		t.Error("the registry does not hold the blob")
	}
	want := []string{
		`POST range="" length=0`,
		`PATCH range="0-4194303" length=4194304`,
		`PATCH range="4194304-8388607" length=4194304`,
		`PATCH range="8388608-8388623" length=16`,
		`PUT range="" length=0`,
	}
	if !slices.Equal(seen, want) {
		t.Errorf("requests\n%q\nwant\n%q", seen, want)
	}
}

// TestOpenBlobWriter: a streamed blob of two chunks and a bit goes as chunks of streamChunk
// bytes, each with its Content-Range, the rest as a PATCH of its own and an empty PUT with the
// digest; one cancelled after a chunk is a DELETE, and leaves nothing.
func TestOpenBlobWriter(t *testing.T) {
	ctx := t.Context()
	var seen []string
	f, repo := uploadRepo(t, func(f *fakeRegistry) {
		f.hook = func(_ http.ResponseWriter, r *http.Request) bool {
			seen = append(seen, fmt.Sprintf("%s range=%q length=%d", r.Method, r.Header.Get("Content-Range"), r.ContentLength))
			return false
		}
	})
	blob := bytes.Repeat([]byte("0123456789abcdef"), (2*streamChunk+16)/16)
	w, err := repo.OpenBlobWriter(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(w, bytes.NewReader(blob)); err != nil {
		t.Fatal(err)
	}
	d := digest.FromBytes(blob)
	if desc, err := w.Commit(ctx, d); err != nil || desc.Digest != d || desc.Size != int64(len(blob)) {
		t.Fatalf("Commit = %+v, %v", desc, err)
	}
	f.mu.Lock()
	stored := bytes.Equal(f.blobs["art/raw/scratch/notes"][d], blob)
	f.mu.Unlock()
	if !stored {
		t.Error("the registry does not hold the blob")
	}
	if w, err = repo.OpenBlobWriter(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write(blob[:streamChunk+1]); err != nil {
		t.Fatal(err)
	}
	if err := w.Cancel(ctx); err != nil {
		t.Fatal(err)
	}
	want := []string{
		`POST range="" length=0`,
		`PATCH range="0-8388607" length=8388608`,
		`PATCH range="8388608-16777215" length=8388608`,
		`PATCH range="16777216-16777231" length=16`,
		`PUT range="" length=0`,
		`POST range="" length=0`,
		`PATCH range="0-8388607" length=8388608`,
		`DELETE range="" length=0`,
	}
	if !slices.Equal(seen, want) {
		t.Errorf("requests\n%q\nwant\n%q", seen, want)
	}
	if f.openUploads() != 0 {
		t.Errorf("%d sessions open, want none", f.openUploads())
	}
}

// splitRepo returns a fake registry that takes at most limit bytes in a PATCH with
// Content-Range and starts a session over with a PATCH without it, as GHCR does with 4 MiB
// (limit 0: a registry without a limit), and raw/scratch/notes on a provider with the chunk
// bound registries[].uploads.chunkSize (0: none) and spoolUnknownLength set to spool. seen gets
// the Content-Range and Content-Length of every PATCH and PUT. Locations change with every
// chunk, so a PATCH must go to the Location of the one before.
func splitRepo(t *testing.T, limit, bound int, spool bool, seen *[]string) (*fakeRegistry, provider.Repository) {
	t.Helper()
	t.Setenv("TMPDIR", t.TempDir())
	f := newFakeRegistry(t)
	f.stateful, f.chunkLimit = true, limit
	f.hook = func(_ http.ResponseWriter, r *http.Request) bool {
		if r.Method == http.MethodPatch || r.Method == http.MethodPut {
			*seen = append(*seen, fmt.Sprintf("%s range=%q length=%d", r.Method, r.Header.Get("Content-Range"), r.ContentLength))
		}
		return false
	}
	log, _ := testLogger()
	repo, err := f.provider(t, log, func(c *config.Registry) {
		c.Uploads.ChunkSize, c.Uploads.SpoolUnknownLength = bound, spool
	}).Repository(t.Context(), "raw/scratch/notes")
	if err != nil {
		t.Fatal(err)
	}
	return f, repo
}

// wantFakeBlob checks that the fake holds blob as its digest in raw/scratch/notes.
func wantFakeBlob(t *testing.T, f *fakeRegistry, blob []byte) {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if got := f.blobs["art/raw/scratch/notes"][digest.FromBytes(blob)]; !bytes.Equal(got, blob) {
		t.Errorf("the registry holds %d bytes as the blob's digest, want the %d bytes of the blob", len(got), len(blob))
	}
}

// TestAppendSplit is #697: on a registry with a chunk bound of 4 MiB, GHCR's, here by
// registries[].uploads.chunkSize, a client's chunk of 10 MiB goes as three PATCHes of at most
// 4 MiB, each with its Content-Range from the session's offset, and the session returned holds
// all of it, as one chunk; a chunk of unknown size streams when it is the first write, which
// GHCR takes whole, and is spooled and split after it with spoolUnknownLength. Without a bound,
// each chunk goes as one PATCH, as it came, which GHCR refuses above 4 MiB.
func TestAppendSplit(t *testing.T) {
	const mib = 1 << 20
	split := func(from int) []string {
		var want []string
		for off := from; off < from+10*mib; off += 4 * mib {
			n := min(4*mib, from+10*mib-off)
			want = append(want, fmt.Sprintf("PATCH range=\"%d-%d\" length=%d", off, off+n-1, n))
		}
		return want
	}
	for _, tt := range []struct {
		name         string
		limit, bound int
		spool        bool
		before       int   // bytes the session holds before the chunk, sent as a chunk of known size
		size         int64 // of the chunk: 10 MiB, or -1 for unknown
		want         []string
		wantErr      int // the status of the registry's refusal; 0 for none
	}{
		{name: "bound, known size", limit: 4 * mib, bound: 4 * mib, size: 10 * mib, want: split(0)},
		{name: "bound, known size, after a chunk", limit: 4 * mib, bound: 4 * mib, before: 1000, size: 10 * mib, want: split(1000)},
		{name: "bound, unknown size, first write", limit: 4 * mib, bound: 4 * mib, size: -1, want: []string{`PATCH range="" length=-1`}},
		{name: "bound, unknown size, after a chunk, spooled", limit: 4 * mib, bound: 4 * mib, spool: true, before: 1000, size: -1, want: split(1000)},
		{name: "bound, known size, below the limit", limit: 4 * mib, bound: 3 * mib, size: 10 * mib, want: []string{
			`PATCH range="0-3145727" length=3145728`, `PATCH range="3145728-6291455" length=3145728`,
			`PATCH range="6291456-9437183" length=3145728`, `PATCH range="9437184-10485759" length=1048576`}},
		{name: "no bound, known size", size: 10 * mib, want: []string{`PATCH range="0-10485759" length=10485760`}},
		{name: "no bound, unknown size, after a chunk", before: 1000, size: -1, want: []string{`PATCH range="" length=-1`}},
		{name: "no bound, known size, GHCR's limit", limit: 4 * mib, size: 10 * mib,
			want: []string{`PATCH range="0-10485759" length=10485760`}, wantErr: http.StatusRequestedRangeNotSatisfiable},
	} {
		t.Run(tt.name, func(t *testing.T) {
			ctx := t.Context()
			var seen []string
			f, repo := splitRepo(t, tt.limit, tt.bound, tt.spool, &seen)
			blob := bytes.Repeat([]byte("0123456789abcdef"), (tt.before+10*mib)/16+1)[:tt.before+10*mib]
			u, err := repo.StartBlobUpload(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if u, err = repo.AppendBlobChunk(ctx, u, bytes.NewReader(blob[:tt.before]), int64(tt.before)); err != nil {
				t.Fatal(err)
			}
			seen = nil
			got, err := repo.AppendBlobChunk(ctx, u, iotest.HalfReader(bytes.NewReader(blob[tt.before:])), tt.size)
			if !slices.Equal(seen, tt.want) {
				t.Errorf("PATCHes\n%q\nwant\n%q", seen, tt.want)
			}
			if tt.wantErr != 0 {
				if rej := (*provider.RejectedError)(nil); !errors.As(err, &rej) || rej.Status != tt.wantErr || got != u {
					t.Errorf("AppendBlobChunk = %+v, %v; want the registry's %d and the session as it was", got, err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			wantLoc := fmt.Sprintf("_state=%d", len(tt.want)+min(tt.before, 1))
			if got.Offset != int64(len(blob)) || !strings.HasSuffix(got.Location, wantLoc) {
				t.Errorf("session = %+v, want offset %d and a Location with %s", got, len(blob), wantLoc)
			}
			if desc, err := repo.FinishBlobUpload(ctx, got, digest.FromBytes(blob), nil, 0); err != nil || desc.Size != int64(len(blob)) {
				t.Fatalf("FinishBlobUpload = %+v, %v", desc, err)
			}
			wantFakeBlob(t, f, blob)
		})
	}
}

// TestAppendSplitStreamedRefused: on a registry with a chunk bound and spoolUnknownLength off, a
// chunk of unknown size into a session that holds bytes, a PATCH or the last chunk of the
// closing PUT, is refused with Gitea's 400 BLOB_UPLOAD_INVALID and a hint that names the key,
// before a byte of it is read or anything is sent; the session stays as it was.
func TestAppendSplitStreamedRefused(t *testing.T) {
	ctx := t.Context()
	var seen []string
	_, repo := splitRepo(t, 4<<20, 4<<20, false, &seen)
	u, err := repo.StartBlobUpload(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if u, err = repo.AppendBlobChunk(ctx, u, strings.NewReader("first"), 5); err != nil {
		t.Fatal(err)
	}
	seen = nil
	unread := iotest.ErrReader(errors.New("the chunk was read"))
	got, err := repo.AppendBlobChunk(ctx, u, unread, -1)
	if rej := (*provider.RejectedError)(nil); !errors.As(err, &rej) || rej.Status != http.StatusBadRequest ||
		rej.Code != "BLOB_UPLOAD_INVALID" || !strings.Contains(rej.Hint, "registries[].uploads.spoolUnknownLength") ||
		!strings.Contains(rej.Hint, "4194304") || got != u {
		t.Errorf("AppendBlobChunk = %+v, %v; want 400 BLOB_UPLOAD_INVALID with a hint, and the session as it was", got, err)
	}
	if _, err := repo.FinishBlobUpload(ctx, u, digest.FromString("firstsecond"), unread, -1); !errors.Is(err, provider.ErrRejected) {
		t.Errorf("FinishBlobUpload = %v, want the refusal", err)
	}
	if len(seen) != 0 {
		t.Errorf("sent %q, want nothing", seen)
	}
}

// TestAppendSplitFails: a piece the registry refuses ends the chunk, with the session after the
// pieces it took, and no more pieces are sent.
func TestAppendSplitFails(t *testing.T) {
	ctx := t.Context()
	patches := 0
	f := newFakeRegistry(t)
	f.hook = func(w http.ResponseWriter, r *http.Request) bool {
		if r.Method != http.MethodPatch {
			return false
		}
		if patches++; patches == 2 {
			writeErr(w, http.StatusRequestedRangeNotSatisfiable, "BLOB_UPLOAD_INVALID", "too large")
			return true
		}
		return false
	}
	log, _ := testLogger()
	repo, err := f.provider(t, log, func(c *config.Registry) { c.Uploads.ChunkSize = 4 }).Repository(ctx, "raw/scratch/notes")
	if err != nil {
		t.Fatal(err)
	}
	u, err := repo.StartBlobUpload(ctx)
	if err != nil {
		t.Fatal(err)
	}
	got, err := repo.AppendBlobChunk(ctx, u, strings.NewReader("0123456789"), 10)
	if err == nil || got.Offset != 4 || patches != 2 {
		t.Errorf("AppendBlobChunk = %+v, %v after %d PATCHes; want an error at offset 4 after 2", got, err, patches)
	}
}
