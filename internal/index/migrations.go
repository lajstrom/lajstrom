package index

import (
	"cmp"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"slices"
	"strconv"
	"strings"
)

// Dialects: the values of database.driver and the directories of the embedded migrations,
// migrations/<dialect>/NNNN_<name>.sql.
const (
	DialectSQLite   = "sqlite"
	DialectPostgres = "postgres"
)

//go:embed migrations/sqlite/*.sql migrations/postgres/*.sql
var migrationFiles embed.FS

// migration is one embedded migration file.
type migration struct {
	version int
	name    string // file name, for errors
	sql     string
}

// MigrationVersions returns the versions of the migrations embedded for dialect, ascending.
// Both dialects have the same set.
func MigrationVersions(dialect string) ([]int, error) {
	ms, err := migrations(dialect)
	if err != nil {
		return nil, err
	}
	versions := make([]int, len(ms))
	for i, m := range ms {
		versions[i] = m.version
	}
	return versions, nil
}

// migrations reads the embedded migrations of dialect, ordered by version.
func migrations(dialect string) ([]migration, error) {
	if _, ok := runners[dialect]; !ok {
		return nil, fmt.Errorf("index: unknown dialect %q (want %s or %s)", dialect, DialectSQLite, DialectPostgres)
	}
	dir := "migrations/" + dialect
	entries, err := fs.ReadDir(migrationFiles, dir)
	if err != nil {
		return nil, fmt.Errorf("index: migrations: %w", err)
	}
	ms := make([]migration, 0, len(entries))
	for _, e := range entries {
		v, err := migrationVersion(e.Name())
		if err != nil {
			return nil, err
		}
		body, err := fs.ReadFile(migrationFiles, dir+"/"+e.Name())
		if err != nil {
			return nil, fmt.Errorf("index: migrations: %w", err)
		}
		ms = append(ms, migration{version: v, name: dialect + "/" + e.Name(), sql: string(body)})
	}
	slices.SortFunc(ms, func(a, b migration) int { return cmp.Compare(a.version, b.version) })
	for i := 1; i < len(ms); i++ {
		if ms[i].version == ms[i-1].version {
			return nil, fmt.Errorf("index: migrations %s and %s have the same version", ms[i-1].name, ms[i].name)
		}
	}
	return ms, nil
}

// migrationVersion returns the version of a migration file named NNNN_<name>.sql.
func migrationVersion(file string) (int, error) {
	prefix, rest, ok := strings.Cut(file, "_")
	if ok && len(prefix) == 4 && strings.HasSuffix(rest, ".sql") && len(rest) > len(".sql") {
		if v, err := strconv.Atoi(prefix); err == nil && v > 0 {
			return v, nil
		}
	}
	return 0, errors.New("index: migration file " + strconv.Quote(file) + " is not named NNNN_<name>.sql")
}
