//go:build integration

package harbor

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/lajstrom/lajstrom/internal/config"
	"github.com/lajstrom/lajstrom/internal/provider"
)

// harborEnv is the Harbor of the integration tests: LAJSTROM_TEST_HARBOR, its base URL
// (http://localhost:5005 after deploy/compose/harbor/harbor.sh up); LAJSTROM_TEST_HARBOR_PROJECT,
// a project; LAJSTROM_TEST_HARBOR_USERNAME and LAJSTROM_TEST_HARBOR_PASSWORD, a robot account of
// that project with the permissions docs/registries/harbor.md names. The test skips without
// them.
func harborEnv(t *testing.T) (base, project string, robot config.RegistryAuth) {
	t.Helper()
	base, project = os.Getenv("LAJSTROM_TEST_HARBOR"), os.Getenv("LAJSTROM_TEST_HARBOR_PROJECT")
	robot = config.RegistryAuth{Username: os.Getenv("LAJSTROM_TEST_HARBOR_USERNAME"), Password: os.Getenv("LAJSTROM_TEST_HARBOR_PASSWORD")}
	if base == "" || project == "" || robot.Username == "" || robot.Password == "" {
		t.Skip("set LAJSTROM_TEST_HARBOR, LAJSTROM_TEST_HARBOR_PROJECT, LAJSTROM_TEST_HARBOR_USERNAME and " +
			"LAJSTROM_TEST_HARBOR_PASSWORD (docs/registries/harbor.md, Type harbor)")
	}
	return base, project, robot
}

// runID is a name of the run's own.
func runID() string {
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	return "it-" + hex.EncodeToString(b)
}

