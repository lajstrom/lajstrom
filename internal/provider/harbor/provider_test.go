package harbor

import (
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/lajstrom/lajstrom/internal/config"
	"github.com/lajstrom/lajstrom/internal/provider"
)

// clock is a time the test moves.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func newClock() *clock { return &clock{t: time.Date(2026, 11, 15, 8, 0, 0, 0, time.UTC)} }

func (c *clock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

const (
	routeHead  = "HEAD /api/v2.0/projects"
	routePost  = "POST /api/v2.0/projects"
	routeList  = "GET /api/v2.0/projects/{project}/repositories"
	routeRules = "GET /api/v2.0/projects/{project}/immutabletagrules"
	routeDel   = "DELETE /api/v2.0/projects/{project}/repositories/{repo}"
)

func TestNew(t *testing.T) {
	cfg := config.Registry{Name: "h", Type: Type, URL: "https://harbor.example.com", Prefix: "lajstrom/team/"}
	p, err := provider.New(cfg, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("provider.New: %v", err)
	}
	hp, ok := p.(*Provider)
	if !ok {
		t.Fatalf("provider.New = %T", p)
	}
	if hp.project != "lajstrom" || hp.prefix != "lajstrom/team/" || hp.Name() != "h" {
		t.Errorf("project %q, prefix %q, name %q", hp.project, hp.prefix, hp.Name())
	}

	for _, tt := range []struct{ name, url, prefix, want string }{
		{"no prefix", "https://harbor.example.com", "", "prefix must be set"},
		{"slashes only", "https://harbor.example.com", "/", "prefix must be set"},
		{"no URL", "harbor.example.com", "lajstrom/", "url must be an absolute http(s) URL"},
		{"a path", "https://harbor.example.com/harbor", "lajstrom/", "url must not have a path"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			_, err := New(config.Registry{Name: "h", Type: Type, URL: tt.url, Prefix: tt.prefix}, nil)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("New: %v; want %q", err, tt.want)
			}
		})
	}
}

func TestCapabilities(t *testing.T) {
	f := newFakeHarbor(t)
	f.set(func(f *fakeHarbor) {
		f.rules["lajstrom"] = []rule{{Action: "immutable"}, {Disabled: true, Action: "immutable"}}
	})
	p := f.provider(f.config("lajstrom/team/"), newClock().now)
	c, err := p.Capabilities(t.Context())
	if err != nil {
		t.Fatalf("Capabilities: %v", err)
	}
	if !c.CreateOnPush || !c.Referrers || !c.Catalog || !c.ImmutableTags || !c.DeleteManifest {
		t.Errorf("Capabilities = %+v", c)
	}
	if want := 245 - len("lajstrom/team/"); c.MaxNameLength != want {
		t.Errorf("MaxNameLength = %d, want %d", c.MaxNameLength, want)
	}
	// The generic provider asks no catalog of /v2, and the REST API is read once.
	if _, err := p.Capabilities(t.Context()); err != nil {
		t.Fatal(err)
	}
	if n := f.count("GET /v2/_catalog"); n != 0 {
		t.Errorf("_catalog asked %d times", n)
	}
	for _, route := range []string{routeHead, routeList, routeRules} {
		if n := f.count(route); n != 1 {
			t.Errorf("%s: %d requests, want 1", route, n)
		}
	}
}

