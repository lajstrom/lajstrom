package probe

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"

	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/lajstrom/lajstrom/internal/config"
)

// apiRepos is a registry, as ECR without a creation template, that takes writes only into the
// repositories its API created: an upload session or a manifest elsewhere is 404 NAME_UNKNOWN.
// Its API refuses a name longer than maxName, and records what it created and deleted.
type apiRepos struct {
	maxName int

	mu      sync.Mutex
	repos   map[string]bool
	created []string
	deleted []string
}

func (a *apiRepos) EnsureRepository(_ context.Context, name string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(name) > a.maxName {
		return errors.New("InvalidParameterException: repositoryName too long")
	}
	if !a.repos[name] {
		a.repos[name] = true
		a.created = append(a.created, name)
	}
	return nil
}

func (a *apiRepos) DeleteRepository(_ context.Context, name string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if !a.repos[name] {
		return errors.New("RepositoryNotFoundException")
	}
	delete(a.repos, name)
	a.deleted = append(a.deleted, name)
	return nil
}

func (a *apiRepos) has(name string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.repos[name]
}

// ServeHTTP is the /v2 API of the registry.
func (a *apiRepos) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	p := strings.TrimPrefix(r.URL.Path, "/v2/")
	name, rest := p, ""
	for _, sep := range []string{"/blobs/", "/manifests/", "/tags/"} {
		if i := strings.LastIndex(p, sep); i >= 0 {
			name, rest = p[:i], p[i+1:]
			break
		}
	}
	unknown := func() {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, `{"errors":[{"code":"NAME_UNKNOWN","message":"repository does not exist"}]}`)
	}
	switch {
	case r.URL.Path == "/v2/":
		_, _ = io.WriteString(w, "{}")
	case r.URL.Path == "/v2/_catalog":
		_, _ = io.WriteString(w, `{"repositories":[]}`)
	case !a.has(name):
		unknown()
	case r.Method == http.MethodPost && rest == "blobs/uploads/":
		w.Header().Set("Location", "/v2/"+name+"/blobs/uploads/1")
		w.WriteHeader(http.StatusAccepted)
	case r.Method == http.MethodPut && strings.HasPrefix(rest, "blobs/uploads/"),
		r.Method == http.MethodPut && strings.HasPrefix(rest, "manifests/"):
		_, _ = io.Copy(io.Discard, r.Body)
		w.WriteHeader(http.StatusCreated)
	case r.Method == http.MethodDelete:
		w.WriteHeader(http.StatusAccepted)
	case rest == "tags/list":
		_, _ = io.WriteString(w, `{"tags":["v1"]}`)
	default:
		unknown()
	}
}

// TestCreateThroughTheAPI: on a registry that creates no repository on push and has an API for
// them (ECR), "Create on push" says no, measured before anything is created; then the run
// creates each repository with the API before its first write, a name the API refuses is
// refused in "Where", and the cleanup deletes every repository the run created (#19).
func TestCreateThroughTheAPI(t *testing.T) {
	api := &apiRepos{maxName: 255, repos: map[string]bool{}}
	srv := httptest.NewServer(api)
	t.Cleanup(srv.Close)
	r, err := newRun(config.Registry{Name: "ecr", URL: srv.URL}, Options{Scratch: "s", Log: quiet()})
	if err != nil {
		t.Fatal(err)
	}
	r.backend.repos = api
	ctx := context.Background()
	if err := r.measureCreate(ctx); err != nil {
		t.Fatalf("measureCreate: %v", err)
	}
	if v := r.report.Cell("createOnPush").Value; v != "no: 404 `NAME_UNKNOWN` to the first upload session; the run creates its repositories with the registry's API" {
		t.Errorf("createOnPush = %q", v)
	}
	where := r.report.Cell("where").Value
	if !strings.Contains(where, "at any depth") || !strings.Contains(where, "256: refused by the registry's API") {
		t.Errorf("where = %q", where)
	}
	if slices.Contains(api.created, "s/create") || !slices.Contains(api.created, "s/create/a/b/c/d/e/f/g") || len(api.created) != 2 {
		t.Errorf("created %q: want the deep repository and the name of 255 characters, not the one create on push was measured in", api.created)
	}

	r.cleanup(ctx)
	reversed := slices.Clone(api.created)
	slices.Reverse(reversed)
	if !slices.Equal(api.deleted, reversed) {
		t.Errorf("deleted %q, want %q in reverse", api.deleted, api.created)
	}
	if len(r.report.LeftBehind) > 0 {
		t.Errorf("left behind %q", r.report.LeftBehind)
	}
	if v := r.report.Cell("deleteRepository").Value; !strings.HasSuffix(v, "the registry's API deleted 2 of the 2 repositories the run created with it") {
		t.Errorf("deleteRepository = %q", v)
	}
}

// TestCreateThroughTheAPIOnlyWhereNeeded: a registry that creates repositories on push is never
// asked to create one through its API.
func TestCreateThroughTheAPIOnlyWhereNeeded(t *testing.T) {
	api := &apiRepos{maxName: 1000, repos: map[string]bool{}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		_ = api.EnsureRepository(req.Context(), strings.SplitN(strings.TrimPrefix(req.URL.Path, "/v2/"), "/blobs/", 2)[0])
		api.ServeHTTP(w, req)
	}))
	t.Cleanup(srv.Close)
	r, err := newRun(config.Registry{Name: "zot", URL: srv.URL}, Options{Scratch: "s", Log: quiet()})
	if err != nil {
		t.Fatal(err)
	}
	r.backend.repos = &apiRepos{maxName: 1000, repos: map[string]bool{}}
	if err := r.measureCreate(context.Background()); err != nil {
		t.Fatal(err)
	}
	if r.viaAPI || len(r.made) > 0 {
		t.Errorf("viaAPI %t, made %q on a registry that creates on push", r.viaAPI, r.made)
	}
}

// TestManifestsECRTakes: every manifest the probe builds has an artifactType, as the image
// specification requires of a manifest whose config is the empty blob, and at least one layer.
// ECR refuses either otherwise: 405 UNSUPPORTED "Invalid JSON syntax" and "Image has to have at
// least 1 layer", measured on 2026-11-13 (#19).
func TestManifestsECRTakes(t *testing.T) {
	r := &run{id: "t"}
	for name, m := range map[string]manifest{
		"plain":        r.newManifest("plain", "", nil),
		"typed":        r.newManifest("typed", "application/vnd.example.sig", nil),
		"with a layer": r.layerManifest("layer", emptyDesc),
		"padded":       r.paddedManifest(4096),
	} {
		var got ocispec.Manifest
		if err := json.Unmarshal(m.body, &got); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if got.Config.MediaType == ocispec.MediaTypeEmptyJSON && got.ArtifactType == "" {
			t.Errorf("%s: the empty config and no artifactType", name)
		}
		if len(got.Layers) == 0 {
			t.Errorf("%s: no layer", name)
		}
	}
}
