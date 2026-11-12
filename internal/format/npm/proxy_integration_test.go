//go:build integration

package npm

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/lajstrom/lajstrom/internal/config"
	"github.com/lajstrom/lajstrom/internal/format"
	"github.com/lajstrom/lajstrom/internal/format/formattest"
	"github.com/lajstrom/lajstrom/internal/index"
	"github.com/lajstrom/lajstrom/internal/index/sqlite"
	"github.com/lajstrom/lajstrom/internal/ingest"
	"github.com/lajstrom/lajstrom/internal/provider"
	"github.com/lajstrom/lajstrom/internal/provider/generic"
	"github.com/lajstrom/lajstrom/internal/proxy"
)

// TestIntegrationProxyFetch is #694 against the registry at LAJSTROM_TEST_REGISTRY: an npm proxy
// repo of a fake npmjs, with a TMPDIR that takes no file, streams a tarball it misses from the
// upstream into an upload session of the registry and serves it from there; a tarball that
// fails its integrity leaves neither its tag nor its bytes in the registry; clients that miss
// one tarball at once share one fetch. The repo's name is random, so runs do not collide in a
// registry that keeps what they pushed.
func TestIntegrationProxyFetch(t *testing.T) {
	addr := os.Getenv("LAJSTROM_TEST_REGISTRY")
	if addr == "" {
		t.Skip("set LAJSTROM_TEST_REGISTRY=host:port to run against a registry")
	}
	ctx := t.Context()
	prov, err := generic.New(config.Registry{
		Name: "it", Type: generic.Type, URL: "http://" + addr, Prefix: os.Getenv("LAJSTROM_TEST_REGISTRY_PREFIX"),
		Auth: config.RegistryAuth{Username: os.Getenv("LAJSTROM_TEST_REGISTRY_USERNAME"), Password: os.Getenv("LAJSTROM_TEST_REGISTRY_PASSWORD")},
	}, slog.New(slog.DiscardHandler))
	must(t, err)
	st, err := sqlite.Open(ctx, fmt.Sprintf("file:npm-it-%d?mode=memory&cache=shared", memoryDBs.Add(1)))
	must(t, err)
	t.Cleanup(func() { _ = st.Close() })
	must(t, st.Migrate(ctx))
	run := make([]byte, 4)
	_, _ = rand.Read(run)
	name := "it-" + hex.EncodeToString(run)
	repo := index.Repo{Format: Name, Name: name, Registry: "it", Prefix: "npm/" + name, Kind: index.KindProxy,
		Upstream: json.RawMessage(`{"name": "npmjs"}`)}
	must(t, st.CreateRepo(ctx, &repo))
	up := newFakeNpmjs(t)
	client, err := proxy.NewUpstream(config.Upstream{Name: "npmjs", Format: Name, URL: up.srv.URL, Timeout: 30 * time.Second},
		proxy.UpstreamOptions{Proxy: func(*http.Request) (*url.URL, error) { return nil, nil }, Log: slog.New(slog.DiscardHandler)})
	must(t, err)
	providers := provider.Map{"it": prov}
	plugins := func(n string) (format.Plugin, bool) {
		return Plugin{ExternalURL: "https://registry.example"}, n == Name
	}
	pipe := ingest.New(ingest.Options{Providers: providers, Index: st, Plugins: plugins, Log: slog.New(slog.DiscardHandler)})
	core := proxy.New(proxy.Options{Index: st, Ingest: pipe, Providers: providers, Upstreams: map[string]*proxy.Upstream{"npmjs": client},
		Plugins: plugins, ExternalURL: "https://registry.example", Log: slog.New(slog.DiscardHandler)})
	deps := format.Deps{Index: st, Ingest: pipe, Providers: providers, Proxy: core, Log: slog.New(slog.DiscardHandler)}
	mux := http.NewServeMux()
	Plugin{ExternalURL: "https://registry.example"}.Routes(format.NewRouter(mux, Name, deps), deps)
	formattest.NoTempDir(t)

	tarball := func(version string) string { return "/npm/" + name + "/is-number/-/is-number-" + version + ".tgz" }
	served := func(rr *httptest.ResponseRecorder, want []byte, what string) {
		t.Helper()
		switch rr.Code {
		case http.StatusTemporaryRedirect:
			t.Logf("%s: the registry redirects blob downloads", what)
		case http.StatusOK:
			if !bytes.Equal(rr.Body.Bytes(), want) {
				t.Errorf("%s: %d bytes served, want the %d the upstream has", what, rr.Body.Len(), len(want))
			}
		default:
			t.Errorf("%s: %d %s", what, rr.Code, rr.Body)
		}
	}
	rr, err := prov.Repository(ctx, "npm/"+name+"/is-number")
	must(t, err)

	// Published before the first packument, which the repo keeps for its TTL.
	want := up.publish(t, "is-number", "7.0.0", false)
	corrupt := append(up.publish(t, "is-number", "6.0.0", true), 0)
	concurrent := up.publish(t, "is-number", "5.0.0", false)
	served(get(mux, tarball("7.0.0")), want, "the first GET")
	served(get(mux, tarball("7.0.0")), want, "the second GET")
	if desc, err := rr.Resolve(ctx, "7.0.0"); err != nil {
		t.Errorf("the tag 7.0.0 in the registry: %v", err)
	} else if ok, err := rr.Exists(ctx, ocispec.Descriptor{Digest: digest.FromBytes(want), Size: int64(len(want))}); err != nil || !ok {
		t.Errorf("the tarball of %s in the registry: %v, %v", desc.Digest, ok, err)
	}
	expect(t, "tarball requests after two GETs", up.files.Load(), int32(1))

	if rr := get(mux, tarball("6.0.0")); rr.Code != http.StatusBadGateway {
		t.Errorf("a tarball that fails its integrity: %d %s, want 502", rr.Code, rr.Body)
	}
	if _, err := rr.Resolve(ctx, "6.0.0"); !errors.Is(err, provider.ErrNotFound) {
		t.Errorf("the tag 6.0.0 after a tarball that fails its integrity: %v, want ErrNotFound", err)
	}
	if ok, err := rr.Exists(ctx, ocispec.Descriptor{Digest: digest.FromBytes(corrupt), Size: int64(len(corrupt))}); err != nil || ok {
		t.Errorf("the bytes of a tarball that fails its integrity in the registry: %v, %v; want none", ok, err)
	}

	before := up.files.Load()
	var wg sync.WaitGroup
	for i := range 6 {
		wg.Go(func() { served(get(mux, tarball("5.0.0")), concurrent, fmt.Sprintf("concurrent GET %d", i)) })
	}
	wg.Wait()
	expect(t, "tarball requests of six concurrent GETs", up.files.Load()-before, int32(1))
}
