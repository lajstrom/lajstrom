package ingest_test

import (
	"context"
	"crypto/md5"  //nolint:gosec // maven's checksums
	"crypto/sha1" //nolint:gosec // maven's checksums
	"crypto/sha256"
	"crypto/sha512"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
	"testing/iotest"
	"time"

	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/lajstrom/lajstrom/internal/auth"
	"github.com/lajstrom/lajstrom/internal/format"
	"github.com/lajstrom/lajstrom/internal/format/formattest"
	"github.com/lajstrom/lajstrom/internal/index"
	"github.com/lajstrom/lajstrom/internal/ingest"
)

// bob is the second principal of the session tests; alice is the first.
var bob = auth.Principal{Kind: auth.KindUser, Name: "bob"}

// clock is a settable time for the sessions' expiry.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) add(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// sessionEnv is the env with a pipeline whose clock moves and whose sessions last a minute.
func sessionEnv(t *testing.T) (*env, *clock) {
	t.Helper()
	e := newEnv(t)
	c := &clock{t: now}
	o := e.options(e.prov)
	o.Now, o.SessionTimeout = c.Now, time.Minute
	e.pipe = ingest.New(o)
	return e, c
}

// add adds a file of content to the session, failing the test on an error.
func add(t *testing.T, e *env, id int64, name, content string) format.SessionFile {
	t.Helper()
	f, err := e.pipe.Add(t.Context(), id, name, strings.NewReader(content), int64(len(content)))
	must(t, err)
	return f
}

// state is the state of session id in the index.
func (e *env) state(t *testing.T, id int64) string {
	t.Helper()
	s, err := e.st.GetSessionByID(t.Context(), id)
	must(t, err)
	return s.State
}

// TestSessionOpen: Open returns the open session of its principal, and another principal's is a
// conflict.
func TestSessionOpen(t *testing.T) {
	ctx := t.Context()
	e, _ := sessionEnv(t)
	s, err := e.pipe.Open(ctx, e.repo, "lib", "1.0", alice)
	must(t, err)
	again, err := e.pipe.Open(ctx, e.repo, "lib", "1.0", alice)
	if err != nil || again.ID != s.ID {
		t.Fatalf("Open again = %+v, %v; want session %d", again, err, s.ID)
	}
	if _, err := e.pipe.Open(ctx, e.repo, "lib", "1.0", bob); !errors.Is(err, index.ErrConflict) {
		t.Errorf("Open by bob = %v, want ErrConflict", err)
	}
}

