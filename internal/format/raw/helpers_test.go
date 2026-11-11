package raw

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/lajstrom/lajstrom/internal/audit"
	"github.com/lajstrom/lajstrom/internal/auth"
	"github.com/lajstrom/lajstrom/internal/format"
	"github.com/lajstrom/lajstrom/internal/index"
	"github.com/lajstrom/lajstrom/internal/index/sqlite"
	"github.com/lajstrom/lajstrom/internal/ingest"
	"github.com/lajstrom/lajstrom/internal/provider"
	"github.com/lajstrom/lajstrom/internal/provider/providertest"
)

// memoryDBs numbers the in-memory databases, so that every test gets its own.
var memoryDBs atomic.Int64

// newStore returns the real SQLite store in memory, migrated, with the repo raw/scratch on the
// registry "local" and policy.
func newStore(t *testing.T, policy string) (*sqlite.Store, index.Repo) {
	t.Helper()
	ctx := t.Context()
	st, err := sqlite.Open(ctx, fmt.Sprintf("file:raw-%d?mode=memory&cache=shared", memoryDBs.Add(1)))
	must(t, err)
	t.Cleanup(func() { _ = st.Close() })
	must(t, st.Migrate(ctx))
	repo := index.Repo{Format: "raw", Name: "scratch", Registry: "local", Prefix: "raw/scratch", Kind: "hosted"}
	if policy != "" {
		repo.Policy = json.RawMessage(policy)
	}
	must(t, st.CreateRepo(ctx, &repo))
	return st, repo
}

// principalHeader names the principal of a test request: a user name, none for anonymous.
const principalHeader = "X-Test-User"

// env is the raw routes on a fresh ServeMux over the real ingest pipeline, the real SQLite store
// in memory and the fake provider as the registry "local".
type env struct {
	t    *testing.T
	st   *sqlite.Store
	prov *providertest.Fake
	repo index.Repo
	h    http.Handler
	logs *logBuffer // what the routes logged
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

// envOptions change what newEnv builds.
type envOptions struct {
	policy   string
	authz    auth.Authorizer
	audit    audit.Writer
	registry provider.Provider             // the registry "local"; nil means the fake (env.prov)
	store    func(index.Store) index.Store // wraps the store the routes read; ingest writes to the store itself
	ingested func(index.Store) index.Store // wraps the store ingest writes to
}

func newEnv(t *testing.T, o envOptions) *env {
	t.Helper()
	st, repo := newStore(t, o.policy)
	e := &env{t: t, st: st, prov: &providertest.Fake{RegistryName: "local"}, repo: repo, logs: &logBuffer{}}
	providers := provider.Map{"local": e.prov}
	if o.registry != nil {
		providers["local"] = o.registry
	}
	var ingestIndex index.Store = st
	if o.ingested != nil {
		ingestIndex = o.ingested(st)
	}
	pipe := ingest.New(ingest.Options{Providers: providers, Index: ingestIndex, Audit: o.audit, Log: slog.New(slog.DiscardHandler),
		Plugins: func(name string) (format.Plugin, bool) { return Plugin{}, name == Name }})
	var routeIndex index.Store = st
	if o.store != nil {
		routeIndex = o.store(st)
	}
	deps := format.Deps{Index: routeIndex, Ingest: pipe, Providers: providers, Auth: o.authz, Audit: o.audit,
		Log: slog.New(slog.NewTextHandler(e.logs, nil))}
	mux := http.NewServeMux()
	Plugin{}.Routes(format.NewRouter(mux, Name, deps), deps)
	e.h = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if name := r.Header.Get(principalHeader); name != "" {
			r = r.WithContext(auth.WithPrincipal(r.Context(), auth.Principal{Kind: auth.KindUser, Name: name}))
		}
		mux.ServeHTTP(w, r)
	})
	return e
}

// target is the request target of a path below /raw/scratch/, each component percent-encoded.
func target(p string) string {
	parts := strings.Split(p, "/")
	for i, c := range parts {
		parts[i] = url.PathEscape(c)
	}
	return "/raw/scratch/" + strings.Join(parts, "/")
}

