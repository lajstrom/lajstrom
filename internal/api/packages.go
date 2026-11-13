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

// PackageEntry is a package as the API answers it.
type PackageEntry struct {
	Name      string    `json:"name"`
	CreatedAt time.Time `json:"createdAt"`
}

// VersionList is the answer of GET …/packages/{name}/versions: a page of the package's
// versions, newest first, and in Next the cursor of the page after it.
type VersionList struct {
	Versions []VersionEntry `json:"versions"`
	Next     string         `json:"next,omitempty"`
}

// VersionEntry is a version with its variants, as the API answers it (docs/api.md, List
// versions): PublishedAt is the publish time of its first variant, LastModified of its newest.
type VersionEntry struct {
	Version      string           `json:"version"`
	PublishedAt  time.Time        `json:"publishedAt"`
	LastModified time.Time        `json:"lastModified"`
	Variants     []VariantEntry   `json:"variants"`
	Pointers     []format.Pointer `json:"pointers"`
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
// <format>/<repo>/<package path>, the OCI repository below the instance prefix that holds its
// manifests (the <name> of /v2).
type VersionDetail struct {
	Version       string           `json:"version"`
	PublishedAt   time.Time        `json:"publishedAt"`
	LastModified  time.Time        `json:"lastModified"`
	Variants      []VariantDetail  `json:"variants"`
	Pointers      []format.Pointer `json:"pointers"`
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
		Variants: make([]VariantDetail, 0, len(rows)), Pointers: v.Pointers,
		OCIRepository: repo.Format + "/" + repo.Name + "/" + pkg.NameOCI}
	slices.SortFunc(rows, func(a, b index.Version) int { return strings.Compare(a.Variant, b.Variant) })
	for _, row := range rows {
		files, err := h.repos.Index.ListFiles(ctx, row.ID)
		if err != nil {
			return 0, nil, fmt.Errorf("api: files of %s of %q in %s/%s: %w", row.Digest, pkg.Name, repo.Format, repo.Name, err)
		}
		out.Variants = append(out.Variants, variantDetail(row, files))
	}
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
	list := PackageList{Packages: []PackageEntry{}}
	for i, p := range pkgs {
		if i == limit {
			list.Next = cursor(list.Packages[i-1].Name)
			break
		}
		list.Packages = append(list.Packages, PackageEntry{Name: p.Name, CreatedAt: p.CreatedAt.UTC()})
	}
	return http.StatusOK, list, nil
}

// listVersions answers GET /api/v1/repos/{format}/{repo}/packages/{name}/versions: a page of the
// package's versions, newest first (versionKey), ?limit of them after the version of ?cursor.
// It needs read; a repo or a package that does not exist is 404.
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
	ctx := r.Context()
	repo, err := h.repos.Index.GetRepo(ctx, f, name)
	if err != nil {
		return 0, nil, repoError("list the versions of", f, name, err)
	}
	pkg, err := h.packageOf(r, repo)
	if err != nil {
		return 0, nil, err
	}
	versions, err := listedVersions(ctx, h.repos.Index, repo, pkg, from, limit+1)
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
// first, with their variants and pointers.
func listedVersions(ctx context.Context, rd index.Reader, repo *index.Repo, pkg *index.Package, from *index.VersionKey, limit int) ([]VersionEntry, error) {
	rows, err := rd.ListVersionPage(ctx, pkg.ID, from, limit)
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
// variant and the pointers of states that name it, ordered newest first (versionKey).
func groupVersions(rows []index.Version, states []index.JournalState) []VersionEntry {
	byVersion := map[string]*VersionEntry{}
	var out []*VersionEntry
	for _, row := range rows {
		at := row.CreatedAt.UTC()
		v := byVersion[row.Version]
		if v == nil {
			v = &VersionEntry{Version: row.Version, PublishedAt: at, LastModified: at,
				Pointers: append([]format.Pointer{}, format.PointersOf(states, row.Version)...)}
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
