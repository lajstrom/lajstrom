package generic

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/lajstrom/lajstrom/internal/config"
	"github.com/lajstrom/lajstrom/internal/provider"
)

// The answers to GET /v2/ recorded in docs/registries: the headers of the 200 and the
// challenge of the 401 before it.
var (
	gitLabA = http.Header{
		"Docker-Distribution-Api-Version":            {"registry/2.0"},
		"Gitlab-Container-Registry-Version":          {"4.43.0-gitlab"},
		"Gitlab-Container-Registry-Features":         {"tag_delete"},
		"Gitlab-Container-Registry-Database-Enabled": {"false"},
	}
	gitLabB = http.Header{
		"Docker-Distribution-Api-Version":            {"registry/2.0"},
		"Gitlab-Container-Registry-Version":          {"4.43.0-gitlab"},
		"Gitlab-Container-Registry-Features":         {"tag_delete"},
		"Gitlab-Container-Registry-Database-Enabled": {"true"},
	}
	plainV2 = http.Header{"Docker-Distribution-Api-Version": {"registry/2.0"}}
	zotV2   = http.Header{"Docker-Distribution-Api-Version": {"registry/2.0"},
		"Access-Control-Allow-Headers": {"Authorization,content-type,X-ZOT-API-CLIENT"}}
	nexusV2                   = http.Header{"Docker-Distribution-Api-Version": {"registry/2.0"}, "Server": {"Nexus/3.98.1-01 (COMMUNITY)"}}
	jfrogV2                   = http.Header{"Docker-Distribution-Api-Version": {"registry/2.0"}, "X-Jfrog-Version": {"Artifactory/7.161.26 86126900"}, "X-Artifactory-Id": {"a1b2"}}
	nexusChallenge            = `BASIC realm="Sonatype Nexus Repository Manager"`
	giteaChallenge            = `Bearer realm="http://localhost:5009/v2/token",service="container_registry",scope="*"`
	gitLabComChallenge        = `Bearer realm="https://gitlab.com/jwt/auth",service="container_registry"`
	harborChallenge           = `Bearer realm="http://localhost:5005/service/token",service="harbor-registry"`
	harborCatalogChallenge    = `Basic realm="harbor"`
	distributionChallenge     = `Basic realm="lajstrom"`
	gitLabTestServerChallenge = `Bearer realm="http://localhost:5005/token"`
)

