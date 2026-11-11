package facade

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/opencontainers/go-digest"
	"github.com/opencontainers/image-spec/specs-go"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/lajstrom/lajstrom/internal/auth"
	"github.com/lajstrom/lajstrom/internal/auth/token"
	"github.com/lajstrom/lajstrom/internal/index"
	"github.com/lajstrom/lajstrom/internal/index/sqlite"
	"github.com/lajstrom/lajstrom/internal/ingest"
	"github.com/lajstrom/lajstrom/internal/keys"
	"github.com/lajstrom/lajstrom/internal/provider"
	"github.com/lajstrom/lajstrom/internal/provider/providertest"
)

// memoryDBs numbers the in-memory databases, so that every test gets its own.
var memoryDBs atomic.Int64

// principalHeader names the principal of a test request: a user name, none for anonymous.
const principalHeader = "X-Test-User"

// env is a facade over the real SQLite store in memory, with the repos raw/assets and
// oci/images on the registry "local", which is the fake provider unless envOptions says
// otherwise, and the real ingest pipeline over both, with the plugins registered in the test
// binary (raw, imported by hook_test.go). Its tokens are signed with one ephemeral key, shared
// by its replicas.
type env struct {
	t      *testing.T
	st     *sqlite.Store
	prov   *providertest.Fake
	reg    provider.Provider // the registry "local"
	raw    index.Repo
	oci    index.Repo
	clock  *clock
	authz  auth.Authorizer
	ingest Ingester
	max    int64         // Options.MaxManifestSize
	behind time.Duration // Options.MaxCreatedBehind
	tokens token.Keys
	extURL string // Options.ExternalURL
	logs   *logBuffer
	h      http.Handler // replica A

	sessions [2]int // Options.UploadSessionsPerRepo and UploadSessionsPerPrincipal
}

// envOptions change what newEnv builds.
type envOptions struct {
	authz       auth.Authorizer
	registry    func(*providertest.Fake) provider.Provider // wraps the fake as the registry "local"
	ingest      Ingester                                   // replaces the real pipeline
	maxManifest int64                                      // Options.MaxManifestSize
	behind      time.Duration                              // Options.MaxCreatedBehind
	externalURL string                                     // Options.ExternalURL

	sessions [2]int // Options.UploadSessionsPerRepo and UploadSessionsPerPrincipal
}

func newEnv(t *testing.T, o envOptions) *env {
	t.Helper()
	ctx := t.Context()
	st, err := sqlite.Open(ctx, fmt.Sprintf("file:facade-%d?mode=memory&cache=shared", memoryDBs.Add(1)))
	must(t, err)
	t.Cleanup(func() { _ = st.Close() })
	must(t, st.Migrate(ctx))
	e := &env{t: t, st: st, prov: &providertest.Fake{RegistryName: "local"}, authz: o.authz, logs: &logBuffer{},
		clock: &clock{now: time.Date(2026, 11, 7, 10, 0, 0, 0, time.UTC)}, ingest: o.ingest, max: o.maxManifest, behind: o.behind,
		tokens: keys.NewEphemeralTokens(), extURL: o.externalURL, sessions: o.sessions}
	e.reg = e.prov
	if o.registry != nil {
		e.reg = o.registry(e.prov)
	}
	if e.ingest == nil {
		e.ingest = ingest.New(ingest.Options{Providers: provider.Map{"local": e.reg}, Index: st,
			Log: slog.New(slog.NewTextHandler(e.logs, nil)), Now: e.clock.Now})
	}
	for _, r := range []*index.Repo{&e.raw, &e.oci} {
		*r = index.Repo{Registry: "local", Kind: "hosted"}
	}
	e.raw.Format, e.raw.Name, e.raw.Prefix = "raw", "assets", "raw/assets"
	e.oci.Format, e.oci.Name, e.oci.Prefix = "oci", "images", "oci/images"
	must(t, st.CreateRepo(ctx, &e.raw))
	must(t, st.CreateRepo(ctx, &e.oci))
	e.h = e.replica()
	return e
}

// replica returns another facade over the env's store and registry: what a second replica of
// the server runs. It shares nothing with the others but the database, the backend and the
// token key.
func (e *env) replica() http.Handler {
	return e.replicaWith(e.tokens)
}

// replicaWith returns a replica whose token keys are k.
func (e *env) replicaWith(k token.Keys) http.Handler {
	h := New(Options{Index: e.st, Providers: provider.Map{"local": e.reg}, Auth: e.authz, Ingest: e.ingest,
		MaxManifestSize: e.max, MaxCreatedBehind: e.behind, Log: slog.New(slog.NewTextHandler(e.logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
		Now: e.clock.Now, Tokens: k, ExternalURL: e.extURL,
		UploadSessionsPerRepo: e.sessions[0], UploadSessionsPerPrincipal: e.sessions[1]})
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if name := r.Header.Get(principalHeader); name != "" {
			r = r.WithContext(auth.WithPrincipal(r.Context(), userP(name)))
		}
		h.ServeHTTP(w, r)
	})
}

// do sends a request to h; hdr are header name and value pairs. A body of a known length gets
// its Content-Length, as a client sends it; a "Content-Length" pair sets it, and -1 makes the
// body chunked.
func (e *env) do(h http.Handler, method, target string, body io.Reader, hdr ...string) *httptest.ResponseRecorder {
	e.t.Helper()
	req := httptest.NewRequest(method, target, body)
	for i := 0; i+1 < len(hdr); i += 2 {
		if hdr[i] == "Content-Length" {
			n, err := strconv.ParseInt(hdr[i+1], 10, 64)
			must(e.t, err)
			req.ContentLength = n
			continue
		}
		req.Header.Set(hdr[i], hdr[i+1])
	}
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	return rr
}