func TestCapabilitiesPermissions(t *testing.T) {
	yes, no := true, false
	tests := []struct {
		name            string
		list, rules     bool
		ruleSet         []rule
		override        config.CapabilityOverrides
		catalog, immut  bool
		listed, ruleAsk int
	}{
		{name: "every permission, no rule", list: true, rules: true, catalog: true, listed: 1, ruleAsk: 1},
		{name: "no repository:list", list: false, rules: true, catalog: false, listed: 1, ruleAsk: 1},
		{name: "no immutable-tag:list", list: true, rules: false, ruleSet: []rule{{Action: "immutable"}}, catalog: true, listed: 1, ruleAsk: 1},
		{name: "only disabled rules", list: true, rules: true, ruleSet: []rule{{Disabled: true}}, catalog: true, listed: 1, ruleAsk: 1},
		{name: "the configuration decides", list: false, rules: true, ruleSet: []rule{{Action: "immutable"}},
			override: config.CapabilityOverrides{Catalog: &yes, ImmutableTags: &no}, catalog: true, immut: false},
		{name: "catalog off", list: true, rules: true, override: config.CapabilityOverrides{Catalog: &no}, catalog: false, ruleAsk: 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFakeHarbor(t)
			f.set(func(f *fakeHarbor) { f.mayList, f.mayReadRules, f.rules["lajstrom"] = tt.list, tt.rules, tt.ruleSet })
			cfg := f.config("lajstrom/")
			cfg.Capabilities = tt.override
			c, err := f.provider(cfg, newClock().now).Capabilities(t.Context())
			if err != nil {
				t.Fatalf("Capabilities: %v", err)
			}
			if c.Catalog != tt.catalog || c.ImmutableTags != tt.immut {
				t.Errorf("catalog %v, immutableTags %v; want %v, %v", c.Catalog, c.ImmutableTags, tt.catalog, tt.immut)
			}
			if n, m := f.count(routeList), f.count(routeRules); n != tt.listed || m != tt.ruleAsk {
				t.Errorf("listing asked %d times, rules %d; want %d, %d", n, m, tt.listed, tt.ruleAsk)
			}
		})
	}
}

func TestMissingProject(t *testing.T) {
	f := newFakeHarbor(t)
	clk := newClock()
	p := f.provider(f.config("nosuch/"), clk.now)
	_, err := p.Capabilities(t.Context())
	var rej *provider.RejectedError
	if !errors.As(err, &rej) || rej.Status != http.StatusNotFound || !strings.Contains(rej.Hint, "registries[].harbor.createProject") {
		t.Fatalf("Capabilities: %v; want a 404 with the hint", err)
	}
	if errors.Is(err, provider.ErrNotFound) || strings.Contains(err.Error(), "nosuch") {
		t.Errorf("Capabilities: %v; want no ErrNotFound and no project", err)
	}
	if err := p.EnsureRepository(t.Context(), "raw/scratch/notes"); !errors.As(err, &rej) {
		t.Errorf("EnsureRepository: %v", err)
	}
	// Created by hand: the next call finds it.
	f.set(func(f *fakeHarbor) { f.projects["nosuch"] = true })
	if err := p.EnsureRepository(t.Context(), "raw/scratch/notes"); err != nil {
		t.Errorf("EnsureRepository after the project was created: %v", err)
	}
	if _, err := p.Capabilities(t.Context()); err != nil {
		t.Errorf("Capabilities after the project was created: %v", err)
	}
}

func TestCreateProject(t *testing.T) {
	f := newFakeHarbor(t)
	cfg := f.config("fresh/")
	cfg.Harbor.CreateProject = true

	// A project robot may not create it: 403, with the hint.
	p := f.provider(cfg, newClock().now)
	err := p.EnsureRepository(t.Context(), "raw/scratch/notes")
	if !errors.Is(err, provider.ErrUnauthorized) || !strings.Contains(err.Error(), "system robot account with project:create") ||
		strings.Contains(err.Error(), "fresh") || strings.Contains(err.Error(), "Only system level robot") {
		t.Fatalf("EnsureRepository without project:create: %v", err)
	}

	// A POST is not sent again: a 503 is the error.
	f.set(func(f *fakeHarbor) { f.mayCreate, f.fail[routePost] = true, []int{http.StatusServiceUnavailable} })
	if err := p.EnsureRepository(t.Context(), "raw/scratch/notes"); !errors.Is(err, provider.ErrUnavailable) || f.count(routePost) != 2 {
		t.Fatalf("EnsureRepository on a 503 of the POST: %v, %d POSTs", err, f.count(routePost))
	}
	for range 3 {
		if err := p.EnsureRepository(t.Context(), "raw/scratch/notes"); err != nil {
			t.Fatalf("EnsureRepository: %v", err)
		}
	}
	if !f.has("fresh") || f.count(routePost) != 3 {
		t.Errorf("project created %v, %d POSTs; want true, 3", f.has("fresh"), f.count(routePost))
	}
	if _, err := p.Capabilities(t.Context()); err != nil {
		t.Errorf("Capabilities: %v", err)
	}

	// Created by another replica between the HEAD and the POST: 409 is success.
	f.set(func(f *fakeHarbor) { f.fail[routeHead] = []int{http.StatusNotFound} })
	q := f.provider(cfg, newClock().now)
	if err := q.EnsureRepository(t.Context(), "raw/scratch/notes"); err != nil {
		t.Errorf("EnsureRepository on a 409: %v", err)
	}
}