func TestDetectVendor(t *testing.T) {
	tests := []struct {
		name   string
		id     identity
		want   vendor
		gitLab bool // the metadata database
	}{
		{name: "GitLab A", id: identity{host: "localhost:5003", header: gitLabA}, want: vendorGitLab},
		{name: "GitLab B", id: identity{host: "localhost:5004", header: gitLabB}, want: vendorGitLab, gitLab: true},
		{name: "GitLab B behind a token server", id: identity{host: "localhost:5006", header: gitLabB, challenge: gitLabTestServerChallenge}, want: vendorGitLab, gitLab: true},
		{name: "GHCR", id: identity{host: "ghcr.io", header: plainV2}, want: vendorGHCR},
		{name: "GHCR with the port", id: identity{host: "ghcr.io:443", header: plainV2}, want: vendorGHCR},
		{name: "GHCR in capitals, with the root dot", id: identity{host: "GHCR.io.", header: plainV2}, want: vendorGHCR},
		{name: "GHCR before a ping", id: identity{host: "ghcr.io"}, want: vendorGHCR},
		{name: "Harbor", id: identity{host: "localhost:5005", header: plainV2, challenge: harborChallenge}, want: vendorHarbor},
		{name: "Harbor, unquoted and spaced", id: identity{host: "harbor.example", challenge: `bearer realm="https://harbor.example/service/token", service = harbor-registry`}, want: vendorHarbor},
		{name: "Harbor's Basic challenge of the catalog", id: identity{host: "localhost:5005", challenge: harborCatalogChallenge}, want: vendorUnknown},
		{name: "another token service", id: identity{host: "localhost:5005", challenge: `Bearer realm="http://localhost:5005/service/token",service="harbor-registry-2"`}, want: vendorUnknown},
		{name: "Nexus", id: identity{host: "localhost:5008", header: nexusV2, challenge: nexusChallenge}, want: vendorNexus},
		{name: "Nexus by its header", id: identity{host: "localhost:5015", header: nexusV2}, want: vendorNexus},
		{name: "Nexus by its challenge", id: identity{host: "localhost:5015", challenge: nexusChallenge}, want: vendorNexus},
		{name: "another Basic realm", id: identity{host: "localhost:5015", challenge: `Basic realm="Sonatype"`}, want: vendorUnknown},
		{name: "another server", id: identity{host: "localhost:5015", header: http.Header{"Server": {"nginx/1.29"}}}, want: vendorUnknown},
		{name: "JFrog", id: identity{host: "localhost:5010", header: jfrogV2, challenge: `Bearer realm="http://localhost:5010/v2/token",service="localhost:5010"`}, want: vendorJFrog},
		{name: "Gitea and Forgejo", id: identity{host: "localhost:5009", header: plainV2, challenge: giteaChallenge}, want: vendorGitea},
		{name: "GitLab.com's challenge without its header", id: identity{host: "registry.gitlab.com", challenge: gitLabComChallenge}, want: vendorUnknown},
		{name: "distribution", id: identity{host: "localhost:5001", header: plainV2}, want: vendorDistribution},
		{name: "distribution with htpasswd", id: identity{host: "localhost:55002", header: plainV2, challenge: distributionChallenge}, want: vendorDistribution},
		{name: "Docker Hub", id: identity{host: "registry-1.docker.io", header: plainV2}, want: vendorUnknown},
		{name: "ACR", id: identity{host: "example.azurecr.io:443", header: plainV2}, want: vendorUnknown},
		{name: "Zot", id: identity{host: "localhost:5000", header: zotV2}, want: vendorZot},
		{name: "a host that ends in ghcr.io", id: identity{host: "notghcr.io"}, want: vendorUnknown},
		{name: "a host below ghcr.io", id: identity{host: "ghcr.io.example.com"}, want: vendorUnknown},
		{name: "nothing", want: vendorUnknown},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := detectVendor(tt.id); got != tt.want {
				t.Errorf("detectVendor = %q, want %q", got, tt.want)
			}
			if got := tt.id.gitLabDatabase(); got != tt.gitLab {
				t.Errorf("gitLabDatabase = %v, want %v", got, tt.gitLab)
			}
		})
	}
}

