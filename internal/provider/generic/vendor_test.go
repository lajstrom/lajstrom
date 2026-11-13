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
	"time"

	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"oras.land/oras-go/v2/registry/remote/auth"

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
	quayV2                    = http.Header{"Docker-Distribution-Api-Version": {"registry/2.0"}, "X-Sparse-Manifest-Support": {"false"}}
	quayChallenge             = `Bearer realm="http://localhost:5012/v2/auth",service="localhost:5012"`
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
		{name: "Gitea, and Forgejo before its own API is asked", id: identity{host: "localhost:5009", header: plainV2, challenge: giteaChallenge}, want: vendorGitea},
		{name: "GitLab.com's challenge without its header", id: identity{host: "registry.gitlab.com", challenge: gitLabComChallenge}, want: vendorUnknown},
		{name: "distribution", id: identity{host: "localhost:5001", header: plainV2}, want: vendorDistribution},
		{name: "distribution with htpasswd", id: identity{host: "localhost:55002", header: plainV2, challenge: distributionChallenge}, want: vendorDistribution},
		{name: "Docker Hub", id: identity{host: "registry-1.docker.io", header: plainV2}, want: vendorDocker},
		{name: "Docker Hub by its short name, with the port", id: identity{host: "docker.io:443"}, want: vendorDocker},
		{name: "a host that ends in docker.io", id: identity{host: "notdocker.io"}, want: vendorUnknown},
		{name: "ACR", id: identity{host: "example.azurecr.io:443", header: plainV2}, want: vendorUnknown},
		{name: "Zot", id: identity{host: "localhost:5000", header: zotV2}, want: vendorZot},
		{name: "a host that ends in ghcr.io", id: identity{host: "notghcr.io"}, want: vendorUnknown},
		{name: "a host below ghcr.io", id: identity{host: "ghcr.io.example.com"}, want: vendorUnknown},
		{name: "ECR", id: identity{host: "123456789012.dkr.ecr.eu-west-1.amazonaws.com", header: plainV2}, want: vendorECR},
		{name: "ECR in China, with the port", id: identity{host: "123456789012.dkr.ecr.cn-north-1.amazonaws.com.cn:443"}, want: vendorECR},
		{name: "ECR's VPC endpoint", id: identity{host: "vpce-0a1b2c3d-x1y2z3.dkr.ecr.eu-west-1.vpce.amazonaws.com"}, want: vendorECR},
		{name: "ECR Public is not ECR", id: identity{host: "public.ecr.aws"}, want: vendorUnknown},
		{name: "a host that names dkr.ecr elsewhere", id: identity{host: "x.dkr.ecr.example.com"}, want: vendorUnknown},
		{name: "Quay", id: identity{host: "localhost:5012", header: quayV2, challenge: quayChallenge}, want: vendorQuay},
		{name: "Quay with sparse manifests on, behind another name", id: identity{host: "registry.example:443",
			header: http.Header{"X-Sparse-Manifest-Support": {"true"}}}, want: vendorQuay},
		{name: "Quay's challenge without its header", id: identity{host: "localhost:5012", header: plainV2, challenge: quayChallenge}, want: vendorDistribution},
		{name: "quay.io", id: identity{host: "quay.io", header: http.Header{
			"Docker-Distribution-Api-Version": {"registry/2.0"}, "X-Sparse-Manifest-Support": {"false"},
			"Www-Authenticate": {`Bearer realm="https://quay.io/v2/auth",service="quay.io"`}}}, want: vendorQuay},
		{name: "quay.io before a ping, with the port", id: identity{host: "QUAY.io:443"}, want: vendorQuay},
		{name: "a host below quay.io", id: identity{host: "cdn01.quay.io", header: plainV2}, want: vendorUnknown},
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

