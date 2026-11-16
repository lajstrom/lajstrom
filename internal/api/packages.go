package api

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/lajstrom/lajstrom/internal/auth"
	"github.com/lajstrom/lajstrom/internal/format"
	"github.com/lajstrom/lajstrom/internal/index"
)

// PackageList is the answer of GET …/packages: a page of the repo's packages by name, and in
// Next the cursor of the page after it, absent on the last page.
type PackageList struct {
	Packages []PackageEntry `json:"packages"`
	Next     string         `json:"next,omitempty"`
}

// PackageEntry is a package as the API answers it (docs/api.md, List packages): its row and
// its facts, read from the derived tables.
type PackageEntry struct {
	Name      string    `json:"name"`
	NameOCI   string    `json:"nameOci"`
	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"` // the publish time of its newest manifest
	// LatestVersion is the version the format's client installs by default (format.LatestPicker),
	// LatestVariants that version's variants but "" in byte order; nil and empty when there is
	// none.
	LatestVersion  *string  `json:"latestVersion"`
	LatestVariants []string `json:"latestVariants"`
	// LastPublished is the first version of its versions list; nil without versions.
	LastPublished *string `json:"lastPublished"`
	Versions      int64   `json:"versions"`
	Manifests     int64   `json:"manifests"`
	Size          int64   `json:"size"` // of the file layers of all its manifests
	// Summary and Attrs are those of the head manifest, the first variant of LatestVersion, or
	// of LastPublished when it is nil; Summary nil when it has none.
	Summary *string           `json:"summary"`
	Attrs   map[string]string `json:"attrs"`
	// Pointers are its pointers that name one of its versions, with the version.
	Pointers []PackagePointer `json:"pointers"`
}

// PackagePointer is a pointer of a package with the version it names: an npm dist-tag.
type PackagePointer struct {
	format.Pointer
	Version string `json:"version"`
}

// PackageDetail is the answer of GET …/packages/{name} (docs/api.md, Get a package): the
// package's row of PackageList, the OCI repository of its manifests as VersionDetail has it, and
// the metadata layer of its head manifest, null when it is absent or omitted for size.
type PackageDetail struct {
	PackageEntry
	OCIRepository  string          `json:"ociRepository"`
	LatestMetadata json.RawMessage `json:"latestMetadata"`
}

// packageEntry is the PackageEntry of a package and its facts.
func packageEntry(p index.Package, f index.PackageFacts) PackageEntry {
	e := PackageEntry{Name: p.Name, NameOCI: p.NameOCI, CreatedAt: p.CreatedAt.UTC(), UpdatedAt: p.CreatedAt.UTC(),
		LatestVariants: []string{}, Versions: f.Versions, Manifests: f.Manifests, Size: f.Size, Attrs: map[string]string{},
		Pointers: []PackagePointer{}}
	if !f.UpdatedAt.IsZero() {
		e.UpdatedAt = f.UpdatedAt.UTC()
	}
	if f.Latest != "" {
		e.LatestVersion = &f.Latest
		for _, v := range f.LatestVariants {
			if v != "" {
				e.LatestVariants = append(e.LatestVariants, v)
			}
		}
	}
	if f.LastPublished != "" {
		e.LastPublished = &f.LastPublished
	}
	if f.Head != nil {
		if f.Head.Summary != "" {
			e.Summary = &f.Head.Summary
		}
		e.Attrs = orEmpty(f.Head.Attrs)
	}
	for _, s := range f.Tags {
		e.Pointers = append(e.Pointers, PackagePointer{Pointer: format.Pointer{Kind: s.Kind, Name: s.Key}, Version: s.Value})
	}
	return e
}

// latestOf returns the index.LatestFunc of pkgs, whose repos have the formats of formats by repo
// id: a package's latest version is the one its format's plugin picks (format.LatestPicker), and
// the newest by VersionKey for a format whose plugin picks none (raw).
func (h *handler) latestOf(pkgs []index.Package, formats map[int64]string) index.LatestFunc {
	pickers := map[int64]format.LatestPicker{}
	for _, p := range pkgs {
		if lp, ok := h.repos.latestPicker(formats[p.RepoID]); ok {
			pickers[p.ID] = lp
		}
	}
	return func(id int64, versions []string, state []index.JournalState) string {
		if lp, ok := pickers[id]; ok {
			return lp.LatestVersion(versions, state)
		}
		return versions[0]
	}
}

