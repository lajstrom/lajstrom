package generic

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/opencontainers/go-digest"
	"github.com/opencontainers/image-spec/specs-go"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/lajstrom/lajstrom/internal/config"
	"github.com/lajstrom/lajstrom/internal/provider"
)

// pushBlob pushes content as a blob of repo and returns its descriptor.
func pushBlob(t *testing.T, repo provider.Repository, content string) ocispec.Descriptor {
	t.Helper()
	desc := ocispec.Descriptor{MediaType: "application/octet-stream", Digest: digest.FromString(content), Size: int64(len(content))}
	if err := repo.Push(context.Background(), desc, strings.NewReader(content)); err != nil {
		t.Fatalf("push blob: %v", err)
	}
	return desc
}

// pushManifest pushes an image manifest with one layer, tagged tag when not empty, with
// subject when not nil, and returns its descriptor.
func pushManifest(t *testing.T, repo provider.Repository, tag string, subject *ocispec.Descriptor) ocispec.Descriptor {
	t.Helper()
	cfg := pushBlob(t, repo, "{}")
	cfg.MediaType = ocispec.MediaTypeEmptyJSON
	layer := pushBlob(t, repo, "layer of "+tag)
	m := ocispec.Manifest{
		Versioned:    specs.Versioned{SchemaVersion: 2},
		MediaType:    ocispec.MediaTypeImageManifest,
		ArtifactType: "application/vnd.lajstrom.test.v1",
		Config:       cfg,
		Layers:       []ocispec.Descriptor{layer},
		Subject:      subject,
	}
	content, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	desc := ocispec.Descriptor{MediaType: m.MediaType, ArtifactType: m.ArtifactType, Digest: digest.FromBytes(content), Size: int64(len(content))}
	if tag == "" {
		err = repo.Push(context.Background(), desc, bytes.NewReader(content))
	} else {
		err = repo.PushReference(context.Background(), desc, bytes.NewReader(content), tag)
	}
	if err != nil {
		t.Fatalf("push manifest: %v", err)
	}
	return desc
}

// listTags returns every tag of repo.
func listTags(ctx context.Context, repo provider.Repository) ([]string, error) {
	var all []string
	err := repo.Tags(ctx, "", func(tags []string) error {
		all = append(all, tags...)
		return nil
	})
	return all, err
}

func TestPrefix(t *testing.T) {
	tests := []struct {
		prefix string
		want   string // the OCI repository of raw/scratch/notes
	}{
		{prefix: "art/", want: "art/raw/scratch/notes"},
		{prefix: "art", want: "art/raw/scratch/notes"},
		{prefix: "/art/", want: "art/raw/scratch/notes"},
		{prefix: "lajstrom/prod/", want: "lajstrom/prod/raw/scratch/notes"},
		{prefix: "", want: "raw/scratch/notes"},
	}
	for _, tt := range tests {
		t.Run(fmt.Sprintf("prefix %q", tt.prefix), func(t *testing.T) {
			ctx := context.Background()
			f := newFakeRegistry(t)
			log, _ := testLogger()
			p := f.provider(t, log, func(c *config.Registry) { c.Prefix = tt.prefix })
			repo, err := p.Repository(ctx, "raw/scratch/notes")
			if err != nil {
				t.Fatal(err)
			}
			blob := pushBlob(t, repo, "hello")
			pushManifest(t, repo, "v1", nil)
			if _, err := repo.Fetch(ctx, blob); err != nil {
				t.Fatalf("Fetch: %v", err)
			}
			if _, err := repo.Resolve(ctx, "v1"); err != nil {
				t.Fatalf("Resolve: %v", err)
			}
			b, err := p.OpenBlob(ctx, "raw/scratch/notes", blob.Digest, "", true)
			if err != nil {
				t.Fatalf("OpenBlob: %v", err)
			}
			_ = b.Body.Close()
			if _, err := p.StatBlob(ctx, "raw/scratch/notes", blob.Digest); err != nil {
				t.Fatalf("StatBlob: %v", err)
			}
			for _, want := range []string{
				"POST /v2/" + tt.want + "/blobs/uploads/",
				"PUT /v2/" + tt.want + "/manifests/v1",
				"GET /v2/" + tt.want + "/blobs/" + blob.Digest.String(),
				"HEAD /v2/" + tt.want + "/blobs/" + blob.Digest.String(),
				"HEAD /v2/" + tt.want + "/manifests/v1",
			} {
				if f.count(want) == 0 {
					t.Errorf("no request %q in\n%s", want, strings.Join(f.log(), "\n"))
				}
			}
			for _, r := range f.log() {
				if strings.HasPrefix(r, "GET /v2/_catalog") || r == "GET /v2/" {
					continue
				}
				if !strings.Contains(r, " /v2/"+tt.want+"/") {
					t.Errorf("request %q is not below %s", r, tt.want)
				}
			}
		})
	}
}

