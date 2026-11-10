package generic

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"
	"testing/iotest"

	"github.com/opencontainers/go-digest"
	"oras.land/oras-go/v2/registry/remote/auth"

	"github.com/lajstrom/lajstrom/internal/config"
	"github.com/lajstrom/lajstrom/internal/provider"
)

// rotation is a credential that the test replaces, and a registry check that accepts only
// the current one.
type rotation struct {
	mu       sync.Mutex
	password string
	fail     error
	rejected int // requests that carried a credential other than the current one, or none
}

func (r *rotation) set(password string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.password = password
}

func (r *rotation) credential(context.Context, string) (auth.Credential, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.fail != nil {
		return auth.EmptyCredential, r.fail
	}
	return auth.Credential{Username: "AWS", Password: r.password}, nil
}

// hook answers a request without the current credential with a Basic challenge.
func (r *rotation) hook(w http.ResponseWriter, req *http.Request) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if u, p, ok := req.BasicAuth(); ok && u == "AWS" && p == r.password {
		return false
	}
	r.rejected++
	w.Header().Set("WWW-Authenticate", `Basic realm="fake"`)
	writeErr(w, http.StatusUnauthorized, "UNAUTHORIZED", "authentication required")
	return true
}

func (r *rotation) rejections() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.rejected
}

func rotatingProvider(t *testing.T, f *fakeRegistry, rot *rotation) *Provider {
	t.Helper()
	log, _ := testLogger()
	cfg := config.Registry{Name: "fake", Type: Type, URL: f.srv.URL, Prefix: "art/"}
	p, err := NewWithOptions(cfg, log, Options{Credential: rot.credential})
	if err != nil {
		t.Fatalf("NewWithOptions: %v", err)
	}
	return p
}

// TestRotatingCredential: a credential of Options is asked for every request once the registry
// asked for Basic, so a replaced one is sent at once, an upload chunk that cannot be replayed
// included; a credential that fails is ErrUnauthorized without the backend's URL.
func TestRotatingCredential(t *testing.T) {
	ctx := t.Context()
	f := newFakeRegistry(t)
	rot := &rotation{password: "first-token"}
	f.hook = rot.hook
	p := rotatingProvider(t, f, rot)
	if _, err := p.Capabilities(ctx); err != nil {
		t.Fatalf("Capabilities: %v", err)
	}
	repo, err := p.Repository(ctx, "raw/scratch/notes")
	if err != nil {
		t.Fatal(err)
	}
	pushBlob(t, repo, "before")
	challenges := rot.rejections() // the first request of the ping, unauthenticated

	rot.set("second-token")
	u, err := repo.StartBlobUpload(ctx)
	if err != nil {
		t.Fatalf("StartBlobUpload after the rotation: %v", err)
	}
	// iotest.OneByteReader hides any Seeker: the chunk cannot be sent twice.
	if u, err = repo.AppendBlobChunk(ctx, u, iotest.OneByteReader(strings.NewReader("after")), -1); err != nil {
		t.Fatalf("AppendBlobChunk after the rotation: %v", err)
	}
	if _, err := repo.FinishBlobUpload(ctx, u, digest.FromString("after"), nil, 0); err != nil {
		t.Fatalf("FinishBlobUpload: %v", err)
	}
	if got := rot.rejections(); got != challenges {
		t.Errorf("%d requests carried the old credential or none after the rotation", got-challenges)
	}

	rot.mu.Lock()
	rot.fail = errors.New("no token today")
	rot.mu.Unlock()
	broken := rotatingProvider(t, f, rot)
	_, err = broken.Capabilities(ctx)
	wantErr(t, "Capabilities with a failing credential", err, provider.ErrUnauthorized)
	if err != nil && (!strings.Contains(err.Error(), "no token today") || strings.Contains(err.Error(), f.srv.URL)) {
		t.Errorf("error %q: want the credential's error and not the backend's URL", err)
	}
}