// packageEntries returns the PackageEntry of each of pkgs, with the facts of all of them read at
// once, the latest versions picked by latest.
func packageEntries(ctx context.Context, rd index.Reader, pkgs []index.Package, latest index.LatestFunc) ([]PackageEntry, error) {
	ids := make([]int64, len(pkgs))
	for i, p := range pkgs {
		ids[i] = p.ID
	}
	facts, err := rd.ListPackageFacts(ctx, ids, latest)
	if err != nil {
		return nil, fmt.Errorf("api: the facts of %d packages: %w", len(pkgs), err)
	}
	out := make([]PackageEntry, len(pkgs))
	for i, p := range pkgs {
		out[i] = packageEntry(p, facts[i])
	}
	return out, nil
}

// getPackage answers GET /api/v1/repos/{format}/{repo}/packages/{name}: the package's row of the
// list with its OCI repository and the metadata of its head manifest, read from the index. It
// needs read; a repo or a package that does not exist is 404, and so is any package of a group,
// which has none.
func (h *handler) getPackage(_ http.ResponseWriter, r *http.Request) (int, any, error) {
	f, name := r.PathValue("format"), r.PathValue("repo")
	if err := h.allow(r, auth.ActionRead, f, name); err != nil {
		return 0, nil, err
	}
	ctx := r.Context()
	repo, err := h.repos.Index.GetRepo(ctx, f, name)
	if err != nil {
		return 0, nil, repoError("read a package of", f, name, err)
	}
	pkg, err := h.packageOf(r, repo)
	if err != nil {
		return 0, nil, err
	}
	facts, err := h.repos.Index.ListPackageFacts(ctx, []int64{pkg.ID}, h.latestOf([]index.Package{*pkg}, map[int64]string{repo.ID: repo.Format}))
	if err != nil {
		return 0, nil, fmt.Errorf("api: the facts of %q in %s/%s: %w", pkg.Name, f, name, err)
	}
	out := PackageDetail{PackageEntry: packageEntry(*pkg, facts[0]), OCIRepository: repo.Format + "/" + repo.Name + "/" + pkg.NameOCI}
	if head := facts[0].Head; head != nil {
		v, err := h.repos.Index.GetVersion(ctx, pkg.ID, head.Version, head.Variant)
		switch {
		case err == nil:
			out.LatestMetadata = v.Metadata
		case !errors.Is(err, index.ErrNotFound): // deleted since: no metadata
			return 0, nil, fmt.Errorf("api: the head of %q in %s/%s: %w", pkg.Name, f, name, err)
		}
	}
	return http.StatusOK, out, nil
}

// VersionList is the answer of GET …/packages/{name}/versions: a page of the package's
// versions, newest first, and in Next the cursor of the page after it.
type VersionList struct {
	Versions []VersionEntry `json:"versions"`
	Next     string         `json:"next,omitempty"`
}

// VersionEntry is a version with its variants, as the API answers it (docs/api.md, List
// versions): PublishedAt is the publish time of its first variant, LastModified of its newest.
// Yanked is the yank of a pypi version, nil when it is not yanked.
type VersionEntry struct {
	Version      string           `json:"version"`
	PublishedAt  time.Time        `json:"publishedAt"`
	LastModified time.Time        `json:"lastModified"`
	Variants     []VariantEntry   `json:"variants"`
	Pointers     []format.Pointer `json:"pointers"`
	Yanked       *Yank            `json:"yanked,omitempty"`
}

// VariantEntry is one manifest of a version.
type VariantEntry struct {
	Variant     string    `json:"variant"`
	Tag         string    `json:"tag"`
	Digest      string    `json:"digest"`
	PublishedAt time.Time `json:"publishedAt"`
}

// VersionDetail is the answer of GET …/packages/{name}/versions/{version} (docs/api.md, Get a
// version): the version's row of VersionList with its variants in detail, and OCIRepository,
// the OCIRepository of its first variant.
type VersionDetail struct {
	Version       string           `json:"version"`
	PublishedAt   time.Time        `json:"publishedAt"`
	LastModified  time.Time        `json:"lastModified"`
	Variants      []VariantDetail  `json:"variants"`
	Pointers      []format.Pointer `json:"pointers"`
	Yanked        *Yank            `json:"yanked,omitempty"`
	OCIRepository string           `json:"ociRepository"`
}

