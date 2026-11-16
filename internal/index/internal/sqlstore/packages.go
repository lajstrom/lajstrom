package sqlstore

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/lajstrom/lajstrom/internal/index"
)

// The package list, one package and search (docs/spec/index-schema.md, The package list, one
// package and search).

// ListPackageFacts returns the facts of the packages with the given ids, one per id in their
// order: the zero facts but PackageID for a package without versions or that does not exist.
// Five statements, whatever the number of ids: the counts and the newest time, the size of the
// files, the distinct versions with their first publish times, the journal state, and the rows
// of each package's head version. latest picks the latest version among the versions; nil, the
// first by VersionKey.
func (c *conn) ListPackageFacts(ctx context.Context, packageIDs []int64, latest index.LatestFunc) ([]index.PackageFacts, error) {
	out := make([]index.PackageFacts, len(packageIDs))
	if len(packageIDs) == 0 {
		return out, nil
	}
	byID := make(map[int64]*index.PackageFacts, len(packageIDs))
	args := make([]any, len(packageIDs))
	for i, id := range packageIDs {
		out[i].PackageID = id
		byID[id] = &out[i]
		args[i] = id
	}
	in := "(" + strings.Repeat(", ?", len(packageIDs))[2:] + ")"
	what := fmt.Sprintf("the facts of %d packages", len(packageIDs))

	type counts struct {
		id                  int64
		versions, manifests int64
		updated             time.Time
	}
	rows, err := getAll(ctx, c, what, func(r *counts) []any {
		return []any{&r.id, &r.versions, &r.manifests, timeDest{&r.updated}}
	}, "SELECT package_id, COUNT(DISTINCT version), COUNT(*), MAX(created_at) FROM versions"+
		" WHERE package_id IN "+in+" GROUP BY package_id", args...)
	if err != nil {
		return nil, err
	}
	for _, r := range rows {
		f := byID[r.id]
		f.Versions, f.Manifests, f.UpdatedAt = r.versions, r.manifests, r.updated
	}

	type size struct{ id, size int64 }
	sizes, err := getAll(ctx, c, what, func(r *size) []any { return []any{&r.id, &r.size} },
		"SELECT v.package_id, CAST(SUM(f.size) AS bigint) FROM versions v JOIN files f ON f.version_id = v.id"+
			" WHERE v.package_id IN "+in+" GROUP BY v.package_id", args...)
	if err != nil {
		return nil, err
	}
	for _, r := range sizes {
		byID[r.id].Size = r.size
	}

	versions, err := c.versionsByKey(ctx, what, in, args)
	if err != nil {
		return nil, err
	}
	states, err := getAll(ctx, c, what, journalStateDests,
		"SELECT "+journalStateCols+" FROM journal_state WHERE package_id IN "+in+" ORDER BY package_id, kind, key", args...)
	if err != nil {
		return nil, err
	}
	state := map[int64][]index.JournalState{}
	for _, s := range states {
		state[s.PackageID] = append(state[s.PackageID], s)
	}
	for i := range out {
		pickVersions(&out[i], versions[out[i].PackageID], state[out[i].PackageID], latest)
	}
	return out, c.headRows(ctx, what, out, byID)
}

// pickVersions sets the last published version of f, the first of its versions vs (newest first
// by VersionKey), its latest version, which latest picks with its journal state (nil: the last
// published one), and its tags, the dist-tags of its state that name one of vs.
func pickVersions(f *index.PackageFacts, vs []string, state []index.JournalState, latest index.LatestFunc) {
	if len(vs) == 0 {
		return
	}
	f.LastPublished, f.Latest = vs[0], vs[0]
	if latest != nil {
		f.Latest = latest(f.PackageID, vs, state)
	}
	if !slices.Contains(vs, f.Latest) {
		f.Latest = ""
	}
	for _, s := range state {
		if s.Kind == index.JournalDistTag && slices.Contains(vs, s.Value) {
			f.Tags = append(f.Tags, s)
		}
	}
}

// headRows reads the rows of the head version of each package of out, its latest version, else
// its last published one, without metadata, ordered by variant: the first is the head, and those
// of the latest version give its variants. One statement.
func (c *conn) headRows(ctx context.Context, what string, out []index.PackageFacts, byID map[int64]*index.PackageFacts) error {
	var conds []string
	var heads []any
	for _, f := range out {
		if f.LastPublished != "" {
			conds = append(conds, "(v.package_id = ? AND v.version = ?)")
			heads = append(heads, f.PackageID, cmp.Or(f.Latest, f.LastPublished))
		}
	}
	if len(conds) == 0 {
		return nil
	}
	rows, err := getAll(ctx, c, what, versionDests,
		"SELECT "+versionColsNoMetadata+" FROM versions v WHERE "+strings.Join(conds, " OR ")+" ORDER BY v.package_id, v.variant", heads...)
	if err != nil {
		return err
	}
	for i := range rows {
		v := &rows[i]
		f := byID[v.PackageID]
		if f.Head == nil {
			f.Head = v
		}
		if v.Version == f.Latest {
			f.LatestVariants = append(f.LatestVariants, v.Variant)
		}
	}
	return nil
}

// versionsByKey returns the distinct versions of each package of the ids in, newest first by
// VersionKey: the earliest created_at of a version's rows, then the version, both descending.
func (c *conn) versionsByKey(ctx context.Context, what, in string, args []any) (map[int64][]string, error) {
	type first struct {
		id      int64
		version string
		at      time.Time
	}
	firsts, err := getAll(ctx, c, what, func(r *first) []any { return []any{&r.id, &r.version, timeDest{&r.at}} },
		"SELECT package_id, version, MIN(created_at) FROM versions WHERE package_id IN "+in+" GROUP BY package_id, version", args...)
	if err != nil {
		return nil, err
	}
	slices.SortFunc(firsts, func(a, b first) int {
		return cmp.Or(cmp.Compare(a.id, b.id), b.at.Compare(a.at), strings.Compare(b.version, a.version))
	})
	versions := map[int64][]string{}
	for _, f := range firsts {
		versions[f.id] = append(versions[f.id], f.version)
	}
	return versions, nil
}

// versionColsNoMetadata is versionColsV with NULL for the metadata, which versionDests reads as
// none.
var versionColsNoMetadata = strings.Replace(versionColsV, "v.metadata", "NULL", 1)
