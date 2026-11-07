//go:build integration

package raw

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"testing"

	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/lajstrom/lajstrom/internal/config"
	"github.com/lajstrom/lajstrom/internal/oci/profile"
	"github.com/lajstrom/lajstrom/internal/provider"
	"github.com/lajstrom/lajstrom/internal/provider/generic"
)

// TestIntegration runs PUT, GET, HEAD, a range and DELETE through the routes, the ingest
// pipeline and the generic provider against the registry at LAJSTROM_TEST_REGISTRY (host:port,
// plain HTTP), with LAJSTROM_TEST_REGISTRY_PREFIX, _USERNAME and _PASSWORD when they are set.
// The range goes to the backend, which answers 206 with those bytes; HEAD is the backend's
// HEAD; on a registry that redirects, GET is a 307 and HEAD still 200.
// The manifest fetched back from the registry has the path as the layer title and the raw
// annotations, which is what oras pull needs to write the original file name (R-08). The path
// starts with a random directory, so runs do not collide in a registry that keeps what they
// pushed.
func TestIntegration(t *testing.T) {
	addr := os.Getenv("LAJSTROM_TEST_REGISTRY")
	if addr == "" {
		t.Skip("set LAJSTROM_TEST_REGISTRY=host:port to run against a registry")
	}
	prov, err := generic.New(config.Registry{
		Name: "local", Type: generic.Type, URL: "http://" + addr, Prefix: os.Getenv("LAJSTROM_TEST_REGISTRY_PREFIX"),
		Auth: config.RegistryAuth{Username: os.Getenv("LAJSTROM_TEST_REGISTRY_USERNAME"), Password: os.Getenv("LAJSTROM_TEST_REGISTRY_PASSWORD")},
	}, slog.New(slog.DiscardHandler))
	must(t, err)
	e := newEnv(t, envOptions{registry: prov})
	run := make([]byte, 4)
	_, _ = rand.Read(run)
	p := "it-" + hex.EncodeToString(run) + "/Docs/File Name.txt"
	content := strings.Repeat("raw integration "+hex.EncodeToString(run)+"\n", 100)

	resp := e.put(p, content, "Content-Type", "text/plain")
	name := "raw/scratch/" + escapePath(p)
	checkStoredManifest(t.Context(), t, prov, name, resp, p)

	rr := e.do(http.MethodGet, target(p), nil)
	switch rr.Code {
	case http.StatusTemporaryRedirect:
		loc, _, _ := strings.Cut(rr.Header().Get("Location"), "?") // the query carries a signature
		t.Logf("the registry redirects blob downloads: %s", loc)
	case http.StatusOK:
		if rr.Body.String() != content {
			t.Errorf("GET returned %d bytes, want the %d uploaded", rr.Body.Len(), len(content))
		}
		header(t, rr, "ETag", `"`+digest.FromString(content).String()+`"`)
		header(t, rr, "Content-Type", "text/plain")
		rr := e.do(http.MethodGet, target(p), nil, "Range", "bytes=1000-1999")
		if rr.Code != http.StatusPartialContent || rr.Body.String() != content[1000:2000] {
			t.Errorf("range = %d, %d bytes", rr.Code, rr.Body.Len())
		}
		header(t, rr, "Content-Range", fmt.Sprintf("bytes 1000-1999/%d", len(content)))
		header(t, rr, "Content-Length", "1000")
		if rr := e.do(http.MethodGet, target(p), nil, "Range", fmt.Sprintf("bytes=%d-", len(content))); rr.Code != http.StatusRequestedRangeNotSatisfiable {
			t.Errorf("range past the end = %d", rr.Code)
		}
	default:
		t.Fatalf("GET = %d %s", rr.Code, rr.Body)
	}
	if rr := e.do(http.MethodHead, target(p), nil); rr.Body.Len() != 0 || rr.Code != http.StatusOK || rr.Header().Get("Content-Length") != fmt.Sprint(len(content)) {
		t.Errorf("HEAD = %d with %d bytes, Content-Length %q", rr.Code, rr.Body.Len(), rr.Header().Get("Content-Length"))
	}

	caps, err := prov.Capabilities(t.Context())
	must(t, err)
	rr = e.do(http.MethodDelete, target(p), nil)
	if !caps.DeleteManifest {
		if rr.Code != http.StatusNotImplemented {
			t.Errorf("DELETE on a registry without manifest deletes = %d", rr.Code)
		}
		return
	}
	if rr.Code != http.StatusNoContent {
		t.Fatalf("DELETE = %d %s", rr.Code, rr.Body)
	}
	if rr := e.do(http.MethodGet, target(p), nil); rr.Code != http.StatusNotFound {
		t.Errorf("GET after DELETE = %d", rr.Code)
	}
	repo, err := prov.Repository(t.Context(), name)
	must(t, err)
	if _, err := repo.Resolve(t.Context(), resp.Digest); !errors.Is(err, provider.ErrNotFound) {
		t.Errorf("the manifest in the registry after DELETE: %v, want ErrNotFound", err)
	}
}

// checkStoredManifest fetches the manifest of resp from the registry and checks the layer
// title and the raw annotations.
func checkStoredManifest(ctx context.Context, t *testing.T, prov provider.Provider, name string, resp putResponse, p string) {
	t.Helper()
	repo, err := prov.Repository(ctx, name)
	must(t, err)
	desc, err := repo.Resolve(ctx, resp.Version)
	must(t, err)
	if desc.Digest.String() != resp.Digest {
		t.Errorf("tag %s holds %s, PUT said %s", resp.Version, desc.Digest, resp.Digest)
	}
	rc, err := repo.Fetch(ctx, desc)
	must(t, err)
	defer func() { _ = rc.Close() }()
	b, err := io.ReadAll(rc)
	must(t, err)
	var m ocispec.Manifest
	must(t, json.Unmarshal(b, &m))
	a := m.Annotations
	if m.ArtifactType != "application/vnd.lajstrom.raw.v1" || a[profile.AnnotationFormat] != "raw" || a[profile.AnnotationName] != p ||
		a[profile.AnnotationRawPath] != p || a[profile.AnnotationVersion] != resp.Version {
		t.Errorf("manifest = %s", b)
	}
	if len(m.Layers) != 2 || m.Layers[0].Annotations[ocispec.AnnotationTitle] != p || m.Layers[1].Annotations[ocispec.AnnotationTitle] != "metadata.json" {
		t.Errorf("layers = %+v", m.Layers)
	}
}
