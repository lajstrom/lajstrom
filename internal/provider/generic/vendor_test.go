package generic

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"

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
	plainV2                   = http.Header{"Docker-Distribution-Api-Version": {"registry/2.0"}}
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
		{name: "distribution", id: identity{host: "localhost:5001", header: plainV2}, want: vendorUnknown},
		{name: "distribution with htpasswd", id: identity{host: "localhost:55002", header: plainV2, challenge: distributionChallenge}, want: vendorUnknown},
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
	tests := []struct {
		vendor vendor
		want   provider.Capabilities
	}{
		{vendorUnknown, probed},
		{vendorGitLab, probed},
		{vendorHarbor, probed},
		{vendorGHCR, provider.Capabilities{CreateOnPush: true, Referrers: true, BlobRedirect: true}},
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
// of the table did.
func TestCapabilitiesVendor(t *testing.T) {
	ctx := context.Background()
	defaults := provider.Capabilities{CreateOnPush: true, Referrers: true, Catalog: true, DeleteManifest: true, BlobRedirect: true}
	headers := func(h http.Header) func(f *fakeRegistry) {
		return func(f *fakeRegistry) {
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
	harbor := func(f *fakeRegistry) {
		f.authMode = "bearer"
		f.hook = func(w http.ResponseWriter, r *http.Request) bool {
			if r.URL.Path != "/v2/" || r.Header.Get("Authorization") != "" {
				return false
			}
			w.Header().Set("WWW-Authenticate", fmt.Sprintf(`Bearer realm=%q,service="harbor-registry"`, f.srv.URL+"/token"))
			writeErr(w, http.StatusUnauthorized, "UNAUTHORIZED", "unauthorized")
			return true
		}
	}
	const warning = `msg="GitLab registry with its metadata database`
	tests := []struct {
		name   string
		setup  func(f *fakeRegistry)
		vendor vendor
		warn   bool
	}{
		{name: "distribution", setup: headers(plainV2), vendor: vendorUnknown},
		{name: "GitLab A", setup: headers(gitLabA), vendor: vendorGitLab},
		{name: "GitLab B", setup: headers(gitLabB), vendor: vendorGitLab, warn: true},
		{name: "Harbor", setup: harbor, vendor: vendorHarbor},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFakeRegistry(t)
			tt.setup(f)
			log, buf := testLogger()
			p := f.provider(t, log, func(c *config.Registry) {
				c.Auth = config.RegistryAuth{Username: f.user, Password: f.pass, PasswordEnv: "FAKE_PASSWORD"}
			})
			got, err := p.Capabilities(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if got != defaults {
				t.Errorf("Capabilities =\n%+v\nwant what the probe found\n%+v", got, defaults)
			}
			logs := buf.String()
			if !strings.Contains(logs, `msg="registry capabilities" registry=fake vendor=`+string(tt.vendor)+" ") {
				t.Errorf("want vendor=%s on the capabilities line, got\n%s", tt.vendor, logs)
			}
			if n := strings.Count(logs, warning); n != map[bool]int{true: 1}[tt.warn] {
				t.Errorf("%d warnings about the metadata database, want %v\n%s", n, tt.warn, logs)
			}
			if _, err := p.Capabilities(ctx); err != nil || strings.Count(buf.String(), warning) != strings.Count(logs, warning) {
				t.Errorf("the second Capabilities = %v, or warned again", err)
			}
			noSecrets(t, "the log", buf.String(), []string{f.pass, f.token, "Bearer ", "realm="})
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
			want:     provider.Capabilities{CreateOnPush: true, Referrers: true, BlobRedirect: true},
			noProbe:  []string{"GET /v2/_catalog"},
		},
		{
			name:     "the configuration wins over GHCR's",
			defaults: ghcr,
			edit:     func(c *config.Registry) { c.Capabilities.Catalog, c.Capabilities.DeleteManifest = new(true), new(true) },
			want:     provider.Capabilities{CreateOnPush: true, Referrers: true, Catalog: true, DeleteManifest: true, BlobRedirect: true},
			noProbe:  []string{"GET /v2/_catalog"},
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
