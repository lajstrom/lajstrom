package rpm

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/lajstrom/lajstrom/internal/auth"
	"github.com/lajstrom/lajstrom/internal/format"
	"github.com/lajstrom/lajstrom/internal/index"
)

// packageContentType is the Content-Type of an .rpm.
const packageContentType = "application/x-rpm"

// errTwoContents is the error of fileOf when the versions that hold a file name disagree on its
// blob; fileOf has logged them.
var errTwoContents = errors.New("rpm: one file name, two contents")

// packageFile serves Packages/{filename}, the location of a package in primary.xml
// (docs/spec/http-routing.md §3, rpm): the file is looked up by its name (fileOf) and its blob
// is served through format.ServeBlob, one request to the backend at most: a 307 to the
// backend's storage when the provider gives a URL and the repo's policy.blobRedirect allows it,
// else the backend's 200, 206 or 416 streamed. The ETag is the blob digest, Last-Modified the
// created of the version that holds the file, Docker-Content-Digest its manifest digest. HEAD
// asks the backend for the headers. A stream that breaks after the headers is logged.
func (h *handlers) packageFile(w http.ResponseWriter, r *http.Request) {
	repo, ok := h.start(w, r, auth.ActionRead)
	if !ok {
		return
	}
	ctx := r.Context()
	m, err := h.fileOf(ctx, repo, r.PathValue("filename"))
	switch {
	case errors.Is(err, errTwoContents):
		format.WriteError(w, err) // 500, logged by fileOf with what the operator needs
		return
	case err != nil:
		h.fail(w, r, err)
		return
	}
	prov, err := registryOf(h.deps, repo)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	err = format.ServeBlob(w, r, prov, ociName(repo, m.Package.NameOCI), m.File.Digest, format.ServeBlobOptions{
		ContentType:    packageContentType,
		LastModified:   m.Version.CreatedAt,
		ManifestDigest: m.Version.Digest,
		Redirect:       format.BlobRedirect(repo),
		Fail:           func(w http.ResponseWriter, err error) { h.fail(w, r, err) },
	})
	if err != nil {
		h.deps.Log.WarnContext(ctx, "rpm: the blob stream failed after the response started", "repo", repo.Format+"/"+repo.Name,
			"file", m.File.Name, "digest", m.File.Digest.String(), "error", err.Error())
	}
}

// fileOf returns the file Packages/<filename> of the repo: of the files called filename
// (index.Reader.FindFiles, one per version that holds it), the newest. The file name has no
// epoch, so two versions that differ only in it hold one name: the upload route refuses another
// content under a name (holders), but an oras push cannot be checked (Validate has no index).
// Matches with two digests are errTwoContents, logged at error level with both digests and
// versions: the answer is 500, and the cause is the operator's to settle. Nothing found is
// index.ErrNotFound.
func (h *handlers) fileOf(ctx context.Context, repo index.Repo, filename string) (*index.FileMatch, error) {
	matches, err := h.deps.Index.FindFiles(ctx, repo.ID, filename)
	if err != nil {
		return nil, fmt.Errorf("rpm: the files called %s in rpm/%s: %w", filename, repo.Name, err)
	}
	var found *index.FileMatch
	for i := range matches {
		m := &matches[i]
		switch {
		case found == nil:
			found = m
		case m.File.Digest != found.File.Digest:
			h.deps.Log.ErrorContext(ctx, "rpm: one file name, two contents", "repo", repo.Format+"/"+repo.Name,
				"path", "Packages/"+filename,
				"digest", found.File.Digest.String(), "version", found.Version.Version+" "+found.Version.Variant,
				"manifest", found.Version.Digest.String(),
				"other_digest", m.File.Digest.String(), "other_version", m.Version.Version+" "+m.Version.Variant,
				"other_manifest", m.Version.Digest.String())
			return nil, fmt.Errorf("%w: rpm/%s Packages/%s", errTwoContents, repo.Name, filename)
		case m.Version.CreatedAt.After(found.Version.CreatedAt):
			found = m
		}
	}
	if found == nil {
		return nil, fmt.Errorf("%w: rpm/%s has no Packages/%s", index.ErrNotFound, repo.Name, filename)
	}
	return found, nil
}
