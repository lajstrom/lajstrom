package facade

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"slices"
	"sync"
	"testing"

	"github.com/opencontainers/go-digest"

	"github.com/lajstrom/lajstrom/internal/provider"
	"github.com/lajstrom/lajstrom/internal/provider/providertest"
)

// explicitRegistry is the fake as a registry that does not create a repository on push (ECR
// without a creation template): a blob upload or a mount to a repository that EnsureRepository
// was not asked to create is refused, as such a registry answers 404 NAME_UNKNOWN.
type explicitRegistry struct {
	*providertest.Fake
	mu      sync.Mutex
	created []string
}

func (f *explicitRegistry) Capabilities(ctx context.Context) (provider.Capabilities, error) {
	c, err := f.Fake.Capabilities(ctx)
	c.CreateOnPush = false
	return c, err
}

func (f *explicitRegistry) EnsureRepository(_ context.Context, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !slices.Contains(f.created, name) {
		f.created = append(f.created, name)
	}
	return nil
}

func (f *explicitRegistry) Repository(ctx context.Context, name string) (provider.Repository, error) {
	rr, err := f.Fake.Repository(ctx, name)
	return explicitRepository{Repository: rr, f: f, name: name}, err
}

// exists refuses a repository that EnsureRepository did not create.
func (f *explicitRegistry) exists(name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !slices.Contains(f.created, name) {
		return fmt.Errorf("explicit: repository %s does not exist: %w", name, provider.ErrNotFound)
	}
	return nil
}

func (f *explicitRegistry) wasCreated(name string) bool { return f.exists(name) == nil }

type explicitRepository struct {
	provider.Repository
	f    *explicitRegistry
	name string
}

func (r explicitRepository) StartBlobUpload(ctx context.Context) (provider.Upload, error) {
	if err := r.f.exists(r.name); err != nil {
		return provider.Upload{}, err
	}
	return r.Repository.StartBlobUpload(ctx)
}

func (r explicitRepository) MountBlob(ctx context.Context, d digest.Digest, from string) (bool, provider.Upload, error) {
	if err := r.f.exists(r.name); err != nil {
		return false, provider.Upload{}, err
	}
	return r.Repository.MountBlob(ctx, d, from)
}

// TestUploadEnsuresRepository: on a registry that does not create a repository on push, each
// way of starting a blob upload creates the repository of a new path first (#356), and a
// session so started finishes.
func TestUploadEnsuresRepository(t *testing.T) {
	var reg *explicitRegistry
	e := newEnv(t, envOptions{registry: func(f *providertest.Fake) provider.Provider {
		reg = &explicitRegistry{Fake: f}
		return reg
	}})
	b := []byte("a new path")
	d := e.pushBlob("oci/images/base", b)
	for _, tt := range []struct{ name, query string }{
		{"session", ""},
		{"monolithic", "?digest=" + d.String()},
		{"mount", "?mount=" + d.String() + "&from=oci/images/base"},
		{"mount-without-from", "?mount=" + d.String()},
	} {
		name := "oci/images/" + tt.name
		rr := e.do(e.h, http.MethodPost, "/v2/"+name+"/blobs/uploads/"+tt.query, bytes.NewReader(b))
		if rr.Code != http.StatusAccepted && rr.Code != http.StatusCreated {
			t.Fatalf("%s: status %d; body %s", tt.name, rr.Code, rr.Body)
		}
		if !reg.wasCreated(name) {
			t.Errorf("%s: EnsureRepository was not called for %s", tt.name, name)
		}
		if rr.Code == http.StatusAccepted {
			rr = e.do(e.h, http.MethodPut, "/v2/"+name+"/blobs/uploads/"+uploadID(t, rr)+"?digest="+d.String(), bytes.NewReader(b))
			wantStatus(t, tt.name+": PUT", rr, http.StatusCreated)
		}
		e.wantBlob(name, b)
	}
}