func TestListRepositories(t *testing.T) {
	ctx := context.Background()
	f := newFakeRegistry(t)
	f.repos = []string{"art/raw/b/y", "art/raw/a/x", "art/pypi/c/w", "other/raw/a/z", "raw/a/q", "art/raw/a/x"}
	log, _ := testLogger()
	prefixed := f.provider(t, log, nil)
	bare := f.provider(t, log, func(c *config.Registry) { c.Prefix = "" })
	tests := []struct {
		name   string
		p      *Provider
		prefix string
		want   []string
	}{
		{name: "everything below the instance prefix", p: prefixed, prefix: "", want: []string{"pypi/c/w", "raw/a/x", "raw/b/y"}},
		{name: "one format", p: prefixed, prefix: "raw/", want: []string{"raw/a/x", "raw/b/y"}},
		{name: "one repo", p: prefixed, prefix: "raw/a/", want: []string{"raw/a/x"}},
		{name: "nothing", p: prefixed, prefix: "maven/", want: []string{}},
		{name: "no instance prefix", p: bare, prefix: "raw/", want: []string{"raw/a/q"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := tt.p.ListRepositories(ctx, tt.prefix)
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(got, tt.want) {
				t.Errorf("ListRepositories(%q) = %q, want %q", tt.prefix, got, tt.want)
			}
		})
	}
}

// catalogPages answers the catalog with pages, each linking to the next, and leaves the other
// requests to the fake.
func catalogPages(pages ...[]string) func(w http.ResponseWriter, r *http.Request) bool {
	return func(w http.ResponseWriter, r *http.Request) bool {
		if r.URL.Path != "/v2/_catalog" {
			return false
		}
		i, _ := strconv.Atoi(strings.TrimPrefix(r.URL.Query().Get("last"), "page"))
		if i+1 < len(pages) {
			w.Header().Set("Link", fmt.Sprintf(`</v2/_catalog?n=100&last=page%d>; rel="next"`, i+1))
		}
		_ = json.NewEncoder(w).Encode(map[string][]string{"repositories": pages[i]})
		return true
	}
}

func TestListRepositoriesPagination(t *testing.T) {
	ctx := context.Background()
	var many []string
	for i := range 250 {
		many = append(many, fmt.Sprintf("art/raw/r/%03d", 249-i))
	}
	tests := []struct {
		name      string
		setup     func(f *fakeRegistry)
		maxPages  int // 0: the default
		prefix    string
		wantNames []string // nil: only the count is checked
		wantCount int
		wantReqs  int
		wantWarn  bool // the incomplete listing is logged
	}{
		{
			name: "three pages, one sorted list",
			setup: func(f *fakeRegistry) {
				f.hook = catalogPages([]string{"art/raw/c", "art/raw/a"}, []string{"art/raw/b", "art/raw/a"}, []string{"art/raw/d"})
			},
			prefix:    "raw/",
			wantNames: []string{"raw/a", "raw/b", "raw/c", "raw/d"},
			wantCount: 4,
			wantReqs:  3,
		},
		{name: "pages of the page size", setup: func(f *fakeRegistry) { f.repos = many }, wantCount: 250, wantReqs: 3},
		{
			name:      "a repeated page stops the walk",
			setup:     func(f *fakeRegistry) { f.catalogMode = "repeat" },
			wantNames: []string{"raw/a/x", "raw/b/y"},
			wantCount: 2,
			wantReqs:  2,
		},
		{name: "the page cap stops the walk", setup: func(f *fakeRegistry) { f.catalogMode = "endless" }, maxPages: 5, prefix: "raw/", wantCount: 5, wantReqs: 5, wantWarn: true},
		{name: "the default cap is 1000 pages", setup: func(f *fakeRegistry) { f.catalogMode = "endless" }, wantCount: 1000, wantReqs: 1000, wantWarn: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFakeRegistry(t)
			tt.setup(f)
			log, buf := testLogger()
			p := f.provider(t, log, nil)
			if tt.maxPages > 0 {
				p.maxPages = tt.maxPages
			}
			got, err := p.ListRepositories(ctx, tt.prefix)
			if err != nil {
				t.Fatal(err)
			}
			if len(got) != tt.wantCount || !slices.IsSorted(got) || (tt.wantNames != nil && !slices.Equal(got, tt.wantNames)) {
				t.Errorf("got %d names %q, want %d sorted names %q", len(got), got, tt.wantCount, tt.wantNames)
			}
			if n := f.count("GET /v2/_catalog"); n != tt.wantReqs {
				t.Errorf("%d catalog requests, want %d", n, tt.wantReqs)
			}
			if warned := strings.Contains(buf.String(), "level=WARN"); warned != tt.wantWarn {
				t.Errorf("warned about an incomplete listing: %v, want %v\n%s", warned, tt.wantWarn, buf)
			}
		})
	}
}

