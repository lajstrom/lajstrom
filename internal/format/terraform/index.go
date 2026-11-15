package terraform

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"

	"github.com/lajstrom/lajstrom/internal/auth"
	"github.com/lajstrom/lajstrom/internal/format"
	"github.com/lajstrom/lajstrom/internal/index"
)

// kindVersions is the index_cache kind of the versions document of a module, whose scope is the
// module's package path.
const kindVersions = "versions"

// versionsDoc is the versions document of a module (docs/spec/http-routing.md §3, terraform), as
// Terraform reads it: one module with its versions.
type versionsDoc struct {
	Modules []versionsModule `json:"modules"`
}

type versionsModule struct {
	Versions []versionEntry `json:"versions"`
}

type versionEntry struct {
	Version string `json:"version"`
}

// RenderIndex renders the versions document of the module whose package path is scope: every
// version of it, by SemVer precedence, lowest first, as compact JSON of kind "versions". A scope
// that names no package of the repo is index.ErrNotFound and renders nothing, so that requests
// for modules that do not exist add no index_cache rows.
func (Plugin) RenderIndex(ctx context.Context, repo index.Repo, scope format.IndexScope, st index.Reader) ([]format.Document, error) {
	pkg, err := index.PackageAt(ctx, st, repo.ID, string(scope))
	if errors.Is(err, index.ErrNotFound) {
		return nil, fmt.Errorf("%w: terraform/%s has no module at %q", index.ErrNotFound, repo.Name, scope)
	}
	if err != nil {
		return nil, fmt.Errorf("terraform: the module at %q: %w", scope, err)
	}
	versions, err := st.ListVersions(ctx, pkg.ID)
	if err != nil {
		return nil, fmt.Errorf("terraform: the versions of %q: %w", pkg.Name, err)
	}
	if len(versions) == 0 {
		return nil, fmt.Errorf("%w: terraform/%s has no version of %q", index.ErrNotFound, repo.Name, pkg.Name)
	}
	names := make([]string, 0, len(versions))
	for _, v := range versions {
		names = append(names, v.Version)
	}
	slices.SortFunc(names, compareVersions)
	doc := versionsDoc{Modules: []versionsModule{{Versions: make([]versionEntry, 0, len(names))}}}
	for _, v := range slices.Compact(names) {
		doc.Modules[0].Versions = append(doc.Modules[0].Versions, versionEntry{Version: v})
	}
	content, err := json.Marshal(doc)
	if err != nil {
		return nil, fmt.Errorf("terraform: encode the versions of %q: %w", pkg.Name, err)
	}
	return []format.Document{{Kind: kindVersions, Path: string(scope) + "/versions", ContentType: jsonContentType, Content: content}}, nil
}

// versions serves the versions document of the module of the request, from index_cache,
// rendered on a miss.
func (h *handlers) versions(w http.ResponseWriter, r *http.Request) {
	repo, ok := h.start(w, r, auth.ActionRead)
	if !ok {
		return
	}
	m, err := module(r, repo)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	c, err := format.CachedDocument(r.Context(), h.deps.Index, Plugin{}, repo, format.IndexScope(m.packagePath()), kindVersions)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	format.ServeDocument(w, r, c, jsonContentType)
}

// compareVersions orders two versions of CheckVersion by SemVer precedence: the major, minor and
// patch numbers, then a version without a pre-release after one with, then the identifiers of
// the pre-releases one by one, numbers before words, numbers by value and words in ASCII order,
// a shorter list first when one is a prefix of the other. Versions outside the grammar, which
// Validate refuses, compare as strings after the others.
func compareVersions(a, b string) int {
	ac, ap, aok := splitVersion(a)
	bc, bp, bok := splitVersion(b)
	if !aok || !bok {
		return cmp.Or(cmp.Compare(boolInt(!aok), boolInt(!bok)), strings.Compare(a, b))
	}
	for i := range ac {
		if c := compareNumbers(ac[i], bc[i]); c != 0 {
			return c
		}
	}
	if len(ap) == 0 || len(bp) == 0 {
		return cmp.Compare(boolInt(len(ap) == 0), boolInt(len(bp) == 0))
	}
	for i := 0; i < len(ap) && i < len(bp); i++ {
		if c := compareIdentifiers(ap[i], bp[i]); c != 0 {
			return c
		}
	}
	return cmp.Compare(len(ap), len(bp))
}

// splitVersion returns the three numbers and the pre-release identifiers of a version of
// CheckVersion; ok is false for another.
func splitVersion(v string) (core [3]string, pre []string, ok bool) {
	if CheckVersion(v) != nil {
		return core, nil, false
	}
	c, p, hasPre := strings.Cut(v, "-")
	copy(core[:], strings.Split(c, "."))
	if hasPre {
		pre = strings.Split(p, ".")
	}
	return core, pre, true
}

// compareNumbers compares two decimal numbers without leading zeros, of any length.
func compareNumbers(a, b string) int {
	return cmp.Or(cmp.Compare(len(a), len(b)), strings.Compare(a, b))
}

// compareIdentifiers compares two pre-release identifiers: numbers before words.
func compareIdentifiers(a, b string) int {
	an, bn := numeric(a), numeric(b)
	switch {
	case an && bn:
		return compareNumbers(a, b)
	case an != bn:
		return cmp.Compare(boolInt(!an), boolInt(!bn))
	}
	return strings.Compare(a, b)
}

// numeric reports whether an identifier is all digits.
func numeric(s string) bool {
	return strings.Trim(s, "0123456789") == ""
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
