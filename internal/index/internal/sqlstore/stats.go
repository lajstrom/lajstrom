package sqlstore

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/lajstrom/lajstrom/internal/index"
)

// RepoStats returns the counts of the derived tables of each repo of repoIDs that has a
// package (docs/spec/index-schema.md, A repo's stats): two statements for every touchBatch
// repos, one over packages and versions grouped by package and then by repo, one over files,
// each on the unique and plain indexes the tables have. A repo without a package has no row.
func (c *conn) RepoStats(ctx context.Context, repoIDs []int64) ([]index.RepoStats, error) {
	out := []index.RepoStats{}
	for chunk := range slices.Chunk(repoIDs, touchBatch) {
		args := make([]any, 0, len(chunk))
		for _, id := range chunk {
			args = append(args, id)
		}
		in := "(" + strings.Repeat(", ?", len(chunk))[2:] + ")"
		what := fmt.Sprintf("the stats of %d repos", len(chunk))
		stats, err := getAll(ctx, c, what, func(s *index.RepoStats) []any {
			return []any{&s.RepoID, &s.Packages, &s.Versions, &s.Manifests, nullTimeDest{&s.LastPublishedAt}}
		},
			"SELECT repo_id, COUNT(*), CAST(SUM(versions) AS BIGINT), CAST(SUM(manifests) AS BIGINT), MAX(last_at) FROM"+
				" (SELECT p.repo_id, p.id, COUNT(DISTINCT v.version) AS versions, COUNT(v.id) AS manifests,"+
				" MAX(v.created_at) AS last_at FROM packages p LEFT JOIN versions v ON v.package_id = p.id"+
				" WHERE p.repo_id IN "+in+" GROUP BY p.repo_id, p.id) s GROUP BY repo_id", args...)
		if err != nil {
			return nil, err
		}
		type fileCount struct{ repoID, files, size int64 }
		files, err := getAll(ctx, c, what, func(f *fileCount) []any { return []any{&f.repoID, &f.files, &f.size} },
			"SELECT p.repo_id, COUNT(*), CAST(COALESCE(SUM(f.size), 0) AS BIGINT) FROM files f"+
				" JOIN versions v ON v.id = f.version_id JOIN packages p ON p.id = v.package_id"+
				" WHERE p.repo_id IN "+in+" GROUP BY p.repo_id", args...)
		if err != nil {
			return nil, err
		}
		byRepo := make(map[int64]fileCount, len(files))
		for _, f := range files {
			byRepo[f.repoID] = f
		}
		for i := range stats {
			f := byRepo[stats[i].RepoID]
			stats[i].Files, stats[i].Size = f.files, f.size
		}
		out = append(out, stats...)
	}
	return out, nil
}