func TestListRepositoriesPageRequests(t *testing.T) {
	f := newFakeRegistry(t)
	for i := range 250 {
		f.repos = append(f.repos, fmt.Sprintf("art/raw/r/%03d", i))
	}
	log, _ := testLogger()
	if _, err := f.provider(t, log, nil).ListRepositories(context.Background(), ""); err != nil {
		t.Fatal(err)
	}
	want := []string{"GET /v2/_catalog?n=100", "GET /v2/_catalog?last=art%2Fraw%2Fr%2F099&n=100", "GET /v2/_catalog?last=art%2Fraw%2Fr%2F199&n=100"}
	if got := f.log(); !slices.Equal(got, want) {
		t.Errorf("requests\n%q\nwant\n%q", got, want)
	}
}

func TestListRepositoriesWithoutCatalog(t *testing.T) {
	ctx := context.Background()
	tests := []struct {
		name   string
		status int
		off    bool // capabilities.catalog: false
	}{
		{name: "capabilities.catalog false", off: true},
		{name: "404", status: http.StatusNotFound},
		{name: "401", status: http.StatusUnauthorized},
		{name: "405", status: http.StatusMethodNotAllowed},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFakeRegistry(t)
			f.catalogStatus = tt.status
			log, _ := testLogger()
			p := f.provider(t, log, func(c *config.Registry) {
				if tt.off {
					c.Capabilities.Catalog = new(false)
				}
			})
			_, err := p.ListRepositories(ctx, "")
			if !errors.Is(err, provider.ErrUnsupported) {
				t.Errorf("ListRepositories error = %v, want ErrUnsupported", err)
			}
			want := 1
			if tt.off {
				want = 0
			}
			if n := len(f.log()); n != want {
				t.Errorf("%d requests, want %d", n, want)
			}
		})
	}

	t.Run("after a probe that found none", func(t *testing.T) {
		f := newFakeRegistry(t)
		f.catalogStatus = http.StatusNotFound
		log, _ := testLogger()
		p := f.provider(t, log, nil)
		if _, err := p.Capabilities(ctx); err != nil {
			t.Fatal(err)
		}
		before := f.count("GET /v2/_catalog")
		if _, err := p.ListRepositories(ctx, ""); !errors.Is(err, provider.ErrUnsupported) {
			t.Errorf("ListRepositories error = %v, want ErrUnsupported", err)
		}
		if n := f.count("GET /v2/_catalog"); n != before {
			t.Errorf("ListRepositories asked the catalog after the probe found none")
		}
	})
}

func TestRetries(t *testing.T) {
	ctx := context.Background()
	tests := []struct {
		name     string
		retries  int
		failures int // 503 answers before the registry recovers
		wantReqs int
		wantErr  bool
	}{
		{name: "503 twice then 200", retries: 0, failures: 2, wantReqs: 3},
		{name: "retries disabled", retries: -1, failures: 2, wantReqs: 1, wantErr: true},
		{name: "retries exhausted", retries: 1, failures: 5, wantReqs: 2, wantErr: true},
		{name: "no failures", retries: 0, failures: 0, wantReqs: 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFakeRegistry(t)
			log, _ := testLogger()
			p := f.provider(t, log, func(c *config.Registry) { c.Retries = tt.retries })
			repo, err := p.Repository(ctx, "raw/scratch/notes")
			if err != nil {
				t.Fatal(err)
			}
			pushManifest(t, repo, "v1", nil)
			failed := 0
			f.hook = func(w http.ResponseWriter, r *http.Request) bool {
				if !strings.HasSuffix(r.URL.Path, "/tags/list") || failed >= tt.failures {
					return false
				}
				failed++
				writeErr(w, http.StatusServiceUnavailable, "UNAVAILABLE", "try later")
				return true
			}
			tags, err := listTags(ctx, repo)
			if (err != nil) != tt.wantErr {
				t.Fatalf("Tags error = %v, want error %v", err, tt.wantErr)
			}
			if !tt.wantErr && !slices.Equal(tags, []string{"v1"}) {
				t.Errorf("tags = %q, want [v1]", tags)
			}
			if n := f.count("GET /v2/art/raw/scratch/notes/tags/list"); n != tt.wantReqs {
				t.Errorf("%d requests, want %d", n, tt.wantReqs)
			}
		})
	}

	t.Run("a streamed body is not retried", func(t *testing.T) {
		tests := []struct {
			name     string
			body     func(s string) io.Reader
			wantPuts int
		}{
			{name: "streamed", body: func(s string) io.Reader { return struct{ io.Reader }{strings.NewReader(s)} }, wantPuts: 1},
			{name: "replayable", body: func(s string) io.Reader { return strings.NewReader(s) }, wantPuts: 1 + config.DefaultRegistryRetries},
		}
		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				f := newFakeRegistry(t)
				f.hook = func(w http.ResponseWriter, r *http.Request) bool {
					if r.Method != http.MethodPut {
						return false
					}
					_, _ = io.Copy(io.Discard, r.Body)
					writeErr(w, http.StatusServiceUnavailable, "UNAVAILABLE", "try later")
					return true
				}
				log, _ := testLogger()
				repo, err := f.provider(t, log, nil).Repository(ctx, "raw/scratch/notes")
				if err != nil {
					t.Fatal(err)
				}
				desc := ocispec.Descriptor{MediaType: "application/octet-stream", Digest: digest.FromString("hello"), Size: 5}
				if err := repo.Push(ctx, desc, tt.body("hello")); err == nil {
					t.Fatal("Push succeeded against a failing registry")
				}
				if n := f.count("PUT /v2/art/raw/scratch/notes/blobs/uploads/"); n != tt.wantPuts {
					t.Errorf("%d PUT requests, want %d", n, tt.wantPuts)
				}
			})
		}
	})
}