// TestSessionCommit: a session of five files, one of them added twice with the same bytes and
// one replaced, commits as one manifest with five file layers; the files were pushed once each,
// with their checksums, and the commit pushed none of them again. The session is closed.
func TestSessionCommit(t *testing.T) {
	ctx := t.Context()
	e, _ := sessionEnv(t)
	s, err := e.pipe.Open(ctx, e.repo, "lib", "1.0", alice)
	must(t, err)
	files := []file{{"a.jar", "aaa"}, {"b.pom", "<project/>"}, {"c.jar", "ccc"}, {"d.asc", "sig"}, {"e.zip", "eee"}}
	for _, f := range files {
		add(t, e, s.ID, f.name, f.content)
	}
	add(t, e, s.ID, "a.jar", "aaa")        // the same bytes again
	got := add(t, e, s.ID, "c.jar", "CCC") // other bytes replace the file
	files[2].content = "CCC"
	want := format.SessionFile{Name: "c.jar", Digest: sha256Digest("CCC"), Size: 3, Checksums: checksums("CCC")}
	same(t, "the replaced file", want, got)
	list, err := e.pipe.Files(ctx, s.ID)
	must(t, err)
	var names []string
	for _, f := range list {
		names = append(names, f.Name)
	}
	same(t, "Files, in the order first added", []string{"a.jar", "b.pom", "c.jar", "d.asc", "e.zip"}, names)
	pushedBefore := len(e.prov.Pushes())
	var params map[string]string
	e.plugin.BuildFunc = func(ctx context.Context, repo index.Repo, up format.Upload) (*format.Built, error) {
		params = up.Params
		return buildRaw(ctx, repo, up)
	}

	pub, err := e.pipe.Commit(ctx, s.ID, nil)
	must(t, err)
	if pub.Existed || pub.Tag != "1.0" {
		t.Errorf("Commit = %+v, want a new version under 1.0", pub)
	}
	same(t, "the parameters of the build", map[string]string{"name": "lib", "version": "1.0", "uploaded-by": "user:alice"}, params)
	e.wantFileLayers(t, "raw/scratch/lib", pub.Tag, files)
	for _, p := range e.prov.Pushes()[pushedBefore:] {
		for _, f := range files {
			if p.Digest == sha256Digest(f.content) {
				t.Errorf("the commit pushed %s again: %+v", f.name, p)
			}
		}
	}
	if r := e.rows(t); len(r.Versions) != 1 || len(r.Files[0]) != 5 {
		t.Errorf("index rows %+v, want one version with five files", r)
	}
	if st := e.state(t, s.ID); st != "committed" {
		t.Errorf("session state %q, want committed", st)
	}
	if _, err := e.pipe.Add(ctx, s.ID, "late.jar", strings.NewReader("x"), 1); !errors.Is(err, index.ErrNotFound) {
		t.Errorf("Add after the commit = %v, want ErrNotFound", err)
	}
	if _, err := e.pipe.Commit(ctx, s.ID, nil); !errors.Is(err, index.ErrNotFound) {
		t.Errorf("Commit again = %v, want ErrNotFound", err)
	}
}

// TestSessionCommitParams: the params of Commit reach Build beside the session's name, version
// and uploaded-by, which they cannot replace (apt passes the suite and the component of a source
// package, #79).
func TestSessionCommitParams(t *testing.T) {
	ctx := t.Context()
	e, _ := sessionEnv(t)
	s, err := e.pipe.Open(ctx, e.repo, "lib", "1.0", alice)
	must(t, err)
	add(t, e, s.ID, "a.jar", "aaa")
	var params map[string]string
	e.plugin.BuildFunc = func(ctx context.Context, repo index.Repo, up format.Upload) (*format.Built, error) {
		params = up.Params
		return buildRaw(ctx, repo, up)
	}
	given := map[string]string{"suite": "jammy", "component": "main", "name": "other", "version": "9", "uploaded-by": "user:bob"}
	if _, err := e.pipe.Commit(ctx, s.ID, given); err != nil {
		t.Fatal(err)
	}
	same(t, "the parameters of the build", map[string]string{"suite": "jammy", "component": "main",
		"name": "lib", "version": "1.0", "uploaded-by": "user:alice"}, params)
	same(t, "the params passed, untouched", "other", given["name"])
}

// TestSessionRepeat: after a commit, a new session of the same files is the idempotent repeat,
// and one with other bytes a conflict.
func TestSessionRepeat(t *testing.T) {
	ctx := t.Context()
	e, _ := sessionEnv(t)
	files := []file{{"a.jar", "aaa"}, {"b.pom", "<project/>"}}
	s, err := e.pipe.Open(ctx, e.repo, "lib", "1.0", alice)
	must(t, err)
	for _, f := range files {
		add(t, e, s.ID, f.name, f.content)
	}
	_, err = e.pipe.Commit(ctx, s.ID, nil)
	must(t, err)
	for i, tt := range []struct {
		change  string
		existed bool
		err     error
	}{{"", true, nil}, {"changed", false, index.ErrConflict}} {
		s2, err := e.pipe.Open(ctx, e.repo, "lib", "1.0", alice)
		must(t, err)
		if s2.ID == s.ID {
			t.Fatalf("round %d: Open returned the committed session", i)
		}
		for _, f := range files {
			add(t, e, s2.ID, f.name, f.content+tt.change)
		}
		pub, err := e.pipe.Commit(ctx, s2.ID, nil)
		if !errors.Is(err, tt.err) || (err == nil && pub.Existed != tt.existed) {
			t.Errorf("round %d: Commit = %+v, %v; want Existed %v, error %v", i, pub, err, tt.existed, tt.err)
		}
		if tt.err != nil {
			must(t, e.pipe.Abort(ctx, s2.ID))
		}
	}
}