func TestChallengeParam(t *testing.T) {
	tests := []struct {
		challenge, name     string
		wantScheme, wantVal string
	}{
		{harborChallenge, "service", "bearer", "harbor-registry"},
		{harborChallenge, "realm", "bearer", "http://localhost:5005/service/token"},
		{harborChallenge, "scope", "bearer", ""},
		{distributionChallenge, "realm", "basic", "lajstrom"},
		{`Bearer realm="a,b",service="s"`, "service", "bearer", "s"},
		{`Bearer realm="say \"hi\"",service=s`, "realm", "bearer", `say "hi"`},
		{`Bearer realm="say \"hi\"",service=s`, "service", "bearer", "s"},
		{`Bearer realm="https://r/token",scope="repository:a/b:pull,push"`, "scope", "bearer", "repository:a/b:pull,push"},
		{`Bearer SERVICE="x"`, "service", "bearer", "x"},
		{`Bearer realm="unterminated`, "realm", "bearer", "unterminated"},
		{`Bearer realm="trailing backslash\`, "realm", "bearer", "trailing backslash"},
		{`Bearer token68==`, "service", "bearer", ""},
		{`Bearer`, "service", "bearer", ""},
		{"", "service", "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.challenge+" "+tt.name, func(t *testing.T) {
			scheme, val := challengeParam(tt.challenge, tt.name)
			if scheme != tt.wantScheme || val != tt.wantVal {
				t.Errorf("challengeParam = %q, %q; want %q, %q", scheme, val, tt.wantScheme, tt.wantVal)
			}
		})
	}
}

func TestVendorDefaults(t *testing.T) {
	probed := provider.Capabilities{CreateOnPush: true, Referrers: true, Catalog: true, DeleteManifest: true, BlobRedirect: true}
	strong := probed
	strong.Consistency = provider.ConsistencyStrong
	tests := []struct {
		vendor vendor
		want   provider.Capabilities
	}{
		{vendorUnknown, probed},
		{vendorGitLab, strong},
		{vendorZot, strong},
		{vendorDistribution, strong},
		{vendorHarbor, probed},
		{vendorGHCR, provider.Capabilities{CreateOnPush: true, Referrers: true, BlobRedirect: true, Consistency: provider.ConsistencyEventual,
			MaxBlobSize: 10_000_000_000}},
	}
	for _, tt := range tests {
		t.Run(string(tt.vendor), func(t *testing.T) {
			got := probed
			apply(&got, vendorDefaults[tt.vendor])
			if got != tt.want {
				t.Errorf("the table applied to %+v =\n%+v\nwant\n%+v", probed, got, tt.want)
			}
		})
	}
}

// TestCapabilitiesVendor runs the probe against the fake answering GET /v2/ as the registries
// of the table did, and checks what the table makes of the probe's answer (afterProbe).
func TestCapabilitiesVendor(t *testing.T) {
	ctx := context.Background()
	defaults := provider.Capabilities{CreateOnPush: true, Referrers: true, Catalog: true, DeleteManifest: true, BlobRedirect: true}
	noReferrers := provider.Capabilities{CreateOnPush: true, Catalog: true, DeleteManifest: true, BlobRedirect: true}
	nexusDocker := provider.Capabilities{CreateOnPush: true, Catalog: true, BlobRedirect: true}
	strong := defaults
	strong.Consistency = provider.ConsistencyStrong
	headers := func(h http.Header, referrers bool) func(f *fakeRegistry) {
		return func(f *fakeRegistry) {
			f.referrers = referrers
			f.hook = func(w http.ResponseWriter, r *http.Request) bool {
				if r.URL.Path == "/v2/" {
					for k, v := range h {
						w.Header()[k] = v
					}
				}
				return false
			}
		}
	}
	// challenge answers GET /v2/ without credentials with the challenge of scheme and params, the
	// realm the fake's token endpoint, and the rest with the fake's auth of mode.
	challenge := func(mode, scheme, params string, referrers bool) func(f *fakeRegistry) {
		return func(f *fakeRegistry) {
			f.authMode, f.referrers = mode, referrers
			f.hook = func(w http.ResponseWriter, r *http.Request) bool {
				if r.URL.Path != "/v2/" || r.Header.Get("Authorization") != "" {
					return false
				}
				realm := f.srv.URL + "/token"
				if mode == "basic" {
					realm = nexusRealm
				}
				w.Header().Set("WWW-Authenticate", fmt.Sprintf(`%s realm=%q%s`, scheme, realm, params))
				writeErr(w, http.StatusUnauthorized, "UNAUTHORIZED", "unauthorized")
				return true
			}
		}
	}
	tests := []struct {
		name   string
		setup  func(f *fakeRegistry)
		edit   func(c *config.Registry)
		vendor vendor
		want   provider.Capabilities // the defaults when zero
		quirks quirks
		warn   bool
		hint   bool // the hint at uploads.spoolUnknownLength
	}{
		{name: "distribution", setup: headers(plainV2, true), vendor: vendorDistribution, want: strong},
		{name: "Zot", setup: headers(zotV2, true), vendor: vendorZot, want: strong},
		{name: "another registry", setup: headers(http.Header{}, true), vendor: vendorUnknown},
		{name: "GitLab A", setup: headers(gitLabA, true), vendor: vendorGitLab, want: strong},
		{name: "GitLab B", setup: headers(gitLabB, true), vendor: vendorGitLab, warn: true, want: strong},
		{name: "Harbor", setup: challenge("bearer", "Bearer", `,service="harbor-registry"`, true), vendor: vendorHarbor},
		{name: "Nexus, docker format", setup: headers(nexusV2, false), vendor: vendorNexus, want: nexusDocker},
		{name: "Nexus, docker format, told by its challenge", setup: challenge("basic", "BASIC", "", false), vendor: vendorNexus, want: nexusDocker},
		{
			name: "Nexus, docker format, deletes configured", setup: headers(nexusV2, false), vendor: vendorNexus, want: noReferrers,
			edit: func(c *config.Registry) { c.Capabilities.DeleteManifest = new(true) },
		},
		{
			name: "Nexus, the referrers API configured off", setup: headers(nexusV2, true), vendor: vendorNexus, want: nexusDocker,
			edit: func(c *config.Registry) { c.Capabilities.Referrers = new(false) },
		},
		{name: "Nexus, oci format", setup: headers(nexusV2, true), vendor: vendorNexus, quirks: quirks{wholeBlobs: true}},
		{name: "Gitea", setup: challenge("bearer", "Bearer", `,service="container_registry",scope="*"`, false), vendor: vendorGitea, want: noReferrers, hint: true},
		{
			name: "Gitea, the spool on", setup: challenge("bearer", "Bearer", `,service="container_registry",scope="*"`, false), vendor: vendorGitea, want: noReferrers,
			edit: func(c *config.Registry) { c.Uploads.SpoolUnknownLength = true },
		},
		{name: "JFrog", setup: headers(jfrogV2, true), vendor: vendorJFrog, quirks: quirks{blobExistsByGET: true}, hint: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFakeRegistry(t)
			tt.setup(f)
			log, buf := testLogger()
			p := f.provider(t, log, func(c *config.Registry) {
				c.Auth = config.RegistryAuth{Username: f.user, Password: f.pass, PasswordEnv: "FAKE_PASSWORD"}
				if tt.edit != nil {
					tt.edit(c)
				}
			})
			want := tt.want
			if want == (provider.Capabilities{}) {
				want = defaults
			}
			got, err := p.Capabilities(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if got != want {
				t.Errorf("Capabilities =\n%+v\nwant\n%+v", got, want)
			}
			if q := p.quirk(); q != tt.quirks {
				t.Errorf("quirks = %+v, want %+v", q, tt.quirks)
			}
			logs := buf.String()
			checkProbeLog(t, logs, tt.vendor, tt.quirks != (quirks{}), tt.warn)
			if hinted := strings.Contains(logs, `msg="hint: the registry refuses a /v2 chunk of unknown length`); hinted != tt.hint {
				t.Errorf("the hint at uploads.spoolUnknownLength logged: %v, want %v\n%s", hinted, tt.hint, logs)
			}
			if _, err := p.Capabilities(ctx); err != nil || buf.String() != logs {
				t.Errorf("the second Capabilities = %v, or logged again", err)
			}
			noSecrets(t, "the log", buf.String(), []string{f.pass, f.token, "Bearer ", "realm="})
		})
	}
}