func TestCapabilities(t *testing.T) {
	ctx := context.Background()
	defaults := provider.Capabilities{CreateOnPush: true, Referrers: true, Catalog: true, DeleteManifest: true, BlobRedirect: true}
	with := func(edit func(*provider.Capabilities)) provider.Capabilities {
		c := defaults
		edit(&c)
		return c
	}
	nameUnknown := func(w http.ResponseWriter, r *http.Request) bool {
		if !strings.Contains(r.URL.Path, "/referrers/") {
			return false
		}
		writeErr(w, http.StatusNotFound, "NAME_UNKNOWN", "repository name not known to registry")
		return true
	}
	tests := []struct {
		name    string
		setup   func(f *fakeRegistry)
		edit    func(c *config.Registry)
		want    provider.Capabilities
		noProbe []string // requests that must not be sent
	}{
		{name: "defaults", want: defaults},
		{name: "no referrers API", setup: func(f *fakeRegistry) { f.referrers = false }, want: with(func(c *provider.Capabilities) { c.Referrers = false })},
		{name: "referrers probe cannot tell", setup: func(f *fakeRegistry) { f.hook = nameUnknown }, want: with(func(c *provider.Capabilities) { c.Referrers = false })},
		{name: "catalog 404", setup: func(f *fakeRegistry) { f.catalogStatus = 404 }, want: with(func(c *provider.Capabilities) { c.Catalog = false })},
		{name: "catalog 401", setup: func(f *fakeRegistry) { f.catalogStatus = 401 }, want: with(func(c *provider.Capabilities) { c.Catalog = false })},
		{name: "catalog 403", setup: func(f *fakeRegistry) { f.catalogStatus = 403 }, want: with(func(c *provider.Capabilities) { c.Catalog = false })},
		{name: "catalog 405", setup: func(f *fakeRegistry) { f.catalogStatus = 405 }, want: with(func(c *provider.Capabilities) { c.Catalog = false })},
		{
			name: "every override",
			edit: func(c *config.Registry) {
				c.Capabilities = config.CapabilityOverrides{
					CreateOnPush: new(false), Referrers: new(false), Catalog: new(false), DeleteManifest: new(false),
					ImmutableTags: new(true), BlobRedirect: new(false), MaxBlobSize: 1 << 30,
				}
			},
			want:    provider.Capabilities{ImmutableTags: true, MaxBlobSize: 1 << 30},
			noProbe: []string{"GET /v2/_catalog", "GET /v2/art/lajstrom-probe/referrers/"},
		},
		{
			name:    "an override wins over the probe",
			setup:   func(f *fakeRegistry) { f.referrers, f.catalogStatus = false, 404 },
			edit:    func(c *config.Registry) { c.Capabilities.Referrers, c.Capabilities.Catalog = new(true), new(true) },
			want:    defaults,
			noProbe: []string{"GET /v2/_catalog", "GET /v2/art/lajstrom-probe/referrers/"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFakeRegistry(t)
			if tt.setup != nil {
				tt.setup(f)
			}
			log, buf := testLogger()
			p := f.provider(t, log, tt.edit)
			got, err := p.Capabilities(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if got != tt.want {
				t.Errorf("Capabilities =\n%+v\nwant\n%+v", got, tt.want)
			}
			for _, r := range tt.noProbe {
				if f.count(r) != 0 {
					t.Errorf("sent %q although the configuration sets the capability", r)
				}
			}
			n := len(f.log())
			if again, err := p.Capabilities(ctx); err != nil || again != got {
				t.Errorf("second Capabilities = %+v, %v; want the cached %+v", again, err, got)
			}
			if len(f.log()) != n {
				t.Errorf("the second Capabilities sent requests: %q", f.log()[n:])
			}
			lines := strings.Count(buf.String(), `msg="registry capabilities"`)
			if lines != 1 || !strings.Contains(buf.String(), "registry=fake") || !strings.Contains(buf.String(), fmt.Sprintf("catalog=%v", tt.want.Catalog)) {
				t.Errorf("want one info line with the matrix, got\n%s", buf)
			}
		})
	}
}

func TestCapabilitiesProbeFailure(t *testing.T) {
	ctx := context.Background()
	f := newFakeRegistry(t)
	down := true
	f.hook = func(w http.ResponseWriter, r *http.Request) bool {
		if !down || r.URL.Path != "/v2/" {
			return false
		}
		writeErr(w, http.StatusServiceUnavailable, "UNAVAILABLE", "down")
		return true
	}
	log, _ := testLogger()
	p := f.provider(t, log, nil)
	if _, err := p.Capabilities(ctx); err == nil {
		t.Fatal("Capabilities succeeded against a registry that is down")
	}
	if n := f.count("GET /v2/"); n != 1+config.DefaultRegistryRetries {
		t.Errorf("%d pings, want %d (with retries)", n, 1+config.DefaultRegistryRetries)
	}
	down = false
	if c, err := p.Capabilities(ctx); err != nil || !c.Catalog {
		t.Errorf("Capabilities after recovery = %+v, %v; want a fresh probe", c, err)
	}

	t.Run("unreachable", func(t *testing.T) {
		f := newFakeRegistry(t)
		p := f.provider(t, log, func(c *config.Registry) { c.Retries = -1 })
		f.srv.Close()
		_, err := p.Capabilities(ctx)
		if err == nil || !strings.HasPrefix(err.Error(), "generic: ping fake: ") {
			t.Errorf("Capabilities error = %v, want one that names the ping", err)
		}
	})
}

func TestReferrersStateReachesRepositories(t *testing.T) {
	ctx := context.Background()
	tests := []struct {
		name          string
		referrers     bool
		override      *bool
		wantAPICalls  bool // Repository.Referrers asks /referrers/ of the repository
		wantTagSchema bool // Repository.Referrers reads the sha256-<hex> tag
	}{
		{name: "API supported", referrers: true, wantAPICalls: true},
		{name: "API missing", referrers: false, wantTagSchema: true},
		{name: "override false", referrers: true, override: new(false), wantTagSchema: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFakeRegistry(t)
			f.referrers = tt.referrers
			log, _ := testLogger()
			p := f.provider(t, log, func(c *config.Registry) { c.Capabilities.Referrers = tt.override })
			if _, err := p.Capabilities(ctx); err != nil {
				t.Fatal(err)
			}
			repo, err := p.Repository(ctx, "raw/scratch/notes")
			if err != nil {
				t.Fatal(err)
			}
			subject := pushManifest(t, repo, "v1", nil)
			sig := pushManifest(t, repo, "", &subject)
			var got []ocispec.Descriptor
			if err := repo.Referrers(ctx, subject, "", func(r []ocispec.Descriptor) error {
				got = append(got, r...)
				return nil
			}); err != nil {
				t.Fatalf("Referrers: %v", err)
			}
			if len(got) != 1 || got[0].Digest != sig.Digest {
				t.Errorf("referrers = %v, want %s", got, sig.Digest)
			}
			api := f.count("GET /v2/art/raw/scratch/notes/referrers/") > 0
			tagSchema := f.count("GET /v2/art/raw/scratch/notes/manifests/sha256-"+subject.Digest.Encoded()) > 0
			if api != tt.wantAPICalls || tagSchema != tt.wantTagSchema {
				t.Errorf("referrers API asked %v, tag schema read %v; want %v and %v\n%s", api, tagSchema, tt.wantAPICalls, tt.wantTagSchema, strings.Join(f.log(), "\n"))
			}
		})
	}
}

