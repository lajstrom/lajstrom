package profile

import (
	"fmt"
	"slices"
	"strings"
)

// The pair that Harbor's tag-list route finds in a URL (the amendment of ADR 0017): a component
// "tags" that a component starting with "list" follows. PackagePath writes such a "tags" as
// "tag_73", its last byte escaped as the reserved words are.
const (
	tagsComponent = "tags"
	markedTags    = "tag_73"
	listPrefix    = "list"
)

// PackagePath joins the escaped components of a package path (EscapeComponent) with "/", as the
// plugins' EscapeName do (docs/spec/manifest-profile.md §4.3), and escapes the last byte of a
// component "tags" that a component starting with "list" follows: "x/tags/list" is
// "x/tag_73/list". Harbor's tag-list route is not anchored at the end, so it takes
// /v2/x/tags/listing/blobs/<digest> for the tag list of x (the amendment of ADR 0017); escaping
// "tags" breaks that whatever follows "list". The first component follows the repo name, which
// PackagePath does not see: ReservedRepoName refuses "tags".
func PackagePath(components ...string) string {
	var marked []string
	for i := range len(components) - 1 {
		if components[i] == tagsComponent && strings.HasPrefix(components[i+1], listPrefix) {
			if marked == nil {
				marked = slices.Clone(components)
			}
			marked[i] = markedTags
		}
	}
	if marked == nil {
		return strings.Join(components, "/")
	}
	return strings.Join(marked, "/")
}

// ReservedRepoName reports whether a repo may not have the name (ADR 0017): a ReservedComponent,
// or "tags", since the first component of a package path follows the repo name and PackagePath
// does not see it.
func ReservedRepoName(name string) bool {
	return ReservedComponent(name) || name == tagsComponent
}

// EarlierPackagePaths returns the forms that a package path p, as PackagePath writes it now, had
// before: before the amendment of ADR 0017, and before ADR 0017 itself (LegacyPackagePath), each
// only when it differs from p and from the one before; none for a name that both left alone. A
// package keeps the OCI repository it was first written to, so a writer looks it up at p and at
// each of these (index.PackageAt), and reindex takes a manifest found at any of them as the
// package of its annotations: written in the newest form, read in all. A path that is no form
// any writer produces has none.
func EarlierPackagePaths(p string) []string {
	var out []string
	amended := unamend(p)
	if amended != p {
		out = append(out, amended)
	}
	if legacy := legacyPath(amended); legacy != amended {
		out = append(out, legacy)
	}
	return out
}

// PackagePathForms returns every form of the package path p, given in any of them: the newest,
// as PackagePath writes it now, then its EarlierPackagePaths. It writes the marks of ADR 0017 and
// its amendment into the components of p that lack them (newestPackagePath) and keeps the result
// when p is one of its forms; a path that is no form of what a writer produces has itself only.
// Like EarlierPackagePaths it maps a path to paths and decodes no name: a reader that starts
// from the path of an OCI repository finds the package at any form of it (index.PackageOf), the
// versions reindex found there in a split (#642).
func PackagePathForms(p string) []string {
	n := newestPackagePath(p)
	forms := append([]string{n}, EarlierPackagePaths(n)...)
	if !slices.Contains(forms, p) {
		return []string{p}
	}
	return forms
}

// newestPackagePath returns p with the marks of ADR 0017 and its amendment written into each
// component: a hashed one with the prefix "h0_h", one of the 1.1 grammar with markComponent (each
// '-' of a run, the last byte of a reserved word), and the "tags" before a "list…" with
// PackagePath. A component written now has every mark already and stays as it is.
func newestPackagePath(p string) string {
	parts := strings.Split(p, "/")
	for i, c := range parts {
		if h, ok := hashedComponentHex(c); ok {
			parts[i] = hashedComponentPrefix + h
		} else if specComponentRE.MatchString(c) {
			parts[i] = markComponent(c, true)
		}
	}
	return PackagePath(parts...)
}

// LegacyPackagePath returns the package path that Lajstrom wrote before ADR 0017 for the name
// whose package path is p now: each hashed component with the prefix "h0__", and the escapes of
// ADR 0017 and its amendment decoded. It is p itself for a name they left alone, and for a path
// in that form; the last of EarlierPackagePaths otherwise.
func LegacyPackagePath(p string) string { return legacyPath(unamend(p)) }

// legacyPath is legacyComponent of each component of p, a package path as ADR 0017 wrote it
// before its amendment, or as it is written now.
func legacyPath(p string) string {
	if !strings.Contains(p, "_") { // every form ADR 0017 changed has one
		return p
	}
	parts := strings.Split(p, "/")
	for i, c := range parts {
		parts[i] = legacyComponent(c, false)
	}
	return strings.Join(parts, "/")
}

// amend writes the escapes of the amendment of ADR 0017 into a package path p as ADR 0017 wrote
// it before: "manifests" and "referrers" with their last byte escaped, and PackagePath.
func amend(p string) string {
	parts := strings.Split(p, "/")
	for i, c := range parts {
		if c == "manifests" || c == "referrers" {
			parts[i] = markComponent(c, true)
		}
	}
	return PackagePath(parts...)
}

// unamend is the inverse of amend: the package path as ADR 0017 wrote it before its amendment
// when p is one written now, else p.
func unamend(p string) string {
	if !strings.Contains(p, "_73") { // each escape of the amendment is of an 's'
		return p
	}
	parts := strings.Split(p, "/")
	for i, c := range parts {
		switch {
		case c == "manifest_73", c == "referrer_73":
			parts[i] = c[:len(c)-3] + "s"
		case c == markedTags && i+1 < len(parts) && strings.HasPrefix(parts[i+1], listPrefix):
			parts[i] = tagsComponent
		}
	}
	if u := strings.Join(parts, "/"); u != p && amend(u) == p {
		return u
	}
	return p
}

// UnescapePackagePath is the inverse of PackagePath over EscapeComponent, for tooling and tests,
// in every form: the lower-cased components of the name whose package path p is now or was
// before (EarlierPackagePaths), so that a name decodes the same in any form. Errors wrap
// ErrInvalidEscape: a hashed component, as for UnescapeComponent, or a path that is no form of
// any name.
func UnescapePackagePath(p string) ([]string, error) {
	parts := strings.Split(unamend(p), "/")
	now := make([]string, len(parts))
	for i, c := range parts {
		u, err := UnescapeComponent(c)
		if err != nil {
			return nil, err
		}
		parts[i], now[i] = u, EscapeComponent(u)
	}
	if n := PackagePath(now...); n != p && !slices.Contains(EarlierPackagePaths(n), p) {
		return nil, fmt.Errorf("%w: %q is not a package path of %q", ErrInvalidEscape, p, strings.Join(parts, "/"))
	}
	return parts, nil
}
