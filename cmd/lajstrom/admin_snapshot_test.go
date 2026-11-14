package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// writeSnapshotConfig writes a configuration with a SQLite file in dir, keys.masterKeyEnv
// LAJSTROM_MASTER_KEY, and the registries of urls by name, and returns its path.
func writeSnapshotConfig(t *testing.T, dir string, urls map[string]string) string {
	t.Helper()
	cfg := filepath.Join(dir, "lajstrom.yaml")
	body := "server:\n  listen: \":8080\"\n  externalURL: https://art.example.com\ndatabase:\n  driver: sqlite\n  dsn: \"file:" +
		filepath.Join(dir, "lajstrom.db") + "\"\nkeys:\n  masterKeyEnv: LAJSTROM_MASTER_KEY\nregistries:\n"
	for _, name := range []string{"a", "b", "local"} {
		if url, ok := urls[name]; ok {
			body += "  - name: " + name + "\n    url: \"" + url + "\"\n"
		}
	}
	if err := os.WriteFile(cfg, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return cfg
}

// TestAdminSnapshotRefuses: the flags of admin snapshot create and list are checked before the
// database and the registry: the registry of the snapshots among several, the master key of a
// sealed one, and --audit.
func TestAdminSnapshotRefuses(t *testing.T) {
	dir := t.TempDir()
	cfg := writeSnapshotConfig(t, dir, map[string]string{"a": "http://127.0.0.1:1", "b": "http://127.0.0.2:1"})
	runEnv(t, cfg, nil, []step{
		{args: []string{"migrate"}, wantOut: "applied 0001"},
		{args: []string{"admin", "snapshot", "create"}, wantErr: "--registry names the registry of the snapshots: the configuration has 2 (a, b)"},
		{args: []string{"admin", "snapshot", "list"}, wantErr: "--registry names the registry"},
		{args: []string{"admin", "snapshot", "create", "--registry", "c"}, wantErr: `--registry "c" is not a registries[].name`},
		{args: []string{"admin", "snapshot", "create", "--registry", "a"},
			wantErr: "a sealed snapshot needs the master key; --unsealed writes it without: keys: no master key"},
		{args: []string{"admin", "snapshot", "create", "--registry", "a", "--unsealed", "--audit", "3x"},
			wantErr: `--audit "3x": an age such as 90d or 36h, or all`},
		{args: []string{"admin", "snapshot", "create", "--registry", "a", "--unsealed"}, wantErr: "snapshot: "},
	})
}

// TestAuditOptions: --audit is none, every row, or a window that ends now.
func TestAuditOptions(t *testing.T) {
	now := time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC)
	for _, tt := range []struct {
		in    string
		with  bool
		since time.Time
		err   bool
	}{
		{"", false, time.Time{}, false},
		{"all", true, time.Time{}, false},
		{"90d", true, now.Add(-90 * 24 * time.Hour), false},
		{"36h", true, now.Add(-36 * time.Hour), false},
		{"0d", false, time.Time{}, true},
		{"never", false, time.Time{}, true},
	} {
		o, err := auditOptions(tt.in, now)
		if (err != nil) != tt.err || o.WithAudit != tt.with || !o.AuditSince.Equal(tt.since) {
			t.Errorf("auditOptions(%q) = %+v, %v", tt.in, o, err)
		}
	}
}
