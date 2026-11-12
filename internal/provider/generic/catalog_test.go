package generic

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/lajstrom/lajstrom/internal/config"
	"github.com/lajstrom/lajstrom/internal/provider"
)

// quayCatalog makes f answer as Quay does (#802): bearer auth everywhere but the catalog, which
// an anonymous caller reads as 200 with an empty list, never a challenge; a token asked for
// registry:catalog:* is 400 at the token endpoint; with the token asked without a scope, the
// catalog is f's, or authed's status when it is not 0.
func quayCatalog(f *fakeRegistry, authed int) {
	f.authMode = "bearer"
	f.hook = func(w http.ResponseWriter, r *http.Request) bool {
		switch {
		case r.URL.Path == "/token" && slices.ContainsFunc(r.URL.Query()["scope"], func(s string) bool { return strings.HasPrefix(s, "registry:catalog:") }):
			writeErr(w, http.StatusBadRequest, "INVALID_REQUEST", "Unable to decode repository and actions")
		case r.URL.Path != "/v2/_catalog":
			return false
		case r.Header.Get("Authorization") != "Bearer "+f.token:
			_ = json.NewEncoder(w).Encode(map[string][]string{"repositories": {}})
		case authed != 0:
			writeErr(w, authed, "DENIED", "no catalog for you")
		default:
			return false
		}
		return true
	}
}

// TestCatalogWithoutChallenge checks that the catalog of a registry that never challenges it is
// read with the credentials, by the probe and by ListRepositories, before and after a probe.
func TestCatalogWithoutChallenge(t *testing.T) {
	ctx := context.Background()
	var many []string
	for i := range 150 {
		many = append(many, fmt.Sprintf("art/raw/r/%03d", i))
	}
	tests := []struct {
		name        string
		repos       []string
		authed      int // the authenticated catalog's status, 0 for the catalog
		wantCatalog bool
		wantNames   []string // nil with wantCatalog: none, and no error
	}{
		{name: "the credentials' catalog", repos: []string{"art/raw/a/x", "art/pypi/b/y", "other/raw/c/z"}, wantCatalog: true,
			wantNames: []string{"pypi/b/y", "raw/a/x"}},
		{name: "two pages", repos: many, wantCatalog: true, wantNames: names(many)},
		// A fresh Zot behind a token service: empty with the credentials too, and still a catalog.
		{name: "a catalog that is empty", wantCatalog: true},
		// The anonymous 200 is not the catalog of the credentials.
		{name: "the credentials may not read it", repos: []string{"art/raw/a/x"}, authed: http.StatusForbidden},
	}
	for _, tt := range tests {
		for _, probeFirst := range []bool{true, false} {
			t.Run(fmt.Sprintf("%s, probed first %v", tt.name, probeFirst), func(t *testing.T) {
				f := newFakeRegistry(t)
				f.repos = tt.repos
				quayCatalog(f, tt.authed)
				log, _ := testLogger()
				p := f.provider(t, log, func(c *config.Registry) { c.Auth = config.RegistryAuth{Username: f.user, Password: f.pass} })
				if probeFirst {
					c, err := p.Capabilities(ctx)
					if err != nil {
						t.Fatal(err)
					}
					if c.Catalog != tt.wantCatalog {
						t.Errorf("Catalog = %v, want %v; requests %q", c.Catalog, tt.wantCatalog, f.log())
					}
				}
				got, err := p.ListRepositories(ctx, "")
				if !tt.wantCatalog {
					if !errors.Is(err, provider.ErrUnsupported) {
						t.Errorf("ListRepositories = %q, %v; want ErrUnsupported", got, err)
					}
					return
				}
				if err != nil {
					t.Fatalf("ListRepositories: %v; requests %q", err, f.log())
				}
				if len(got) != len(tt.wantNames) || (len(got) > 0 && !slices.Equal(got, tt.wantNames)) {
					t.Errorf("ListRepositories = %q, want %q", got, tt.wantNames)
				}
			})
		}
	}
}

// names drops the fake's prefix art/ from repos.
func names(repos []string) []string {
	out := make([]string, len(repos))
	for i, r := range repos {
		out[i] = strings.TrimPrefix(r, "art/")
	}
	return out
}

// TestCatalogChallenged checks that a catalog the registry challenges, or that is read without
// credentials to send, costs no request more than before #802; an open registry with
// credentials costs a GET /v2/ a page, which tells that it wants none.
func TestCatalogChallenged(t *testing.T) {
	ctx := context.Background()
	tests := []struct {
		name  string
		mode  string
		creds bool
		want  []string // the requests of ListRepositories on a fresh provider
	}{
		{name: "bearer", mode: "bearer", creds: true,
			want: []string{"GET /v2/_catalog?n=100", "GET /token?scope=registry%3Acatalog%3A%2A&service=fake", "GET /v2/_catalog?n=100"}},
		{name: "basic", mode: "basic", creds: true, want: []string{"GET /v2/_catalog?n=100", "GET /v2/_catalog?n=100"}},
		{name: "open, with credentials", creds: true, want: []string{"GET /v2/_catalog?n=100", "GET /v2/"}},
		{name: "open, without credentials", want: []string{"GET /v2/_catalog?n=100"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFakeRegistry(t)
			f.authMode = tt.mode
			f.repos = []string{"art/raw/a/x"}
			log, _ := testLogger()
			p := f.provider(t, log, func(c *config.Registry) {
				if tt.creds {
					c.Auth = config.RegistryAuth{Username: f.user, Password: f.pass}
				}
			})
			got, err := p.ListRepositories(ctx, "")
			if err != nil || !slices.Equal(got, []string{"raw/a/x"}) {
				t.Fatalf("ListRepositories = %q, %v", got, err)
			}
			if reqs := f.log(); !slices.Equal(reqs, tt.want) {
				t.Errorf("requests\n%q\nwant\n%q", reqs, tt.want)
			}
			// Once the scheme is known, a second walk is one request.
			n := len(f.log())
			if _, err := p.ListRepositories(ctx, ""); err != nil {
				t.Fatal(err)
			}
			if tt.mode != "" && len(f.log()) != n+1 {
				t.Errorf("second walk: requests %q", f.log()[n:])
			}
		})
	}
}
