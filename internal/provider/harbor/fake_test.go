package harbor

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/lajstrom/lajstrom/internal/config"
)

// The robot account of the fake, and its secret.
const (
	robotName   = "robot$lajstrom+ci"
	robotSecret = "s3cr3t-of-the-robot"
)

// fakeHarbor is the Harbor of the unit tests: /v2 as far as the generic provider's probe asks
// (GET /v2/, the referrers of the probe repository), and the REST API that the provider uses,
// answered as Harbor v2.15.4 answers them (docs/registries/harbor.md). Every request must carry
// the robot's Basic credentials. The knobs say what the robot may do; fail answers the next
// requests of a route with a status. Every request is counted by its route.
type fakeHarbor struct {
	t   *testing.T
	srv *httptest.Server

	mu       sync.Mutex
	calls    map[string]int
	uris     []string            // the request URIs of the REST API, in order
	projects map[string]bool     // the projects that exist
	repos    map[string][]string // project → its repositories, with the project
	rules    map[string][]rule   // project → its immutability rules
	fail     map[string][]int    // route → statuses of its next answers

	mayCreate    bool // a system robot with project:create
	mayList      bool // repository:list
	mayReadRules bool // immutable-tag:list
}

func newFakeHarbor(t *testing.T) *fakeHarbor {
	f := &fakeHarbor{
		t:        t,
		calls:    map[string]int{},
		projects: map[string]bool{"lajstrom": true},
		repos:    map[string][]string{},
		rules:    map[string][]rule{},
		fail:     map[string][]int{},
		mayList:  true, mayReadRules: true,
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v2/{$}", func(w http.ResponseWriter, _ *http.Request) { writeJSON(w, http.StatusOK, map[string]any{}) })
	mux.HandleFunc("GET /v2/", f.v2)
	mux.HandleFunc("HEAD /api/v2.0/projects", f.headProject)
	mux.HandleFunc("POST /api/v2.0/projects", f.createProject)
	mux.HandleFunc("GET /api/v2.0/projects/{project}/repositories", f.listRepositories)
	mux.HandleFunc("GET /api/v2.0/projects/{project}/immutabletagrules", f.listRules)
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if u, p, ok := r.BasicAuth(); !ok || u != robotName || p != robotSecret {
			// Harbor challenges /v2 with its token service; Basic, which it takes as well, keeps
			// the fake small. oras-go sends the credentials only once challenged.
			w.Header().Set("WWW-Authenticate", `Basic realm="harbor"`)
			writeErr(w, http.StatusUnauthorized, "UNAUTHORIZED", "unauthorized")
			return
		}
		route := r.Method + " " + r.URL.Path
		if _, pattern := mux.Handler(r); pattern != "" {
			route = pattern
		}
		f.mu.Lock()
		f.calls[route]++
		if strings.HasPrefix(r.URL.Path, apiPath) {
			f.uris = append(f.uris, r.RequestURI)
		}
		var status int
		if s := f.fail[route]; len(s) > 0 {
			status, f.fail[route] = s[0], s[1:]
		}
		f.mu.Unlock()
		if status != 0 {
			writeErr(w, status, "UNKNOWN", "injected for "+route)
			return
		}
		mux.ServeHTTP(w, r)
	}))
	t.Cleanup(f.srv.Close)
	return f
}

// set changes the fake under its lock, as its handlers read it.
func (f *fakeHarbor) set(change func(f *fakeHarbor)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	change(f)
}

// has reports whether the project exists.
func (f *fakeHarbor) has(project string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.projects[project]
}

// count returns the requests of route, a pattern of the mux.
func (f *fakeHarbor) count(route string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[route]
}

// config is a registry of type harbor on the fake, below prefix.
func (f *fakeHarbor) config(prefix string) config.Registry {
	return config.Registry{Name: "harbor", Type: Type, URL: f.srv.URL, Prefix: prefix,
		Auth: config.RegistryAuth{Username: robotName, Password: robotSecret}}
}

// provider is the provider of cfg on the clock, its retries without a wait.
func (f *fakeHarbor) provider(cfg config.Registry, now func() time.Time) *Provider {
	f.t.Helper()
	p, err := newProvider(cfg, slog.New(slog.DiscardHandler), now)
	if err != nil {
		f.t.Fatalf("newProvider: %v", err)
	}
	p.api.wait = func(int) time.Duration { return 0 }
	return p
}

