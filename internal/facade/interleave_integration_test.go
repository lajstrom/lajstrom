//go:build integration

package facade

import (
	"fmt"
	"log/slog"
	"os"
	"testing"

	"github.com/lajstrom/lajstrom/internal/config"
	"github.com/lajstrom/lajstrom/internal/index"
	"github.com/lajstrom/lajstrom/internal/index/sqlite"
	"github.com/lajstrom/lajstrom/internal/ingest"
	"github.com/lajstrom/lajstrom/internal/keys"
	"github.com/lajstrom/lajstrom/internal/provider"
	"github.com/lajstrom/lajstrom/internal/provider/generic"
)

// TestIntegrationDeleteInterleaved runs the scenarios of TestDeleteInterleaved against the
// registry of LAJSTROM_TEST_REGISTRY, with the referrers API it has: on Zot through the API, on
// distribution through the sha256-<hex> tag schema, where a referrer pushed after its subject's
// delete gets an index of its own (raceReferrerUnderLock).
func TestIntegrationDeleteInterleaved(t *testing.T) {
	addr := os.Getenv("LAJSTROM_TEST_REGISTRY")
	if addr == "" {
		t.Skip("set LAJSTROM_TEST_REGISTRY=host:port to run against a registry")
	}
	for _, byDigest := range []bool{false, true} {
		for _, s := range interleavings() {
			t.Run(deletedBy(byDigest)+"/"+s.name, func(t *testing.T) { raceRegistry(t, addr, s, byDigest) })
		}
	}
}

// raceRegistry runs the scenario s on a facade over the registry at addr and a database of its
// own, the registry and the store paused, under names of its own.
func raceRegistry(t *testing.T, addr string, s interleaving, byDigest bool) {
	ctx := t.Context()
	st, err := sqlite.Open(ctx, fmt.Sprintf("file:facade-it-%d?mode=memory&cache=shared", memoryDBs.Add(1)))
	must(t, err)
	t.Cleanup(func() { _ = st.Close() })
	must(t, st.Migrate(ctx))
	must(t, st.CreateRepo(ctx, &index.Repo{Format: "oci", Name: "images", Registry: "it", Prefix: "oci/images", Kind: "hosted"}))
	logs := &logBuffer{}
	log := slog.New(slog.NewTextHandler(logs, nil))
	prov, err := generic.New(config.Registry{Name: "it", Type: generic.Type, URL: "http://" + addr,
		Prefix: os.Getenv("LAJSTROM_TEST_REGISTRY_PREFIX"), Auth: config.RegistryAuth{Username: os.Getenv("LAJSTROM_TEST_REGISTRY_USERNAME"),
			Password: os.Getenv("LAJSTROM_TEST_REGISTRY_PASSWORD")}}, log)
	must(t, err)
	caps, err := prov.Capabilities(ctx)
	must(t, err)
	if !caps.DeleteManifest {
		t.Skip("the registry does not delete manifests")
	}
	p := newPauses()
	providers := provider.Map{"it": pausedRegistry{Provider: prov, p: p}}
	idx := pausedStore{Store: st, p: p}
	h := New(Options{Index: idx, Providers: providers, Log: log, Tokens: keys.NewEphemeralTokens(),
		Ingest: ingest.New(ingest.Options{Providers: providers, Index: idx, Log: log})})
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("the server's log:\n%s", logs)
		}
	})
	s.run(t, newRacer(t, h, st, p, racing{registry: "it", base: "it-" + randomHex(t) + "/", api: caps.Referrers, schema: !caps.Referrers,
		byDigest: byDigest}))
}