// checkProbeLog checks the lines of a probe: the vendor on the capabilities line, the line of
// the workarounds when there are, and the warning about GitLab's metadata database when warn.
func checkProbeLog(t *testing.T, logs string, v vendor, workarounds, warn bool) {
	t.Helper()
	if !strings.Contains(logs, `msg="registry capabilities" registry=fake vendor=`+string(v)+" ") {
		t.Errorf("want vendor=%s on the capabilities line, got\n%s", v, logs)
	}
	if logged := strings.Contains(logs, `msg="registry needs workarounds"`); logged != workarounds {
		t.Errorf("the workarounds line logged: %v, want %v\n%s", logged, workarounds, logs)
	}
	if n := strings.Count(logs, `msg="GitLab registry with its metadata database`); n != map[bool]int{true: 1}[warn] {
		t.Errorf("%d warnings about the metadata database, want %v\n%s", n, warn, logs)
	}
}

// TestConsistencyLog: the startup line says the consistency, and an eventually consistent
// registry without a bound of its lag gets a warning.
func TestConsistencyLog(t *testing.T) {
	tests := []struct {
		name  string
		set   config.CapabilityOverrides
		line  string
		warns bool
	}{
		{"unknown", config.CapabilityOverrides{}, "consistency=unknown", false},
		{"strong", config.CapabilityOverrides{Consistency: config.ConsistencyStrong}, "consistency=strong", false},
		{"eventual", config.CapabilityOverrides{Consistency: config.ConsistencyEventual}, "consistency=eventual", true},
		{"eventual with a bound", config.CapabilityOverrides{Consistency: config.ConsistencyEventual, ConsistencyBound: 2 * time.Second},
			`consistency="eventual within 2s"`, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFakeRegistry(t)
			log, buf := testLogger()
			p := f.provider(t, log, func(c *config.Registry) { c.Capabilities = tt.set })
			if _, err := p.Capabilities(context.Background()); err != nil {
				t.Fatal(err)
			}
			logs := buf.String()
			if !strings.Contains(logs, tt.line) {
				t.Errorf("want %s on the capabilities line, got\n%s", tt.line, logs)
			}
			if warned := strings.Contains(logs, `level=WARN msg="the registry is eventually consistent`); warned != tt.warns {
				t.Errorf("warned: %v, want %v\n%s", warned, tt.warns, logs)
			}
		})
	}
}

