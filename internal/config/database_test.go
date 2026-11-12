package config

import (
	"os"
	"path/filepath"
	"testing"
)

// TestDatabaseFile: the file of a SQLite DSN, as SQLite opens it, absolute; a relative one is
// resolved against the working directory (#719); nothing for postgres or a database in memory.
func TestDatabaseFile(t *testing.T) {
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		driver, dsn string
		want        string
	}{
		{"sqlite", "file:lajstrom.db", filepath.Join(wd, "lajstrom.db")},
		{"sqlite", "lajstrom.db", filepath.Join(wd, "lajstrom.db")},
		{"sqlite", "../data/./lajstrom.db?_busy_timeout=1000", filepath.Join(filepath.Dir(wd), "data", "lajstrom.db")},
		{"sqlite", "file:/var/lib/lajstrom/lajstrom.db?_journal_mode=DELETE", "/var/lib/lajstrom/lajstrom.db"},
		{"sqlite", "file:///var/lib/lajstrom/lajstrom.db", "/var/lib/lajstrom/lajstrom.db"},
		{"sqlite", "file://localhost/var/lib/lajstrom/lajstrom.db", "/var/lib/lajstrom/lajstrom.db"},
		{"sqlite", "file:/var/lib/my%20index.db#x", "/var/lib/my index.db"},
		{"sqlite", "/var/lib/my%20index.db", "/var/lib/my%20index.db"},          // a file name is not decoded
		{"sqlite", "lajstrom.db?mode=memory", filepath.Join(wd, "lajstrom.db")}, // the driver drops the query of a file name
		{"sqlite", "file:test?mode=memory&cache=shared", ""},
		{"sqlite", "file::memory:?cache=shared", ""},
		{"sqlite", ":memory:", ""},
		{"sqlite", "file:", ""},
		{"sqlite", "file://example.com/lajstrom.db", ""}, // SQLite refuses the authority itself
		{"postgres", "postgres://db/lajstrom", ""},
		{"postgres", "file:lajstrom.db", ""},
	}
	for _, tt := range tests {
		if got := (Database{Driver: tt.driver, DSN: tt.dsn}).File(); got != tt.want {
			t.Errorf("%s %q: %q, want %q", tt.driver, tt.dsn, got, tt.want)
		}
	}
}