// v2 answers the referrers of the generic provider's probe with an empty index; everything else
// is 404.
func (f *fakeHarbor) v2(w http.ResponseWriter, r *http.Request) {
	if strings.Contains(r.URL.Path, "/lajstrom-probe/referrers/") {
		w.Header().Set("Content-Type", "application/vnd.oci.image.index.v1+json")
		writeJSON(w, http.StatusOK, map[string]any{"schemaVersion": 2, "manifests": []any{}})
		return
	}
	writeErr(w, http.StatusNotFound, "NOT_FOUND", "not found")
}

func (f *fakeHarbor) headProject(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	ok := f.projects[r.URL.Query().Get("project_name")]
	f.mu.Unlock()
	if !ok {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	w.WriteHeader(http.StatusOK)
}

func (f *fakeHarbor) createProject(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ProjectName string            `json:"project_name"`
		Metadata    map[string]string `json:"metadata"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.ProjectName == "" || r.Header.Get("Content-Type") != "application/json" {
		writeErr(w, http.StatusBadRequest, "BAD_REQUEST", "bad request")
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	switch {
	case !f.mayCreate:
		writeErr(w, http.StatusForbidden, "FORBIDDEN", "Only system level robot can create project")
	case f.projects[req.ProjectName]:
		writeErr(w, http.StatusConflict, "CONFLICT", "The project named "+req.ProjectName+" already exists")
	case req.Metadata["public"] != "false":
		writeErr(w, http.StatusBadRequest, "BAD_REQUEST", "the fake wants a private project")
	default:
		f.projects[req.ProjectName] = true
		w.Header().Set("Location", "/api/v2.0/projects/7")
		w.WriteHeader(http.StatusCreated)
	}
}

// listRepositories pages as Harbor does: page and page_size (at most 100), q=name=~<part>, sort=name,
// X-Total-Count and a Link to the next and previous pages. A project that does not exist is 403,
// as Harbor answers it.
func (f *fakeHarbor) listRepositories(w http.ResponseWriter, r *http.Request) {
	project := r.PathValue("project")
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.mayList || !f.projects[project] {
		writeErr(w, http.StatusForbidden, "FORBIDDEN", "forbidden")
		return
	}
	q := r.URL.Query()
	page, size := atoiOr(q.Get("page"), 1), atoiOr(q.Get("page_size"), 10)
	if size > 100 {
		writeErr(w, http.StatusUnprocessableEntity, "UNPROCESSABLE_ENTITY", "page_size in query should be less than or equal to 100")
		return
	}
	var names []string
	part, _ := strings.CutPrefix(q.Get("q"), "name=~")
	for _, n := range f.repos[project] {
		if strings.Contains(n, part) {
			names = append(names, n)
		}
	}
	if q.Get("sort") == "name" {
		slices.Sort(names)
	}
	w.Header().Set("X-Total-Count", strconv.Itoa(len(names)))
	var links []string
	link := func(n int, rel string) string {
		v := url.Values{}
		for k := range q {
			v.Set(k, q.Get(k))
		}
		v.Set("page", strconv.Itoa(n))
		return fmt.Sprintf(`<%s?%s>; rel="%s"`, r.URL.Path, v.Encode(), rel)
	}
	if page > 1 {
		links = append(links, link(page-1, "prev"))
	}
	if page*size < len(names) {
		links = append(links, link(page+1, "next"))
	}
	if len(links) > 0 {
		w.Header().Set("Link", strings.Join(links, " , "))
	}
	out := []map[string]any{}
	for i := (page - 1) * size; i < len(names) && i < page*size; i++ {
		out = append(out, map[string]any{"name": names[i], "artifact_count": 1})
	}
	writeJSON(w, http.StatusOK, out)
}

func (f *fakeHarbor) listRules(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.mayReadRules {
		writeErr(w, http.StatusForbidden, "FORBIDDEN", "forbidden")
		return
	}
	rules := f.rules[r.PathValue("project")]
	if rules == nil {
		rules = []rule{}
	}
	writeJSON(w, http.StatusOK, rules)
}

func atoiOr(s string, d int) int {
	if n, err := strconv.Atoi(s); err == nil && n > 0 {
		return n
	}
	return d
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	if w.Header().Get("Content-Type") == "" {
		w.Header().Set("Content-Type", "application/json")
	}
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, map[string]any{"errors": []map[string]string{{"code": code, "message": msg}}})
}
