package npm

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"slices"

	"github.com/lajstrom/lajstrom/internal/auth"
	"github.com/lajstrom/lajstrom/internal/format"
	"github.com/lajstrom/lajstrom/internal/index"
	"github.com/lajstrom/lajstrom/internal/oci/profile"
	"github.com/lajstrom/lajstrom/internal/server"
)

// npm deprecate and npm unpublish (docs/spec/http-routing.md, npm): the packument update, which
// both send, and the deletes of a version's tarball and of a whole package.

// packumentState is what the packument update compares: the _rev, the dist-tags and the
// deprecation of each version.
type packumentState struct {
	Rev      string            `json:"_rev"`
	DistTags map[string]string `json:"dist-tags"`
	Versions map[string]struct {
		Deprecated *string `json:"deprecated"`
	} `json:"versions"`
}

// deprecated returns the deprecation message of version, "" for none.
func (s packumentState) deprecated(version string) string {
	if d := s.Versions[version].Deprecated; d != nil {
		return *d
	}
	return ""
}

// current returns the package called name and the state of its full packument as it is now.
func (h *handlers) current(r *http.Request, repo index.Repo, name string) (*index.Package, packumentState, error) {
	var s packumentState
	c, err := h.document(r, repo, name, kindPackument)
	if err != nil {
		return nil, s, err
	}
	if err := json.Unmarshal(c.Content, &s); err != nil {
		return nil, s, fmt.Errorf("npm: the packument of %s: %w", name, err)
	}
	pkg, err := h.deps.Index.GetPackage(r.Context(), repo.ID, name)
	return pkg, s, err
}

// checkRev refuses a _rev that is not the current one: 400 without one, 409 for another.
func checkRev(name, rev string, cur packumentState) error {
	switch rev {
	case "":
		return fmt.Errorf("%w: a change of %s needs the _rev it was read at", format.ErrInvalid, name)
	case cur.Rev:
		return nil
	}
	return fmt.Errorf("%w: %s has changed since %s, it is %s now: read it again", index.ErrConflict, name, rev, cur.Rev)
}

// update is the packument update of npm deprecate and of the first step of npm unpublish of a
// version: the whole packument as read with ?write=true and changed, at PUT /<name> without
// _attachments or PUT /<name>/-rev/<rev>. Its _rev (urlRev, else the body's) must be the
// current one. Against the current packument: a version the body leaves out is deleted as on
// /v2 (delete); each difference of dist-tags is an add or a rm, latest left as it is when the
// body has none (write); each difference of a version's deprecated is a deprecate or an
// undeprecate (write); a version the package does not have, or a dist-tag naming a version not
// in the body, is 400. Every other field is ignored. The actions are checked before anything
// changes; the deletes go first, then the journal entries in one append. 201 {"ok": true}.
func (h *handlers) update(w http.ResponseWriter, r *http.Request, repo index.Repo, name, urlRev string, fields map[string]json.RawMessage) {
	ctx := r.Context()
	if err := format.Authorize(ctx, h.deps, auth.ActionWrite, repo); err != nil {
		h.fail(w, r, err)
		return
	}
	body, err := readUpdate(fields, urlRev)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	pkg, cur, err := h.current(r, repo, name)
	if err == nil {
		err = checkRev(name, body.Rev, cur)
	}
	var deletes []string
	var ops []format.JournalOp
	if err == nil {
		deletes, ops, err = diff(cur, body)
	}
	if err == nil && len(deletes) > 0 {
		err = format.Authorize(ctx, h.deps, auth.ActionDelete, repo)
	}
	if err == nil {
		err = h.deleteVersions(r, repo, pkg, deletes)
	}
	if err == nil && len(ops) > 0 && len(deletes) < len(cur.Versions) {
		_, err = h.journal(ctx, repo, pkg, ops)
	}
	if err != nil {
		h.fail(w, r, err)
		return
	}
	h.deps.Log.InfoContext(ctx, "npm: packument updated", "repo", repo.Format+"/"+repo.Name, "package", name,
		"deleted", deletes, "entries", len(ops))
	writeJSON(w, http.StatusCreated, map[string]bool{"ok": true})
}

