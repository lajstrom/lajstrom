//go:build integration

package pypi

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
	"github.com/lajstrom/lajstrom/internal/oci/profile"
	"github.com/lajstrom/lajstrom/internal/provider"
	"github.com/lajstrom/lajstrom/internal/provider/generic"
	"github.com/lajstrom/lajstrom/internal/proxy"
	"github.com/lajstrom/lajstrom/internal/testutil"
)

// TestIntegrationProxyFetch is #67 for pypi against the registry at LAJSTROM_TEST_REGISTRY: a
// pypi proxy repo of a fake simple index, with a TMPDIR that takes no file, streams a wheel it
// misses from the upstream into an upload session of the registry and serves it from there; a
// wheel that fails its sha256 leaves neither its tag nor its bytes in the registry; clients that
// miss one wheel at once share one fetch. The repo's name is random, so runs do not collide in a
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
	st, err := sqlite.Open(ctx, "file:pypi-"+t.Name()+"?mode=memory&cache=shared")
	must(t, err)
	t.Cleanup(func() { _ = st.Close() })
	must(t, st.Migrate(ctx))
	run := make([]byte, 4)
	_, _ = rand.Read(run)
	name := "it-" + hex.EncodeToString(run)
	repo := index.Repo{Format: Name, Name: name, Registry: "it", Prefix: "pypi/" + name, Kind: index.KindProxy,
		Upstream: json.RawMessage(`{"name": "pypi"}`)}
	must(t, st.CreateRepo(ctx, &repo))
	up := newFakePyPI(t)
	client, err := proxy.NewUpstream(config.Upstream{Name: "pypi", Format: Name, URL: up.srv.URL + "/simple/", Timeout: 30 * time.Second},
		proxy.UpstreamOptions{Proxy: func(*http.Request) (*url.URL, error) { return nil, nil }, Log: slog.New(slog.DiscardHandler)})
	must(t, err)
	providers := provider.Map{"it": prov}
	plugins := func(n string) (format.Plugin, bool) { return Plugin{}, n == Name }
	pipe := ingest.New(ingest.Options{Providers: providers, Index: st, Plugins: plugins, Log: slog.New(slog.DiscardHandler)})
	core := proxy.New(proxy.Options{Index: st, Ingest: pipe, Providers: providers, Upstreams: map[string]*proxy.Upstream{"pypi": client},
		Plugins: plugins, ExternalURL: "https://pypi.example", Log: slog.New(slog.DiscardHandler)})
	deps := format.Deps{Index: st, Ingest: pipe, Providers: providers, Proxy: core, Log: slog.New(slog.DiscardHandler)}
	mux := http.NewServeMux()
	Plugin{}.Routes(format.NewRouter(mux, Name, deps), deps)
	formattest.NoTempDir(t)

	get := func(path string) *httptest.ResponseRecorder {
		rr := httptest.NewRecorder()
		mux.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, path, nil))
		return rr
	}
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
	wheel := func(version string, other bool) ([]byte, string) {
		b, info := testutil.Wheel(testutil.WheelOptions{Name: "my-pkg", Version: version})
		up.publish("my-pkg", info.Filename, b, info.Metadata, other, nil)
		return b, "/pypi/" + name + "/files/my-pkg/" + version + "/" + info.Filename
	}
	rr, err := prov.Repository(ctx, "pypi/"+name+"/my-pkg")
	must(t, err)

	// Published before the first page, which the repo keeps for its TTL.
	want, path := wheel("2.0", false)
	corrupt, badPath := wheel("1.0", true)
	concurrent, sharedPath := wheel("0.9", false)
	served(get(path), want, "the first GET")
	served(get(path), want, "the second GET")
	if desc, err := rr.Resolve(ctx, profile.Tag("2.0", "py3-none-any")); err != nil {
		t.Errorf("the tag of 2.0 in the registry: %v", err)
	} else if ok, err := rr.Exists(ctx, ocispec.Descriptor{Digest: digest.FromBytes(want), Size: int64(len(want))}); err != nil || !ok {
		t.Errorf("the wheel of %s in the registry: %v, %v", desc.Digest, ok, err)
	}
	expect(t, "file requests after two GETs", up.fetches.Load(), int32(1))

	if rr := get(badPath); rr.Code != http.StatusBadGateway {
		t.Errorf("a wheel that fails its sha256: %d %s, want 502", rr.Code, rr.Body)
	}
	if _, err := rr.Resolve(ctx, profile.Tag("1.0", "py3-none-any")); !errors.Is(err, provider.ErrNotFound) {
		t.Errorf("the tag of 1.0 after a wheel that fails its sha256: %v, want ErrNotFound", err)
	}
	if ok, err := rr.Exists(ctx, ocispec.Descriptor{Digest: digest.FromBytes(corrupt), Size: int64(len(corrupt))}); err != nil || ok {
		t.Errorf("the bytes of a wheel that fails its sha256 in the registry: %v, %v; want none", ok, err)
	}

	before := up.fetches.Load()
	var wg sync.WaitGroup
	for i := range 6 {
		wg.Go(func() { served(get(sharedPath), concurrent, fmt.Sprintf("concurrent GET %d", i)) })
	}
	wg.Wait()
	expect(t, "file requests of six concurrent GETs", up.fetches.Load()-before, int32(1))
}
