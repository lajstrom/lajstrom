package npm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/lajstrom/lajstrom/internal/auth"
	"github.com/lajstrom/lajstrom/internal/format"
	"github.com/lajstrom/lajstrom/internal/index"
	"github.com/lajstrom/lajstrom/internal/oci/profile"
)

// maxDistTagBody bounds the body of PUT …/dist-tags/<tag>, a JSON string of a version.
const maxDistTagBody = 4 << 10

// distTagTarget is what a path below /npm/<repo>/-/package/ names: a package and, for a change,
// one of its dist-tags.
type distTagTarget struct {
	name string
	tag  string // "" for the list
}

// parseDistTags reads the segments after -/package/: <name>/dist-tags[/<tag>], the name in one
// segment or, scoped, in two. ok is false for anything else.
func parseDistTags(segs []string) (distTagTarget, bool) {
	t, ok := parseTarget(strings.Join(segs, "/"))
	switch {
	case !ok || len(t.rest) == 0 || t.rest[0] != "dist-tags" || len(t.rest) > 2:
		return distTagTarget{}, false
	case len(t.rest) == 2 && t.rest[1] == "":
		return distTagTarget{}, false
	case len(t.rest) == 2:
		return distTagTarget{name: t.name, tag: t.rest[1]}, true
	}
	return distTagTarget{name: t.name}, true
}

// distTags serves npm dist-tag ls, add and rm (docs/spec/http-routing.md, npm):
//
//	GET    /-/package/<name>/dist-tags         read: the packument's dist-tags
//	PUT    /-/package/<name>/dist-tags/<tag>   write: a journal add; the body is a JSON string, a version
//	DELETE /-/package/<name>/dist-tags/<tag>   write: a journal rm; latest is 400, a tag the package does not have 404
//
// A proxy repo's dist-tags are its upstream's: a change is 405 there (ADR 0013).
func (h *handlers) distTags(w http.ResponseWriter, r *http.Request, repo index.Repo, segs []string) {
	t, ok := parseDistTags(segs)
	switch {
	case !ok:
		h.fail(w, r, fmt.Errorf("%w: %s", index.ErrNotFound, r.URL.Path))
	case r.Method == http.MethodGet || r.Method == http.MethodHead:
		if t.tag != "" {
			h.fail(w, r, fmt.Errorf("%w: %s", index.ErrNotFound, r.URL.Path))
			return
		}
		h.listDistTags(w, r, repo, t.name)
	case t.tag == "":
		h.fail(w, r, fmt.Errorf("%w: %s %s", index.ErrNotFound, r.Method, r.URL.Path))
	case r.Method == http.MethodPut:
		h.addDistTag(w, r, repo, t)
	default:
		h.rmDistTag(w, r, repo, t)
	}
}

// listDistTags answers the dist-tags of the packument: those of the journal whose version the
// package has, and latest.
func (h *handlers) listDistTags(w http.ResponseWriter, r *http.Request, repo index.Repo, name string) {
	if err := format.Authorize(r.Context(), h.deps, auth.ActionRead, repo); err != nil {
		h.fail(w, r, err)
		return
	}
	c, err := h.document(r, repo, name, kindPackument)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	var doc struct {
		DistTags map[string]string `json:"dist-tags"`
	}
	if err := json.Unmarshal(c.Content, &doc); err != nil {
		h.fail(w, r, fmt.Errorf("npm: the packument of %s: %w", name, err))
		return
	}
	writeJSON(w, http.StatusOK, doc.DistTags)
}