func TestDeleteManifestLearnt(t *testing.T) {
	ctx := context.Background()
	tests := []struct {
		name     string
		override *bool
		bare     bool // answer DELETE with a 405 without a body instead of 405 UNSUPPORTED
		want     bool
	}{
		{name: "learnt false on 405 UNSUPPORTED", want: false},
		{name: "learnt false on a bare 405", bare: true, want: false},
		{name: "override true wins", override: new(true), want: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFakeRegistry(t)
			f.deleteDisabled = !tt.bare
			if tt.bare {
				f.hook = func(w http.ResponseWriter, r *http.Request) bool {
					if r.Method != http.MethodDelete {
						return false
					}
					w.WriteHeader(http.StatusMethodNotAllowed)
					return true
				}
			}
			log, buf := testLogger()
			p := f.provider(t, log, func(c *config.Registry) { c.Capabilities.DeleteManifest = tt.override })
			if c, err := p.Capabilities(ctx); err != nil || !c.DeleteManifest {
				t.Fatalf("Capabilities before a delete = %+v, %v; want DeleteManifest", c, err)
			}
			repo, err := p.Repository(ctx, "raw/scratch/notes")
			if err != nil {
				t.Fatal(err)
			}
			blob := pushBlob(t, repo, "hello")
			if err := repo.Delete(ctx, blob); !errors.Is(err, provider.ErrUnsupported) {
				t.Fatalf("Delete blob error = %v, want ErrUnsupported", err)
			}
			if c, _ := p.Capabilities(ctx); !c.DeleteManifest {
				t.Errorf("a refused blob delete turned DeleteManifest false")
			}
			m := pushManifest(t, repo, "v1", nil)
			for range 2 {
				if err := repo.Delete(ctx, m); !errors.Is(err, provider.ErrUnsupported) {
					t.Fatalf("Delete manifest error = %v, want ErrUnsupported", err)
				}
			}
			if c, _ := p.Capabilities(ctx); c.DeleteManifest != tt.want {
				t.Errorf("DeleteManifest after a 405 = %v, want %v", c.DeleteManifest, tt.want)
			}
			if n := strings.Count(buf.String(), "refuses manifest deletes"); n != 1 {
				t.Errorf("%d warnings about refused deletes, want 1:\n%s", n, buf)
			}
		})
	}
}

