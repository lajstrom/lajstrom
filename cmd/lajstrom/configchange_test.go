package main

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/lajstrom/lajstrom/internal/config"
	"github.com/lajstrom/lajstrom/internal/index"
	"github.com/lajstrom/lajstrom/internal/index/sqlite"
)

// configRows returns the config.change rows of st, the oldest first, with their details.
func configRows(t *testing.T, st index.Reader) ([]index.AuditEvent, []map[string]any) {
	t.Helper()
	evs, err := st.ListAudit(t.Context(), index.AuditFilter{Action: actionConfigChange})
	if err != nil {
		t.Fatal(err)
	}
	ds := make([]map[string]any, len(evs))
	for i, j := 0, len(evs)-1; i < j; i, j = i+1, j-1 {
		evs[i], evs[j] = evs[j], evs[i]
	}
	for i, ev := range evs {
		if err := json.Unmarshal(ev.Details, &ds[i]); err != nil {
			t.Fatal(err)
		}
	}
	return evs, ds
}

// newRecorder returns a configRecorder of the replica replica-1 over a migrated SQLite database.
func newRecorder(t *testing.T) configRecorder {
	t.Helper()
	st, err := sqlite.Open(t.Context(), "file:"+filepath.Join(t.TempDir(), "lajstrom.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := st.Migrate(t.Context()); err != nil {
		t.Fatal(err)
	}
	return configRecorder{st: st, host: "replica-1", release: "v1.2.3", now: time.Now, log: slog.New(slog.DiscardHandler)}
}

// recordedWith returns the default configuration with the URL url, changed by change.
func recordedWith(url string, change func(*config.Config)) *config.Config {
	c := config.Default()
	c.Server.ExternalURL = url
	if change != nil {
		change(c)
	}
	return c
}

// fingerprint is the fingerprint of c.
func fingerprint(t *testing.T, c *config.Config) string {
	t.Helper()
	_, fp, err := c.Recorded(nil)
	if err != nil {
		t.Fatal(err)
	}
	return fp
}

// TestRecordConfig is #1284's 3A (rbac.md §5): the first start on a database records its
// configuration with initial; a start with the same one writes nothing.
func TestRecordConfig(t *testing.T) {
	r := newRecorder(t)
	a := recordedWith("https://art.example.com", nil)
	for range 2 {
		if err := r.recordConfig(t.Context(), a); err != nil {
			t.Fatal(err)
		}
	}
	evs, ds := configRows(t, r.st)
	if len(evs) != 1 || evs[0].Principal != "anonymous" || evs[0].Subject != fingerprint(t, a) {
		t.Fatalf("rows %+v, want one, the initial record", evs)
	}
	want := map[string]any{"initial": true, "previous": nil, "source": "config", "host": "replica-1", "release": "v1.2.3"}
	for k, v := range want {
		if ds[0][k] != v {
			t.Errorf("%s = %v, want %v", k, ds[0][k], v)
		}
	}
	if !strings.Contains(string(evs[0].Details), `{"key":"auth.anonymousRead","to":false}`) {
		t.Errorf("the initial record %s lists no key with its value", evs[0].Details)
	}
}

// TestRecordConfigChange: a start with another configuration writes one row of the keys that
// changed with their values before and after, and the fingerprint before.
func TestRecordConfigChange(t *testing.T) {
	r := newRecorder(t)
	a := recordedWith("https://art.example.com", nil)
	b := recordedWith("https://art.example.com", func(c *config.Config) { c.Auth.AnonymousRead, c.Keys.RetireGrace = true, 0 })
	for _, c := range []*config.Config{a, b} {
		if err := r.recordConfig(t.Context(), c); err != nil {
			t.Fatal(err)
		}
	}
	evs, ds := configRows(t, r.st)
	want := `"changes":[{"key":"auth.anonymousRead","from":false,"to":true},{"key":"keys.retireGrace","from":"720h0m0s","to":"0s"}]`
	if len(evs) != 2 || evs[1].Subject != fingerprint(t, b) || ds[1]["previous"] != fingerprint(t, a) || ds[1]["initial"] != nil ||
		!strings.Contains(string(evs[1].Details), want) {
		t.Fatalf("the row of the change: %s, want %s", evs[len(evs)-1].Details, want)
	}
	cur, err := r.st.GetConfigState(t.Context())
	if err != nil || cur.Fingerprint != fingerprint(t, b) || cur.Host != "replica-1" || cur.Release != "v1.2.3" {
		t.Fatalf("config_state %+v, %v", cur, err)
	}
}

// TestRecordConfigReplicas: replicas that start together with one new configuration write one
// row.
func TestRecordConfigReplicas(t *testing.T) {
	r := newRecorder(t)
	c := recordedWith("https://registry.example.com", nil)
	var wg sync.WaitGroup
	errs := make([]error, 6)
	for i := range errs {
		wg.Go(func() {
			ri := r
			ri.host = fmt.Sprintf("replica-%d", i)
			errs[i] = ri.recordConfig(t.Context(), c)
		})
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Errorf("replica %d: %v", i, err)
		}
	}
	if evs, _ := configRows(t, r.st); len(evs) != 1 {
		t.Errorf("%d rows after replicas started together, want 1", len(evs))
	}
}

