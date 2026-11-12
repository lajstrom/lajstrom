//go:build integration

package facade

import (
	"bytes"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/lajstrom/lajstrom/internal/config"
	"github.com/lajstrom/lajstrom/internal/index"
	"github.com/lajstrom/lajstrom/internal/index/sqlite"
	"github.com/lajstrom/lajstrom/internal/ingest"
	"github.com/lajstrom/lajstrom/internal/keys"
	"github.com/lajstrom/lajstrom/internal/oci/profile"
	"github.com/lajstrom/lajstrom/internal/provider"
	"github.com/lajstrom/lajstrom/internal/provider/generic"
)

// TestIntegrationProxyRepoPush is #760 against a real registry: a proxy repo takes the blob
// uploads of a client, and the signature of a version it holds, but an artifact pushed as oras
// pushes it is 405 UNSUPPORTED and its tag is not in the backend.
func TestIntegrationProxyRepoPush(t *testing.T) {
	addr := os.Getenv("LAJSTROM_TEST_REGISTRY")
	if addr == "" {
		t.Skip("set LAJSTROM_TEST_REGISTRY=host:port to run against a registry")
	}
	ctx := t.Context()
	st, err := sqlite.Open(ctx, fmt.Sprintf("file:facade-it-%d?mode=memory&cache=shared", memoryDBs.Add(1)))
	must(t, err)
	t.Cleanup(func() { _ = st.Close() })
	must(t, st.Migrate(ctx))
	repo := index.Repo{Format: "raw", Name: "proxy", Registry: "it", Prefix: "raw/proxy", Kind: index.KindProxy}
	must(t, st.CreateRepo(ctx, &repo))
	p, err := generic.New(config.Registry{Name: "it", Type: generic.Type, URL: "http://" + addr, Prefix: os.Getenv("LAJSTROM_TEST_REGISTRY_PREFIX"),
		Auth: config.RegistryAuth{Username: os.Getenv("LAJSTROM_TEST_REGISTRY_USERNAME"), Password: os.Getenv("LAJSTROM_TEST_REGISTRY_PASSWORD")}}, nil)
	must(t, err)
	providers := provider.Map{"it": p}
	logs := &logBuffer{}
	log := slog.New(slog.NewTextHandler(logs, nil))
	pipeline := ingest.New(ingest.Options{Providers: providers, Index: st, Log: log})
	srv := httptest.NewServer(New(Options{Index: st, Providers: providers, Log: log, Tokens: keys.NewEphemeralTokens(), Ingest: pipeline}))
	t.Cleanup(srv.Close)
	upload := func(name string, b []byte) {
		t.Helper()
		resp, body := request(t, http.MethodPost, name+"/blobs/uploads/?digest="+digest.FromBytes(b).String(), bytes.NewReader(b), int64(len(b)))
		if resp.StatusCode != http.StatusCreated {
			t.Fatalf("blob upload to the proxy repo: %d %s", resp.StatusCode, body)
		}
	}

	// The version the repo holds, stored by the pipeline as the fetcher stores it.
	dir := "it-" + randomHex(t)
	held := rawFileOf(dir+"/held.txt", "held "+randomHex(t)+"\n").at(time.Now())
	held.m.Annotations[profile.AnnotationRepo] = repo.Name
	heldName := srv.URL + "/v2/raw/proxy/" + held.path
	upload(heldName, held.content)
	heldBody, subject := held.body(t)
	_, err = pipeline.Ingest(ctx, repo, held.path, held.tag(), held.m, heldBody, userP("alice"))
	must(t, err)

	f := rawFileOf(dir+"/new.txt", "not from the upstream "+randomHex(t)+"\n").at(time.Now())
	f.m.Annotations[profile.AnnotationRepo] = repo.Name
	name := srv.URL + "/v2/raw/proxy/" + f.path
	upload(name, f.content)
	m, _ := f.body(t)
	resp, body := request(t, http.MethodPut, name+"/manifests/"+f.tag(), bytes.NewReader(m), int64(len(m)), "Content-Type", ocispecManifest)
	if resp.StatusCode != http.StatusMethodNotAllowed || !strings.Contains(string(body), `"code":"UNSUPPORTED"`) {
		t.Errorf("artifact: %d %s, want 405 UNSUPPORTED", resp.StatusCode, body)
	}
	backend, err := p.Repository(ctx, "raw/proxy/"+f.path)
	must(t, err)
	if _, err := backend.Resolve(ctx, f.tag()); !errors.Is(err, provider.ErrNotFound) {
		t.Errorf("the backend has the tag of the refused push: %v", err)
	}

	for _, b := range [][]byte{ocispec.DescriptorEmptyJSON.Data, []byte("signature")} {
		upload(heldName, b)
	}
	sig, sigDigest := referrerOf(t, subject, cosignBundle, "signature")
	resp, body = request(t, http.MethodPut, heldName+"/manifests/"+sigDigest.String(), bytes.NewReader(sig), int64(len(sig)), "Content-Type", ocispecManifest)
	if resp.StatusCode != http.StatusCreated || resp.Header.Get("OCI-Subject") != subject.String() {
		t.Fatalf("signature: %d, OCI-Subject %q: %s\n%s", resp.StatusCode, resp.Header.Get("OCI-Subject"), body, logs)
	}
	resp, body = request(t, http.MethodGet, heldName+"/referrers/"+subject.String(), nil, 0)
	if got := referrersOf(t, "referrers", body); resp.StatusCode != http.StatusOK || len(got) != 1 || got[0] != sigDigest.String()+" "+cosignBundle {
		t.Errorf("referrers: %d %q, want %s", resp.StatusCode, got, sigDigest)
	}
}