// TestVendorDefaultsInTheProbe applies the defaults of the table the way probeOnce does for
// GHCR, which is told by its host and so cannot be the fake.
func TestVendorDefaultsInTheProbe(t *testing.T) {
	ctx := context.Background()
	ghcr := vendorDefaults[vendorGHCR]
	tests := []struct {
		name     string
		defaults config.CapabilityOverrides
		edit     func(c *config.Registry)
		want     provider.Capabilities
		noProbe  []string
	}{
		{
			name:     "GHCR",
			defaults: ghcr,
			want: provider.Capabilities{CreateOnPush: true, Referrers: true, BlobRedirect: true, Consistency: provider.ConsistencyEventual,
				MaxBlobSize: 10_000_000_000},
			noProbe: []string{"GET /v2/_catalog"},
		},
		{
			name:     "the configuration wins over GHCR's",
			defaults: ghcr,
			edit: func(c *config.Registry) {
				c.Capabilities.Catalog, c.Capabilities.DeleteManifest = new(true), new(true)
				c.Capabilities.ConsistencyBound = 3 * time.Second
			},
			want: provider.Capabilities{CreateOnPush: true, Referrers: true, Catalog: true, DeleteManifest: true, BlobRedirect: true,
				Consistency: provider.ConsistencyEventual, ConsistencyBound: 3 * time.Second, MaxBlobSize: 10_000_000_000},
			noProbe: []string{"GET /v2/_catalog"},
		},
		{
			name:     "a vendor without referrers",
			defaults: config.CapabilityOverrides{Referrers: new(false)},
			want:     provider.Capabilities{CreateOnPush: true, Catalog: true, DeleteManifest: true, BlobRedirect: true},
			noProbe:  []string{"GET /v2/art/lajstrom-probe/referrers/"},
		},
		{
			name:     "a vendor with referrers",
			defaults: config.CapabilityOverrides{Referrers: new(true)},
			want:     provider.Capabilities{CreateOnPush: true, Referrers: true, Catalog: true, DeleteManifest: true, BlobRedirect: true},
			noProbe:  []string{"GET /v2/art/lajstrom-probe/referrers/"},
		},
		{
			name:     "the configuration wins over the vendor's referrers",
			defaults: config.CapabilityOverrides{Referrers: new(false)},
			edit:     func(c *config.Registry) { c.Capabilities.Referrers = new(true) },
			want:     provider.Capabilities{CreateOnPush: true, Referrers: true, Catalog: true, DeleteManifest: true, BlobRedirect: true},
			noProbe:  []string{"GET /v2/art/lajstrom-probe/referrers/"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFakeRegistry(t)
			log, _ := testLogger()
			p := f.provider(t, log, tt.edit)
			c, err := p.probe(ctx, tt.defaults)
			if err != nil {
				t.Fatal(err)
			}
			p.probed.Store(&c)
			if got := p.effective(c); got != tt.want {
				t.Errorf("effective =\n%+v\nwant\n%+v", got, tt.want)
			}
			for _, r := range tt.noProbe {
				if f.count(r) != 0 {
					t.Errorf("sent %q although the table or the configuration sets it", r)
				}
			}
			if capable, known := p.referrersCapability(); capable != tt.want.Referrers || !known {
				t.Errorf("referrersCapability = %v, %v; want %v, true: the repositories must not probe", capable, known, tt.want.Referrers)
			}
			if got, want := p.catalogUsable(), tt.want.Catalog; got != want {
				t.Errorf("catalogUsable = %v, want %v", got, want)
			}
		})
	}
}

