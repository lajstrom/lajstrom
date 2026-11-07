//go:build integration

package deb

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/lajstrom/lajstrom/internal/config"
	"github.com/lajstrom/lajstrom/internal/format"
	"github.com/lajstrom/lajstrom/internal/index"
	"github.com/lajstrom/lajstrom/internal/index/sqlite"
	"github.com/lajstrom/lajstrom/internal/ingest"
	"github.com/lajstrom/lajstrom/internal/provider"
	"github.com/lajstrom/lajstrom/internal/provider/generic"
	"github.com/lajstrom/lajstrom/internal/provider/providertest"
	"github.com/lajstrom/lajstrom/internal/testutil"
)

// TestAptGet serves an apt repo through httptest, the routes, the real ingest pipeline over
// SQLite in memory and the registry at LAJSTROM_TEST_REGISTRY (host:port, plain HTTP, with
// LAJSTROM_TEST_REGISTRY_PREFIX, _USERNAME and _PASSWORD when set), or the fake registry when it
// is unset. Two packages are uploaded to jammy/main, hello 1.0.0-1 for amd64 and hello-data
// 1:2.0-1 for all, and apt-get update and apt-cache policy run twice in ubuntu:24.04 with
// [trusted=yes arch=amd64], as the quick start of #42 will: the Release is unsigned until #41.
// apt fetches InRelease and Release.gpg (404), the Release, and Packages.gz, then Packages.xz,
// by hash, and the policy shows both versions. Then the metadata is dropped from the index,
// and Packages is the same, rendered from the metadata layers in the registry. The container
// reaches the server through --network host, which works on Linux and under OrbStack. Skipped
// when docker is not on the PATH. The repo name is random, so runs do not collide in a
// registry that keeps what they pushed.
func TestAptGet(t *testing.T) {
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker is not on the PATH; this test runs apt-get in a container")
	}
	ctx := t.Context()
	prov := testRegistry(t)
	run := make([]byte, 4)
	_, _ = rand.Read(run)
	name := "it-" + hex.EncodeToString(run)

	st, err := sqlite.Open(ctx, "file:deb-apt-get?mode=memory&cache=shared")
	must(t, err)
	t.Cleanup(func() { _ = st.Close() })
	must(t, st.Migrate(ctx))
	repo := index.Repo{Format: Name, Name: name, Registry: "local", Prefix: Name + "/" + name, Kind: "hosted"}
	must(t, st.CreateRepo(ctx, &repo))
	providers := provider.Map{"local": prov}
	pipe := ingest.New(ingest.Options{Providers: providers, Index: st, Log: slog.New(slog.DiscardHandler),
		Plugins: func(n string) (format.Plugin, bool) { return Plugin{}, n == Name }})
	hello, _ := testutil.Deb(testutil.DebOptions{})
	data, _ := testutil.Deb(testutil.DebOptions{Name: "hello-data", Version: "1:2.0-1", Arch: "all"})
	for _, deb := range [][]byte{hello, data} {
		if _, err := pipe.Upload(ctx, repo, upload([]format.UploadedFile{debFile(deb)}), alice); err != nil {
			t.Fatalf("upload: %v", err)
		}
	}

	deps := format.Deps{Index: st, Ingest: pipe, Providers: providers, Log: slog.New(slog.DiscardHandler)}
	mux := http.NewServeMux()
	Plugin{}.Routes(format.NewRouter(mux, Name, deps), deps)
	var mu sync.Mutex
	var requests []string // "<status> <method> <path below the repo>"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
		mux.ServeHTTP(sw, r)
		mu.Lock()
		defer mu.Unlock()
		requests = append(requests, fmt.Sprintf("%d %s %s", sw.status, r.Method, strings.TrimPrefix(r.URL.Path, "/apt/"+name+"/")))
	}))
	t.Cleanup(srv.Close)
	u, err := url.Parse(srv.URL)
	must(t, err)

	// The image asks apt for gzip indexes (docker-gzip-indexes); the second update, without
	// that file and with the lists removed, takes Packages.xz, apt's first choice.
	update := `apt-get -o Dir::Etc::sourcelist=/etc/apt/sources.list.d/t.list -o Dir::Etc::sourceparts=- -o APT::Get::List-Cleanup=0 update && ` +
		`apt-cache policy hello hello-data`
	script := fmt.Sprintf(`echo "deb [trusted=yes arch=amd64] http://localhost:%s/apt/%s jammy main" > /etc/apt/sources.list.d/t.list && `+
		`%s && find /var/lib/apt/lists -type f -delete && rm /etc/apt/apt.conf.d/docker-gzip-indexes && %s`, u.Port(), name, update, update)
	runCtx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	out, err := exec.CommandContext(runCtx, "docker", "run", "--rm", "--network", "host", "ubuntu:24.04", //nolint:gosec // the test's own script
		"bash", "-c", script).CombinedOutput()
	t.Logf("docker run output:\n%s", out)
	mu.Lock()
	t.Logf("requests:\n%s", strings.Join(requests, "\n"))
	reqs := slices.Clone(requests)
	mu.Unlock()
	if err != nil {
		t.Fatalf("docker run: %v", err)
	}
	// On a host that is not amd64 the policy names hello hello:amd64, a foreign architecture;
	// hello-data is all.
	for _, want := range []*regexp.Regexp{
		regexp.MustCompile(`(?m)^hello(:amd64)?:\n  Installed: \(none\)\n  Candidate: 1\.0\.0-1\n`),
		regexp.MustCompile(`(?m)^hello-data:\n  Installed: \(none\)\n  Candidate: 1:2\.0-1\n`),
	} {
		if n := len(want.FindAllIndex(out, -1)); n != 2 {
			t.Errorf("apt-cache policy matches %s %d times, want once per update", want, n)
		}
	}
	for _, prefix := range []string{"404 GET dists/jammy/InRelease", "200 GET dists/jammy/Release",
		"200 GET dists/jammy/main/binary-amd64/by-hash/SHA256/"} {
		if !slices.ContainsFunc(reqs, func(r string) bool { return strings.HasPrefix(r, prefix) }) {
			t.Errorf("no request %q", prefix)
		}
	}
	sums := listed(get(t, srv.URL+"/apt/"+name+"/dists/jammy/Release"))
	for _, kind := range []string{"main/binary-amd64/Packages.gz", "main/binary-amd64/Packages.xz"} {
		if !slices.Contains(reqs, "200 GET dists/jammy/main/binary-amd64/by-hash/SHA256/"+sums[kind]) {
			t.Errorf("apt did not fetch %s by hash", kind)
		}
	}

	// The same index without metadata in the index: the renderer reads the metadata layers
	// from the registry.
	packages := srv.URL + "/apt/" + name + "/dists/jammy/main/binary-amd64/Packages"
	before := get(t, packages)
	pkgs, err := st.ListPackages(ctx, repo.ID, "")
	must(t, err)
	for _, pkg := range pkgs {
		vs, err := st.ListVersions(ctx, pkg.ID)
		must(t, err)
		for _, v := range vs {
			v.Metadata = nil
			must(t, st.UpsertVersion(ctx, &v))
		}
	}
	must(t, st.InvalidateIndexCache(ctx, repo.ID, ""))
	if after := get(t, packages); string(after) != string(before) {
		t.Errorf("Packages rendered from the metadata layers in the registry:\n%s\nwant\n%s", after, before)
	}
}