// TestSessionRules: what Open, Add, Commit and Abort refuse, and abort.
func TestSessionRules(t *testing.T) {
	ctx := t.Context()
	e, _ := sessionEnv(t)
	if _, err := e.pipe.Open(ctx, e.repo, "lib", "", alice); !errors.Is(err, ingest.ErrInvalid) {
		t.Errorf("Open without a version = %v, want ErrInvalid", err)
	}
	if _, err := e.pipe.Open(ctx, e.repo, "a//b", "1.0", alice); !errors.Is(err, ingest.ErrInvalid) {
		t.Errorf("Open of a name the plugin refuses = %v, want ErrInvalid", err)
	}
	s, err := e.pipe.Open(ctx, e.repo, "lib", "1.0", alice)
	must(t, err)
	if _, err := e.pipe.Commit(ctx, s.ID, nil); !errors.Is(err, ingest.ErrInvalid) {
		t.Errorf("Commit without files = %v, want ErrInvalid", err)
	}
	if _, err := e.pipe.Add(ctx, s.ID, "", strings.NewReader("x"), 1); !errors.Is(err, ingest.ErrInvalid) {
		t.Errorf("Add without a name = %v, want ErrInvalid", err)
	}
	if _, err := e.pipe.Add(ctx, s.ID, "a.jar", strings.NewReader("short"), 10); !errors.Is(err, ingest.ErrInvalid) {
		t.Errorf("Add of a body shorter than its size = %v, want ErrInvalid", err)
	}
	add(t, e, s.ID, "a.jar", "aaa")
	must(t, e.pipe.Abort(ctx, s.ID))
	must(t, e.pipe.Abort(ctx, s.ID)) // closed already: nothing to do
	if st := e.state(t, s.ID); st != "aborted" {
		t.Errorf("state %q, want aborted", st)
	}
	if r := e.rows(t); len(r.Versions) != 0 {
		t.Errorf("an aborted session indexed %+v", r.Versions)
	}
	for what, err := range map[string]error{
		"Add":                                    second(e.pipe.Add(ctx, s.ID, "b.jar", strings.NewReader("b"), 1)),
		"Commit":                                 second(e.pipe.Commit(ctx, s.ID, nil)),
		"Files of a session that does not exist": second(e.pipe.Files(ctx, 999)),
	} {
		if !errors.Is(err, index.ErrNotFound) {
			t.Errorf("%s = %v, want ErrNotFound", what, err)
		}
	}
}