// TestCatalogBeforeTheProbe: the host names GHCR before any request, so ListRepositories does
// not walk GHCR's catalog even when the probe has not run.
func TestCatalogBeforeTheProbe(t *testing.T) {
	ctx := context.Background()
	log, _ := testLogger()
	for _, u := range []string{"https://ghcr.io", "https://ghcr.io:443"} {
		p, err := New(config.Registry{Name: "ghcr", Type: Type, URL: u, Prefix: "lajstrom-extras/"}, log)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := p.ListRepositories(ctx, "raw/"); !errors.Is(err, provider.ErrUnsupported) {
			t.Errorf("%s: ListRepositories before the probe = %v, want ErrUnsupported", u, err)
		}
	}
	f := newFakeRegistry(t)
	if !f.provider(t, log, nil).catalogUsable() {
		t.Error("catalogUsable before the probe of a registry the table does not know = false, want true")
	}
}

// TestPingNotFound: a server without /v2/ is no registry, as oras-go's Ping had it.
func TestPingNotFound(t *testing.T) {
	f := newFakeRegistry(t)
	f.hook = func(w http.ResponseWriter, r *http.Request) bool {
		if r.URL.Path != "/v2/" {
			return false
		}
		http.NotFound(w, r)
		return true
	}
	log, _ := testLogger()
	_, err := f.provider(t, log, nil).Capabilities(context.Background())
	if !errors.Is(err, provider.ErrNotFound) || !strings.HasPrefix(err.Error(), "generic: ping fake: ") {
		t.Errorf("Capabilities error = %v, want ErrNotFound from the ping", err)
	}
}

// TestWholeBlobs: on Nexus's oci format, which answers every ranged GET with 500, OpenBlob asks
// for the whole blob and says it takes no ranges, once the probe named the vendor and format.
func TestWholeBlobs(t *testing.T) {
	ctx := context.Background()
	const repo = "art/raw/scratch/notes"
	data := []byte("0123456789")
	d := digest.FromBytes(data)
	f := newFakeRegistry(t)
	var ranged atomic.Int32
	f.hook = func(w http.ResponseWriter, r *http.Request) bool {
		w.Header().Set("Server", "Nexus/3.98.1-01 (COMMUNITY)")
		if r.Header.Get("Range") == "" {
			return false
		}
		ranged.Add(1)
		writeErr(w, http.StatusInternalServerError, "UNKNOWN", "IllegalArgumentException: Paths must start with a slash")
		return true
	}
	f.blobs[repo] = map[digest.Digest][]byte{d: data}
	log, _ := testLogger()
	p := f.provider(t, log, func(c *config.Registry) { c.Retries = -1 })
	if _, err := p.OpenBlob(ctx, "raw/scratch/notes", d, "bytes=2-4", false); err == nil {
		t.Fatal("OpenBlob with a range before the probe succeeded; the fake answers 500")
	}
	if _, err := p.Capabilities(ctx); err != nil {
		t.Fatal(err)
	}
	ranged.Store(0)
	b, err := p.OpenBlob(ctx, "raw/scratch/notes", d, "bytes=2-4", false)
	if err != nil {
		t.Fatalf("OpenBlob: %v", err)
	}
	got, err := io.ReadAll(b.Body)
	_ = b.Body.Close()
	if err != nil || b.Status != http.StatusOK || !bytes.Equal(got, data) || b.Size != int64(len(data)) || b.ContentRange != "" || b.AcceptRanges {
		t.Errorf("OpenBlob = %+v, %q, %v; want the whole blob, 200, without ranges", b, got, err)
	}
	st, err := p.StatBlob(ctx, "raw/scratch/notes", d)
	if err != nil || st.Size != int64(len(data)) || st.AcceptRanges {
		t.Errorf("StatBlob = %+v, %v; want the size without ranges", st, err)
	}
	if n := ranged.Load(); n != 0 {
		t.Errorf("%d requests with a Range after the probe", n)
	}
}

