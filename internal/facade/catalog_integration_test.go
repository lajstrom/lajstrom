//go:build integration

package facade

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"testing"
	"time"

	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/lajstrom/lajstrom/internal/config"
	"github.com/lajstrom/lajstrom/internal/index"
	"github.com/lajstrom/lajstrom/internal/index/sqlite"
	"github.com/lajstrom/lajstrom/internal/ingest"
	"github.com/lajstrom/lajstrom/internal/keys"
	"github.com/lajstrom/lajstrom/internal/provider"
	"github.com/lajstrom/lajstrom/internal/provider/generic"
)

// TestIntegrationCatalogAfterDelete is #728 on a real registry: a raw path whose one version is
// deleted through /v2 leaves the catalog, while the path beside it stays. The oci repository
// whose one manifest is deleted stays: an oci repo indexes no tags, and its oci_repos row stays
// until reindex no longer finds the repository in the registry (Zot keeps it).
func TestIntegrationCatalogAfterDelete(t *testing.T) {
	addr := os.Getenv("LAJSTROM_TEST_REGISTRY")
	if addr == "" {
		t.Skip("set LAJSTROM_TEST_REGISTRY=host:port to run against a registry")
	}
	ctx := t.Context()
	st, err := sqlite.Open(ctx, fmt.Sprintf("file:facade-it-%d?mode=memory&cache=shared", memoryDBs.Add(1)))
	must(t, err)
	t.Cleanup(func() { _ = st.Close() })
	must(t, st.Migrate(ctx))
	for _, repo := range []index.Repo{
		{Format: "raw", Name: "assets", Registry: "it", Prefix: "raw/assets", Kind: "hosted"},
		{Format: "oci", Name: "images", Registry: "it", Prefix: "oci/images", Kind: "hosted"},
	} {
		must(t, st.CreateRepo(ctx, &repo))
	}
	logs := &logBuffer{}
	log := slog.New(slog.NewTextHandler(logs, nil))
	p, err := generic.New(config.Registry{Name: "it", Type: generic.Type, URL: "http://" + addr, Prefix: os.Getenv("LAJSTROM_TEST_REGISTRY_PREFIX"),
		Auth: config.RegistryAuth{Username: os.Getenv("LAJSTROM_TEST_REGISTRY_USERNAME"), Password: os.Getenv("LAJSTROM_TEST_REGISTRY_PASSWORD")}}, log)
	must(t, err)
	providers := provider.Map{"it": p}
	srv := httptest.NewServer(New(Options{Index: st, Providers: providers, Log: log, Tokens: keys.NewEphemeralTokens(),
		Ingest: ingest.New(ingest.Options{Providers: providers, Index: st, Log: log})}))
	t.Cleanup(srv.Close)
	expect := func(what, method, path string, body []byte, status int, hdr ...string) *http.Response {
		t.Helper()
		if method == http.MethodPut {
			hdr = append(hdr, "Content-Type", ocispecManifest)
		}
		resp, b := request(t, method, srv.URL+path, bytes.NewReader(body), int64(len(body)), hdr...)
		if resp.StatusCode != status {
			t.Fatalf("%s: %s %s: %d, want %d: %s\n%s", what, method, path, resp.StatusCode, status, b, logs)
		}
		return resp
	}

	run := "it-" + randomHex(t)
	deleted, kept := rawFileOf(run+"/deleted.txt", "deleted "+run+"\n").at(time.Now()), rawFileOf(run+"/kept.txt", "kept "+run+"\n").at(time.Now())
	for _, f := range []*rawFile{deleted, kept} {
		for _, b := range [][]byte{f.content, ocispec.DescriptorEmptyJSON.Data} {
			expect("blob", http.MethodPost, "/v2/raw/assets/"+f.path+"/blobs/uploads/?digest="+digest.FromBytes(b).String(), b, http.StatusCreated)
		}
		m, _ := f.body(t)
		expect("the version", http.MethodPut, "/v2/raw/assets/"+f.path+"/manifests/"+f.tag(), m, http.StatusCreated)
	}
	image := "oci/images/" + run + "/hello"
	layer := []byte("layer " + run)
	for _, b := range [][]byte{layer, ocispec.DescriptorEmptyJSON.Data} {
		expect("blob", http.MethodPost, "/v2/"+image+"/blobs/uploads/?digest="+digest.FromBytes(b).String(), b, http.StatusCreated)
	}
	m, md := manifest(t, layer, run)
	expect("the image", http.MethodPut, "/v2/"+image+"/manifests/v1", m, http.StatusCreated)

	resp, body := request(t, http.MethodGet, srv.URL+"/v2/token?scope=registry:catalog:*", nil, 0)
	var tok struct{ Token string }
	if err := json.Unmarshal(body, &tok); resp.StatusCode != http.StatusOK || err != nil || tok.Token == "" {
		t.Fatalf("token for the catalog: %d %s", resp.StatusCode, body)
	}
	catalog := func() []string {
		t.Helper()
		resp, body := request(t, http.MethodGet, srv.URL+"/v2/_catalog", nil, 0, "Authorization", "Bearer "+tok.Token)
		var list catalogList
		if err := json.Unmarshal(body, &list); resp.StatusCode != http.StatusOK || err != nil {
			t.Fatalf("_catalog: %d %s", resp.StatusCode, body)
		}
		return list.Repositories
	}
	rawDeleted, rawKept := "raw/assets/"+deleted.path, "raw/assets/"+kept.path
	if got := catalog(); !slices.Contains(got, rawDeleted) || !slices.Contains(got, rawKept) || !slices.Contains(got, image) {
		t.Fatalf("the catalog before the deletes has not %s, %s and %s: %q", rawDeleted, rawKept, image, got)
	}

	expect("DELETE of the raw version", http.MethodDelete, "/v2/raw/assets/"+deleted.path+"/manifests/"+deleted.tag(), nil, http.StatusAccepted)
	expect("DELETE of the image", http.MethodDelete, "/v2/"+image+"/manifests/"+md.String(), nil, http.StatusAccepted)
	got := catalog()
	if slices.Contains(got, rawDeleted) {
		t.Errorf("the catalog lists %s, whose one version was deleted: %q", rawDeleted, got)
	}
	if !slices.Contains(got, rawKept) {
		t.Errorf("the catalog lost %s: %q", rawKept, got)
	}
	if !slices.Contains(got, image) {
		t.Errorf("the catalog lost %s, which oci_repos records: %q", image, got)
	}
}