// TestIntegrationHarbor runs the provider as the project robot, below a prefix of its own in the
// project, <project>/it-<random>/: the probe, the project check, the listing of the REST API
// across pages, and the repository delete. It deletes what it pushed, also when it fails.
func TestIntegrationHarbor(t *testing.T) {
	base, project, robot := harborEnv(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	prefix := project + "/" + runID() + "/"
	cfg := config.Registry{Name: "harbor-it", Type: Type, URL: base, Prefix: prefix, Auth: robot}
	np, err := provider.New(cfg, slog.Default())
	if err != nil {
		t.Fatal(err)
	}
	p := np.(*Provider)
	t.Cleanup(func() { deleteBelow(t, p) })

	caps, err := p.Capabilities(ctx)
	if err != nil {
		t.Fatalf("Capabilities: %v", err)
	}
	t.Logf("capabilities: %+v", caps)
	if !caps.CreateOnPush || !caps.Referrers || !caps.Catalog || !caps.DeleteManifest || caps.MaxNameLength != maxNameLen-len(prefix) {
		t.Errorf("Capabilities = %+v", caps)
	}

	names := []string{"raw/scratch/a", "raw/scratch/b/c", "raw/scratch/d", "pypi/internal/requests"}
	for _, n := range names {
		if err := p.EnsureRepository(ctx, n); err != nil {
			t.Fatalf("EnsureRepository(%s): %v", n, err)
		}
		push(ctx, t, p, n, "v1", []byte("hello "+n))
	}

	p.pageSize = 2 // three pages of the project's repositories below the prefix, filtered
	got, err := p.ListRepositories(ctx, "raw/")
	if want := []string{"raw/scratch/a", "raw/scratch/b/c", "raw/scratch/d"}; err != nil || !slices.Equal(got, want) {
		t.Errorf("ListRepositories(raw/) = %q, %v; want %q", got, err, want)
	}
	got, err = p.ListRepositories(ctx, "")
	if err != nil || len(got) != len(names) {
		t.Errorf("ListRepositories() = %q, %v", got, err)
	}

	if err := p.DeleteRepository(ctx, "raw/scratch/a"); err != nil {
		t.Fatalf("DeleteRepository: %v", err)
	}
	got, err = p.ListRepositories(ctx, "raw/")
	if want := []string{"raw/scratch/b/c", "raw/scratch/d"}; err != nil || !slices.Equal(got, want) {
		t.Errorf("ListRepositories after the delete = %q, %v; want %q", got, err, want)
	}
	if err := p.DeleteRepository(ctx, "raw/scratch/a"); !errors.Is(err, provider.ErrNotFound) {
		t.Errorf("DeleteRepository of a deleted repository = %v, want ErrNotFound", err)
	}

	// A version's delete stays /v2's: by digest, its tag with it.
	repo, err := p.Repository(ctx, "raw/scratch/d")
	if err != nil {
		t.Fatal(err)
	}
	d, err := repo.Resolve(ctx, "v1")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if err := repo.Delete(ctx, d); err != nil {
		t.Errorf("Delete of a manifest: %v", err)
	}
	if _, err := repo.Resolve(ctx, "v1"); !errors.Is(err, provider.ErrNotFound) {
		t.Errorf("Resolve after the delete = %v, want ErrNotFound", err)
	}

	// A project that does not exist: a refusal with the hint, without the project's name.
	missing := cfg
	missing.Prefix = "nosuch-" + runID() + "/"
	mp, err := New(missing, slog.Default())
	if err != nil {
		t.Fatal(err)
	}
	var rej *provider.RejectedError
	if err := mp.EnsureRepository(ctx, "raw/scratch/a"); !errors.As(err, &rej) || rej.Status != http.StatusNotFound ||
		strings.Contains(err.Error(), "nosuch") {
		t.Errorf("EnsureRepository in a project that does not exist = %v", err)
	}
}

// TestIntegrationHarborAdmin needs an administrator besides, LAJSTROM_TEST_HARBOR_ADMIN_USERNAME
// and LAJSTROM_TEST_HARBOR_ADMIN_PASSWORD, to make projects of the run's own: one the provider
// creates (createProject), one with an immutability rule for every tag, one with a storage quota
// of 1 MiB. It deletes them, also when it fails.
func TestIntegrationHarborAdmin(t *testing.T) {
	base, _, _ := harborEnv(t)
	admin := config.RegistryAuth{Username: os.Getenv("LAJSTROM_TEST_HARBOR_ADMIN_USERNAME"), Password: os.Getenv("LAJSTROM_TEST_HARBOR_ADMIN_PASSWORD")}
	if admin.Username == "" || admin.Password == "" {
		t.Skip("set LAJSTROM_TEST_HARBOR_ADMIN_USERNAME and LAJSTROM_TEST_HARBOR_ADMIN_PASSWORD")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	run := runID()
	api := &adminAPI{t: t, base: base, auth: admin}
	newProvider := func(project string, create bool) *Provider {
		cfg := config.Registry{Name: "harbor-it", Type: Type, URL: base, Prefix: project + "/", Auth: admin,
			Harbor: config.Harbor{CreateProject: create}}
		p, err := New(cfg, slog.Default())
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { deleteBelow(t, p); api.do(http.MethodDelete, "/projects/"+project, nil) })
		return p
	}

	t.Run("createProject", func(t *testing.T) {
		p := newProvider(run+"-created", true)
		if err := p.EnsureRepository(ctx, "raw/scratch/a"); err != nil {
			t.Fatalf("EnsureRepository: %v", err)
		}
		if s := api.do(http.MethodHead, "/projects?project_name="+run+"-created", nil); s != http.StatusOK {
			t.Errorf("HEAD of the created project: %d", s)
		}
		push(ctx, t, p, "raw/scratch/a", "v1", []byte("created"))
	})

	t.Run("immutability rule", func(t *testing.T) {
		project := run + "-imm"
		api.mustCreate(project, -1)
		rule := map[string]any{"disabled": false, "action": "immutable", "template": "immutable_template",
			"tag_selectors":   []map[string]string{{"kind": "doublestar", "decoration": "matches", "pattern": "**"}},
			"scope_selectors": map[string]any{"repository": []map[string]string{{"kind": "doublestar", "decoration": "repoMatches", "pattern": "**"}}}}
		if s := api.do(http.MethodPost, "/projects/"+project+"/immutabletagrules", rule); s != http.StatusCreated {
			t.Fatalf("create the immutability rule: %d", s)
		}
		p := newProvider(project, false)
		caps, err := p.Capabilities(ctx)
		if err != nil || !caps.ImmutableTags {
			t.Fatalf("Capabilities = %+v, %v; want ImmutableTags", caps, err)
		}
		a := push(ctx, t, p, "raw/scratch/held", "v1", []byte("first"))
		repo, err := p.Repository(ctx, "raw/scratch/held")
		if err != nil {
			t.Fatal(err)
		}
		if err := pushManifest(ctx, repo, []byte("second"), "v1"); !errors.Is(err, provider.ErrConflict) {
			t.Errorf("move of an immutable tag = %v, want ErrConflict", err)
		}
		if err := repo.Delete(ctx, a); !errors.Is(err, provider.ErrConflict) {
			t.Errorf("delete of a manifest under an immutable tag = %v, want ErrConflict", err)
		}
		if err := p.DeleteRepository(ctx, "raw/scratch/held"); !errors.Is(err, provider.ErrConflict) {
			t.Errorf("DeleteRepository of a repository with an immutable tag = %v, want ErrConflict", err)
		}
		// Clean up: the rule goes first.
		var rules []struct {
			ID int `json:"id"`
		}
		api.get("/projects/"+project+"/immutabletagrules", &rules)
		for _, r := range rules {
			api.do(http.MethodDelete, "/projects/"+project+"/immutabletagrules/"+strconv.Itoa(r.ID), nil)
		}
	})

	t.Run("quota", func(t *testing.T) {
		project := run + "-quota"
		api.mustCreate(project, 1<<20)
		p := newProvider(project, false)
		repo, err := p.Repository(ctx, "raw/scratch/big")
		if err != nil {
			t.Fatal(err)
		}
		blob := bytes.Repeat([]byte{'q'}, 3<<19) // 1.5 MiB
		bd := ocispec.Descriptor{MediaType: "application/octet-stream", Digest: digest.FromBytes(blob), Size: int64(len(blob))}
		err = repo.Push(ctx, bd, bytes.NewReader(blob))
		if !errors.Is(err, provider.ErrLimit) || strings.Contains(err.Error(), project) || strings.Contains(err.Error(), "MiB") {
			t.Errorf("push over the quota = %v, want ErrLimit without the project and Harbor's message", err)
		}
	})
}

// push pushes a blob of content and a manifest of it as tag into the repository name, and
// returns the manifest's descriptor.
func push(ctx context.Context, t *testing.T, p *Provider, name, tag string, content []byte) ocispec.Descriptor {
	t.Helper()
	repo, err := p.Repository(ctx, name)
	if err != nil {
		t.Fatal(err)
	}
	if err := pushManifest(ctx, repo, content, tag); err != nil {
		t.Fatalf("push %s:%s: %v", name, tag, err)
	}
	d, err := repo.Resolve(ctx, tag)
	if err != nil {
		t.Fatalf("Resolve(%s): %v", tag, err)
	}
	return d
}

// pushManifest pushes a blob of content and a manifest whose config and layer it is, as tag.
func pushManifest(ctx context.Context, repo provider.Repository, content []byte, tag string) error {
	bd := ocispec.Descriptor{MediaType: "application/octet-stream", Digest: digest.FromBytes(content), Size: int64(len(content))}
	if err := repo.Push(ctx, bd, bytes.NewReader(content)); err != nil {
		return err
	}
	layer := `{"mediaType":"application/octet-stream","digest":"` + bd.Digest.String() + `","size":` + strconv.Itoa(len(content)) + `}`
	manifest := []byte(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json","config":` + layer + `,"layers":[` + layer + `]}`)
	md := ocispec.Descriptor{MediaType: ocispec.MediaTypeImageManifest, Digest: digest.FromBytes(manifest), Size: int64(len(manifest))}
	return repo.PushReference(ctx, md, bytes.NewReader(manifest), tag)
}

// deleteBelow deletes every repository below the provider's prefix.
func deleteBelow(t *testing.T, p *Provider) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	names, err := p.ListRepositories(ctx, "")
	if err != nil {
		t.Logf("the repositories below %s, to delete: %v", p.prefix, err)
		return
	}
	for _, n := range names {
		if err := p.DeleteRepository(ctx, n); err != nil && !errors.Is(err, provider.ErrConflict) {
			t.Errorf("delete %s%s: %v", p.prefix, n, err)
		}
	}
}

// adminAPI is Harbor's REST API as its administrator, for what the tests set up.
type adminAPI struct {
	t    *testing.T
	base string
	auth config.RegistryAuth
}

// do sends method to path below apiPath with the JSON body and returns the status.
func (a *adminAPI) do(method, path string, body any) int {
	resp := a.send(method, path, body)
	drain(resp)
	return resp.StatusCode
}

// get decodes the answer of a GET of path into v.
func (a *adminAPI) get(path string, v any) {
	resp := a.send(http.MethodGet, path, nil)
	defer drain(resp)
	if err := json.NewDecoder(resp.Body).Decode(v); err != nil {
		a.t.Errorf("GET %s: %v", path, err)
	}
}

func (a *adminAPI) send(method, path string, body any) *http.Response {
	a.t.Helper()
	var b []byte
	if body != nil {
		var err error
		if b, err = json.Marshal(body); err != nil {
			a.t.Fatal(err)
		}
	}
	req, err := http.NewRequest(method, a.base+apiPath+path, bytes.NewReader(b))
	if err != nil {
		a.t.Fatal(err)
	}
	req.SetBasicAuth(a.auth.Username, a.auth.Password)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Is-Resource-Name", "true")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		a.t.Fatalf("%s %s: %v", method, path, err)
	}
	return resp
}

// mustCreate creates a private project with a storage limit in bytes (-1 for none).
func (a *adminAPI) mustCreate(project string, limit int64) {
	a.t.Helper()
	body := map[string]any{"project_name": project, "metadata": map[string]string{"public": "false"}, "storage_limit": limit}
	if s := a.do(http.MethodPost, "/projects", body); s != http.StatusCreated {
		a.t.Fatalf("create the project %s: %d", project, s)
	}
}