// VariantDetail is one manifest of a version with what the index has of it: its versions row
// and its files rows (index-schema.md, derived), all of which reindex rebuilds.
type VariantDetail struct {
	VariantEntry
	ArtifactType string            `json:"artifactType"`
	Attrs        map[string]string `json:"attrs"`    // lajstrom.* annotations by their full key
	Metadata     json.RawMessage   `json:"metadata"` // the metadata layer; null when absent or omitted for size
	Degraded     bool              `json:"degraded"`
	IndexedAt    time.Time         `json:"indexedAt"`
	Files        []FileEntry       `json:"files"` // in manifest order
	// OCIRepository is <format>/<repo>/<package path>, the OCI repository below the instance
	// prefix that holds the manifest (the <name> of /v2): index.ManifestPath, which differs
	// between the variants of a version split across two forms of its package path (#642).
	OCIRepository string `json:"ociRepository"`
}

// FileEntry is one file layer of a manifest.
type FileEntry struct {
	Name      string            `json:"name"`
	MediaType string            `json:"mediaType"`
	Digest    string            `json:"digest"`
	Size      int64             `json:"size"`
	Attrs     map[string]string `json:"attrs"` // the layer's annotations
}

// getVersion answers GET /api/v1/repos/{format}/{repo}/packages/{name}/versions/{version}: the
// version with every variant, or the one ?variant= names, each with its files, read from the
// index alone. The version's times and pointers are those of all its variants either way. It
// needs read; a repo, package, version or variant that does not exist is 404, and so is any
// version of a group, which has none.
func (h *handler) getVersion(_ http.ResponseWriter, r *http.Request) (int, any, error) {
	f, name := r.PathValue("format"), r.PathValue("repo")
	if err := h.allow(r, auth.ActionRead, f, name); err != nil {
		return 0, nil, err
	}
	var variant *string
	if v, ok := r.URL.Query()["variant"]; ok {
		variant = &v[0]
	}
	ctx := r.Context()
	repo, err := h.repos.Index.GetRepo(ctx, f, name)
	if err != nil {
		return 0, nil, repoError("read a version of", f, name, err)
	}
	pkg, rows, err := h.versionOf(ctx, repo, r.PathValue("name"), r.PathValue("version"), nil)
	if err != nil {
		return 0, nil, err
	}
	states, err := h.repos.Index.ListJournalState(ctx, pkg.ID)
	if err != nil {
		return 0, nil, fmt.Errorf("api: dist-tags of %q in %s/%s: %w", pkg.Name, repo.Format, repo.Name, err)
	}
	v := groupVersions(rows, states)[0] // the times of every variant, whatever ?variant= keeps
	if variant != nil {
		rows = slices.DeleteFunc(rows, func(row index.Version) bool { return row.Variant != *variant })
		if len(rows) == 0 {
			return 0, nil, &clientError{msg: fmt.Sprintf("package %q of %s/%s has no version %q with the variant %q",
				pkg.Name, repo.Format, repo.Name, v.Version, *variant), err: index.ErrNotFound}
		}
	}
	out := VersionDetail{Version: v.Version, PublishedAt: v.PublishedAt, LastModified: v.LastModified,
		Variants: make([]VariantDetail, 0, len(rows)), Pointers: v.Pointers, Yanked: v.Yanked}
	slices.SortFunc(rows, func(a, b index.Version) int { return strings.Compare(a.Variant, b.Variant) })
	for _, row := range rows {
		files, err := h.repos.Index.ListFiles(ctx, row.ID)
		if err != nil {
			return 0, nil, fmt.Errorf("api: files of %s of %q in %s/%s: %w", row.Digest, pkg.Name, repo.Format, repo.Name, err)
		}
		d := variantDetail(row, files)
		d.OCIRepository = repo.Format + "/" + repo.Name + "/" + index.ManifestPath(*pkg, row)
		out.Variants = append(out.Variants, d)
	}
	out.OCIRepository = out.Variants[0].OCIRepository
	return http.StatusOK, out, nil
}