// TestSessionRefuse: a file that Refuse refused keeps Commit from publishing, ErrInvalid naming
// the file and the reason, and the session open, until Add replaces the file; then the session
// commits. Refuse of a file the session does not hold is ErrNotFound.
func TestSessionRefuse(t *testing.T) {
	ctx := t.Context()
	e, _ := sessionEnv(t)
	s, err := e.pipe.Open(ctx, e.repo, "lib", "1.0", alice)
	must(t, err)
	add(t, e, s.ID, "a.jar", "corrupted on the way")
	add(t, e, s.ID, "b.pom", "<project/>")
	if err := e.pipe.Refuse(ctx, s.ID, "c.jar", "never added"); !errors.Is(err, index.ErrNotFound) {
		t.Errorf("Refuse of a file the session does not hold = %v, want ErrNotFound", err)
	}
	const reason = "the sha1 of a.jar is 0a, a.jar.sha1 says 0b"
	must(t, e.pipe.Refuse(ctx, s.ID, "a.jar", reason))
	for range 2 { // and again: the session stays as it is
		_, err = e.pipe.Commit(ctx, s.ID, nil)
		if !errors.Is(err, ingest.ErrInvalid) || !strings.Contains(err.Error(), "holds a.jar, which was refused: "+reason) {
			t.Fatalf("Commit of a session with a refused file = %v, want ErrInvalid naming a.jar and why", err)
		}
	}
	if r := e.rows(t); len(r.Versions) != 0 || len(e.pushRefs("1.0")) != 0 || e.state(t, s.ID) != "open" {
		t.Errorf("rows %+v, %d manifests pushed, session %s; want nothing published and the session open",
			r, len(e.pushRefs("1.0")), e.state(t, s.ID))
	}
	add(t, e, s.ID, "a.jar", "aaa") // uploaded again
	pub, err := e.pipe.Commit(ctx, s.ID, nil)
	must(t, err)
	e.wantFileLayers(t, "raw/scratch/lib", pub.Tag, []file{{"a.jar", "aaa"}, {"b.pom", "<project/>"}})
}

// TestSessionAddStreams: with a TMPDIR that takes no file, Add streams a file of several chunks
// into the registry and commits its blob with the checksums it computed; one whose body fails,
// or is shorter than its size, stores no blob, leaves no session open in the registry and adds
// no file to the session.
func TestSessionAddStreams(t *testing.T) {
	ctx := t.Context()
	formattest.NoTempDir(t)
	e, _ := sessionEnv(t)
	e.prov.ChunkSize = 4
	s, err := e.pipe.Open(ctx, e.repo, "lib", "1.0", alice)
	must(t, err)
	const content = "streamed in several chunks"
	got := add(t, e, s.ID, "a.jar", content)
	same(t, "the file", format.SessionFile{Name: "a.jar", Digest: sha256Digest(content), Size: int64(len(content)),
		Checksums: checksums(content)}, got)
	const refused = "refused part way"
	for what, body := range map[string]struct {
		r    io.Reader
		size int64
	}{
		"a body that fails":            {iotest.TimeoutReader(strings.NewReader(refused)), -1},
		"a body shorter than its size": {strings.NewReader(refused), int64(len(refused)) + 1},
	} {
		if _, err := e.pipe.Add(ctx, s.ID, "b.jar", body.r, body.size); err == nil {
			t.Errorf("Add of %s = nil, want an error", what)
		}
	}
	rr, err := e.prov.Repository(ctx, "raw/scratch/lib")
	must(t, err)
	for c, want := range map[string]bool{content: true, refused: false} {
		if ok, err := rr.Exists(ctx, ocispec.Descriptor{Digest: sha256Digest(c), Size: int64(len(c))}); err != nil || ok != want {
			t.Errorf("the registry holds %q: %v, %v; want %v", c, ok, err, want)
		}
	}
	files, err := e.pipe.Files(ctx, s.ID)
	must(t, err)
	if len(files) != 1 || e.prov.OpenUploads() != 0 {
		t.Errorf("the session holds %d files, %d upload sessions open; want 1 and none", len(files), e.prov.OpenUploads())
	}
}

