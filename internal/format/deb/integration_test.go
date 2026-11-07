//go:build integration

package deb

import (
	"bytes"
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
	"github.com/lajstrom/lajstrom/internal/keys"
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
// [trusted=yes arch=amd64]: the routes have no Signer, as on a server without a master key, so
// the Release is unsigned (TestAptGetSigned signs).
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
	st, repo, pipe, providers := integrationRepo(t)
	name := repo.Name

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

// integrationRepo creates the apt repo of an integration test over SQLite in memory and the
// registry of testRegistry, named it-<random hex> so that runs do not collide in a registry
// that keeps what they pushed, and uploads hello 1.0.0-1 for amd64 and hello-data 1:2.0-1 for
// all to jammy/main through the real ingest pipeline.
func integrationRepo(t *testing.T) (*sqlite.Store, index.Repo, *ingest.Pipeline, provider.Map) {
	t.Helper()
	ctx := t.Context()
	st, repo, pipe, providers := emptyRepo(t, testRegistry(t))
	hello, _ := testutil.Deb(testutil.DebOptions{})
	data, _ := testutil.Deb(testutil.DebOptions{Name: "hello-data", Version: "1:2.0-1", Arch: "all"})
	for _, deb := range [][]byte{hello, data} {
		if _, err := pipe.Upload(ctx, repo, upload([]format.UploadedFile{debFile(deb)}), alice); err != nil {
			t.Fatalf("upload: %v", err)
		}
	}
	return st, repo, pipe, providers
}

// emptyRepo creates an apt repo over SQLite in memory and the registry prov, "local", named
// it-<random hex> so that runs do not collide in a registry that keeps what they pushed, and
// the real ingest pipeline over both.
func emptyRepo(t *testing.T, prov provider.Provider) (*sqlite.Store, index.Repo, *ingest.Pipeline, provider.Map) {
	t.Helper()
	ctx := t.Context()
	run := make([]byte, 4)
	_, _ = rand.Read(run)
	name := "it-" + hex.EncodeToString(run)

	st, err := sqlite.Open(ctx, "file:deb-"+name+"?mode=memory&cache=shared")
	must(t, err)
	t.Cleanup(func() { _ = st.Close() })
	must(t, st.Migrate(ctx))
	repo := index.Repo{Format: Name, Name: name, Registry: "local", Prefix: Name + "/" + name, Kind: "hosted"}
	must(t, st.CreateRepo(ctx, &repo))
	providers := provider.Map{"local": prov}
	pipe := ingest.New(ingest.Options{Providers: providers, Index: st, Log: slog.New(slog.DiscardHandler),
		Plugins: func(n string) (format.Plugin, bool) { return Plugin{}, n == Name }})
	return st, repo, pipe, providers
}

// TestAptGetInstall is the criterion of #42: apt-get install of two packages uploaded through
// PUT /apt/<repo>/upload, from a signed repo. An httptest server serves the routes over the real
// pipeline and the keys service; hello 1.0.0-1 for amd64 and hello-data 1:2.0-1 for all, which
// depends on it, are uploaded to jammy/main through the upload route; ubuntu:24.04 for amd64
// (--platform linux/amd64, emulated on an arm64 host, so that hello's libc6 is the image's)
// fetches key.asc with apt-helper, has only the source deb [signed-by=…] of the repo,
// and runs apt-get update, apt-get install of both, and apt-get download of both. The installed
// files have their content, the downloaded files the sha256 of the uploads, and every pool
// download went through the pool route: a 200 stream from the registry of testRegistry (the
// fake, or Zot or distribution with LAJSTROM_TEST_REGISTRY), and with
// LAJSTROM_TEST_REGISTRY_S3=localhost:5002, distribution on S3 (make dev-up PROFILES=s3), a
// 307 of ServeBlob that apt follows to MinIO.
func TestAptGetInstall(t *testing.T) {
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker is not on the PATH; this test runs apt-get in a container")
	}
	t.Run("registry", func(t *testing.T) { aptGetInstall(t, testRegistry(t), http.StatusOK) })
	t.Run("s3", func(t *testing.T) {
		addr := os.Getenv("LAJSTROM_TEST_REGISTRY_S3")
		if addr == "" {
			t.Skip("set LAJSTROM_TEST_REGISTRY_S3=host:port (the s3 profile of the dev stack: localhost:5002) for the 307 of a pool download")
		}
		prov, err := generic.New(config.Registry{Name: "local", Type: generic.Type, URL: "http://" + addr, Prefix: "lajstrom-it/"},
			slog.New(slog.DiscardHandler))
		must(t, err)
		aptGetInstall(t, prov, http.StatusTemporaryRedirect)
	})
}

