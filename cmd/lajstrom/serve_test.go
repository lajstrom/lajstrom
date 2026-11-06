package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/lajstrom/lajstrom/internal/format"
	"github.com/lajstrom/lajstrom/internal/index"
)

// reservedPlugin is a format plugin named after a reserved URL segment.
type reservedPlugin struct{}

func (reservedPlugin) Name() string                      { return "api" }
func (reservedPlugin) ArtifactType() string              { return "application/vnd.lajstrom.api.v1" }
func (reservedPlugin) EscapeName(string) (string, error) { return "", format.ErrUnsupported }
func (reservedPlugin) Routes(format.Router, format.Deps) {}
func (reservedPlugin) Build(context.Context, index.Repo, format.Upload) (*format.Built, error) {
	return nil, format.ErrUnsupported
}

func (reservedPlugin) Validate(context.Context, index.Repo, ocispec.Manifest, format.BlobFetcher) (*format.Entry, error) {
	return nil, format.ErrUnsupported
}

func (reservedPlugin) RenderIndex(context.Context, index.Repo, format.IndexScope, index.Reader) ([]format.Document, error) {
	return nil, format.ErrUnsupported
}

// A plugin cannot take a reserved first segment: serve fails before it listens. The plugin
// stays registered for the rest of this test binary; no other test gets past serve's New.
func TestServeRejectsReservedPlugin(t *testing.T) {
	if _, ok := format.Lookup("api"); !ok { // the registry is global; go test -count runs this again
		format.Register(reservedPlugin{})
	}
	cfg := filepath.Join(t.TempDir(), "lajstrom.yaml")
	if err := os.WriteFile(cfg, []byte("server:\n  listen: \"127.0.0.1:0\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, _, err := run(t, nil, "serve", "--config", cfg)
	if err == nil || !strings.Contains(err.Error(), `format plugin "api"`) || !strings.Contains(err.Error(), "reserved") {
		t.Fatalf("serve error = %v, want the reserved plugin api named", err)
	}
}