// variantDetail is the VariantDetail of a versions row and its files rows.
func variantDetail(row index.Version, files []index.File) VariantDetail {
	d := VariantDetail{
		VariantEntry: VariantEntry{Variant: row.Variant, Tag: row.Tag, Digest: row.Digest.String(), PublishedAt: row.CreatedAt.UTC()},
		ArtifactType: row.ArtifactType, Attrs: orEmpty(row.Attrs), Degraded: row.Degraded, IndexedAt: row.IndexedAt.UTC(),
		Files: make([]FileEntry, len(files)),
	}
	if len(row.Metadata) > 0 {
		d.Metadata = row.Metadata
	}
	for i, file := range files {
		d.Files[i] = FileEntry{Name: file.Name, MediaType: file.MediaType, Digest: file.Digest.String(), Size: file.Size,
			Attrs: orEmpty(file.Attrs)}
	}
	return d
}

// orEmpty returns m, or an empty map when it is nil, so that it is {} in JSON and never null.
func orEmpty(m map[string]string) map[string]string {
	if m == nil {
		return map[string]string{}
	}
	return m
}

// listPackages answers GET /api/v1/repos/{format}/{repo}/packages: a page of the repo's packages
// whose name starts with ?prefix, ordered by name, ?limit of them after the name of ?cursor. The
// cursor is that name, so packages published or deleted meanwhile shift nothing. It needs read.
func (h *handler) listPackages(_ http.ResponseWriter, r *http.Request) (int, any, error) {
	f, name := r.PathValue("format"), r.PathValue("repo")
	if err := h.allow(r, auth.ActionRead, f, name); err != nil {
		return 0, nil, err
	}
	limit, after, err := page(r)
	if err != nil {
		return 0, nil, err
	}
	ctx := r.Context()
	repo, err := h.repos.Index.GetRepo(ctx, f, name)
	if err != nil {
		return 0, nil, repoError("list the packages of", f, name, err)
	}
	// One more than the page: whether another follows. The store orders by bytes, as the
	// cursor compares.
	pkgs, err := h.repos.Index.ListPackagesAfter(ctx, repo.ID, r.URL.Query().Get("prefix"), after, limit+1)
	if err != nil {
		return 0, nil, fmt.Errorf("api: packages of %s/%s: %w", f, name, err)
	}
	list := PackageList{}
	if len(pkgs) > limit {
		pkgs = pkgs[:limit]
		list.Next = cursor(pkgs[limit-1].Name)
	}
	latest := h.latestOf(pkgs, map[int64]string{repo.ID: repo.Format})
	if list.Packages, err = packageEntries(ctx, h.repos.Index, pkgs, latest); err != nil {
		return 0, nil, err
	}
	return http.StatusOK, list, nil
}

// listVersions answers GET /api/v1/repos/{format}/{repo}/packages/{name}/versions: a page of the
// package's versions, newest first (versionKey), ?limit of them after the version of ?cursor.
// Each ?without=<kind> keeps the variants without an attachment of that kind (#89); a kind that
// is none of attachmentKinds is 400. It needs read; a repo or a package that does not exist is
// 404.
func (h *handler) listVersions(_ http.ResponseWriter, r *http.Request) (int, any, error) {
	f, name := r.PathValue("format"), r.PathValue("repo")
	if err := h.allow(r, auth.ActionRead, f, name); err != nil {
		return 0, nil, err
	}
	limit, after, err := page(r)
	if err != nil {
		return 0, nil, err
	}
	var from *index.VersionKey
	if after != "" {
		k, err := parseVersionKey(after)
		if err != nil {
			return 0, nil, invalid("cursor: %q is not the next of a page of this list", r.URL.Query().Get("cursor"))
		}
		from = &index.VersionKey{CreatedAt: k.At, Version: k.Version}
	}
	without := r.URL.Query()["without"]
	for _, k := range without {
		if !slices.Contains(attachmentKinds, k) {
			return 0, nil, invalid("without: %q is not one of %v", k, attachmentKinds)
		}
	}
	ctx := r.Context()
	repo, err := h.repos.Index.GetRepo(ctx, f, name)
	if err != nil {
		return 0, nil, repoError("list the versions of", f, name, err)
	}
	pkg, err := h.packageOf(r, repo)
	if err != nil {
		return 0, nil, err
	}
	versions, err := listedVersions(ctx, h.repos.Index, repo, pkg, from, limit+1, without...)
	if err != nil {
		return 0, nil, err
	}
	list := VersionList{Versions: []VersionEntry{}}
	for i, v := range versions {
		if i == limit {
			list.Next = cursor(keyOf(list.Versions[i-1]).String())
			break
		}
		list.Versions = append(list.Versions, v)
	}
	return http.StatusOK, list, nil
}

