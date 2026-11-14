package index

import (
	"slices"
	"testing"
)

// TestMigrationVersions checks that every dialect embeds the same migrations, numbered from 1
// without gaps.
func TestMigrationVersions(t *testing.T) {
	sqlite, err := MigrationVersions(DialectSQLite)
	if err != nil {
		t.Fatal(err)
	}
	for _, dialect := range []string{DialectPostgres, DialectCockroach} {
		other, err := MigrationVersions(dialect)
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(sqlite, other) {
			t.Errorf("sqlite has migrations %v, %s %v", sqlite, dialect, other)
		}
	}
	for i, v := range sqlite {
		if v != i+1 {
			t.Errorf("migration versions %v are not 1, 2, 3, …", sqlite)
			break
		}
	}
	if len(sqlite) < 9 {
		t.Errorf("migration versions %v, want at least 0001_init to 0009_principal_uid", sqlite)
	}
	if _, err := MigrationVersions("mysql"); err == nil {
		t.Error("MigrationVersions(mysql) succeeded")
	}
}

func TestMigrationVersion(t *testing.T) {
	tests := []struct {
		file string
		want int // 0: error
	}{
		{"0001_init.sql", 1},
		{"0042_add_proxy_meta.sql", 42},
		{"0000_zero.sql", 0},
		{"1_short.sql", 0},
		{"0001.sql", 0},
		{"0001_.sql", 0},
		{"0001_init.txt", 0},
		{"abcd_init.sql", 0},
	}
	for _, tt := range tests {
		t.Run(tt.file, func(t *testing.T) {
			got, err := migrationVersion(tt.file)
			if got != tt.want || (err != nil) != (tt.want == 0) {
				t.Errorf("migrationVersion = %d, %v; want %d", got, err, tt.want)
			}
		})
	}
}