// clock is the facade's clock in a test.
type clock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *clock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// logBuffer collects log output; slog handlers may write from more than one goroutine.
type logBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *logBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *logBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// authorizer allows each user the actions listed for it on each repo (<format>/<name>);
// anonymous gets nothing.
type authorizer map[string]map[string][]auth.Action

func (z authorizer) Allowed(_ context.Context, p auth.Principal, a auth.Action, f, r string) (bool, error) {
	name := p.Name
	if n, ok := testNames.Load(p.Name); ok {
		name = n.(string)
	}
	return slices.Contains(z[name][f+"/"+r], a), nil
}

// userP is the principal of the test user called name, user:<uid>, as the authenticator makes
// it: the uid is the first 16 bytes of the sha256 of the name, so that a test can name it.
func userP(name string) auth.Principal {
	sum := sha256.Sum256([]byte(name))
	h := hex.EncodeToString(sum[:16])
	uid := h[:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:]
	testNames.Store(uid, name)
	return auth.Principal{Kind: auth.KindUser, Name: uid}
}

// testNames maps the uids userP made to the names, for authorizer, whose grants name users.
var testNames sync.Map

// must stops the test on an error.
func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// wantStatus fails the test when the answer's status is not want, with the body.
func wantStatus(t *testing.T, what string, rr *httptest.ResponseRecorder, want int) {
	t.Helper()
	if rr.Code != want {
		t.Errorf("%s: status %d, want %d; body %s", what, rr.Code, want, rr.Body)
	}
}

// wantError fails the test when the answer is not status with one error of code in the
// distribution format, and returns its message.
func wantError(t *testing.T, what string, rr *httptest.ResponseRecorder, status int, code string) string {
	t.Helper()
	wantStatus(t, what, rr, status)
	var body errorBody
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil || len(body.Errors) != 1 {
		t.Errorf("%s: body %q is not one error of the distribution format (%v)", what, rr.Body, err)
		return ""
	}
	if body.Errors[0].Code != code {
		t.Errorf("%s: code %s, want %s (message %q)", what, body.Errors[0].Code, code, body.Errors[0].Message)
	}
	if ct := rr.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("%s: Content-Type %q", what, ct)
	}
	return body.Errors[0].Message
}

// wantHeader fails the test when the answer's header name is not want.
func wantHeader(t *testing.T, what string, rr *httptest.ResponseRecorder, name, want string) {
	t.Helper()
	if got := rr.Header().Get(name); got != want {
		t.Errorf("%s: %s = %q, want %q", what, name, got, want)
	}
}

// pushBlob stores b in the OCI repository name of the fake and returns its digest.
func (e *env) pushBlob(name string, b []byte) digest.Digest {
	e.t.Helper()
	rr, err := e.prov.Repository(e.t.Context(), name)
	must(e.t, err)
	d := digest.FromBytes(b)
	must(e.t, rr.Push(e.t.Context(), ocispec.Descriptor{MediaType: octetStream, Digest: d, Size: int64(len(b))}, bytes.NewReader(b)))
	return d
}

// manifest returns an image manifest with one layer of the given bytes, and its digest.
func manifest(t *testing.T, layer []byte, annotation string) ([]byte, digest.Digest) {
	t.Helper()
	m := ocispec.Manifest{
		Versioned:    specs.Versioned{SchemaVersion: 2},
		MediaType:    ocispec.MediaTypeImageManifest,
		ArtifactType: "application/vnd.example.test",
		Config:       ocispec.DescriptorEmptyJSON,
		Layers:       []ocispec.Descriptor{{MediaType: octetStream, Digest: digest.FromBytes(layer), Size: int64(len(layer))}},
		Annotations:  map[string]string{"test": annotation},
	}
	b, err := json.Marshal(m)
	must(t, err)
	return b, digest.FromBytes(b)
}

// zeroDigest is the digest of no blob.
const zeroDigest = digest.Digest("sha256:0000000000000000000000000000000000000000000000000000000000000000")

// ocispecManifest is the media type of an OCI image manifest.
const ocispecManifest = ocispec.MediaTypeImageManifest

// failingRegistry is the fake whose repositories fail every Resolve as a backend that rejects
// Lajstrom's credentials, with a message that names the backend.
type failingRegistry struct{ *providertest.Fake }

func (f failingRegistry) Repository(ctx context.Context, name string) (provider.Repository, error) {
	rr, err := f.Fake.Repository(ctx, name)
	return failingRepository{rr}, err
}

type failingRepository struct{ provider.Repository }

func (failingRepository) Resolve(context.Context, string) (ocispec.Descriptor, error) {
	return ocispec.Descriptor{}, fmt.Errorf("GET https://backend.example/v2/x: %w", provider.ErrUnauthorized)
}

// uploadID returns the upload id of a Location the facade answered.
func uploadID(t *testing.T, rr *httptest.ResponseRecorder) string {
	t.Helper()
	loc := rr.Header().Get("Location")
	i := strings.LastIndex(loc, "/blobs/uploads/")
	if i < 0 {
		t.Fatalf("Location %q is not an upload's (status %d, body %s)", loc, rr.Code, rr.Body)
	}
	return loc[i+len("/blobs/uploads/"):]
}