// packageOf returns the package of the path's {name} in repo: index.ErrNotFound naming it when
// there is none.
func (h *handler) packageOf(r *http.Request, repo *index.Repo) (*index.Package, error) {
	name := r.PathValue("name")
	pkg, err := h.repos.Index.GetPackage(r.Context(), repo.ID, name)
	switch {
	case errors.Is(err, index.ErrNotFound):
		return nil, &clientError{msg: fmt.Sprintf("%s/%s has no package %q", repo.Format, repo.Name, name), err: err}
	case err != nil:
		return nil, fmt.Errorf("api: package %q of %s/%s: %w", name, repo.Format, repo.Name, err)
	}
	return pkg, nil
}

// listedVersions returns limit versions of pkg after the key from (nil: from the newest), newest
// first, with their variants and pointers; with without, attachment kinds, the variants without
// an attachment of any of them.
func listedVersions(ctx context.Context, rd index.Reader, repo *index.Repo, pkg *index.Package, from *index.VersionKey, limit int,
	without ...string) ([]VersionEntry, error) {
	rows, err := rd.ListVersionPage(ctx, pkg.ID, from, limit, without...)
	if err != nil {
		return nil, fmt.Errorf("api: versions of %q in %s/%s: %w", pkg.Name, repo.Format, repo.Name, err)
	}
	states, err := rd.ListJournalState(ctx, pkg.ID)
	if err != nil {
		return nil, fmt.Errorf("api: dist-tags of %q in %s/%s: %w", pkg.Name, repo.Format, repo.Name, err)
	}
	return groupVersions(rows, states), nil
}

// groupVersions groups the rows of a package's versions by version, each with its variants by
// variant, the pointers of states that name it and its yank, ordered newest first (versionKey).
func groupVersions(rows []index.Version, states []index.JournalState) []VersionEntry {
	byVersion := map[string]*VersionEntry{}
	var out []*VersionEntry
	for _, row := range rows {
		at := row.CreatedAt.UTC()
		v := byVersion[row.Version]
		if v == nil {
			v = &VersionEntry{Version: row.Version, PublishedAt: at, LastModified: at,
				Pointers: append([]format.Pointer{}, format.PointersOf(states, row.Version)...), Yanked: yankOf(states, row.Version)}
			byVersion[row.Version] = v
			out = append(out, v)
		}
		v.Variants = append(v.Variants, VariantEntry{Variant: row.Variant, Tag: row.Tag, Digest: row.Digest.String(), PublishedAt: at})
		if at.Before(v.PublishedAt) {
			v.PublishedAt = at
		}
		if at.After(v.LastModified) {
			v.LastModified = at
		}
	}
	versions := make([]VersionEntry, len(out))
	for i, v := range out {
		slices.SortFunc(v.Variants, func(a, b VariantEntry) int { return strings.Compare(a.Variant, b.Variant) })
		versions[i] = *v
	}
	slices.SortFunc(versions, func(a, b VersionEntry) int { return keyOf(a).compare(keyOf(b)) })
	return versions
}

// versionKey is the place of a version in the order of the list: the publish time of its first
// variant, then the version string, both descending.
type versionKey struct {
	At      time.Time
	Version string
}

func keyOf(v VersionEntry) versionKey { return versionKey{At: v.PublishedAt, Version: v.Version} }

// compare orders k before o when k is newer, or as new with the greater version string.
func (k versionKey) compare(o versionKey) int {
	return cmp.Or(o.At.Compare(k.At), strings.Compare(o.Version, k.Version))
}

// String is the key as a cursor holds it: the time in RFC 3339 with nanoseconds, |, the version.
func (k versionKey) String() string { return k.At.UTC().Format(time.RFC3339Nano) + "|" + k.Version }

func parseVersionKey(s string) (*versionKey, error) {
	at, version, ok := strings.Cut(s, "|")
	if !ok || version == "" {
		return nil, errors.New("no |")
	}
	t, err := time.Parse(time.RFC3339Nano, at)
	if err != nil {
		return nil, err
	}
	return &versionKey{At: t, Version: version}, nil
}