// TestDeleteDenied: a 405 DENIED refuses one delete, as Zot refuses a manifest that an image
// index lists and a blob that a manifest uses (#153). It is ErrConflict, DeleteManifest stays
// true, and the next delete goes through.
func TestDeleteDenied(t *testing.T) {
	ctx := context.Background()
	f := newFakeRegistry(t)
	var deletes atomic.Int32
	f.hook = func(w http.ResponseWriter, r *http.Request) bool {
		if r.Method != http.MethodDelete || deletes.Add(1) > 2 {
			return false
		}
		writeErr(w, http.StatusMethodNotAllowed, "DENIED", "requested access to the resource is denied")
		return true
	}
	log, buf := testLogger()
	p := f.provider(t, log, nil)
	repo, err := p.Repository(ctx, "raw/scratch/notes")
	if err != nil {
		t.Fatal(err)
	}
	blob := pushBlob(t, repo, "hello")
	m := pushManifest(t, repo, "v1", nil)
	for _, d := range []ocispec.Descriptor{blob, m} {
		err := repo.Delete(ctx, d)
		if !errors.Is(err, provider.ErrConflict) || errors.Is(err, provider.ErrUnsupported) {
			t.Fatalf("Delete(%s) of a 405 DENIED = %v, want ErrConflict alone", d.MediaType, err)
		}
		if !strings.Contains(err.Error(), "HTTP 405, DENIED") {
			t.Errorf("Delete error %q does not name the answer", err)
		}
	}
	if c, err := p.Capabilities(ctx); err != nil || !c.DeleteManifest {
		t.Errorf("Capabilities after a 405 DENIED = %+v, %v; want DeleteManifest", c, err)
	}
	if strings.Contains(buf.String(), "refuses manifest deletes") {
		t.Errorf("a 405 DENIED logged that deletes are off:\n%s", buf)
	}
	if err := repo.Delete(ctx, m); err != nil {
		t.Fatalf("the next Delete of the manifest: %v", err)
	}
	if ok, err := repo.Exists(ctx, m); err != nil || ok {
		t.Errorf("Exists after Delete = %v, %v; want false", ok, err)
	}
}

// errAny is a wanted error that is not a particular one.
var errAny = errors.New("any error")

// wantErr fails t when err is not want: nil for none, errAny for any error, else a sentinel
// that err must wrap.
func wantErr(t *testing.T, what string, err, want error) {
	t.Helper()
	switch {
	case want == nil:
		if err != nil {
			t.Errorf("%s: %v", what, err)
		}
	case errors.Is(want, errAny):
		if err == nil {
			t.Errorf("%s succeeded, want an error", what)
		}
	case !errors.Is(err, want):
		t.Errorf("%s error = %v, want %v", what, err, want)
	}
}