// TestGitLabTokenService: a GitLab's registry gets the depth of GitLab's token service only when
// its challenge names that service, <GitLab>/jwt/auth; the registry alone takes any depth.
func TestGitLabTokenService(t *testing.T) {
	tests := []struct {
		challenge string
		want      bool
	}{
		{gitLabComChallenge, true},
		{`Bearer realm="https://git.example/gitlab/jwt/auth",service="container_registry"`, true},
		{`Bearer realm="https://git.example:8443/jwt/auth/",service="container_registry"`, true},
		{`Bearer realm="https://git.example/jwt/auth?x=1"`, true},
		{`Bearer realm="https://proxy.example/token"`, false}, // a proxy that rewrote the realm
		{gitLabTestServerChallenge, false},
		{`Basic realm="https://gitlab.com/jwt/auth"`, false},
		{`Bearer realm="https://gitlab.com/jwt/authorize"`, false},
		{"", false},
	}
	for _, tt := range tests {
		id := identity{host: "registry.example", header: gitLabB, challenge: tt.challenge}
		if got := id.gitLabTokenService(); got != tt.want {
			t.Errorf("gitLabTokenService of %q = %v, want %v", tt.challenge, got, tt.want)
		}
		want := map[bool]int{true: gitLabNameDepth}[tt.want]
		if got := vendorOverrides(vendorGitLab, id).MaxNameDepth; got != want {
			t.Errorf("the GitLab's maxNameDepth with %q = %d, want %d", tt.challenge, got, want)
		}
	}
	if got := vendorOverrides(vendorHarbor, identity{challenge: gitLabComChallenge}).MaxNameDepth; got != 0 {
		t.Errorf("another vendor with GitLab's challenge: maxNameDepth = %d, want 0", got)
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
		{vendorDocker, provider.Capabilities{CreateOnPush: true, Referrers: true, Catalog: true, DeleteManifest: true, BlobRedirect: true,
			MaxNameDepth: 1}},
		{vendorQuay, provider.Capabilities{CreateOnPush: true, DeleteManifest: true, BlobRedirect: true,
			Consistency: provider.ConsistencyEventual, ConsistencyBound: 130 * time.Second}},
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
	// Every vendor gets 255 bytes less the fake's prefix, art/.
	defaults := provider.Capabilities{CreateOnPush: true, Referrers: true, Catalog: true, DeleteManifest: true, BlobRedirect: true, MaxNameLength: 251}
	noReferrers := provider.Capabilities{CreateOnPush: true, Catalog: true, DeleteManifest: true, BlobRedirect: true, MaxNameLength: 251}
	nexusDocker := provider.Capabilities{CreateOnPush: true, Catalog: true, BlobRedirect: true, MaxNameLength: 251}
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
	gitLabDeep := strong
	gitLabDeep.MaxNameDepth = gitLabNameDepth
	quay := provider.Capabilities{CreateOnPush: true, DeleteManifest: true, BlobRedirect: true, MaxNameLength: 251,
		Consistency: provider.ConsistencyEventual, ConsistencyBound: 130 * time.Second}
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
		{name: "GitLab behind its token service", setup: gitLabTokens, vendor: vendorGitLab, want: gitLabDeep},
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
		{name: "Forgejo", setup: forgejo(challenge, forgejoVersion), vendor: vendorForgejo, want: noReferrers, hint: true},
		{
			name: "Forgejo, the spool on", setup: forgejo(challenge, forgejoVersion), vendor: vendorForgejo, want: noReferrers,
			edit: func(c *config.Registry) { c.Uploads.SpoolUnknownLength = true },
		},
		{name: "Gitea, its own API a web page", setup: forgejo(challenge, "<!DOCTYPE html><html></html>"), vendor: vendorGitea, want: noReferrers, hint: true},
		{name: "Gitea, a version without one", setup: forgejo(challenge, `{"version":""}`), vendor: vendorGitea, want: noReferrers, hint: true},
		{name: "JFrog", setup: headers(jfrogV2, true), vendor: vendorJFrog, quirks: quirks{blobExistsByGET: true}, hint: true},
		// The fake has a catalog and the referrers API: the table's catalog and referrers win.
		{name: "Quay", setup: headers(quayV2, true), vendor: vendorQuay, want: quay, quirks: quirks{pushForDelete: true}},
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

// forgejoVersion is Forgejo 17.0.1's answer to GET /api/forgejo/v1/version
// (docs/registries/forgejo.md, item 6).
const forgejoVersion = `{"version":"17.0.1+gitea-1.22.0"}`

// forgejo makes the fake answer GET /v2/ as Gitea and Forgejo do, by challenge (the helper of
// TestCapabilitiesVendor), and GET /api/forgejo/v1/version 200 with body, as Forgejo does; the
// fake answers 404 there otherwise, as Gitea does.
func forgejo(challenge func(mode, scheme, params string, referrers bool) func(*fakeRegistry), body string) func(*fakeRegistry) {
	return func(f *fakeRegistry) {
		challenge("bearer", "Bearer", `,service="container_registry",scope="*"`, false)(f)
		next := f.hook
		f.hook = func(w http.ResponseWriter, r *http.Request) bool {
			if r.URL.Path != forgejoVersionPath {
				return next(w, r)
			}
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			_, _ = io.WriteString(w, body)
			return true
		}
	}
}

// TestForgejoAsked: the provider asks Forgejo's own API only of a registry that answers GET /v2/
// as Gitea does, and only in its first probe. The fake asks for a token there too, so one ask
// may be two requests.
func TestForgejoAsked(t *testing.T) {
	ctx := context.Background()
	for _, tt := range []struct {
		name  string
		setup func(f *fakeRegistry)
		asked bool
	}{
		{name: "Gitea's challenge", setup: func(f *fakeRegistry) {
			f.authMode = "bearer"
			f.hook = func(w http.ResponseWriter, r *http.Request) bool {
				if r.URL.Path != "/v2/" || r.Header.Get("Authorization") != "" {
					return false
				}
				w.Header().Set("WWW-Authenticate", fmt.Sprintf(`Bearer realm=%q,service="container_registry",scope="*"`, f.srv.URL+"/token"))
				writeErr(w, http.StatusUnauthorized, "UNAUTHORIZED", "unauthorized")
				return true
			}
		}, asked: true},
		{name: "distribution", setup: func(f *fakeRegistry) {
			f.hook = func(w http.ResponseWriter, r *http.Request) bool {
				if r.URL.Path == "/v2/" {
					w.Header()["Docker-Distribution-Api-Version"] = plainV2["Docker-Distribution-Api-Version"]
				}
				return false
			}
		}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			f := newFakeRegistry(t)
			tt.setup(f)
			log, _ := testLogger()
			p := f.provider(t, log, func(c *config.Registry) {
				c.Auth = config.RegistryAuth{Username: f.user, Password: f.pass, PasswordEnv: "FAKE_PASSWORD"}
			})
			asked := func() int {
				f.mu.Lock()
				defer f.mu.Unlock()
				n := 0
				for _, r := range f.requests {
					if r == "GET "+forgejoVersionPath {
						n++
					}
				}
				return n
			}
			if _, err := p.Capabilities(ctx); err != nil {
				t.Fatal(err)
			}
			first := asked()
			if (first > 0) != tt.asked {
				t.Errorf("GET %s: %d requests, want any: %v", forgejoVersionPath, first, tt.asked)
			}
			if _, err := p.Capabilities(ctx); err != nil {
				t.Fatal(err)
			}
			if again := asked(); again != first {
				t.Errorf("the second Capabilities asked GET %s again: %d requests, then %d", forgejoVersionPath, first, again)
			}
		})
	}
}

// gitLabTokens makes the fake GitLab A behind GitLab's token service, whose realm is
// <GitLab>/jwt/auth.
func gitLabTokens(f *fakeRegistry) {
	f.authMode, f.realm = "bearer", f.srv.URL+"/jwt/auth"
	f.hook = func(w http.ResponseWriter, r *http.Request) bool {
		switch r.URL.Path {
		case "/jwt/auth":
			f.serveToken(w, r)
			return true
		case "/v2/":
			for k, v := range gitLabA {
				w.Header()[k] = v
			}
		}
		return false
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

// TestNameDepthLog: the startup line says the depth of a name the registry takes, and a registry
// that takes none of Lajstrom's, three components or more, is an error line (#599).
func TestNameDepthLog(t *testing.T) {
	tests := []struct {
		depth int
		line  string
		err   bool
	}{
		{0, "maxNameDepth=0", false},
		{1, "maxNameDepth=1", true},
		{2, "maxNameDepth=2", true},
		{3, "maxNameDepth=3", false},
		{4, "maxNameDepth=4", false},
	}
	for _, tt := range tests {
		t.Run(tt.line, func(t *testing.T) {
			f := newFakeRegistry(t)
			log, buf := testLogger()
			p := f.provider(t, log, func(c *config.Registry) { c.Capabilities.MaxNameDepth = tt.depth })
			c, err := p.Capabilities(context.Background())
			if err != nil || c.MaxNameDepth != tt.depth {
				t.Fatalf("Capabilities = %+v, %v; want maxNameDepth %d", c, err, tt.depth)
			}
			logs := buf.String()
			if !strings.Contains(logs, `msg="registry capabilities"`) || !strings.Contains(logs, tt.line) {
				t.Errorf("want %s on the capabilities line, got\n%s", tt.line, logs)
			}
			if logged := strings.Contains(logs, `level=ERROR msg="the registry takes no name of Lajstrom's`); logged != tt.err {
				t.Errorf("the error line logged: %v, want %v\n%s", logged, tt.err, logs)
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
			// The fake has a catalog and the referrers API; the table's answers win unasked.
			name:     "Quay",
			defaults: vendorDefaults[vendorQuay],
			want: provider.Capabilities{CreateOnPush: true, DeleteManifest: true, BlobRedirect: true,
				Consistency: provider.ConsistencyEventual, ConsistencyBound: 130 * time.Second},
			noProbe: []string{"GET /v2/_catalog", "GET /v2/art/lajstrom-probe/referrers/"},
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

// TestCatalogBeforeTheProbe: the host names GHCR or quay.io before any request, so
// ListRepositories does not walk their catalog even when the probe has not run.
func TestCatalogBeforeTheProbe(t *testing.T) {
	ctx := context.Background()
	log, _ := testLogger()
	for _, u := range []string{"https://ghcr.io", "https://ghcr.io:443", "https://quay.io"} {
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
// for the whole blob and says it takes no ranges, from the first request of a provider that has
// not probed yet: the read probes first (#797).
func TestWholeBlobs(t *testing.T) {
	ctx := context.Background()
	f := newFakeRegistry(t)
	ranged := nexusOCI(f, nil)
	log, _ := testLogger()
	p := f.provider(t, log, func(c *config.Registry) { c.Retries = -1 })
	openWhole(ctx, t, p)
	st, err := p.StatBlob(ctx, quirkName, digest.FromBytes(quirkData))
	if err != nil || st.Size != int64(len(quirkData)) || st.AcceptRanges {
		t.Errorf("StatBlob = %+v, %v; want the size without ranges", st, err)
	}
	if n := ranged.Load(); n != 0 {
		t.Errorf("%d requests with a Range", n)
	}
}

// TestBlobExistsByGET: on JFrog, whose HEAD answers 200 for a blob that GET answers 404 in the
// image path, StatBlob and a blob's Exists ask GET of the first byte, from the first request of a
// provider that has not probed yet (#797); a manifest's Exists still asks HEAD.
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

// quayTokens makes the fake's token service answer as Quay 3.18.2's did: it knows the actions
// pull, push and * only, and any other in a scope is 400 INVALID_REQUEST (docs/registries/quay.md,
// Tokens and the delete action). With header, GET /v2/ answers X-Sparse-Manifest-Support, by
// which the table tells Quay; without, the registry is another one with such a token service.
func quayTokens(header bool) func(f *fakeRegistry) {
	return func(f *fakeRegistry) {
		f.authMode = "bearer"
		f.hook = func(w http.ResponseWriter, r *http.Request) bool {
			switch r.URL.Path {
			case "/v2/":
				if header {
					w.Header().Set("X-Sparse-Manifest-Support", "false")
				}
			case "/token":
				for _, s := range r.URL.Query()["scope"] {
					for a := range strings.SplitSeq(s[strings.LastIndex(s, ":")+1:], ",") {
						if a != "pull" && a != "push" && a != "*" {
							writeErr(w, http.StatusBadRequest, "INVALID_REQUEST", "Unable to decode repository and actions: "+s)
							return true
						}
					}
				}
			}
			return false
		}
	}
}

// tokenScopes returns the scopes of every token request the fake got, in order.
func tokenScopes(t *testing.T, f *fakeRegistry) []string {
	t.Helper()
	var scopes []string
	for _, r := range f.log() {
		query, ok := strings.CutPrefix(r, "GET /token?")
		if !ok {
			continue
		}
		q, err := url.ParseQuery(query)
		if err != nil {
			t.Fatal(err)
		}
		scopes = append(scopes, q["scope"]...)
	}
	return scopes
}

// asksDelete reports whether the actions of scope have delete.
func asksDelete(scope string) bool {
	return slices.Contains(strings.Split(scope[strings.LastIndex(scope, ":")+1:], ","), auth.ActionDelete)
}

// scopeDeletes is what deleteThroughTokens did: the deletes' errors, the scopes of the token
// requests they made and the DELETEs that reached the fake.
type scopeDeletes struct {
	f                       *fakeRegistry
	repo                    provider.Repository
	subject, referrer       ocispec.Descriptor
	errReferrer, errSubject error
	asked                   []string
	deletes                 int
}

// scopeRepo is the repository of deleteThroughTokens.
const scopeRepo = "raw/scratch/notes"

// deleteThroughTokens pushes a subject and a referrer of it to the fake behind Quay's token
// service (quayTokens), with or without Quay's header, on the tag schema, whose clean-up oras-go
// does on a delete; then it deletes the referrer and the subject.
func deleteThroughTokens(t *testing.T, header bool) scopeDeletes {
	t.Helper()
	ctx := context.Background()
	f := newFakeRegistry(t)
	quayTokens(header)(f)
	log, _ := testLogger()
	p := f.provider(t, log, func(c *config.Registry) {
		c.Auth = config.RegistryAuth{Username: f.user, Password: f.pass}
		c.Capabilities.Referrers = new(false)
	})
	if _, err := p.Capabilities(ctx); err != nil {
		t.Fatal(err)
	}
	repo, err := p.Repository(ctx, scopeRepo)
	if err != nil {
		t.Fatal(err)
	}
	d := scopeDeletes{f: f, repo: repo, subject: pushManifest(t, repo, "v1", nil)}
	d.referrer = pushManifest(t, repo, "sig", &d.subject)
	before := len(tokenScopes(t, f))
	d.errReferrer, d.errSubject = repo.Delete(ctx, d.referrer), repo.Delete(ctx, d.subject)
	d.asked = tokenScopes(t, f)[before:]
	d.deletes = f.count("DELETE /v2/art/" + scopeRepo + "/manifests/")
	return d
}

// TestDeleteScopeQuay: oras-go asks a token with the action delete for every DELETE, and
// pull,delete for the reads of a referrer's delete that cleans the tag schema up; Quay's token
// service refuses both (#801). Once the probe named Quay, the provider asks push instead, and the
// referrer, its referrers index and the subject are deleted.
func TestDeleteScopeQuay(t *testing.T) {
	ctx := context.Background()
	d := deleteThroughTokens(t, true)
	if d.errReferrer != nil || d.errSubject != nil {
		t.Fatalf("Delete of the referrer, the subject = %v, %v", d.errReferrer, d.errSubject)
	}
	// The referrer, the referrers index it was the last of, and the subject.
	if d.deletes != 3 {
		t.Errorf("%d DELETEs sent, want 3:\n%s", d.deletes, strings.Join(d.f.log(), "\n"))
	}
	if want := "repository:art/" + scopeRepo + ":pull,push"; slices.ContainsFunc(d.asked, asksDelete) || !slices.Contains(d.asked, want) {
		t.Errorf("scopes asked for the deletes %q; want none with delete, %q among them", d.asked, want)
	}
	for _, desc := range []ocispec.Descriptor{d.referrer, d.subject} {
		if ok, err := d.repo.Exists(ctx, desc); ok || err != nil {
			t.Errorf("Exists(%s) after its Delete = %v, %v; want false", desc.Digest, ok, err)
		}
	}
	if _, err := d.repo.Resolve(ctx, strings.Replace(d.subject.Digest.String(), ":", "-", 1)); !errors.Is(err, provider.ErrNotFound) {
		t.Errorf("the referrers index of the subject after the referrer's delete: %v, want ErrNotFound", err)
	}
}

// TestDeleteScopeOther: a registry the table does not take for Quay is asked delete, as oras-go
// asks it, even behind the same token service: the provider widens no scope for a registry it
// does not know, and the token service's 400 is the delete's error.
func TestDeleteScopeOther(t *testing.T) {
	d := deleteThroughTokens(t, false)
	var rej *provider.RejectedError
	if !errors.As(d.errReferrer, &rej) || rej.Status != http.StatusBadRequest || d.errSubject == nil {
		t.Errorf("Delete = %v, %v; want the token service's 400 for both", d.errReferrer, d.errSubject)
	}
	if !slices.ContainsFunc(d.asked, asksDelete) || d.deletes != 0 {
		t.Errorf("scopes asked %q and %d DELETEs sent; want delete asked as oras-go asks it, and no DELETE", d.asked, d.deletes)
	}
}

// TestPushForDelete: the action delete of a repository scope becomes push, merged as oras-go
// merges scopes; anything else stays as it was asked.
func TestPushForDelete(t *testing.T) {
	tests := []struct {
		scopes, want []string
		changed      bool
	}{
		{[]string{"repository:art/raw/a/b:delete"}, []string{"repository:art/raw/a/b:push"}, true},
		{[]string{"repository:art/raw/a/b:delete,pull"}, []string{"repository:art/raw/a/b:pull,push"}, true},
		{[]string{"repository:art/raw/a/b:delete,pull,push"}, []string{"repository:art/raw/a/b:pull,push"}, true},
		{[]string{"repository:art/raw/a/b:delete,*"}, []string{"repository:art/raw/a/b:*"}, true},
		{
			[]string{"repository:art/raw/a/b:delete", "repository:art/raw/c/d:pull"},
			[]string{"repository:art/raw/a/b:push", "repository:art/raw/c/d:pull"}, true,
		},
		{[]string{"repository:art/raw/a/b:pull,push"}, []string{"repository:art/raw/a/b:pull,push"}, false},
		{[]string{"repository:art/raw/a/deleted:pull"}, []string{"repository:art/raw/a/deleted:pull"}, false},
		{[]string{"registry:catalog:*"}, []string{"registry:catalog:*"}, false},
		{[]string{"other:x:delete"}, []string{"other:x:delete"}, false},
		{[]string{"delete"}, []string{"delete"}, false},
		{nil, nil, false},
	}
	for _, tt := range tests {
		got, changed := pushForDelete(tt.scopes)
		if !slices.Equal(got, tt.want) || changed != tt.changed {
			t.Errorf("pushForDelete(%q) = %q, %v; want %q, %v", tt.scopes, got, changed, tt.want, tt.changed)
		}
	}
}
