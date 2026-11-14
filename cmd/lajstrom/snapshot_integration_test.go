//go:build integration

package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/lajstrom/lajstrom/internal/index"
	"github.com/lajstrom/lajstrom/internal/index/sqlite"
)

// writeIntegrationConfig writes a configuration whose index is the SQLite file name in dir and
// whose one registry, local, is the one at LAJSTROM_TEST_REGISTRY below prefix, and returns its
// path; the test is skipped without the variable.
func writeIntegrationConfig(t *testing.T, dir, name, prefix string) string {
	t.Helper()
	addr := os.Getenv("LAJSTROM_TEST_REGISTRY")
	if addr == "" {
		t.Skip("set LAJSTROM_TEST_REGISTRY to run against a registry")
	}
	cfg := filepath.Join(dir, name+".yaml")
	body := "server:\n  listen: \":8080\"\n  externalURL: https://art.example.com\ndatabase:\n  driver: sqlite\n  dsn: \"file:" +
		filepath.Join(dir, name+".db") + "\"\nkeys:\n  masterKeyEnv: LAJSTROM_MASTER_KEY\nregistries:\n  - name: local\n    url: \"http://" +
		addr + "\"\n    prefix: " + prefix + "\n"
	if err := os.WriteFile(cfg, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return cfg
}

// itPrefix returns an instance prefix of its own for a test, so that its snapshots are its own.
func itPrefix() string {
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	return "it-snap-" + hex.EncodeToString(b) + "/"
}

// TestIntegrationSnapshotCreate writes a sealed snapshot and an unsealed one with every audit row
// into the registry at LAJSTROM_TEST_REGISTRY, through the commands: list shows both, the newest
// first, and each has its audit row snapshot.create.
func TestIntegrationSnapshotCreate(t *testing.T) {
	dir := t.TempDir()
	cfg := writeIntegrationConfig(t, dir, "one", itPrefix())
	env := map[string]string{"LAJSTROM_MASTER_KEY": testMasterKey}
	outs := runEnv(t, cfg, env, []step{
		{args: []string{"migrate"}, wantOut: "applied 0001"},
		{args: []string{"admin", "repo", "create", "raw/assets", "--registry", "local"}},
		{args: []string{"admin", "snapshot", "list"}, wantOut: "TAG  CREATED  SCHEMA  SEALED  SIZE  DIGEST\n"},
		{args: []string{"admin", "snapshot", "create"}, wantOut: `"repository":"lajstrom/control-plane"`},
	})
	// The next snapshot in the next second: of two of one second, the tags sort by digest.
	time.Sleep(time.Until(time.Now().Truncate(time.Second).Add(time.Second)))
	outs = append(outs, runEnv(t, cfg, env, []step{
		{args: []string{"admin", "snapshot", "create", "--unsealed", "--audit", "all"}, wantOut: `"sealed":false`},
		{args: []string{"admin", "snapshot", "list", "--json"}},
	})...)
	var sealed, unsealed struct {
		Tag    string         `json:"tag"`
		Digest string         `json:"digest"`
		Sealed bool           `json:"sealed"`
		Rows   map[string]int `json:"rows"`
	}
	if err := json.Unmarshal([]byte(outs[3]), &sealed); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(outs[4]), &unsealed); err != nil {
		t.Fatal(err)
	}
	if !sealed.Sealed || sealed.Rows["repos"] != 1 || unsealed.Rows["audit"] != 2 {
		t.Errorf("the snapshots: %s, %s", outs[3], outs[4])
	}
	lines := strings.Split(strings.TrimSpace(outs[5]), "\n")
	if len(lines) != 2 || !strings.Contains(lines[0], unsealed.Digest) || !strings.Contains(lines[1], sealed.Digest) {
		t.Errorf("list --json: %q, want the unsealed snapshot, then the sealed one", lines)
	}

	st, err := sqlite.Open(t.Context(), "file:"+filepath.Join(dir, "one.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	evs, err := st.ListAudit(t.Context(), index.AuditFilter{Action: actionSnapshotCreate})
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != 2 || evs[0].Subject != "lajstrom/control-plane:"+unsealed.Tag || string(evs[0].Digest) != unsealed.Digest ||
		evs[0].Principal != "anonymous" {
		t.Errorf("the audit rows snapshot.create: %+v", evs)
	}
}
