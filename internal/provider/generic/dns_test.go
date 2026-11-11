package generic

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"strings"
	"testing"

	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/lajstrom/lajstrom/internal/config"
	"github.com/lajstrom/lajstrom/internal/provider"
)

// noDNS makes every host name fail to resolve for the test: the resolver of the process has no
// name server to ask, as a registry's name on a compose network once its container stopped.
func noDNS(t *testing.T) {
	t.Helper()
	prev := net.DefaultResolver
	net.DefaultResolver = &net.Resolver{PreferGo: true, Dial: func(context.Context, string, string) (net.Conn, error) {
		return nil, errors.New("no name server in this test")
	}}
	t.Cleanup(func() { net.DefaultResolver = prev })
}

// TestUnresolvableRegistry calls every path of a registry whose host name does not resolve
// (#561): each fails with provider.ErrUnavailable, the blob HEAD of Exists and StatBlob as
// much as GET, with the *net.DNSError as its cause and not the circuit breaker's refusal, and
// a message that does not name the host.
func TestUnresolvableRegistry(t *testing.T) {
	noDNS(t)
	lowerBreaker(t, 1000) // the breaker stays closed: every call meets the DNS error itself
	p, err := New(config.Registry{Name: "gone", Type: Type, URL: "http://registry.lajstrom.test:5000", Retries: -1}, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	ctx := t.Context()
	repo, err := p.Repository(ctx, "raw/scratch")
	if err != nil {
		t.Fatal(err)
	}
	d := digest.FromString("blob")
	blob := ocispec.Descriptor{MediaType: "application/octet-stream", Digest: d, Size: 4}
	manifest := ocispec.Descriptor{MediaType: ocispec.MediaTypeImageManifest, Digest: d, Size: 4}
	paths := []struct {
		name string
		call func() error
	}{
		{"StatBlob", func() error { _, err := p.StatBlob(ctx, "raw/scratch", d); return err }},
		{"OpenBlob", func() error { _, err := p.OpenBlob(ctx, "raw/scratch", d, "", true); return err }},
		{"Exists of a blob", func() error { _, err := repo.Exists(ctx, blob); return err }},
		{"Exists of a manifest", func() error { _, err := repo.Exists(ctx, manifest); return err }},
		{"Fetch", func() error { _, err := repo.Fetch(ctx, manifest); return err }},
		{"Resolve", func() error { _, err := repo.Resolve(ctx, "v1"); return err }},
		{"Tags", func() error { return repo.Tags(ctx, "", func([]string) error { return nil }) }},
		{"Push", func() error { return repo.Push(ctx, blob, strings.NewReader("blob")) }},
		{"StartBlobUpload", func() error { _, err := repo.StartBlobUpload(ctx); return err }},
		{"MountBlob", func() error { _, _, err := repo.MountBlob(ctx, d, "raw/other"); return err }},
		{"Ping", func() error { return p.Ping(ctx) }},
		{"Capabilities", func() error { _, err := p.Capabilities(ctx); return err }},
	}
	for _, tt := range paths {
		err := tt.call()
		var dns *net.DNSError
		switch {
		case !errors.Is(err, provider.ErrUnavailable) || !errors.As(err, &dns):
			t.Errorf("%s: %v, want provider.ErrUnavailable from the DNS error", tt.name, err)
		case strings.Contains(err.Error(), "lajstrom.test"):
			t.Errorf("%s: the message %q names the host", tt.name, err)
		}
	}
}