// wantTrue fails t when ok is false.
func wantTrue(t *testing.T, what string, ok bool) {
	t.Helper()
	if !ok {
		t.Errorf("not true: %s", what)
	}
}

func TestRepositoryOperations(t *testing.T) {
	ctx := context.Background()
	f := newFakeRegistry(t)
	log, _ := testLogger()
	repo, err := f.provider(t, log, nil).Repository(ctx, "raw/scratch/notes")
	if err != nil {
		t.Fatal(err)
	}
	m := pushManifest(t, repo, "v1", nil)
	missing := ocispec.Descriptor{MediaType: "application/octet-stream", Digest: digest.FromString("missing"), Size: 7}

	ok, err := repo.Exists(ctx, m)
	wantErr(t, "Exists(manifest)", err, nil)
	wantTrue(t, "Exists(manifest)", ok)
	ok, err = repo.Exists(ctx, missing)
	wantErr(t, "Exists(missing)", err, nil)
	wantTrue(t, "!Exists(missing)", !ok)
	_, err = repo.Fetch(ctx, missing)
	wantErr(t, "Fetch(missing)", err, provider.ErrNotFound)
	_, err = repo.Resolve(ctx, "v2")
	wantErr(t, "Resolve(v2)", err, provider.ErrNotFound)
	wantErr(t, "Tag", repo.Tag(ctx, m, "latest"), nil)
	tags, err := listTags(ctx, repo)
	wantErr(t, "Tags", err, nil)
	wantTrue(t, fmt.Sprintf("tags %q are [latest v1]", tags), slices.Equal(tags, []string{"latest", "v1"}))
	wantErr(t, "Delete", repo.Delete(ctx, m), nil)
	wantErr(t, "second Delete", repo.Delete(ctx, m), provider.ErrNotFound)
}

func TestRepositoryNames(t *testing.T) {
	ctx := context.Background()
	f := newFakeRegistry(t)
	log, _ := testLogger()
	p := f.provider(t, log, nil)
	other, err := p.Repository(ctx, "raw/other/x")
	if err != nil {
		t.Fatal(err)
	}
	_, err = listTags(ctx, other)
	wantErr(t, "Tags of a missing repository", err, provider.ErrNotFound)
	if err != nil && (strings.Contains(err.Error(), "art/") || strings.Contains(err.Error(), f.srv.URL)) {
		t.Errorf("error %q shows the instance prefix or the backend URL", err)
	}
	_, err = p.Repository(ctx, "")
	wantErr(t, `Repository("")`, err, errAny)
	_, err = p.Repository(ctx, "Raw/Upper")
	wantErr(t, "Repository with an invalid name", err, errAny)
	wantErr(t, "EnsureRepository", p.EnsureRepository(ctx, "raw/scratch/notes"), nil)
	wantErr(t, "DeleteRepository", p.DeleteRepository(ctx, "raw/scratch/notes"), provider.ErrUnsupported)
}

func TestCredentials(t *testing.T) {
	ctx := context.Background()
	for _, mode := range []string{"basic", "bearer"} {
		t.Run(mode, func(t *testing.T) {
			f := newFakeRegistry(t)
			f.authMode = mode
			log, buf := testLogger()
			p := f.provider(t, log, func(c *config.Registry) {
				c.Auth = config.RegistryAuth{Username: f.user, Password: f.pass, PasswordEnv: "FAKE_PASSWORD"}
			})
			if _, err := p.Capabilities(ctx); err != nil {
				t.Fatalf("Capabilities: %v", err)
			}
			repo, err := p.Repository(ctx, "raw/scratch/notes")
			if err != nil {
				t.Fatal(err)
			}
			blob := pushBlob(t, repo, "hello")
			rc, err := repo.Fetch(ctx, blob)
			if err != nil {
				t.Fatalf("Fetch: %v", err)
			}
			_ = rc.Close()
			readBlobWithCredentials(t, f, p, blob.Digest)

			basic := "Basic " + base64.StdEncoding.EncodeToString([]byte(f.user+":"+f.pass))
			want := map[string]string{"basic": basic, "bearer": "Bearer " + f.token}[mode]
			f.mu.Lock()
			sent := slices.Contains(f.auths, want)
			f.mu.Unlock()
			if !sent {
				t.Errorf("no request carried %q", want)
			}
			if mode == "bearer" && f.count("GET /token") == 0 {
				t.Error("the token endpoint was not asked")
			}

			// Wrong and missing credentials are ErrUnauthorized, and nothing secret is logged.
			wrong := f.provider(t, log, func(c *config.Registry) {
				c.Auth = config.RegistryAuth{Username: f.user, Password: "wrong-" + f.pass, PasswordEnv: "FAKE_PASSWORD"}
			})
			_, err = wrong.Capabilities(ctx)
			if !errors.Is(err, provider.ErrUnauthorized) {
				t.Errorf("wrong password: Capabilities error = %v, want ErrUnauthorized", err)
			}
			anonymous := f.provider(t, log, nil)
			if _, err := anonymous.Capabilities(ctx); !errors.Is(err, provider.ErrUnauthorized) {
				t.Errorf("anonymous: Capabilities error = %v, want ErrUnauthorized", err)
			}
			logs := buf.String()
			if !strings.Contains(logs, "registry request") {
				t.Fatalf("no debug lines about requests: the test would prove nothing\n%s", logs)
			}
			secrets := []string{f.pass, f.token, basic, base64.StdEncoding.EncodeToString([]byte(f.user + ":wrong-" + f.pass)), "Authorization", "Bearer "}
			noSecrets(t, "the log", logs, secrets)
			noSecrets(t, "the error", fmt.Sprint(err), secrets)
		})
	}
}

