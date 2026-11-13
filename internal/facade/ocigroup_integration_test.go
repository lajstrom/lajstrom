//go:build integration

package facade

import (
	"bytes"
	"cmp"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"slices"
	"testing"
	"time"

	"github.com/lajstrom/lajstrom/internal/config"
	"github.com/lajstrom/lajstrom/internal/index"
	"github.com/lajstrom/lajstrom/internal/index/sqlite"
	"github.com/lajstrom/lajstrom/internal/ingest"
	"github.com/lajstrom/lajstrom/internal/keys"
	"github.com/lajstrom/lajstrom/internal/provider"
	"github.com/lajstrom/lajstrom/internal/provider/generic"
	"github.com/lajstrom/lajstrom/internal/proxy"
)

// TestIntegrationOCIGroup is #848 against real registries (docs/spec/proxy-and-group.md §12):
// the group oci/all of the hosted oci/internal, which serves acme/*, and of the proxy oci/hub,
// which serves the rest, its upstream LAJSTROM_TEST_UPSTREAM_REGISTRY (by default the backend
// under another repository). One URL pulls the hosted image from the backend, with no request
// upstream, and the proxied one through the proxy, whose proxy.fetch rows name the group; a push
// through the group is 405; tags/list names the group's path; the catalog lists both paths.
func TestIntegrationOCIGroup(t *testing.T) {
	addr := os.Getenv("LAJSTROM_TEST_REGISTRY")
	if addr == "" {
		t.Skip("set LAJSTROM_TEST_REGISTRY=host:port to run against a registry")
	}
	upAddr := cmp.Or(os.Getenv("LAJSTROM_TEST_UPSTREAM_REGISTRY"), addr)
	ctx := t.Context()
	auth := config.RegistryAuth{Username: os.Getenv("LAJSTROM_TEST_REGISTRY_USERNAME"), Password: os.Getenv("LAJSTROM_TEST_REGISTRY_PASSWORD")}
	backend, err := generic.New(config.Registry{Name: "it", Type: generic.Type, URL: "http://" + addr,
		Prefix: os.Getenv("LAJSTROM_TEST_REGISTRY_PREFIX"), Auth: auth}, nil)
	must(t, err)
	seed, err := generic.New(config.Registry{Name: "seed", Type: generic.Type, URL: "http://" + upAddr, Auth: auth}, nil)
	must(t, err)
	run := randomHex(t)
	upPath := "it-group-" + run + "/img"
	upImg := seedImage(t, seed, upPath, "1.0", "amd64")
	hostedPath := "acme/api-" + run
	hostedImg := seedImage(t, backend, "oci/internal/"+hostedPath, "1.4", "amd64")

	target, _ := url.Parse("http://" + upAddr)
	count := &counter{blobs: map[string]int{}}
	front := httptest.NewServer(count.handler(target))
	t.Cleanup(front.Close)
	cred := generic.Credential{}
	if upAddr == addr {
		cred = generic.Credential{Username: auth.Username, Password: auth.Password}
	}
	client, err := generic.NewUpstream(config.Upstream{Name: "up", Format: config.FormatOCI, URL: front.URL}, cred, nil)
	must(t, err)

	st, err := sqlite.Open(ctx, fmt.Sprintf("file:facade-ocigroup-it-%d?mode=memory&cache=shared", memoryDBs.Add(1)))
	must(t, err)
	t.Cleanup(func() { _ = st.Close() })
	must(t, st.Migrate(ctx))
	internal := index.Repo{Format: "oci", Name: "internal", Registry: "it", Prefix: "oci/internal", Kind: index.KindHosted}
	hub := index.Repo{Format: "oci", Name: "hub", Registry: "it", Prefix: "oci/hub", Kind: index.KindProxy, Upstream: json.RawMessage(`{"name": "up"}`)}
	all := index.Repo{Format: "oci", Name: "all", Prefix: "oci/all", Kind: index.KindGroup,
		Group: json.RawMessage(`{"paths": {"internal": ["acme/*"], "hub": ["*"]}}`)}
	for _, r := range []*index.Repo{&internal, &hub, &all} {
		must(t, st.CreateRepo(ctx, r))
	}
	must(t, st.SetGroupMembers(ctx, all.ID, []int64{internal.ID, hub.ID}))
	must(t, st.UpsertOCIRepo(ctx, &index.OCIRepo{RepoID: internal.ID, Path: "oci/internal/" + hostedPath, SeenAt: time.Now()}))
	providers := provider.Map{"it": backend}
	log := slog.New(slog.NewTextHandler(&logBuffer{}, nil))
	pipeline := ingest.New(ingest.Options{Providers: providers, Index: st, Log: log})
	core := proxy.New(proxy.Options{Index: st, Providers: providers, Mirror: pipeline, Log: log,
		OCIUpstreams: map[string]*proxy.OCIUpstream{"up": proxy.NewOCIUpstream("up", client, false)}})
	srv := httptest.NewServer(New(Options{Index: st, Providers: providers, Log: log, Tokens: keys.NewEphemeralTokens(), Ingest: pipeline,
		OCIProxy: core}))
	t.Cleanup(srv.Close)
	base := srv.URL + "/v2/oci/all/"

	pullPlatform(t, base+hostedPath, "1.4", hostedImg, 0)
	if n, gets := count.blobGETs(), count.manifestGETs(); n+gets != 0 {
		t.Errorf("the hosted member's pull asked the upstream: %d blob and %d manifest GETs", n, gets)
	}
	pullPlatform(t, base+upPath, "1.0", upImg, 0)
	if n := count.blobGETs(); n != 2 {
		t.Errorf("%d upstream blob GETs for the proxied pull, want 2: its config and its layer", n)
	}
	var fetches []index.AuditEvent
	done := func(evs []index.AuditEvent) bool {
		return len(evs) >= 2 && !slices.ContainsFunc(evs, func(ev index.AuditEvent) bool { return ev.State != index.AuditOK })
	}
	for range 50 { // the push of what the blobs completed follows the tee (§11.8), its row completed after it
		if fetches, err = st.ListAudit(ctx, index.AuditFilter{Action: "proxy.fetch"}); err == nil && done(fetches) {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if !done(fetches) {
		t.Fatalf("proxy.fetch rows %+v, %v; want the manifest's and the index's, completed", fetches, err)
	}
	for _, ev := range fetches {
		var d map[string]any
		must(t, json.Unmarshal(ev.Details, &d))
		if ev.Repo != "oci/hub" || d["via"] != "oci/all" {
			t.Errorf("proxy.fetch %s details %v, want the proxy's row with via oci/all", ev.Repo, d)
		}
	}

	resp, body := request(t, http.MethodGet, base+hostedPath+"/tags/list", nil, 0)
	if want := `{"name":"oci/all/` + hostedPath + `","tags":["1.4"]}`; resp.StatusCode != http.StatusOK || string(bytes.TrimSpace(body)) != want {
		t.Errorf("tags/list: %d %s, want %s", resp.StatusCode, body, want)
	}
	raw := []byte(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json","config":{"mediaType":"application/vnd.oci.empty.v1+json","digest":"sha256:44136fa355b3678a1146ad16f7e8649e94fb4fc21fe77e8310c060f61caaff8a","size":2},"layers":[]}`)
	resp, body = request(t, http.MethodPut, base+hostedPath+"/manifests/mine", bytes.NewReader(raw), int64(len(raw)), "Content-Type", "application/vnd.oci.image.manifest.v1+json")
	if resp.StatusCode != http.StatusMethodNotAllowed || resp.Header.Get("Allow") != "GET, HEAD" {
		t.Errorf("a push through the group: %d, Allow %q, %s", resp.StatusCode, resp.Header.Get("Allow"), body)
	}
	resp, body = request(t, http.MethodGet, srv.URL+"/v2/token?scope=registry:catalog:*", nil, 0)
	var tok tokenResponse
	must(t, json.Unmarshal(body, &tok))
	resp, body = request(t, http.MethodGet, srv.URL+"/v2/_catalog?n=1000", nil, 0, "Authorization", "Bearer "+tok.Token)
	var cat catalogList
	must(t, json.Unmarshal(body, &cat))
	for _, want := range []string{"oci/all/" + hostedPath, "oci/all/" + upPath} {
		if resp.StatusCode != http.StatusOK || !slices.Contains(cat.Repositories, want) {
			t.Errorf("the catalog %d %q lacks %s", resp.StatusCode, cat.Repositories, want)
		}
	}
}

// manifestGETs is the count of the manifest GETs the counter passed on.
func (c *counter) manifestGETs() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.gets
}