// aptGetInstall runs TestAptGetInstall against the registry prov; poolStatus is the status of
// every pool download, 200 or 307.
func aptGetInstall(t *testing.T, prov provider.Provider, poolStatus int) {
	ctx := t.Context()
	st, repo, pipe, providers := emptyRepo(t, prov)
	secret := make([]byte, 32)
	_, _ = rand.Read(secret)
	m, err := keys.ParseMaster(hex.EncodeToString(secret))
	must(t, err)
	svc, err := keys.New(keys.Options{Index: st, Master: m, RetireGrace: 720 * time.Hour, Log: slog.New(slog.DiscardHandler)})
	must(t, err)
	deps := format.Deps{Index: st, Ingest: pipe, Providers: providers, Signer: svc, Log: slog.New(slog.DiscardHandler)}
	mux := http.NewServeMux()
	Plugin{}.Routes(format.NewRouter(mux, Name, deps), deps)
	var mu sync.Mutex
	var requests []string // "<status> <method> <path below the repo>"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
		mux.ServeHTTP(sw, r)
		mu.Lock()
		defer mu.Unlock()
		requests = append(requests, fmt.Sprintf("%d %s %s", sw.status, r.Method, strings.TrimPrefix(r.URL.Path, "/apt/"+repo.Name+"/")))
	}))
	t.Cleanup(srv.Close)
	u, err := url.Parse(srv.URL)
	must(t, err)

	hello, helloInfo := testutil.Deb(testutil.DebOptions{})
	data, dataInfo := testutil.Deb(testutil.DebOptions{Name: "hello-data", Version: "1:2.0-1", Arch: "all", Depends: "hello (>= 1.0.0)",
		Files: map[string]string{"usr/share/hello-data/greetings": "hello\nbonjour\n"}})
	for _, deb := range [][]byte{hello, data} {
		req, err := http.NewRequestWithContext(ctx, http.MethodPut, srv.URL+"/apt/"+repo.Name+"/upload?suite=jammy&component=main",
			bytes.NewReader(deb))
		must(t, err)
		resp, err := http.DefaultClient.Do(req)
		must(t, err)
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusCreated {
			t.Fatalf("PUT upload: %d %s", resp.StatusCode, body)
		}
		t.Logf("PUT upload: %d %s", resp.StatusCode, body)
	}

	base := "http://localhost:" + u.Port() + "/apt/" + repo.Name
	script := strings.Join([]string{
		"set -e",
		"rm -f /etc/apt/sources.list /etc/apt/sources.list.d/*", // nothing from the internet
		"mkdir -p /etc/apt/keyrings",
		"/usr/lib/apt/apt-helper -o APT::Sandbox::User=root download-file " + base + "/key.asc /etc/apt/keyrings/t.asc",
		"echo 'deb [signed-by=/etc/apt/keyrings/t.asc] " + base + " jammy main' > /etc/apt/sources.list.d/t.list",
		"apt-get update",
		"apt-get install -y --no-install-recommends hello hello-data",
		"echo \"=== greeting: $(cat /usr/share/hello/greeting)\"",
		"echo \"=== greetings: $(cat /usr/share/hello-data/greetings | tr '\\n' ' ')\"",
		"cd /var/tmp && apt-get -o APT::Sandbox::User=root download hello hello-data && sha256sum *.deb | sed 's/^/=== sha256 /'",
	}, "\n")
	runCtx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	out, err := exec.CommandContext(runCtx, "docker", "run", "--rm", "--network", "host", "--platform", "linux/amd64", //nolint:gosec // the test's own script
		"ubuntu:24.04", "bash", "-c", script).CombinedOutput()
	t.Logf("docker run output:\n%s", out)
	mu.Lock()
	t.Logf("requests:\n%s", strings.Join(requests, "\n"))
	reqs := slices.Clone(requests)
	mu.Unlock()
	if err != nil {
		t.Fatalf("docker run: %v", err)
	}
	for _, want := range []string{
		"=== greeting: hello, world\n",
		"=== greetings: hello bonjour \n",
		"=== sha256 " + helloInfo.SHA256 + "  hello_1.0.0-1_amd64.deb\n",
		"=== sha256 " + dataInfo.SHA256 + "  hello-data_1%3a2.0-1_all.deb\n",
	} {
		if !strings.Contains(string(out), want) {
			t.Errorf("the output has no %q", want)
		}
	}
	if regexp.MustCompile(`(?m)^E: |NO_PUBKEY|Hash Sum mismatch`).Match(out) {
		t.Error("apt reported an error")
	}
	for _, f := range []string{helloInfo.Filename, dataInfo.Filename} {
		want := fmt.Sprintf("%d GET pool/main/h/%s/%s", poolStatus, strings.SplitN(f, "_", 2)[0], f)
		if n := len(slices.DeleteFunc(slices.Clone(reqs), func(r string) bool { return r != want })); n != 2 { // install and download
			t.Errorf("%d requests %q, want 2", n, want)
		}
	}
	if slices.ContainsFunc(reqs, func(r string) bool { return strings.HasPrefix(r, "404 ") || strings.HasPrefix(r, "5") }) {
		t.Error("a request failed")
	}
}