func TestEnsureRepositoryKnown(t *testing.T) {
	f := newFakeHarbor(t)
	clk := newClock()
	p := f.provider(f.config("lajstrom/"), clk.now)
	for range 5 {
		if err := p.EnsureRepository(t.Context(), "raw/scratch/notes"); err != nil {
			t.Fatal(err)
		}
	}
	if n := f.count(routeHead); n != 1 {
		t.Errorf("%d HEADs within knownFor, want 1", n)
	}
	// Deleted behind Lajstrom's back: found gone once knownFor has passed.
	f.set(func(f *fakeHarbor) { delete(f.projects, "lajstrom") })
	if err := p.EnsureRepository(t.Context(), "raw/scratch/notes"); err != nil {
		t.Errorf("EnsureRepository within knownFor: %v", err)
	}
	clk.advance(knownFor)
	var rej *provider.RejectedError
	if err := p.EnsureRepository(t.Context(), "raw/scratch/notes"); !errors.As(err, &rej) {
		t.Errorf("EnsureRepository after knownFor: %v", err)
	}
	if err := p.EnsureRepository(t.Context(), ""); err == nil {
		t.Error("EnsureRepository of an empty name succeeded")
	}
}

func TestEnsureRepositoryErrors(t *testing.T) {
	tests := []struct {
		name   string
		status []int
		want   error
	}{
		{"credentials refused", []int{http.StatusUnauthorized}, provider.ErrUnauthorized},
		{"busy after the retries", []int{503, 503, 503, 503}, provider.ErrUnavailable},
		{"a 503 retried", []int{503}, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFakeHarbor(t)
			f.set(func(f *fakeHarbor) { f.fail[routeHead] = tt.status })
			err := f.provider(f.config("lajstrom/"), newClock().now).EnsureRepository(t.Context(), "raw/scratch/notes")
			if !errors.Is(err, tt.want) || (tt.want == nil) != (err == nil) {
				t.Errorf("EnsureRepository: %v; want %v", err, tt.want)
			}
		})
	}
	// Harbor down: the host refuses the connection.
	f := newFakeHarbor(t)
	cfg := f.config("lajstrom/")
	f.srv.Close()
	err := f.provider(cfg, newClock().now).EnsureRepository(t.Context(), "raw/scratch/notes")
	if !errors.Is(err, provider.ErrUnavailable) || strings.Contains(err.Error(), "127.0.0.1") {
		t.Errorf("EnsureRepository of a closed server: %v", err)
	}
}

func TestRedirectNotFollowed(t *testing.T) {
	other := newFakeHarbor(t)
	redirect := httptest.NewServer(http.RedirectHandler(other.srv.URL+"/api/v2.0/projects?project_name=lajstrom", http.StatusFound))
	t.Cleanup(redirect.Close)
	cfg := other.config("lajstrom/")
	cfg.URL = redirect.URL
	err := other.provider(cfg, newClock().now).EnsureRepository(t.Context(), "raw/scratch/notes")
	if err == nil || !strings.Contains(err.Error(), "HTTP 302") {
		t.Errorf("EnsureRepository on a redirect: %v", err)
	}
	if n := other.count(routeHead); n != 0 {
		t.Errorf("the redirect was followed: %d requests with the credentials", n)
	}
}