// TestSessionExpiry: a session lives a minute after its last file; then Add and Commit find
// it gone, nothing is published, Open starts an empty one, and ExpireSessions marks the rows.
func TestSessionExpiry(t *testing.T) {
	ctx := t.Context()
	e, c := sessionEnv(t)
	s, err := e.pipe.Open(ctx, e.repo, "lib", "1.0", alice)
	must(t, err)
	add(t, e, s.ID, "a.jar", "aaa")
	c.add(50 * time.Second)
	add(t, e, s.ID, "b.jar", "bbb") // moves the expiry a minute on
	c.add(50 * time.Second)
	add(t, e, s.ID, "c.jar", "ccc")
	c.add(61 * time.Second)
	if _, err := e.pipe.Add(ctx, s.ID, "d.jar", strings.NewReader("d"), 1); !errors.Is(err, index.ErrNotFound) {
		t.Errorf("Add after the timeout = %v, want ErrNotFound", err)
	}
	if _, err := e.pipe.Commit(ctx, s.ID, nil); !errors.Is(err, index.ErrNotFound) {
		t.Errorf("Commit after the timeout = %v, want ErrNotFound", err)
	}
	other, err := e.pipe.Open(ctx, e.repo, "other", "1.0", alice)
	must(t, err)
	n, err := e.pipe.ExpireSessions(ctx)
	if err != nil || n != 1 {
		t.Errorf("ExpireSessions = %d, %v; want the session of lib", n, err)
	}
	if st := e.state(t, s.ID); st != "expired" {
		t.Errorf("state %q, want expired", st)
	}
	fresh, err := e.pipe.Open(ctx, e.repo, "lib", "1.0", alice)
	must(t, err)
	files, err := e.pipe.Files(ctx, fresh.ID)
	if err != nil || fresh.ID == s.ID || len(files) != 0 {
		t.Errorf("Open after the expiry = session %d with %d files, %v; want a new, empty one", fresh.ID, len(files), err)
	}
	if r := e.rows(t); len(r.Versions) != 0 {
		t.Errorf("an expired session indexed %+v", r.Versions)
	}
	if st := e.state(t, other.ID); st != "open" {
		t.Errorf("the session of other: %q, want open", st)
	}
}

// TestSessionConcurrent: Opens at once end in one session, and Adds at once all land.
func TestSessionConcurrent(t *testing.T) {
	ctx := t.Context()
	e, _ := sessionEnv(t)
	const n = 8
	ids := make([]int64, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Go(func() {
			s, err := e.pipe.Open(ctx, e.repo, "lib", "1.0", alice)
			if err != nil {
				t.Errorf("Open %d: %v", i, err)
				return
			}
			ids[i] = s.ID
			if _, err := e.pipe.Add(ctx, s.ID, fmt.Sprintf("f%d.jar", i), strings.NewReader(fmt.Sprint(i)), -1); err != nil {
				t.Errorf("Add %d: %v", i, err)
			}
		})
	}
	wg.Wait()
	for _, id := range ids {
		if id != ids[0] {
			t.Fatalf("the Opens got sessions %v, want one", ids)
		}
	}
	files, err := e.pipe.Files(ctx, ids[0])
	if err != nil || len(files) != n {
		t.Errorf("Files = %d, %v; want %d", len(files), err, n)
	}
}

// TestSessionCommitOnce: while a Commit runs, a second Commit, an Add and an Abort are
// conflicts; a Commit whose Upload fails leaves the session open and committable.
func TestSessionCommitOnce(t *testing.T) {
	ctx := t.Context()
	e, _ := sessionEnv(t)
	s, err := e.pipe.Open(ctx, e.repo, "lib", "1.0", alice)
	must(t, err)
	add(t, e, s.ID, "a.jar", "aaa")

	entered, release := make(chan struct{}), make(chan struct{})
	e.plugin.BuildFunc = func(context.Context, index.Repo, format.Upload) (*format.Built, error) {
		close(entered)
		<-release
		return nil, fmt.Errorf("%w: the build failed", format.ErrInvalid)
	}
	done := make(chan error, 1)
	go func() { _, err := e.pipe.Commit(ctx, s.ID, nil); done <- err }()
	<-entered
	if _, err := e.pipe.Commit(ctx, s.ID, nil); !errors.Is(err, index.ErrConflict) {
		t.Errorf("a second Commit = %v, want ErrConflict", err)
	}
	if _, err := e.pipe.Add(ctx, s.ID, "b.jar", strings.NewReader("b"), 1); !errors.Is(err, index.ErrConflict) {
		t.Errorf("Add during the commit = %v, want ErrConflict", err)
	}
	if err := e.pipe.Abort(ctx, s.ID); !errors.Is(err, index.ErrConflict) {
		t.Errorf("Abort during the commit = %v, want ErrConflict", err)
	}
	if err := e.pipe.Refuse(ctx, s.ID, "a.jar", "too late"); !errors.Is(err, index.ErrConflict) {
		t.Errorf("Refuse during the commit = %v, want ErrConflict", err)
	}
	close(release)
	if err := <-done; !errors.Is(err, ingest.ErrInvalid) {
		t.Errorf("the failed Commit = %v, want the build's ErrInvalid", err)
	}

	e.plugin.BuildFunc = buildRaw
	add(t, e, s.ID, "b.jar", "bbb")
	pub, err := e.pipe.Commit(ctx, s.ID, nil)
	if err != nil || pub.Existed {
		t.Errorf("Commit after the failure = %+v, %v; want a new version", pub, err)
	}
}