// TestAptGetSigned is TestAptGet with signing, in ubuntu:22.04, ubuntu:24.04 and debian:12. The
// container fetches key.asc with apt-helper into /etc/apt/keyrings/t.asc, and the source is
// deb [signed-by=/etc/apt/keyrings/t.asc arch=amd64], with no [trusted=yes]: apt-get update
// verifies InRelease against that keyring, and apt-cache policy shows both versions. Then the
// container asks the test server to rotate the repo's key (GET /test/rotate) and updates
// twice, with the lists removed each time: with the keyring of before the rotation, which the
// retired key's signature satisfies while the grace holds, and with key.asc fetched again,
// which both signatures satisfy. apt's gpgv debug lines (Debug::Acquire::gpgv) say which keys
// verified each update. The master key is random.
func TestAptGetSigned(t *testing.T) {
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker is not on the PATH; this test runs apt-get in a container")
	}
	for _, image := range []string{"ubuntu:22.04", "ubuntu:24.04", "debian:12"} {
		t.Run(image, func(t *testing.T) { aptGetSigned(t, image) })
	}
}

// aptGetSigned runs TestAptGetSigned in image.
func aptGetSigned(t *testing.T, image string) {
	ctx := t.Context()
	st, repo, pipe, providers := integrationRepo(t)
	secret := make([]byte, 32)
	_, _ = rand.Read(secret)
	m, err := keys.ParseMaster(hex.EncodeToString(secret))
	must(t, err)
	svc, err := keys.New(keys.Options{Index: st, Master: m, RetireGrace: 720 * time.Hour, Log: slog.New(slog.DiscardHandler)})
	must(t, err)

	deps := format.Deps{Index: st, Ingest: pipe, Providers: providers, Signer: svc, Log: slog.New(slog.DiscardHandler)}
	mux := http.NewServeMux()
	Plugin{}.Routes(format.NewRouter(mux, Name, deps), deps)
	mux.HandleFunc("GET /test/rotate", func(w http.ResponseWriter, r *http.Request) {
		k, err := svc.Rotate(r.Context(), repo)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		_, _ = fmt.Fprintln(w, k.Key.Fingerprint)
	})
	var mu sync.Mutex
	var requests []string // "<status> <method> <path below the repo>"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
		mux.ServeHTTP(sw, r)
		mu.Lock()
		defer mu.Unlock()
		requests = append(requests, fmt.Sprintf("%d %s %s", sw.status, r.Method, strings.TrimPrefix(r.URL.Path, "/apt/"+repo.Name+"/")))
	}))
	t.Cleanup(srv.Close)
	u, err := url.Parse(srv.URL)
	must(t, err)

	base := "http://localhost:" + u.Port()
	script := strings.Join([]string{
		"set -e",
		"H=/usr/lib/apt/apt-helper K=/etc/apt/keyrings/t.asc L=/etc/apt/sources.list.d/t.list",
		"update() { find /var/lib/apt/lists -type f -delete; " +
			"apt-get -o Debug::Acquire::gpgv=1 -o Dir::Etc::sourcelist=$L -o Dir::Etc::sourceparts=- -o APT::Get::List-Cleanup=0 update; " +
			"apt-cache policy hello hello-data; }",
		// On a host that is not amd64 (arm64 under OrbStack) hello is of a foreign architecture,
		// which apt lists only once dpkg knows it.
		"[ \"$(dpkg --print-architecture)\" = amd64 ] || dpkg --add-architecture amd64",
		"mkdir -p /etc/apt/keyrings",
		"$H -o APT::Sandbox::User=root download-file " + base + "/apt/" + repo.Name + "/key.asc $K",
		"echo \"deb [signed-by=$K arch=amd64] " + base + "/apt/" + repo.Name + " jammy main\" > $L",
		"echo '=== update 1: the first key'", "update",
		"$H -o APT::Sandbox::User=root download-file " + base + "/test/rotate /var/tmp/rotated",
		"echo \"=== rotated to $(cat /var/tmp/rotated)\"",
		"echo '=== update 2: the keyring of before the rotation'", "update",
		"$H -o APT::Sandbox::User=root download-file " + base + "/apt/" + repo.Name + "/key.asc $K",
		"echo '=== update 3: key.asc fetched again'", "update",
	}, "\n")
	runCtx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	out, err := exec.CommandContext(runCtx, "docker", "run", "--rm", "--network", "host", image, //nolint:gosec // the test's own script
		"bash", "-c", script).CombinedOutput()
	t.Logf("docker run output:\n%s", out)
	mu.Lock()
	t.Logf("requests:\n%s", strings.Join(requests, "\n"))
	reqs := slices.Clone(requests)
	mu.Unlock()
	if err != nil {
		t.Fatalf("docker run: %v", err)
	}

	keyRows, err := st.ListKeys(ctx, &repo.ID)
	must(t, err)
	if len(keyRows) != 2 || keyRows[0].RetiredAt == nil || keyRows[1].RetiredAt != nil {
		t.Fatalf("keys after the run: %+v, want the first retired and the second active", keyRows)
	}
	first, second := keyRows[0].Fingerprint, keyRows[1].Fingerprint
	updates := strings.Split(string(out), "=== update ")
	if len(updates) != 4 {
		t.Fatalf("the output has %d updates, want 3", len(updates)-1)
	}
	checkSignedUpdate(t, 1, updates[1], []string{first}, "")
	checkSignedUpdate(t, 2, updates[2], []string{first}, second)
	checkSignedUpdate(t, 3, updates[3], []string{second, first}, "")
	for want, n := range map[string]int{"200 GET key.asc": 2, "200 GET dists/jammy/InRelease": 3, "200 GET /test/rotate": 1} {
		if got := len(slices.DeleteFunc(slices.Clone(reqs), func(r string) bool { return !strings.HasPrefix(r, want) })); got != n {
			t.Errorf("%d requests %q, want %d", got, want, n)
		}
	}
	if slices.ContainsFunc(reqs, func(r string) bool { return strings.HasPrefix(r, "404 ") || strings.HasPrefix(r, "500 ") }) {
		t.Error("a request failed")
	}
}

