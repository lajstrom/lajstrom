package deb

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"github.com/lajstrom/lajstrom/internal/format"
	"github.com/lajstrom/lajstrom/internal/index"
	"github.com/lajstrom/lajstrom/internal/oci/profile"
	"github.com/lajstrom/lajstrom/internal/provider"
)

// packageContentType is the Content-Type of a .deb: the media type Debian registers for a
// binary package.
const packageContentType = "application/vnd.debian.binary-package"

// errTwoContents is the error of poolFile when the versions that hold a pool file disagree on
// its blob; poolFile has logged them.
var errTwoContents = errors.New("deb: one pool file name, two contents")

// pool serves pool/{component}/{p}/{source}/{filename}, the Filename of a paragraph of Packages
// (docs/spec/http-routing.md §3, apt): the file is looked up by its name (poolFile) and its
// blob is served through format.ServeBlob, one request to the backend at most: a 307 to the
// backend's storage when the provider gives a URL and the repo's policy.blobRedirect allows
// it, else the backend's 200, 206 or 416 streamed. The ETag is the blob digest, Last-Modified
// the created of the newest version that holds the file, Docker-Content-Digest that version's
// manifest digest. HEAD asks the backend for the headers. A stream that breaks after the
// headers is logged.
func (h *handlers) pool(w http.ResponseWriter, r *http.Request) {
	repo, ok := h.start(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	m, err := h.poolFile(ctx, repo, r.PathValue("component"), r.PathValue("p")+"/"+r.PathValue("source"), r.PathValue("filename"))
	switch {
	case errors.Is(err, errTwoContents):
		format.WriteError(w, err) // 500, logged by poolFile with what the operator needs
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
		h.deps.Log.WarnContext(ctx, "apt: the blob stream failed after the response started", "repo", repo.Format+"/"+repo.Name,
			"file", m.File.Name, "digest", m.File.Digest.String(), "error", err.Error())
	}
}

// poolFile returns the file pool/<component>/<dir>/<filename> of the repo, dir being
// <p>/<source>: of the files called filename (index.Reader.FindFiles, one per version that
// holds it, so one per suite for a .deb uploaded to several), those whose version is in the
// component and whose pool path, PoolPath of the source and the file name, is dir/filename.
// The source is that of the Source field of the version's metadata, the package name without
// one, as the renderer writes Filename into Packages; a version without metadata in the index
// is taken on the file name alone, logged at debug, since its source cannot be told without a
// read of the registry. Nothing found is index.ErrNotFound. The match returned is that of the
// newest version, for Last-Modified and Docker-Content-Digest.
//
// The matches must agree on the blob: a Debian pool holds one content per file name, and the
// upload route refuses another (poolHolders), but an oras push cannot be checked (Validate
// has no index) and two uploads at once both pass the check. Matches with two digests are
// errTwoContents, logged at error level with both digests and versions: the answer is 500, and
// the cause is the operator's to settle, which reindex (#29) reports.
func (h *handlers) poolFile(ctx context.Context, repo index.Repo, component, dir, filename string) (*index.FileMatch, error) {
	path := dir + "/" + filename
	matches, err := h.deps.Index.FindFiles(ctx, repo.ID, filename)
	if err != nil {
		return nil, fmt.Errorf("deb: the files called %s in apt/%s: %w", filename, repo.Name, err)
	}
	var found *index.FileMatch
	for i := range matches {
		m := &matches[i]
		if m.Version.Attrs[profile.AnnotationDebComponent] != component {
			continue
		}
		switch want, known := poolPathOf(m); {
		case !known:
			h.deps.Log.DebugContext(ctx, "apt: a pool file without metadata in the index is taken on its name",
				"repo", repo.Format+"/"+repo.Name, "path", "pool/"+component+"/"+path, "version", m.Version.Version, "variant", m.Version.Variant)
		case want != path:
			continue
		}
		switch {
		case found == nil:
			found = m
		case m.File.Digest != found.File.Digest:
			h.deps.Log.ErrorContext(ctx, "apt: one pool file name, two contents", "repo", repo.Format+"/"+repo.Name,
				"path", "pool/"+component+"/"+path,
				"digest", found.File.Digest.String(), "version", found.Version.Version+" "+found.Version.Variant,
				"manifest", found.Version.Digest.String(),
				"other_digest", m.File.Digest.String(), "other_version", m.Version.Version+" "+m.Version.Variant,
				"other_manifest", m.Version.Digest.String())
			return nil, fmt.Errorf("%w: apt/%s pool/%s/%s", errTwoContents, repo.Name, component, path)
		case m.Version.CreatedAt.After(found.Version.CreatedAt):
			found = m
		}
	}
	if found == nil {
		return nil, fmt.Errorf("%w: apt/%s has no pool/%s/%s", index.ErrNotFound, repo.Name, component, path)
	}
	return found, nil
}

// poolPathOf returns PoolPath of the file of m, with the source of the Source field of the
// version's metadata, or the package name without one; known is false when the index has no
// metadata of the version, or none that tells.
func poolPathOf(m *index.FileMatch) (path string, known bool) {
	var md struct {
		Control map[string]string `json:"control"`
	}
	if m.Version.Metadata == nil || json.Unmarshal(m.Version.Metadata, &md) != nil || md.Control == nil {
		return "", false
	}
	source := m.Package.Name
	if s, ok := lookup(md.Control, FieldSource); ok {
		var err error
		if source, _, err = SplitSource(s); err != nil {
			return "", false
		}
	}
	if source == "" {
		return "", false
	}
	return PoolPath(source, m.File.Name), true
}

// registryOf returns the provider of the repo's registry.
func registryOf(deps format.Deps, repo index.Repo) (provider.Provider, error) {
	if deps.Providers != nil {
		if p, ok := deps.Providers.Provider(repo.Registry); ok && p != nil {
			return p, nil
		}
	}
	return nil, fmt.Errorf("deb: repo apt/%s: registry %q is not configured", repo.Name, repo.Registry)
}
