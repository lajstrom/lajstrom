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
// After the delete of a manifest by digest, HEAD and GET by digest and by each of its tags still
// answer 200 and the tag list still names the tags, gone times each; refuseDelete answers it 405
// UNSUPPORTED. With keepTags, it leaves the tags, as Nexus's docker format does: they serve the
// manifest, and its digest answers 404 to the next relapse reads and then 200 again. With cache,
// a tag list read is kept and answered to the next cache reads, as Quay keeps its tag lists.
type lagRegistry struct {
	lag, gone, cache, relapse int
	refuseDelete, keepTags    bool

	mu        sync.Mutex
	blobs     map[string]int            // digest → reads left that miss it
	manifests map[string]int            // digest or tag → reads left that miss it
	digests   map[string]string         // tag → digest
	tags      map[string]map[string]int // repository → tag → tag-list reads left that miss it
	repos     map[string]int            // repository → catalog reads left that miss it
	deleted   map[string]int            // digest or tag deleted → HEADs left that still find it
	listed    map[string]map[string]int // repository → tag deleted → tag-list reads left that still name it
	cached    map[string]cachedList     // repository → the tag list kept
	events    []string                  // "tags", "catalog" and "put", in the order of the requests
	deletes   map[string]int            // repository → manifest DELETEs
}

// cachedList is a tag list a lagRegistry keeps, and how many reads it answers still.
type cachedList struct {
	tags []string
	left int
}

func newLagRegistry(t *testing.T, lag int) *httptest.Server {
	return serveLag(t, &lagRegistry{lag: lag})
}

// serveLag serves g, its maps made.
func serveLag(t *testing.T, g *lagRegistry) *httptest.Server {
	g.blobs, g.manifests, g.digests, g.tags, g.repos = map[string]int{}, map[string]int{}, map[string]string{}, map[string]map[string]int{}, map[string]int{}
	g.deleted, g.listed, g.cached, g.deletes = map[string]int{}, map[string]map[string]int{}, map[string]cachedList{}, map[string]int{}
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
		g.events = append(g.events, "catalog")
		_ = json.NewEncoder(w).Encode(map[string][]string{"repositories": seen(g.repos)})
	case strings.Contains(path, "/blobs/"):
		g.blob(w, r)
	case strings.Contains(path, "/manifests/"):
		g.manifest(w, r)
	case strings.HasSuffix(path, "/tags/list"):
		repo := strings.TrimSuffix(strings.TrimPrefix(path, "/v2/"), "/tags/list")
		_ = json.NewEncoder(w).Encode(map[string]any{"name": repo, "tags": g.tagList(repo)})
	default:
		http.NotFound(w, r)
	}
}