// TestBlobExistsByGET: on JFrog, whose HEAD answers 200 for a blob that GET answers 404 in the
// image path, StatBlob and a blob's Exists ask GET of the first byte; a manifest's Exists still
// asks HEAD.
func TestBlobExistsByGET(t *testing.T) {
	ctx := context.Background()
	const name = "raw/scratch/notes"
	data := []byte("hello, world\n")
	present, empty, absent := digest.FromBytes(data), digest.FromBytes(nil), digest.FromString("elsewhere")
	f := newFakeRegistry(t)
	f.hook = func(w http.ResponseWriter, r *http.Request) bool {
		w.Header().Set("X-Jfrog-Version", "Artifactory/7.161.26 86126900")
		if r.Method == http.MethodHead && strings.Contains(r.URL.Path, "/blobs/") {
			w.Header().Set("Content-Length", "13")
			w.WriteHeader(http.StatusOK) // JFrog: the digest is named in another image path
			return true
		}
		return false
	}
	f.blobs["art/"+name] = map[digest.Digest][]byte{present: data, empty: {}}
	log, _ := testLogger()
	p := f.provider(t, log, nil)
	if _, err := p.Capabilities(ctx); err != nil {
		t.Fatal(err)
	}
	rr, err := p.Repository(ctx, name)
	if err != nil {
		t.Fatal(err)
	}
	checkStat(t, p, rr, name, present, int64(len(data)))
	checkStat(t, p, rr, name, empty, 0)
	checkStat(t, p, rr, name, absent, -1)
	if n := f.count("HEAD /v2/art/" + name + "/blobs/"); n != 0 {
		t.Errorf("%d HEAD requests of blobs, want none", n)
	}
	m := []byte(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json","config":{"mediaType":"application/vnd.oci.empty.v1+json","digest":"sha256:44136fa355b3678a1146ad16f7e8649e94fb4fc21fe77e8310c060f61caaff8a","size":2},"layers":[]}`)
	md := ocispec.Descriptor{MediaType: ocispec.MediaTypeImageManifest, Digest: digest.FromBytes(m), Size: int64(len(m))}
	if err := rr.Push(ctx, md, bytes.NewReader(m)); err != nil {
		t.Fatal(err)
	}
	if ok, err := rr.Exists(ctx, md); !ok || err != nil || f.count("HEAD /v2/art/"+name+"/manifests/") != 1 {
		t.Errorf("Exists of a manifest = %v, %v with %d HEAD requests; want true by one HEAD", ok, err, f.count("HEAD /v2/art/"+name+"/manifests/"))
	}
}

// checkStat checks StatBlob and Exists of the blob d of name: 200 with size, or, for size -1,
// provider.ErrNotFound and false.
func checkStat(t *testing.T, p *Provider, rr provider.Repository, name string, d digest.Digest, size int64) {
	t.Helper()
	ctx := context.Background()
	st, err := p.StatBlob(ctx, name, d)
	switch {
	case size >= 0 && (err != nil || st.Size != size || st.Status != http.StatusOK || st.ContentRange != ""):
		t.Errorf("StatBlob(%s) = %+v, %v; want 200 with the size %d", d, st, err, size)
	case size < 0 && !errors.Is(err, provider.ErrNotFound):
		t.Errorf("StatBlob(%s) = %+v, %v; want ErrNotFound", d, st, err)
	}
	ok, err := rr.Exists(ctx, ocispec.Descriptor{MediaType: "application/octet-stream", Digest: d, Size: max(size, 0)})
	if ok != (size >= 0) || err != nil {
		t.Errorf("Exists(%s) = %v, %v; want %v", d, ok, err, size >= 0)
	}
}