// checkSignedUpdate checks the output of update n of aptGetSigned: apt-cache policy shows both
// versions, apt's gpgv reports a VALIDSIG of every key of verified and a NO_PUBKEY of missing
// (the key id, the last 16 hexadecimal characters of its fingerprint) when it is set, and apt
// reports no error.
func checkSignedUpdate(t *testing.T, n int, out string, verified []string, missing string) {
	t.Helper()
	for _, re := range []*regexp.Regexp{
		regexp.MustCompile(`(?m)^hello(:amd64)?:\n  Installed: \(none\)\n  Candidate: 1\.0\.0-1\n`),
		regexp.MustCompile(`(?m)^hello-data:\n  Installed: \(none\)\n  Candidate: 1:2\.0-1\n`),
	} {
		if !re.MatchString(out) {
			t.Errorf("update %d: apt-cache policy does not match %s", n, re)
		}
	}
	for _, fp := range verified {
		if !strings.Contains(out, "VALIDSIG "+fp) {
			t.Errorf("update %d: gpgv reports no VALIDSIG of %s", n, fp)
		}
	}
	if missing != "" && !strings.Contains(out, "NO_PUBKEY "+missing[len(missing)-16:]) {
		t.Errorf("update %d: gpgv reports no NO_PUBKEY of %s", n, missing)
	}
	if regexp.MustCompile(`(?m)^E: `).MatchString(out) {
		t.Errorf("update %d has an error", n)
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