// tagList answers a tag list of repo: the one kept while it answers reads, otherwise the tags a
// read does not miss and the tags deleted that it still names.
func (g *lagRegistry) tagList(repo string) []string {
	g.events = append(g.events, "tags")
	if c := g.cached[repo]; c.left > 0 {
		c.left--
		g.cached[repo] = c
		return c.tags
	}
	tags := seen(g.tags[repo])
	for tag, n := range g.listed[repo] {
		if n > 0 {
			g.listed[repo][tag] = n - 1
			tags = append(tags, tag)
		}
	}
	slices.Sort(tags)
	if g.cache > 0 {
		g.cached[repo] = cachedList{tags: tags, left: g.cache}
	}
	return tags
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

// manifest answers a PUT, a DELETE, and a HEAD or a GET (without a body) of a manifest.
func (g *lagRegistry) manifest(w http.ResponseWriter, r *http.Request) {
	repo, ref, _ := strings.Cut(strings.TrimPrefix(r.URL.Path, "/v2/"), "/manifests/")
	if r.Method == http.MethodDelete {
		g.deletes[repo]++
		g.delete(w, repo, ref)
		return
	}
	if n := g.deleted[ref]; n > 0 && r.Method != http.MethodPut {
		g.deleted[ref] = n - 1
		w.Header().Set("Docker-Content-Digest", ref)
		return
	}
	if r.Method == http.MethodPut {
		g.events = append(g.events, "put")
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

// delete answers a DELETE of the manifest d of repo, and its tags with it.
func (g *lagRegistry) delete(w http.ResponseWriter, repo, d string) {
	if g.refuseDelete {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusMethodNotAllowed)
		_, _ = io.WriteString(w, `{"errors":[{"code":"UNSUPPORTED"}]}`)
		return
	}
	if g.keepTags && strings.HasPrefix(d, "sha256:") {
		g.manifests[d] = g.relapse // the reads that miss it before it is back
		w.WriteHeader(http.StatusAccepted)
		return
	}
	delete(g.manifests, d)
	g.deleted[d] = g.gone
	for tag, td := range g.digests {
		if td != d {
			continue
		}
		delete(g.manifests, tag)
		delete(g.digests, tag)
		delete(g.tags[repo], tag)
		g.deleted[tag] = g.gone
		if g.listed[repo] == nil {
			g.listed[repo] = map[string]int{}
		}
		g.listed[repo][tag] = g.gone
	}
	w.WriteHeader(http.StatusAccepted)
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
		{"strong", 0, "none: each read saw each of 3 writes at once (manifest by digest, manifest by tag, blob, tag list, catalog); " +
			"each read saw each of 3 deletes at once (manifest by digest, manifest by tag, tag list)", "none"},
		{"eventual", 2, "manifest by digest **after 100ms**", "`consistency: eventual`, `consistencyBound: 1s`"},
		{"past the limit", 1000, "manifest by digest **not within 150ms**", "`consistency: eventual`; a read did not see a write or a delete within 150ms"},
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

// TestMeasureListCache: a registry that keeps a tag list for the next reads, as Quay does,
// shows its lag only to a read after one before the write, which the measurement makes.
func TestMeasureListCache(t *testing.T) {
	defer func(l time.Duration) { lagLimit = l }(lagLimit)
	lagLimit = 150 * time.Millisecond
	g := &lagRegistry{cache: 2}
	r := lagRun(t, g)
	g.mu.Lock()
	events := strings.Join(g.events, " ")
	g.mu.Unlock()
	for i, before := range strings.Split(events, "put")[:lagWrites] {
		if !strings.HasSuffix(before, "tags catalog ") {
			t.Errorf("before the push of manifest %d: %q, want a read of the tag list and of the catalog", i+1, before)
		}
	}
	if c := r.report.Cell("consistency"); !strings.Contains(c.Value, "tag list **after") {
		t.Errorf("consistency = %q, want the tag list late", c.Value)
	}
	if got := overridesVerdict(r.report.Capabilities, r.found); got != "`consistency: eventual`, `consistencyBound: 1s`" {
		t.Errorf("overrides = %q", got)
	}
}

// TestMeasureDeleteLag: the measurement deletes each manifest it pushed and reads it until it
// is gone; a registry that serves a manifest deleted for a while is eventual, one that refuses
// the delete has its deletes not measured, and one that serves it past the limit ends them.
func TestMeasureDeleteLag(t *testing.T) {
	defer func(l time.Duration) { lagLimit = l }(lagLimit)
	lagLimit = 150 * time.Millisecond
	tests := []struct {
		name      string
		g         *lagRegistry
		cell      string
		overrides string
	}{
		{"a lag", &lagRegistry{gone: 2}, "none: each read saw each of 3 writes at once (manifest by digest, manifest by tag, blob, tag list, catalog); " +
			"after a delete: manifest by digest **after 100ms**, manifest by tag **after 100ms**, tag list **after 100ms**; the worst of 3 deletes",
			"`consistency: eventual`, `consistencyBound: 1s`"},
		{"past the limit", &lagRegistry{gone: 1000}, "after a delete: manifest by digest **not within 150ms**, manifest by tag **not within " +
			"150ms**, tag list **not within 150ms**; the worst of 1 delete", "`consistency: eventual`; a read did not see a write or a delete within 150ms"},
		{"refused", &lagRegistry{refuseDelete: true}, "; deletes not measured: the registry refused a delete by digest (405 `UNSUPPORTED`)", "none"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := lagRun(t, tt.g)
			if c := r.report.Cell("consistency"); !strings.Contains(c.Value, tt.cell) {
				t.Errorf("consistency = %q, want …%q…", c.Value, tt.cell)
			}
			if got := overridesVerdict(r.report.Capabilities, r.found); got != tt.overrides {
				t.Errorf("overrides = %q, want %q", got, tt.overrides)
			}
		})
	}
}

// TestDeletesThatLeaveTheTags: where a delete by digest leaves the tags and the digest answers
// 200 again after it (Nexus's docker format, #1149), the reads after the deletes are not judged:
// "Overrides needed" holds the delete's override only, and "Monotonic reads" makes no deletes,
// which would wait out lagLimit each. A registry that lags after a real delete is still eventual,
// though the read by tag right after the delete of "Delete a manifest by digest" met the lag.
func TestDeletesThatLeaveTheTags(t *testing.T) {
	defer func(l time.Duration) { lagLimit = l }(lagLimit)
	lagLimit = 150 * time.Millisecond
	const unjudged = `a delete by digest leaves the tags ("Delete a manifest by digest")`
	tests := []struct {
		name        string
		g           *lagRegistry
		consistency []string
		monotonic   string
		monoDeletes int
		overrides   string
	}{
		{"the tags stay", &lagRegistry{keepTags: true, relapse: 1}, []string{
			"none: each read saw each of 3 writes at once (manifest by digest, manifest by tag, blob, tag list, catalog); ",
			"after a delete: manifest by digest at once, manifest by tag **not within 150ms**, tag list **not within 150ms**; " +
				"the worst of 1 delete; deletes not judged: " + unjudged,
		}, "none: no read saw a change undone; 3 writes, a tag move, each read 5 times more over new connections after the first " +
			"that saw it, from 1 address; deletes not measured: " + unjudged, 0,
			"`deleteManifest: false`, or a delete leaves the tags"},
		{"a lag after a real delete", &lagRegistry{gone: 2}, []string{
			"none: each read saw each of 3 writes at once",
			"after a delete: manifest by digest **after ",
			"; the worst of 3 deletes",
		}, "none: no read saw a change undone; 3 writes, a tag move, 3 deletes, each read 5 times more", 3,
			"`deleteManifest: false`, or a delete leaves the tags; `consistency: eventual`, `consistencyBound: 1s`"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := deleteRun(t, tt.g, "consistency", "deleteDigest", "monotonic")
			c := r.report.Cell("consistency").Value
			for _, want := range tt.consistency {
				if !strings.Contains(c, want) {
					t.Errorf("consistency = %q, want …%q…", c, want)
				}
			}
			if judged := tt.monoDeletes > 0; judged == strings.Contains(c, "not judged") {
				t.Errorf("consistency = %q, want its deletes judged %v", c, judged)
			}
			if v := r.report.Cell("deleteDigest").Value; !strings.HasPrefix(v, "yes, 202, **but its tags stay and serve it**") {
				t.Errorf("deleteDigest = %q, want the tags to stay", v)
			}
			if v := r.report.Cell("monotonic").Value; !strings.HasPrefix(v, tt.monotonic) {
				t.Errorf("monotonic = %q, want %q…", v, tt.monotonic)
			}
			tt.g.mu.Lock()
			if n := tt.g.deletes["lajstrom-probe-lag/mono"]; n != tt.monoDeletes {
				t.Errorf("%d deletes in mono, want %d", n, tt.monoDeletes)
			}
			tt.g.mu.Unlock()
			if got := r.report.Cell("overrides").Value; got != tt.overrides {
				t.Errorf("overrides = %q, want %q", got, tt.overrides)
			}
		})
	}
	// Measured before the deletes, as before #1149, "Monotonic reads" deletes and sees the digest
	// come back: still not judged.
	r := deleteRun(t, &lagRegistry{keepTags: true, relapse: 1}, "monotonic", "deleteDigest")
	if v := r.report.Cell("monotonic").Value; !strings.Contains(v, "delete 1, manifest by digest: gone, then 200 (5 of 5)") {
		t.Errorf("monotonic = %q, want the digest back after its delete", v)
	}
	if got := r.report.Cell("overrides").Value; got != "`deleteManifest: false`, or a delete leaves the tags" {
		t.Errorf("overrides = %q, want the delete's override only", got)
	}
}

// deleteRun runs against g, with a catalog, the measurements whose first cells are keys, in that
// order, and then judges them as a run does.
func deleteRun(t *testing.T, g *lagRegistry, keys ...string) *run {
	t.Helper()
	srv := serveLag(t, g)
	r, err := newRun(config.Registry{Name: "lag", URL: srv.URL}, Options{Scratch: "lajstrom-probe-lag", MonotonicReads: 5, Log: quiet()})
	if err != nil {
		t.Fatal(err)
	}
	r.found.catalog, r.report.Capabilities.Catalog = true, true
	ctx := context.WithValue(context.Background(), tapeKey{}, r.tape)
	groups := map[string]group{}
	for _, gr := range r.groups() {
		groups[gr.keys[0]] = gr
	}
	for _, k := range keys {
		r.measure(ctx, groups[k])
		if c := r.report.Cell(k); !c.Measured {
			t.Fatalf("%s = %q, not measured", k, c.Value)
		}
	}
	r.judge()
	return r
}

// TestGroupsDeleteBeforeMonotonicReads: a run measures the deletes before "Monotonic reads" and
// after "Read-after-write lag", whose wait tells a tag that stays from one that lags.
func TestGroupsDeleteBeforeMonotonicReads(t *testing.T) {
	var order []string
	for _, g := range (&run{}).groups() {
		switch g.keys[0] {
		case "consistency", "deleteDigest", "monotonic":
			order = append(order, g.keys[0])
		}
	}
	if want := []string{"consistency", "deleteDigest", "monotonic"}; !slices.Equal(order, want) {
		t.Errorf("order = %q, want %q", order, want)
	}
}

// lagRun runs the consistency measurement against g, with a catalog.
func lagRun(t *testing.T, g *lagRegistry) *run {
	t.Helper()
	srv := serveLag(t, g)
	r, err := newRun(config.Registry{Name: "lag", URL: srv.URL}, Options{Scratch: "lajstrom-probe-lag", Log: quiet()})
	if err != nil {
		t.Fatal(err)
	}
	r.found.catalog, r.report.Capabilities.Catalog = true, true
	ctx := context.WithValue(context.Background(), tapeKey{}, r.tape)
	if err := r.measureConsistency(r.in(ctx, "consistency")); err != nil {
		t.Fatal(err)
	}
	return r
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
		{"a delete lags longer", Capabilities{}, facts{lag: time.Second, lagged: true, deleteLag: 4500 * time.Millisecond, deleteLagged: true},
			[]string{"`consistency: eventual`, `consistencyBound: 5s`"}},
		{"a delete lags, the tags stay", Capabilities{}, facts{tagsStay: true, deleteLag: lagLimit, deleteLagged: true, deleteLagUnbounded: true}, nil},
		{"a delete lags, refused elsewhere", Capabilities{}, facts{lag: time.Second, lagged: true, deleteRefused: true,
			deleteLag: lagLimit, deleteLagged: true, deleteLagUnbounded: true}, []string{"`consistency: eventual`, `consistencyBound: 2s`"}},
	}
	for _, c := range cases {
		if got := consistencyOverrides(c.caps, c.f); !slices.Equal(got, c.want) {
			t.Errorf("%s: consistencyOverrides = %q, want %q", c.name, got, c.want)
		}
	}
}
