package sqlstore

import (
	"context"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/lajstrom/lajstrom/internal/index"
)

// Search returns the hits of one rank of the search after its key, at most its limit, in the
// order of (repos.format, repos.name, packages.name), then of the version, the variant and the
// file's name for a digest. A digest's prefix is a range of versions_digest or files_digest:
// every digest that starts with sha256:<hex> is at least it and below sha256:<hex>g, since g
// comes after every hex digit. A name or a summary reads every package of the repos, folded as
// PackageQuery.Text.
func (c *conn) Search(ctx context.Context, q index.SearchQuery) ([]index.SearchHit, error) {
	if q.Limit < 1 || (q.RepoIDs != nil && len(q.RepoIDs) == 0) {
		return []index.SearchHit{}, nil
	}
	var w where
	if q.RepoIDs != nil {
		args := make([]any, len(q.RepoIDs))
		for i, id := range q.RepoIDs {
			args[i] = id
		}
		w.add("p.repo_id IN ("+strings.Repeat(", ?", len(q.RepoIDs))[2:]+")", args...)
	}
	from, cols, keyCols := "packages p JOIN repos r ON r.id = p.repo_id", "r.format, r.name, "+packageColsP, []string{"r.format", "r.name", "p.name"}
	switch q.Rank {
	case index.RankExact, index.RankPrefix, index.RankSubstring, index.RankSummary:
		c.nameCond(&w, q.Rank, foldASCII(q.Text))
	case index.RankManifest:
		from, cols = digestFrom, cols+", "+versionColsNoMetadata
		keyCols = append(keyCols, "v.version", "v.variant")
		w.add("v.digest >= ? AND v.digest < ?", q.Text, q.Text+"g")
	case index.RankFile:
		from, cols = digestFrom+" JOIN files f ON f.version_id = v.id", cols+", "+versionColsNoMetadata+", "+fileColsF
		keyCols = append(keyCols, "v.version", "v.variant", "f.name")
		w.add("f.digest >= ? AND f.digest < ?", q.Text, q.Text+"g")
	default:
		return nil, fmt.Errorf("index: search: no rank %d", q.Rank)
	}
	if k := q.After; k != nil {
		key := []any{k.Format, k.Repo, k.Package, k.Version, k.Variant, k.File}[:len(keyCols)]
		w.add("("+strings.Join(keyCols, ", ")+") > ("+strings.Repeat(", ?", len(keyCols))[2:]+")", key...)
	}
	hits, err := getAll(ctx, c, fmt.Sprintf("the search of rank %d", q.Rank), func(h *index.SearchHit) []any { return hitDests(h, q.Rank) },
		"SELECT "+cols+" FROM "+from+w.clause()+" ORDER BY "+strings.Join(keyCols, ", ")+" LIMIT ?", append(w.args, q.Limit)...)
	for i := range hits {
		h := &hits[i]
		h.Key.Package = h.Package.Name
		if h.Version != nil {
			h.Key.Version, h.Key.Variant = h.Version.Version, h.Version.Variant
		}
		if h.File != nil {
			h.Key.File = h.File.Name
		}
	}
	return hits, err
}

// digestFrom is the join of a hit by a digest: its versions row, its package and its repo.
const digestFrom = "versions v JOIN packages p ON p.id = v.package_id JOIN repos r ON r.id = p.repo_id"

// nameCond adds to w the condition of a rank of the search by name, text folded: the name is
// it, starts with it but is not it, contains it but does not start with it, or does not
// contain it and a summary of the package does.
func (c *conn) nameCond(w *where, rank index.SearchRank, text string) {
	name, pattern, n := c.fold("p.name"), "%"+escapeLike(text)+"%", utf8.RuneCountInString(text)
	switch rank {
	case index.RankExact:
		w.add(name+" = ?", text)
	case index.RankPrefix:
		w.add("substr("+name+", 1, ?) = ? AND "+name+" <> ?", n, text, text)
	case index.RankSubstring:
		w.add(name+" LIKE ? ESCAPE '\\' AND substr("+name+", 1, ?) <> ?", pattern, n, text)
	case index.RankSummary:
		w.add("NOT ("+name+" LIKE ? ESCAPE '\\') AND EXISTS (SELECT 1 FROM versions v WHERE v.package_id = p.id AND "+
			c.fold("v.summary")+" LIKE ? ESCAPE '\\')", pattern, pattern)
	case index.RankManifest, index.RankFile:
	}
}

// hitDests are the destinations of a hit of rank: the repo's format and name, the package, and
// for a digest the versions row and, of a file, the files row.
func hitDests(h *index.SearchHit, rank index.SearchRank) []any {
	d := append([]any{&h.Key.Format, &h.Key.Repo}, packageDests(&h.Package)...)
	if rank == index.RankManifest || rank == index.RankFile {
		h.Version = &index.Version{}
		d = append(d, versionDests(h.Version)...)
	}
	if rank == index.RankFile {
		h.File = &index.File{}
		d = append(d, fileDests(h.File)...)
	}
	return d
}