// get returns the body of a GET of u, which must answer 200.
func get(t *testing.T, u string) []byte {
	t.Helper()
	resp, err := http.Get(u) //nolint:gosec // the test server
	must(t, err)
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(resp.Body)
	must(t, err)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s = %d %s", u, resp.StatusCode, b)
	}
	return b
}

// testRegistry returns the generic provider for LAJSTROM_TEST_REGISTRY, or the fake registry
// when it is unset.
func testRegistry(t *testing.T) provider.Provider {
	t.Helper()
	addr := os.Getenv("LAJSTROM_TEST_REGISTRY")
	if addr == "" {
		t.Log("LAJSTROM_TEST_REGISTRY is unset: the fake registry")
		return &providertest.Fake{RegistryName: "local"}
	}
	prov, err := generic.New(config.Registry{
		Name: "local", Type: generic.Type, URL: "http://" + addr, Prefix: os.Getenv("LAJSTROM_TEST_REGISTRY_PREFIX"),
		Auth: config.RegistryAuth{Username: os.Getenv("LAJSTROM_TEST_REGISTRY_USERNAME"), Password: os.Getenv("LAJSTROM_TEST_REGISTRY_PASSWORD")},
	}, slog.New(slog.DiscardHandler))
	must(t, err)
	return prov
}

// statusWriter records the status a handler answers with.
type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(status int) {
	w.status = status
	w.ResponseWriter.WriteHeader(status)
}