// readBlobWithCredentials opens and stats the blob d of raw/scratch/notes through p, which
// has credentials, and then through a redirect to another origin, which the provider follows
// without any of Lajstrom's credentials.
func readBlobWithCredentials(t *testing.T, f *fakeRegistry, p *Provider, d digest.Digest) {
	t.Helper()
	ctx := t.Context()
	b, err := p.OpenBlob(ctx, "raw/scratch/notes", d, "bytes=1-2", true)
	if err != nil || b.Status != http.StatusPartialContent {
		t.Fatalf("OpenBlob = %+v, %v", b, err)
	}
	_ = b.Body.Close()
	if _, err := p.StatBlob(ctx, "raw/scratch/notes", d); err != nil {
		t.Fatalf("StatBlob: %v", err)
	}
	store := newObjectStore(t, f, "art/raw/scratch/notes")
	f.redirect = store.srv.URL + "/bucket"
	defer func() { f.redirect = "" }()
	b, err = p.OpenBlob(ctx, "raw/scratch/notes", d, "", false)
	if err != nil || b.Status != http.StatusOK {
		t.Fatalf("OpenBlob through the object store = %+v, %v", b, err)
	}
	_ = b.Body.Close()
	if got := store.log(); len(got) != 1 || got[0] != "GET /bucket/"+d.String()+" Range= Authorization=" {
		t.Errorf("object store requests %q, want one GET without Authorization", got)
	}
}

// noSecrets fails t when text contains one of secrets.
func noSecrets(t *testing.T, what, text string, secrets []string) {
	t.Helper()
	for _, s := range secrets {
		if strings.Contains(text, s) {
			t.Errorf("%s contains %q:\n%s", what, s, text)
		}
	}
}

func TestNew(t *testing.T) {
	tests := []struct {
		name string
		edit func(c *config.Registry)
		want string // substring of the error; empty for success
	}{
		{name: "valid", edit: func(*config.Registry) {}},
		{name: "trailing slash", edit: func(c *config.Registry) { c.URL += "/" }},
		{name: "path", edit: func(c *config.Registry) { c.URL += "/registry" }, want: "must not have a path"},
		{name: "scheme", edit: func(c *config.Registry) { c.URL = "ftp://example.com" }, want: "absolute http(s) URL"},
		{name: "credentials", edit: func(c *config.Registry) { c.URL = "https://u:secret@example.com" }, want: "credentials"},
		{name: "negative timeout", edit: func(c *config.Registry) { c.Timeout = -time.Second }, want: "timeout"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := config.Registry{Name: "r", Type: Type, URL: "http://localhost:5000"}
			tt.edit(&cfg)
			_, err := New(cfg, nil)
			switch {
			case tt.want == "" && err != nil:
				t.Errorf("New: %v", err)
			case tt.want != "" && (err == nil || !strings.Contains(err.Error(), tt.want)):
				t.Errorf("New error = %v, want it to contain %q", err, tt.want)
			case err != nil && strings.Contains(err.Error(), "secret"):
				t.Errorf("error %q contains the password", err)
			}
		})
	}

	t.Run("registered with the factory", func(t *testing.T) {
		if !slices.Contains(provider.Types(), Type) {
			t.Fatalf("provider.Types() = %q, want it to contain %q", provider.Types(), Type)
		}
		p, err := provider.New(config.Registry{Name: "r", Type: Type, URL: "https://registry.example"}, nil)
		if err != nil {
			t.Fatal(err)
		}
		if _, ok := p.(*Provider); !ok || p.Name() != "r" {
			t.Errorf("provider.New = %T named %q, want *generic.Provider named r", p, p.Name())
		}
	})
}