// readUpdate reads what a packument update changes from the body's fields: its versions, with
// their deprecated, its dist-tags, and its _rev, which urlRev replaces when it is set.
func readUpdate(fields map[string]json.RawMessage, urlRev string) (packumentState, error) {
	var body packumentState
	if err := json.Unmarshal(fields["versions"], &body.Versions); err != nil || body.Versions == nil {
		return body, fmt.Errorf("%w: a packument update has the versions of the package", format.ErrInvalid)
	}
	if raw, ok := fields["dist-tags"]; ok && json.Unmarshal(raw, &body.DistTags) != nil {
		return body, fmt.Errorf("%w: dist-tags must be an object of tags and versions", format.ErrInvalid)
	}
	_ = json.Unmarshal(fields["_rev"], &body.Rev)
	if urlRev != "" {
		body.Rev = urlRev
	}
	return body, nil
}

// diff compares the body of a packument update with the current packument: the versions to
// delete and the journal ops, in the order of their tags and versions.
func diff(cur, body packumentState) (deletes []string, ops []format.JournalOp, err error) {
	for _, v := range slices.Sorted(maps.Keys(body.Versions)) {
		if _, ok := cur.Versions[v]; !ok {
			return nil, nil, fmt.Errorf("%w: the package has no version %s: a version comes with its tarball (npm publish)", format.ErrInvalid, v)
		}
	}
	for _, v := range slices.Sorted(maps.Keys(cur.Versions)) {
		if _, ok := body.Versions[v]; !ok {
			deletes = append(deletes, v)
		}
	}
	if ops, err = diffTags(cur, body); err != nil {
		return nil, nil, err
	}
	ops = append(ops, diffDeprecations(cur, body)...)
	for _, op := range ops {
		if err := (profile.JournalChange(op)).Check(); err != nil {
			return nil, nil, fmt.Errorf("%w: %w", format.ErrInvalid, err)
		}
	}
	return deletes, ops, nil
}

// diffTags returns the adds of the body's dist-tags that differ, and the rms of the current
// ones it leaves out but latest, which it then leaves as it is.
func diffTags(cur, body packumentState) ([]format.JournalOp, error) {
	var ops []format.JournalOp
	for _, tag := range slices.Sorted(maps.Keys(body.DistTags)) {
		v := body.DistTags[tag]
		if _, ok := body.Versions[v]; !ok {
			return nil, fmt.Errorf("%w: the dist-tag %s names %s, which the body does not have", format.ErrInvalid, tag, v)
		}
		if cur.DistTags[tag] != v {
			ops = append(ops, format.JournalOp{ArtifactType: profile.ArtifactTypeNpmDistTags, Op: profile.JournalAdd, Tag: tag, Version: v})
		}
	}
	for _, tag := range slices.Sorted(maps.Keys(cur.DistTags)) {
		if _, ok := body.DistTags[tag]; !ok && tag != profile.LatestTag {
			ops = append(ops, format.JournalOp{ArtifactType: profile.ArtifactTypeNpmDistTags, Op: profile.JournalRm, Tag: tag})
		}
	}
	return ops, nil
}

// diffDeprecations returns a deprecate or an undeprecate for each version of the body whose
// deprecated differs from the current one ("" or nothing for none).
func diffDeprecations(cur, body packumentState) []format.JournalOp {
	var ops []format.JournalOp
	for _, v := range slices.Sorted(maps.Keys(body.Versions)) {
		switch msg := body.deprecated(v); {
		case msg == cur.deprecated(v):
		case msg == "":
			ops = append(ops, format.JournalOp{ArtifactType: profile.ArtifactTypeNpmDeprecation, Op: profile.JournalUndeprecate, Version: v})
		default:
			ops = append(ops, format.JournalOp{ArtifactType: profile.ArtifactTypeNpmDeprecation, Op: profile.JournalDeprecate, Version: v, Message: msg})
		}
	}
	return ops
}

