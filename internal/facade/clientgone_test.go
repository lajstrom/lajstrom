package facade

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/lajstrom/lajstrom/internal/provider"
	"github.com/lajstrom/lajstrom/internal/provider/providertest"
	"github.com/lajstrom/lajstrom/internal/server"
)

// hanging is a backend that answers no Resolve until the request's context ends.
type hanging struct{ *providertest.Fake }

func (h hanging) Repository(ctx context.Context, name string) (provider.Repository, error) {
	rr, err := h.Fake.Repository(ctx, name)
	return hangingRepository{rr}, err
}

type hangingRepository struct{ provider.Repository }

func (hangingRepository) Resolve(ctx context.Context, _ string) (ocispec.Descriptor, error) {
	<-ctx.Done()
	return ocispec.Descriptor{}, fmt.Errorf("resolve: %w", ctx.Err())
}

// TestClientGone sends a HEAD of a manifest through the server and gives up while the backend
// resolves it, as containerd does (#877): the cause is logged at Debug, not at Error, and the
// access line has status 499.
func TestClientGone(t *testing.T) {
	e := newEnv(t, envOptions{registry: func(f *providertest.Fake) provider.Provider { return hanging{f} }})
	logs := &logBuffer{}
	srv, err := server.New(server.Options{Listen: "127.0.0.1:0", Facade: e.h, Log: slog.New(slog.NewTextHandler(logs, nil))})
	must(t, err)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, ts.URL+"/v2/oci/images/app/manifests/v1", nil)
	must(t, err)
	if resp, err := ts.Client().Do(req); err == nil {
		_ = resp.Body.Close()
		t.Fatalf("HEAD answered %d, want the client to give up", resp.StatusCode)
	}
	for deadline := time.Now().Add(5 * time.Second); !strings.Contains(logs.String(), "msg=request "); time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("no access line:\n%s", logs)
		}
	}
	if !strings.Contains(logs.String(), " status=499 ") ||
		!strings.Contains(e.logs.String(), `level=DEBUG msg="facade: request failed"`) || strings.Contains(e.logs.String(), "level=ERROR") {
		t.Errorf("want the access line with status 499 and the cause at DEBUG; access log:\n%s\nfacade log:\n%s", logs, e.logs)
	}
}
