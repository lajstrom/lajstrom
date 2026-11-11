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
