//go:build integration

package facade

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"sync"
	"testing"

	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/lajstrom/lajstrom/internal/config"
	"github.com/lajstrom/lajstrom/internal/index"
	"github.com/lajstrom/lajstrom/internal/index/postgres"
	"github.com/lajstrom/lajstrom/internal/ingest"
	"github.com/lajstrom/lajstrom/internal/keys"
	"github.com/lajstrom/lajstrom/internal/oci/profile"
	"github.com/lajstrom/lajstrom/internal/provider"
	"github.com/lajstrom/lajstrom/internal/provider/generic"
)

// referrersAtOnce is how many referrers of one subject TestIntegrationReferrersConcurrent pushes
// at once.
const referrersAtOnce = 12

// TestIntegrationReferrersConcurrent is the HA test of #28: referrers of one subject pushed at
// once through two replicas, each with its own provider and pipeline, over one Postgres store,
// and every one listed afterwards. On a registry without the referrers API (distribution) each
// push rewrites the subject's sha256-<hex> index, and only the referrers lock keeps one push
// from writing an index without another's referrer; with the API (Zot) the registry keeps the
// list. It needs LAJSTROM_TEST_REGISTRY and LAJSTROM_TEST_POSTGRES_DSN, a database or schema
// the test may migrate and write a repo, audit events and token keys to: CI's is fresh; on the
// dev stack, give the DSN search_path=<a schema of its own>.
func TestIntegrationReferrersConcurrent(t *testing.T) {
	addr, dsn := os.Getenv("LAJSTROM_TEST_REGISTRY"), os.Getenv("LAJSTROM_TEST_POSTGRES_DSN")
	if addr == "" || dsn == "" {
		t.Skip("set LAJSTROM_TEST_REGISTRY and LAJSTROM_TEST_POSTGRES_DSN to run against a registry and Postgres")
	}
	ctx := t.Context()
	st, err := postgres.Open(ctx, dsn)
	must(t, err)
	t.Cleanup(func() { _ = st.Close() })
	must(t, st.Migrate(ctx))
	name := "it-" + randomHex(t)
	repo := index.Repo{Format: "raw", Name: name, Registry: "it", Prefix: "raw/" + name, Kind: "hosted"}
	must(t, st.CreateRepo(ctx, &repo))
	t.Cleanup(func() { _ = st.DeleteRepo(context.Background(), repo.ID) })
	cfg := config.Registry{Name: "it", Type: generic.Type, URL: "http://" + addr, Prefix: os.Getenv("LAJSTROM_TEST_REGISTRY_PREFIX"),
		Auth: config.RegistryAuth{Username: os.Getenv("LAJSTROM_TEST_REGISTRY_USERNAME"), Password: os.Getenv("LAJSTROM_TEST_REGISTRY_PASSWORD")}}
	logs := &logBuffer{}
	replica := func() string {
		log := slog.New(slog.NewTextHandler(logs, nil))
		p, err := generic.New(cfg, log)
		must(t, err)
		providers := provider.Map{"it": p}
		srv := httptest.NewServer(New(Options{Index: st, Providers: providers, Tokens: keys.NewEphemeralTokens(), Log: log,
			Ingest: ingest.New(ingest.Options{Providers: providers, Index: st, Log: log})}))
		t.Cleanup(srv.Close)
		return srv.URL
	}
	replicas := []string{replica(), replica()}

	f := rawFileOf("it/hello.txt", "hello lajstrom "+randomHex(t)+"\n")
	f.m.Annotations[profile.AnnotationRepo] = name
	path := "/v2/raw/" + name + "/" + f.path
	blobs := [][]byte{f.content, ocispec.DescriptorEmptyJSON.Data}
	sigs, want := make([][]byte, referrersAtOnce), make([]string, referrersAtOnce)
	for i := range referrersAtOnce {
		blobs = append(blobs, fmt.Appendf(nil, "signature %d", i))
	}
	for _, b := range blobs {
		resp, body := request(t, http.MethodPost, replicas[0]+path+"/blobs/uploads/?digest="+digest.FromBytes(b).String(), bytes.NewReader(b), int64(len(b)))
		if resp.StatusCode != http.StatusCreated {
			t.Fatalf("blob: %d %s", resp.StatusCode, body)
		}
	}
	m, subject := f.body(t)
	resp, body := request(t, http.MethodPut, replicas[0]+path+"/manifests/"+f.tag(), bytes.NewReader(m), int64(len(m)), "Content-Type", ocispecManifest)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("the subject: %d %s\n%s", resp.StatusCode, body, logs)
	}
	for i := range referrersAtOnce {
		var d digest.Digest
		sigs[i], d = referrerOf(t, subject, cosignBundle, fmt.Sprintf("signature %d", i))
		want[i] = d.String() + " " + cosignBundle
	}

	var wg sync.WaitGroup
	for i := range referrersAtOnce {
		wg.Go(func() {
			url := replicas[i%2] + path + "/manifests/" + digest.FromBytes(sigs[i]).String()
			req, err := http.NewRequestWithContext(ctx, http.MethodPut, url, bytes.NewReader(sigs[i]))
			if err != nil {
				t.Error(err)
				return
			}
			req.Header.Set("Content-Type", ocispecManifest)
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Error(err)
				return
			}
			_ = resp.Body.Close()
			if resp.StatusCode != http.StatusCreated {
				t.Errorf("referrer %d: %d", i, resp.StatusCode)
			}
		})
	}
	wg.Wait()

	resp, body = request(t, http.MethodGet, replicas[1]+path+"/referrers/"+subject.String(), nil, 0)
	slices.Sort(want)
	if got := referrersOf(t, "referrers", body); resp.StatusCode != http.StatusOK || !slices.Equal(got, want) {
		t.Errorf("referrers: %d, %d listed of %d:\n%q\nwant\n%q", resp.StatusCode, len(got), len(want), got, want)
	}
}