// do sends a request; hdr are header name and value pairs.
func (e *env) do(method, target string, body io.Reader, hdr ...string) *httptest.ResponseRecorder {
	e.t.Helper()
	req := httptest.NewRequest(method, target, body)
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	rr := httptest.NewRecorder()
	e.h.ServeHTTP(rr, req)
	return rr
}

// put uploads content to path p and returns the response, which must be 201.
func (e *env) put(p, content string, hdr ...string) putResponse {
	e.t.Helper()
	rr := e.do(http.MethodPut, target(p), strings.NewReader(content), hdr...)
	if rr.Code != http.StatusCreated {
		e.t.Fatalf("PUT %s = %d %s", p, rr.Code, rr.Body)
	}
	var resp putResponse
	must(e.t, json.Unmarshal(rr.Body.Bytes(), &resp))
	return resp
}

// versions returns the versions of path p in the index.
func (e *env) versions(p string) []index.Version {
	e.t.Helper()
	pkg, err := e.st.GetPackage(e.t.Context(), e.repo.ID, p)
	if err != nil {
		return nil
	}
	vs, err := e.st.ListVersions(e.t.Context(), pkg.ID)
	must(e.t, err)
	return vs
}

// manifest returns the annotations of the manifest d in the fake's repository name, or nil when
// the fake does not have it.
func (e *env) manifest(name string, d digest.Digest) map[string]string {
	e.t.Helper()
	ctx := e.t.Context()
	rr, err := e.prov.Repository(ctx, name)
	must(e.t, err)
	desc, err := rr.Resolve(ctx, d.String())
	if err != nil {
		return nil
	}
	rc, err := rr.Fetch(ctx, desc)
	must(e.t, err)
	defer func() { _ = rc.Close() }()
	var m ocispec.Manifest
	must(e.t, json.NewDecoder(rc).Decode(&m))
	return m.Annotations
}

// audit returns the audit rows of action.
func (e *env) audit(action string) []index.AuditEvent {
	e.t.Helper()
	evs, err := e.st.ListAudit(e.t.Context(), index.AuditFilter{Action: action})
	must(e.t, err)
	slices.Reverse(evs) // oldest first
	return evs
}

// authorizer allows each user the actions listed for it; anonymous gets nothing.
type authorizer map[string][]auth.Action

func (z authorizer) Allowed(_ context.Context, p auth.Principal, a auth.Action, f, r string) (bool, error) {
	return f == "raw" && r == "scratch" && slices.Contains(z[p.Name], a), nil
}

// auditWriter records the events written through it.
type auditWriter struct{ events []index.AuditEvent }

func (w *auditWriter) Write(_ context.Context, ev index.AuditEvent) error {
	w.events = append(w.events, ev)
	return nil
}

// must stops the test on an error.
func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// header fails the test when the response header name is not want.
func header(t *testing.T, rr *httptest.ResponseRecorder, name, want string) {
	t.Helper()
	if got := rr.Header().Get(name); got != want {
		t.Errorf("%s = %q, want %q", name, got, want)
	}
}

// sign pushes to the OCI repository name a cosign bundle whose subject is the manifest of
// digest subject, straight to the registry, and returns the bundle's digest.
func (e *env) sign(t *testing.T, name, subject string) digest.Digest {
	t.Helper()
	m := []byte(`{"schemaVersion":2,"mediaType":"` + ocispec.MediaTypeImageManifest + `",` +
		`"artifactType":"application/vnd.dev.sigstore.bundle.v0.3+json",` +
		`"config":{"mediaType":"` + ocispec.MediaTypeEmptyJSON + `","digest":"` + ocispec.DescriptorEmptyJSON.Digest.String() + `","size":2},` +
		`"layers":[],"subject":{"mediaType":"` + ocispec.MediaTypeImageManifest + `","digest":"` + subject + `","size":1}}`)
	d := ocispec.Descriptor{MediaType: ocispec.MediaTypeImageManifest, Digest: digest.FromBytes(m), Size: int64(len(m))}
	r, err := e.prov.Repository(t.Context(), name)
	if err == nil {
		err = r.Push(t.Context(), d, bytes.NewReader(m))
	}
	if err != nil {
		t.Fatal(err)
	}
	return d.Digest
}