// deleteVersions deletes the versions of pkg as their manifests' DELETE on /v2 deletes them,
// referrers first, an audit row each; a version gone meanwhile is no error.
func (h *handlers) deleteVersions(r *http.Request, repo index.Repo, pkg *index.Package, versions []string) error {
	ctx := r.Context()
	for _, v := range versions {
		row, err := h.deps.Index.GetVersion(ctx, pkg.ID, v, "")
		if errors.Is(err, index.ErrNotFound) {
			continue
		}
		if err == nil {
			err = h.deleteManifest(r, repo, pkg.NameOCI, row.Tag)
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// deleteManifest deletes the manifest under ref of the package path through deps.Ingest; one
// that is gone is no error.
func (h *handlers) deleteManifest(r *http.Request, repo index.Repo, pkgPath, ref string) error {
	if h.deps.Ingest == nil {
		return errors.New("npm: no publisher in format.Deps")
	}
	ctx := r.Context()
	_, err := h.deps.Ingest.Delete(ctx, repo, format.Deletion{Path: pkgPath, Reference: ref, ClientIP: server.ClientAddr(ctx)},
		auth.PrincipalFrom(ctx))
	if errors.Is(err, index.ErrNotFound) {
		return nil
	}
	return err
}

// unpublishPackage is npm unpublish of a whole package, DELETE /<name>/-rev/<rev> (delete): the
// rev must be the current one (409). Every version is deleted as on /v2, then every journal
// entry, under the journal lock (ingest deletes a _j tag so): those the index has, and any the
// registry has after them. 200 {"ok": true}; a backend that cannot delete manifests is 501 at the
// first delete, and nothing changes; versions deleted before a failure stay deleted.
func (h *handlers) unpublishPackage(w http.ResponseWriter, r *http.Request, repo index.Repo, name, rev string) {
	ctx := r.Context()
	if err := format.Authorize(ctx, h.deps, auth.ActionDelete, repo); err != nil {
		h.fail(w, r, err)
		return
	}
	pkg, cur, err := h.current(r, repo, name)
	if err == nil {
		err = checkRev(name, rev, cur)
	}
	var rows []index.JournalEntry
	var versions []index.Version
	if err == nil {
		rows, err = h.deps.Index.ListJournal(ctx, pkg.ID)
	}
	if err == nil {
		versions, err = h.deps.Index.ListVersions(ctx, pkg.ID)
	}
	for i := 0; err == nil && i < len(versions); i++ {
		err = h.deleteManifest(r, repo, pkg.NameOCI, versions[i].Tag)
	}
	if err == nil {
		err = h.deleteJournal(r, repo, pkg.NameOCI, rows)
	}
	if err != nil {
		h.fail(w, r, err)
		return
	}
	h.deps.Log.InfoContext(ctx, "npm: package unpublished", "repo", repo.Format+"/"+repo.Name, "package", name,
		"versions", len(versions), "entries", len(rows))
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// deleteJournal deletes the journal entries of the package path, 1 to the last seq the index
// had, and on until the registry has no entry: an entry of a writer that stopped before its
// index write is deleted too.
func (h *handlers) deleteJournal(r *http.Request, repo index.Repo, pkgPath string, rows []index.JournalEntry) error {
	var last int64
	if len(rows) > 0 {
		last = rows[len(rows)-1].Seq
	}
	for seq := int64(1); ; seq++ {
		_, err := h.deps.Ingest.Delete(r.Context(), repo, format.Deletion{Path: pkgPath, Reference: profile.JournalTag(seq),
			ClientIP: server.ClientAddr(r.Context())}, auth.PrincipalFrom(r.Context()))
		switch {
		case errors.Is(err, index.ErrNotFound) && seq >= last:
			return nil
		case err != nil && !errors.Is(err, index.ErrNotFound):
			return err
		}
	}
}

// unpublishFile is the last step of npm unpublish of a version, DELETE /<name>/-/<file>/-rev/<rev>
// at the path of its dist.tarball (delete): the version whose tarball is file is deleted as on
// /v2 when it is still there. 200 {"ok": true} either way; the rev is not checked, npm read the
// packument again just before.
func (h *handlers) unpublishFile(w http.ResponseWriter, r *http.Request, repo index.Repo, name, file string) {
	ctx := r.Context()
	if err := format.Authorize(ctx, h.deps, auth.ActionDelete, repo); err != nil {
		h.fail(w, r, err)
		return
	}
	matches, err := h.deps.Index.FindFiles(ctx, repo.ID, file)
	for i := 0; err == nil && i < len(matches); i++ {
		if m := matches[i]; m.Package.Name == name {
			err = h.deleteManifest(r, repo, m.Package.NameOCI, m.Version.Tag)
		}
	}
	if err != nil {
		h.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}
