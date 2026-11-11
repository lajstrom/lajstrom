package probe

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/opencontainers/go-digest"

	"github.com/lajstrom/lajstrom/internal/config"
)

// lagRegistry is a registry that answers 404 to the first reads of each write: HEAD of a blob
// or a manifest, and a tag list or catalog without it, lag times per write and kind of read.
type lagRegistry struct {
	lag int

	mu        sync.Mutex
	blobs     map[string]int            // digest → reads left that miss it
	manifests map[string]int            // digest or tag → reads left that miss it
	digests   map[string]string         // tag → digest
	tags      map[string]map[string]int // repository → tag → tag-list reads left that miss it
	repos     map[string]int            // repository → catalog reads left that miss it
}

func newLagRegistry(t *testing.T, lag int) *httptest.Server {
	g := &lagRegistry{lag: lag, blobs: map[string]int{}, manifests: map[string]int{}, digests: map[string]string{},
		tags: map[string]map[string]int{}, repos: map[string]int{}}
	srv := httptest.NewServer(g)
	t.Cleanup(srv.Close)
	return srv
}

// miss reports whether a read of key in m misses it, and counts the read.
func miss(m map[string]int, key string) (missing bool) {
	n, ok := m[key]
	if !ok {
		return true
	}
	if n > 0 {
		m[key] = n - 1
		return true
	}
	return false
}

func (g *lagRegistry) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	g.mu.Lock()
	defer g.mu.Unlock()
	path := r.URL.Path
	switch {
	case path == "/v2/":
		_, _ = io.WriteString(w, "{}")
	case path == "/v2/_catalog":
		_ = json.NewEncoder(w).Encode(map[string][]string{"repositories": seen(g.repos)})
	case strings.Contains(path, "/blobs/"):
		g.blob(w, r)
	case strings.Contains(path, "/manifests/"):
		g.manifest(w, r)
	case strings.HasSuffix(path, "/tags/list"):
		repo := strings.TrimSuffix(strings.TrimPrefix(path, "/v2/"), "/tags/list")
		_ = json.NewEncoder(w).Encode(map[string]any{"name": repo, "tags": seen(g.tags[repo])})
	default:
		http.NotFound(w, r)
	}
}

// seen returns the keys of m that a read does not miss, sorted, and counts the read.
func seen(m map[string]int) []string {
	keys := []string{}
	for k := range m {
		if !miss(m, k) {
			keys = append(keys, k)
		}
	}
	slices.Sort(keys)
	return keys
}

// blob answers an upload session (POST, then PUT) and a HEAD of a blob.
func (g *lagRegistry) blob(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path
	switch {
	case r.Method == http.MethodPost:
		w.Header().Set("Location", path+"session")
		w.WriteHeader(http.StatusAccepted)
	case r.Method == http.MethodPut:
		g.blobs[r.URL.Query().Get("digest")] = g.lag
		w.WriteHeader(http.StatusCreated)
	case miss(g.blobs, path[strings.LastIndex(path, "/")+1:]):
		w.WriteHeader(http.StatusNotFound)
	}
}

// manifest answers a PUT and a HEAD of a manifest.
func (g *lagRegistry) manifest(w http.ResponseWriter, r *http.Request) {
	repo, ref, _ := strings.Cut(strings.TrimPrefix(r.URL.Path, "/v2/"), "/manifests/")
	if r.Method == http.MethodPut {
		body, _ := io.ReadAll(r.Body)
		d := digest.FromBytes(body).String()
		g.manifests[d], g.manifests[ref], g.digests[ref] = g.lag, g.lag, d
		if g.tags[repo] == nil {
			g.tags[repo] = map[string]int{}
		}
		g.tags[repo][ref] = g.lag
		if _, ok := g.repos[repo]; !ok {
			g.repos[repo] = g.lag
		}
		w.WriteHeader(http.StatusCreated)
		return
	}
	if miss(g.manifests, ref) {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	d := ref
	if t, ok := g.digests[ref]; ok {
		d = t
	}
	w.Header().Set("Docker-Content-Digest", d)
}

// TestMeasureConsistency runs the consistency measurement against registries that see a write
// at once, after a few reads, and not within the limit.
func TestMeasureConsistency(t *testing.T) {
	defer func(l time.Duration) { lagLimit = l }(lagLimit)
	lagLimit = 150 * time.Millisecond
	tests := []struct {
		name      string
		lag       int
		cell      string
		overrides string
	}{
		{"strong", 0, "none: each read saw each of 3 writes at once (manifest by digest, manifest by tag, blob, tag list, catalog)", "none"},
		{"eventual", 2, "manifest by digest **after 100ms**", "`consistency: eventual`, `consistencyBound: 1s`"},
		{"past the limit", 1000, "manifest by digest **not within 150ms**", "`consistency: eventual`; a read did not see a write within 150ms"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := newLagRegistry(t, tt.lag)
			r, err := newRun(config.Registry{Name: "lag", URL: srv.URL}, Options{Scratch: "lajstrom-probe-lag", Log: quiet()})
			if err != nil {
				t.Fatal(err)
			}
			r.found.catalog, r.report.Capabilities.Catalog = true, true
			ctx := context.WithValue(context.Background(), tapeKey{}, r.tape)
			if err := r.measureConsistency(r.in(ctx, "consistency")); err != nil {
				t.Fatal(err)
			}
			if c := r.report.Cell("consistency"); !c.Measured || !strings.HasPrefix(c.Value, tt.cell) {
				t.Errorf("consistency = %q (measured %v), want %q…", c.Value, c.Measured, tt.cell)
			}
			if got := overridesVerdict(r.report.Capabilities, r.found); got != tt.overrides {
				t.Errorf("overrides = %q, want %q", got, tt.overrides)
			}
		})
	}
}

func TestLagVerdict(t *testing.T) {
	s := lagSeen{writes: 3,
		worst: map[string]time.Duration{readManifest: 1234 * time.Millisecond, readTag: 0, readTagList: 45 * time.Second, readBlob: 0},
		late:  map[string]bool{readManifest: true, readTagList: true}, unseen: map[string]bool{}}
	want := "manifest by digest **after 1.3s**, manifest by tag at once, blob at once, tag list **after 45s**; the worst of 3 writes"
	if got := lagVerdict(s); got != want {
		t.Errorf("lagVerdict = %q, want %q", got, want)
	}
}

func TestConsistencyOverrides(t *testing.T) {
	cases := []struct {
		name string
		caps Capabilities
		f    facts
		want []string
	}{
		{"no lag", Capabilities{}, facts{}, nil},
		{"a lag, unknown", Capabilities{}, facts{lag: 1500 * time.Millisecond, lagged: true}, []string{"`consistency: eventual`, `consistencyBound: 2s`"}},
		{"a lag, eventual, no bound", Capabilities{Consistency: "eventual"}, facts{lag: 3 * time.Second, lagged: true}, []string{"`consistencyBound: 4s`"}},
		{"a lag within the bound", Capabilities{Consistency: "eventual", ConsistencyBound: "5s"}, facts{lag: 3 * time.Second, lagged: true}, nil},
	}
	for _, c := range cases {
		if got := consistencyOverrides(c.caps, c.f); !slices.Equal(got, c.want) {
			t.Errorf("%s: consistencyOverrides = %q, want %q", c.name, got, c.want)
		}
	}
}