// addDistTag points the dist-tag at the version of the body with a journal add, none when it
// names that version already: 201 {"ok": true}.
func (h *handlers) addDistTag(w http.ResponseWriter, r *http.Request, repo index.Repo, t distTagTarget) {
	ctx := r.Context()
	if err := h.refuseWrite(ctx, repo); err != nil {
		h.fail(w, r, err)
		return
	}
	if err := format.Authorize(ctx, h.deps, auth.ActionWrite, repo); err != nil {
		h.fail(w, r, err)
		return
	}
	var version string
	b, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxDistTagBody))
	if err == nil && json.Unmarshal(b, &version) != nil {
		err = fmt.Errorf("%w: the body of a dist-tag is a JSON string, a version of %s", format.ErrInvalid, t.name)
	}
	if err != nil {
		h.fail(w, r, err)
		return
	}
	if !profile.IsDistTag(t.tag) {
		h.fail(w, r, fmt.Errorf("%w: the dist-tag %q is not a letter and up to 127 letters, digits, '.', '_' or '-'", format.ErrInvalid, t.tag))
		return
	}
	if err := h.setTags(ctx, repo, t.name, version, []string{t.tag}); err != nil {
		h.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]bool{"ok": true})
}

// rmDistTag removes the dist-tag with a journal rm: 200 {"ok": true}.
func (h *handlers) rmDistTag(w http.ResponseWriter, r *http.Request, repo index.Repo, t distTagTarget) {
	ctx := r.Context()
	if err := h.refuseWrite(ctx, repo); err != nil {
		h.fail(w, r, err)
		return
	}
	if err := format.Authorize(ctx, h.deps, auth.ActionWrite, repo); err != nil {
		h.fail(w, r, err)
		return
	}
	if t.tag == profile.LatestTag {
		h.fail(w, r, fmt.Errorf("%w: the dist-tag latest is never removed; point it at another version", format.ErrInvalid))
		return
	}
	pkg, tags, err := h.distTagState(ctx, repo, t.name)
	if err == nil && tags[t.tag] == "" {
		err = fmt.Errorf("%w: %s has no dist-tag %s", index.ErrNotFound, t.name, t.tag)
	}
	if err == nil {
		_, err = h.journal(ctx, repo, pkg, []format.JournalOp{{ArtifactType: profile.ArtifactTypeNpmDistTags, Op: profile.JournalRm, Tag: t.tag}})
	}
	if err != nil {
		h.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// setTags points each of tags at version with a journal add, but those that name it already:
// the dist-tags of a publish, and of npm dist-tag add.
func (h *handlers) setTags(ctx context.Context, repo index.Repo, name, version string, tags []string) error {
	pkg, current, err := h.distTagState(ctx, repo, name)
	if err != nil {
		return err
	}
	var ops []format.JournalOp
	for _, tag := range tags {
		if current[tag] != version {
			ops = append(ops, format.JournalOp{ArtifactType: profile.ArtifactTypeNpmDistTags, Op: profile.JournalAdd, Tag: tag, Version: version})
		}
	}
	if len(ops) == 0 {
		return nil
	}
	_, err = h.journal(ctx, repo, pkg, ops)
	return err
}

// distTagState returns the package called name and its dist-tags as the journal has them,
// whether or not the package still has their versions.
func (h *handlers) distTagState(ctx context.Context, repo index.Repo, name string) (*index.Package, map[string]string, error) {
	if CheckName(name) != nil {
		return nil, nil, fmt.Errorf("%w: the package %s is not in %s/%s", index.ErrNotFound, name, repo.Format, repo.Name)
	}
	pkg, err := h.deps.Index.GetPackage(ctx, repo.ID, name)
	if errors.Is(err, index.ErrNotFound) {
		return nil, nil, fmt.Errorf("%w: the package %s is not in %s/%s", index.ErrNotFound, name, repo.Format, repo.Name)
	}
	if err != nil {
		return nil, nil, err
	}
	j, err := readJournal(ctx, h.deps.Index, pkg.ID)
	return pkg, j.tags, err
}

// journal appends ops to the journal of pkg through deps.Ingest, as the request's principal.
func (h *handlers) journal(ctx context.Context, repo index.Repo, pkg *index.Package, ops []format.JournalOp) ([]int64, error) {
	if h.deps.Ingest == nil {
		return nil, errors.New("npm: no publisher in format.Deps")
	}
	return h.deps.Ingest.Journal(ctx, repo, pkg.NameOCI, ops, auth.PrincipalFrom(ctx))
}
