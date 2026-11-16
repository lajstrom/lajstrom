package sqlstore

import (
	"cmp"
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

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

// sortValues are the values of the sorts other than by name, of the packages of the repo whose
// id is the one parameter: per package_id as v, grouped once over the repo's rows, which both
// engines join with packages at the cost of a pass over those rows. A package without versions
// is not among them (sortValue).
var sortValues = map[index.PackageSort]string{
	index.SortUpdated: "SELECT v.package_id, MAX(v.created_at) AS v FROM versions v JOIN packages q ON q.id = v.package_id" +
		" WHERE q.repo_id = ? GROUP BY v.package_id",
	index.SortVersions: "SELECT v.package_id, COUNT(DISTINCT v.version) AS v FROM versions v JOIN packages q ON q.id = v.package_id" +
		" WHERE q.repo_id = ? GROUP BY v.package_id",
	index.SortSize: "SELECT v.package_id, CAST(SUM(f.size) AS bigint) AS v FROM versions v JOIN packages q ON q.id = v.package_id" +
		" JOIN files f ON f.version_id = v.id WHERE q.repo_id = ? GROUP BY v.package_id",
}

// sortValue is the value of sort of package p, with its row of sortValues as s: its created_at,
// or 0, without versions.
func sortValue(sort index.PackageSort) string {
	if sort == index.SortUpdated {
		return "COALESCE(s.v, p.created_at)"
	}
	return "COALESCE(s.v, 0)"
}

// ListPackagePage returns a page of the packages of a repo that q keeps, in its order after its
// key. By name it reads a range of the index (repo_id, name), as ListPackagesAfter; another sort
// joins its values over the repo (sortValues) with the packages that the filters keep, then
// keeps the page after the key with a row comparison of (value, name).
func (c *conn) ListPackagePage(ctx context.Context, repoID int64, q index.PackageQuery) ([]index.PackagePlace, error) {
	if q.Limit < 1 {
		return []index.PackagePlace{}, nil
	}
	sort := cmp.Or(q.Sort, index.SortName)
	value, ok := sortValues[sort]
	if sort != index.SortName && !ok {
		return nil, fmt.Errorf("index: packages in repo %d: no sort %q", repoID, sort)
	}
	var w where
	w.add("p.repo_id = ?", repoID)
	if q.Prefix != "" { // a name that starts with the prefix is not before it: a bound of the index
		w.add("p.name >= ? AND substr(p.name, 1, ?) = ?", q.Prefix, utf8.RuneCountInString(q.Prefix), q.Prefix)
	}
	if q.Text != "" {
		pattern := "%" + escapeLike(foldASCII(q.Text)) + "%"
		w.add("("+c.fold("p.name")+" LIKE ? ESCAPE '\\' OR EXISTS (SELECT 1 FROM versions v WHERE v.package_id = p.id AND "+
			c.fold("v.summary")+" LIKE ? ESCAPE '\\'))", pattern, pattern)
	}
	if len(q.Attrs) > 0 {
		c.attrsCond(&w, q.Attrs)
	}
	dir, op := "", ">"
	if q.Desc {
		dir, op = " DESC", "<"
	}
	what := fmt.Sprintf("a page of the packages in repo %d", repoID)
	if sort == index.SortName {
		if q.After != nil {
			w.add("p.name "+op+" ?", q.After.Name)
		}
		page, err := getAll(ctx, c, what, func(r *index.PackagePlace) []any { return packageDests(&r.Package) },
			"SELECT "+packageColsP+" FROM packages p"+w.clause()+" ORDER BY p.name"+dir+" LIMIT ?", append(w.args, q.Limit)...)
		return named(page), err
	}
	keyDest := func(r *index.PackagePlace) any { return &r.Key.N }
	var after any
	if sort == index.SortUpdated {
		keyDest = func(r *index.PackagePlace) any { return timeDest{&r.Key.Updated} }
		if q.After != nil {
			after = c.d.EncodeTime(q.After.Updated)
		}
	} else if q.After != nil {
		after = q.After.N
	}
	query := "SELECT " + qualify("t", packageCols) + ", t.k FROM (SELECT " + packageColsP + ", " + sortValue(sort) + " AS k" +
		" FROM packages p LEFT JOIN (" + value + ") s ON s.package_id = p.id" + w.clause() + ") t"
	args := append([]any{repoID}, w.args...)
	if q.After != nil {
		query += " WHERE (t.k, t.name) " + op + " (?, ?)"
		args = append(args, after, q.After.Name)
	}
	page, err := getAll(ctx, c, what, func(r *index.PackagePlace) []any { return append(packageDests(&r.Package), keyDest(r)) },
		query+" ORDER BY t.k"+dir+", t.name"+dir+" LIMIT ?", append(args, q.Limit)...)
	return named(page), err
}

// named sets the name of the key of each package of page.
func named(page []index.PackagePlace) []index.PackagePlace {
	for i := range page {
		page[i].Key.Name = page[i].Package.Name
	}
	return page
}

// attrsCond adds to w the condition of PackageQuery.Attrs: a versions row of the package whose
// attrs have, for every key, one of its values; a key attrKey refuses matches nothing.
func (c *conn) attrsCond(w *where, attrs map[string][]string) {
	conds, args := []string{"v.package_id = p.id"}, []any{}
	for _, key := range slices.Sorted(maps.Keys(attrs)) {
		values := attrs[key]
		if !attrKey(key) || len(values) == 0 {
			w.add("1 = 0")
			return
		}
		conds = append(conds, c.attrText("v.attrs")+" IN ("+strings.Repeat(", ?", len(values))[2:]+")")
		args = append(args, key)
		for _, v := range values {
			args = append(args, v)
		}
	}
	w.add("EXISTS (SELECT 1 FROM versions v WHERE "+strings.Join(conds, " AND ")+")", args...)
}

// ListAttrValues returns the values of key in the attrs of the versions rows of a repo, each
// with the number of packages that have it, sorted here in byte order: on Postgres ->> answers
// text in the database's collation.
func (c *conn) ListAttrValues(ctx context.Context, repoID int64, key string) ([]index.AttrValue, error) {
	if !attrKey(key) {
		return []index.AttrValue{}, nil
	}
	values, err := getAll(ctx, c, fmt.Sprintf("the values of the attr %q in repo %d", key, repoID),
		func(v *index.AttrValue) []any { return []any{&v.Value, &v.Packages} },
		"SELECT t.value, COUNT(DISTINCT t.package_id) FROM (SELECT "+c.attrText("v.attrs")+" AS value, v.package_id"+
			" FROM versions v JOIN packages p ON p.id = v.package_id WHERE p.repo_id = ?) t"+
			" WHERE t.value IS NOT NULL GROUP BY t.value", key, repoID)
	slices.SortFunc(values, func(a, b index.AttrValue) int { return strings.Compare(a.Value, b.Value) })
	return values, err
}

// fold is the dialect's FoldASCII of the expression e.
func (c *conn) fold(e string) string { return fmt.Sprintf(c.d.FoldASCII, e) }

// attrText is the dialect's AttrText of the json column col; its key is a parameter.
func (c *conn) attrText(col string) string { return fmt.Sprintf(c.d.AttrText, col) }

// attrKey reports whether key may be a key of AttrText: letters, digits, '.', '_' and '-'.
func attrKey(key string) bool {
	return key != "" && strings.Trim(key, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789._-") == ""
}

// foldASCII folds the ASCII letters of s to lowercase, as FoldASCII does, and leaves every other
// byte.
func foldASCII(s string) string {
	b := []byte(s)
	for i, ch := range b {
		if 'A' <= ch && ch <= 'Z' {
			b[i] = ch + 'a' - 'A'
		}
	}
	return string(b)
}

// escapeLike escapes the wildcards of LIKE in s, and the escape \ itself.
func escapeLike(s string) string {
	return strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(s)
}