// TestConfigDetails: the changes that would take the details past maxConfigDetails are left out,
// and counted in truncated.
func TestConfigDetails(t *testing.T) {
	var changes []config.Change
	for i := range 2000 {
		changes = append(changes, config.Change{Key: fmt.Sprintf("upstreams[u%04d].url", i),
			To: json.RawMessage(`"https://example.com/` + strings.Repeat("x", 40) + `"`)})
	}
	raw, listed, err := configDetails(map[string]any{"source": "config", "host": "h"}, changes)
	if err != nil {
		t.Fatal(err)
	}
	var d struct {
		Changes   []config.Change `json:"changes"`
		Truncated int             `json:"truncated"`
	}
	if err := json.Unmarshal(raw, &d); err != nil {
		t.Fatal(err)
	}
	if len(raw) > maxConfigDetails || listed == 0 || len(d.Changes) != listed || d.Truncated != len(changes)-listed {
		t.Errorf("%d bytes, %d listed, %d truncated", len(raw), len(d.Changes), d.Truncated)
	}
	raw, _, _ = configDetails(map[string]any{}, nil)
	if string(raw) != `{"changes":[]}` {
		t.Errorf("no change: %s", raw)
	}
}

// TestServeRecordsConfig: serve records its configuration before it listens, here before it stops
// on a bootstrap token it refuses; a start with the same configuration writes nothing, one with
// another server.externalURL writes its change.
func TestServeRecordsConfig(t *testing.T) {
	dir := t.TempDir()
	cfg := writeConfig(t, dir)
	short := filepath.Join(dir, "token")
	if err := os.WriteFile(short, []byte("too-short\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, url := range []string{"", "", "https://other.example.com"} {
		env := map[string]string{"LAJSTROM_BOOTSTRAP_TOKEN_FILE": short, "LAJSTROM_SERVER_EXTERNAL_URL": url}
		if _, _, err := run(t, env, "serve", "--config", cfg); err == nil || !strings.Contains(err.Error(), "auth.bootstrap.tokenFile") {
			t.Fatalf("serve: %v, want the refusal of the token file", err)
		}
	}
	evs, ds := configRows(t, openTestStore(t, "file:"+filepath.Join(dir, "lajstrom.db")))
	if len(evs) != 2 || ds[0]["initial"] != true || !strings.Contains(string(evs[1].Details),
		`{"key":"server.externalURL","from":"https://art.example.com","to":"https://other.example.com"}`) {
		t.Fatalf("rows %+v, want the initial record and the change of server.externalURL", evs)
	}
}
