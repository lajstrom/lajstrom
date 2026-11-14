package api

import (
	"cmp"
	"fmt"
	"net/http"
	"unicode/utf8"

	"github.com/lajstrom/lajstrom/internal/auth"
	"github.com/lajstrom/lajstrom/internal/format"
	"github.com/lajstrom/lajstrom/internal/index"
	"github.com/lajstrom/lajstrom/internal/oci/profile"
)

// pypiFormat is the format whose versions are yanked (PEP 592).
const pypiFormat = "pypi"

// Yank is the yank of a pypi version as the version resource shows it (docs/api.md, List
// versions): Reason is "" when the yank gave none.
type Yank struct {
	Reason string `json:"reason"`
}

// yankOf returns the yank of version among a package's state, nil when it is not yanked.
func yankOf(states []index.JournalState, version string) *Yank {
	for _, s := range states {
		if s.Kind == index.JournalYanked && s.Key == version {
			return &Yank{Reason: s.Value}
		}
	}
	return nil
}

// yankVersion answers PUT …/packages/{name}/versions/{version}/yank, with an optional body
// {"reason"}, and unyankVersion DELETE …/yank (docs/spec/http-routing.md §4, #81).
func (h *handler) yankVersion(w http.ResponseWriter, r *http.Request) (int, any, error) {
	return h.yank(w, r, true)
}

// unyankVersion is yankVersion's DELETE: the unyank.
func (h *handler) unyankVersion(w http.ResponseWriter, r *http.Request) (int, any, error) {
	return h.yank(w, r, false)
}

// yank appends a yank (set) or an unyank of a version of a hosted pypi repo to its package's
// journal through the Journaler, the ingest pipeline, which writes the entry, the state, the
// invalidation of the repo's documents and the audit row pypi.yank. write on the repo is checked
// before anything is read. A yank with the reason the version has, or an unyank of a version
// that is not yanked, appends nothing: 204 either way. A repo, package or version that does not
// exist is 404; a repo of another format or kind, ?variant=, or a reason that is not 1 to
// profile.MaxJournalMessage bytes of UTF-8 is 400. The version names its every variant: a yank
// is the release's, as on PyPI.
func (h *handler) yank(w http.ResponseWriter, r *http.Request, set bool) (int, any, error) {
	f, name := r.PathValue("format"), r.PathValue("repo")
	if err := h.allow(r, auth.ActionWrite, f, name); err != nil {
		return 0, nil, err
	}
	if h.journal == nil {
		return 0, nil, &clientError{msg: "yanking versions is not enabled in this server", err: format.ErrUnsupported}
	}
	reason, err := yankReason(w, r, set)
	if err != nil {
		return 0, nil, err
	}
	ctx := r.Context()
	repo, err := h.repos.Index.GetRepo(ctx, f, name)
	if err != nil {
		return 0, nil, repoError("yank a version in", f, name, err)
	}
	if repo.Format != pypiFormat || cmp.Or(repo.Kind, index.KindHosted) != index.KindHosted {
		return 0, nil, invalid("repo: %s/%s is a %s repo; only the versions of a hosted pypi repo are yanked",
			repo.Format, repo.Name, cmp.Or(repo.Kind, index.KindHosted))
	}
	pkg, versions, err := h.versionOf(ctx, repo, r.PathValue("name"), r.PathValue("version"), nil)
	if err != nil {
		return 0, nil, err
	}
	states, err := h.repos.Index.ListJournalState(ctx, pkg.ID)
	if err != nil {
		return 0, nil, fmt.Errorf("api: yanks of %q in %s/%s: %w", pkg.Name, repo.Format, repo.Name, err)
	}
	version := versions[0].Version
	if cur := yankOf(states, version); (cur != nil) == set && (!set || cur.Reason == reason) {
		return http.StatusNoContent, nil, nil // nothing changes: no entry, no audit row
	}
	op := format.JournalOp{ArtifactType: profile.ArtifactTypePypiYank, Op: profile.JournalUnyank, Version: version}
	if set {
		op.Op, op.Message = profile.JournalYank, reason
	}
	if _, err := h.journal.Journal(ctx, *repo, pkg.NameOCI, []format.JournalOp{op}, auth.PrincipalFrom(ctx)); err != nil {
		return 0, nil, fmt.Errorf("api: %s %s of %q in %s/%s: %w", op.Op, version, pkg.Name, repo.Format, repo.Name, err)
	}
	return http.StatusNoContent, nil, nil
}

// yankReason reads the reason of a yank (set) from the request's optional body {"reason"}: ""
// for none, and for an unyank, whose body is not read. ?variant= is refused: a yank names the
// whole version.
func yankReason(w http.ResponseWriter, r *http.Request, set bool) (string, error) {
	if _, ok := r.URL.Query()["variant"]; ok {
		return "", invalid("variant: a yank names the whole version, every file of it")
	}
	if !set {
		return "", nil
	}
	var body struct {
		Reason *string `json:"reason"`
	}
	if err := decodeOptionalBody(w, r, &body); err != nil || body.Reason == nil {
		return "", err
	}
	if reason := *body.Reason; len(reason) <= profile.MaxJournalMessage && utf8.ValidString(reason) {
		return reason, nil
	}
	return "", invalid("reason: %d bytes; a reason is at most %d bytes of UTF-8", len(*body.Reason), profile.MaxJournalMessage)
}