// wantFileLayers checks that the manifest under tag in the repository name has the files as
// its layers, in order, and the metadata layer of the test plugin after them.
func (e *env) wantFileLayers(t *testing.T, name, tag string, files []file) {
	t.Helper()
	m := e.manifest(t, name, tag)
	if len(m.Layers) != len(files)+1 {
		t.Fatalf("the manifest has %d layers, want %d files and the metadata layer", len(m.Layers), len(files))
	}
	for i, f := range files {
		if m.Layers[i].Digest != sha256Digest(f.content) {
			t.Errorf("layer %d is %s, want %s of %s", i, m.Layers[i].Digest, sha256Digest(f.content), f.name)
		}
	}
}

// manifest returns the manifest the fake holds under tag in the repository name.
func (e *env) manifest(t *testing.T, name, tag string) ocispec.Manifest {
	t.Helper()
	rr, err := e.prov.Repository(t.Context(), name)
	must(t, err)
	desc, err := rr.Resolve(t.Context(), tag)
	must(t, err)
	rc, err := rr.Fetch(t.Context(), desc)
	must(t, err)
	defer func() { _ = rc.Close() }()
	b, err := io.ReadAll(rc)
	must(t, err)
	var m ocispec.Manifest
	must(t, json.Unmarshal(b, &m))
	return m
}

// sha256Digest is the digest of s.
func sha256Digest(s string) digest.Digest { return digest.FromString(s) }

// checksums are the four checksums of s.
func checksums(s string) map[string]string {
	m5, s1, s5 := md5.Sum([]byte(s)), sha1.Sum([]byte(s)), sha512.Sum512([]byte(s)) //nolint:gosec // maven's checksums
	s2 := sha256.Sum256([]byte(s))
	return map[string]string{"md5": hex.EncodeToString(m5[:]), "sha1": hex.EncodeToString(s1[:]),
		"sha256": hex.EncodeToString(s2[:]), "sha512": hex.EncodeToString(s5[:])}
}

// second returns the error of a call that returns a value and an error.
func second[T any](_ T, err error) error { return err }

// TestSessionCommitClientGone: a Commit whose client goes away during the publish leaves the
// session open and unmarked, so that the next Commit runs at once.
func TestSessionCommitClientGone(t *testing.T) {
	e, _ := sessionEnv(t)
	s, err := e.pipe.Open(t.Context(), e.repo, "lib", "1.0", alice)
	must(t, err)
	add(t, e, s.ID, "a.jar", "aaa")
	ctx, cancel := context.WithCancel(t.Context())
	e.plugin.BuildFunc = func(ctx context.Context, _ index.Repo, _ format.Upload) (*format.Built, error) {
		cancel()
		return nil, ctx.Err()
	}
	if _, err := e.pipe.Commit(ctx, s.ID, nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("the Commit whose client went away = %v, want context.Canceled", err)
	}
	e.plugin.BuildFunc = buildRaw
	if pub, err := e.pipe.Commit(t.Context(), s.ID, nil); err != nil || pub.Existed {
		t.Errorf("the next Commit = %+v, %v; want a new version", pub, err)
	}
}
