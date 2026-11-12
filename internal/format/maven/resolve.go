package maven

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"

	"github.com/lajstrom/lajstrom/internal/auth"
	"github.com/lajstrom/lajstrom/internal/format"
	"github.com/lajstrom/lajstrom/internal/index"
	"github.com/lajstrom/lajstrom/internal/provider"
	"github.com/lajstrom/lajstrom/internal/server"
)

// The Content-Types of the files of a version by their extension; any other is
// application/octet-stream. Maven and Gradle do not read them.
var contentTypes = map[string]string{
	"pom":    contentTypeXML,
	"xml":    contentTypeXML,
	"jar":    "application/java-archive",
	"war":    "application/java-archive",
	"ear":    "application/java-archive",
	"aar":    "application/zip",
	"zip":    "application/zip",
	"tar.gz": "application/gzip",
	"tgz":    "application/gzip",
	"tar":    "application/x-tar",
	"module": "application/json", // Gradle module metadata
	"json":   "application/json",
}

// contentTypeSignature is the Content-Type of an OpenPGP signature, a file whose extension ends
// with .asc.
const contentTypeSignature = "application/pgp-signature"

// resolveRoutes mounts the read side of the maven frontend (docs/spec/http-routing.md §3,
// maven): a GET (and so a HEAD) of a path below the repo, which needs read on it. What the path
// names is told by its last element (Classify):
//
//	<groupPath>/<artifactId>/<version>/<file>              a file of a version, through format.ServeBlob
//	<groupPath>/<artifactId>/<version>/<file>.<algorithm>  its md5, sha1, sha256 or sha512, from the metadata layer in the index
//	<groupPath>/<artifactId>/maven-metadata.xml            the versions of the artifact, from index_cache (RenderIndex)
//	<groupPath>/<artifactId>/maven-metadata.xml.<algorithm> its checksum, a document of the same render
//
// Anything else is 404: a path the repo does not have, a SNAPSHOT version (after the MVP), the
// maven-metadata.xml of a version or of a group, a checksum of a checksum. Ingest drops the
// cached documents of the repo on every publish, so the first GET of a maven-metadata.xml after
// a deploy renders it again.
func resolveRoutes(r format.Router, deps format.Deps) {
	if deps.Log == nil {
		deps.Log = slog.Default().With("format", Name)
	}
	h := &resolver{deps: deps}
	r.HandleFunc("GET /{repo}/{path...}", h.get)
}

// resolver is the read side of the maven frontend over one format.Deps.
type resolver struct {
	deps format.Deps
}

// get answers a GET or a HEAD of a path below the repo once the principal may read the repo.
func (h *resolver) get(w http.ResponseWriter, r *http.Request) {
	repo, ok := format.RepoFrom(r.Context())
	if !ok {
		h.fail(w, r, errors.New("maven: the route has no repo"))
		return
	}
	if err := format.Authorize(r.Context(), h.deps, auth.ActionRead, repo); err != nil {
		h.fail(w, r, err)
		return
	}
	p := r.PathValue("path")
	target, algorithm, checksum := ChecksumOf(p)
	switch {
	case !checksum && Classify(p) == KindMetadata:
		h.metadata(w, r, repo, p, "")
	case !checksum:
		h.file(w, r, repo, p)
	case Classify(target) == KindMetadata:
		h.metadata(w, r, repo, target, algorithm)
	case Classify(target) == KindChecksum:
		h.fail(w, r, pathNotFound(repo, p, errors.New("a checksum of a checksum file")))
	default:
		h.checksum(w, r, repo, target, algorithm)
	}
}

// file serves a file of a version: its blob through format.ServeBlob, one request to the backend
// at most, a 307 to the backend's storage when the provider gives a URL and the repo's
// policy.blobRedirect allows it, else the backend's answer streamed with its Content-Length.
// The ETag is the blob digest, Last-Modified the created of the version, Docker-Content-Digest
// its manifest digest. HEAD asks the backend for the headers. A stream that breaks after the
// headers is logged.
func (h *resolver) file(w http.ResponseWriter, r *http.Request, repo index.Repo, p string) {
	ctx := r.Context()
	m, f, err := h.lookup(ctx, repo, p)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	prov, err := registryOf(h.deps, repo)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	err = format.ServeBlob(w, r, prov, ociName(repo, m.Package.NameOCI), m.File.Digest, format.ServeBlobOptions{
		ContentType:    contentTypeOf(f.Extension),
		LastModified:   m.Version.CreatedAt,
		ManifestDigest: m.Version.Digest,
		Redirect:       format.BlobRedirect(repo),
		Fail:           func(w http.ResponseWriter, err error) { h.fail(w, r, err) },
	})
	if err != nil {
		h.deps.Log.WarnContext(ctx, "maven: the blob stream failed after the response started", "repo", repo.Format+"/"+repo.Name,
			"file", p, "digest", m.File.Digest.String(), "error", err.Error())
	}
}

// checksum serves the checksum of algorithm of the file of a version at target: the lowercase
// hex that the metadata layer holds for it, kept in the index with the version, never read from
// the registry. A version whose metadata the index does not hold (a degraded row) has no
// checksum files.
func (h *resolver) checksum(w http.ResponseWriter, r *http.Request, repo index.Repo, target, algorithm string) {
	m, f, err := h.lookup(r.Context(), repo, target)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	sum, ok := checksumOf(m.Version.Metadata, f.Filename(), algorithm)
	if !ok {
		h.fail(w, r, pathNotFound(repo, target+"."+algorithm, errors.New("the index holds no checksums of the version")))
		return
	}
	etag := sha256.Sum256([]byte(sum))
	format.ServeDocument(w, r, &index.Cache{Content: []byte(sum), SHA256: hex.EncodeToString(etag[:]), RenderedAt: m.Version.CreatedAt},
		contentTypeChecksum)
}

// checksumOf returns the checksum of algorithm of the file name from the metadata layer md of a
// version; ok is false when md is absent, does not parse, has no such file, or the checksum is
// not lowercase hex of its length.
func checksumOf(md json.RawMessage, name, algorithm string) (string, bool) {
	var parsed metadata
	if md == nil || json.Unmarshal(md, &parsed) != nil {
		return "", false
	}
	digits := 0
	for _, a := range algorithms {
		if a.name == algorithm {
			digits = a.digits
		}
	}
	for i := range parsed.Files {
		if f := &parsed.Files[i]; f.Name == name {
			sum := *f.checksum(algorithm)
			return sum, digits > 0 && isHex(sum, digits)
		}
	}
	return "", false
}

// metadata serves the maven-metadata.xml of an artifact, for algorithm "", or its checksum file
// of algorithm, from index_cache, rendered on a miss (format.CachedDocument).
func (h *resolver) metadata(w http.ResponseWriter, r *http.Request, repo index.Repo, p, algorithm string) {
	c, err := ParseMetadataPath(p)
	if err != nil {
		h.fail(w, r, pathNotFound(repo, p, err))
		return
	}
	doc, err := format.CachedDocument(r.Context(), h.deps.Index, Plugin{}, repo, metadataScope(c), metadataKind(algorithm))
	if err != nil {
		h.fail(w, r, err)
		return
	}
	contentType := contentTypeXML
	if algorithm != "" {
		contentType = contentTypeChecksum
	}
	format.ServeDocument(w, r, doc, contentType)
}

// lookup returns the file of a version at path p in the repo, with what its path says: of the
// files of the repo with its name (index.Reader.FindFiles), the one of the package
// groupId:artifactId at the version. A path that is not that of a file of a release version, or
// that the repo does not have, is index.ErrNotFound.
func (h *resolver) lookup(ctx context.Context, repo index.Repo, p string) (*index.FileMatch, File, error) {
	f, err := ParsePath(p)
	if err != nil {
		return nil, File{}, pathNotFound(repo, p, err)
	}
	matches, err := h.deps.Index.FindFiles(ctx, repo.ID, f.Filename())
	if err != nil {
		return nil, File{}, fmt.Errorf("maven: the files called %s in maven/%s: %w", f.Filename(), repo.Name, err)
	}
	for i := range matches {
		m := &matches[i]
		if m.Package.Name == f.Name() && m.Version.Version == f.Version && m.Version.Variant == "" {
			return m, f, nil
		}
	}
	return nil, File{}, pathNotFound(repo, p, nil)
}

// fail answers err with format.WriteError and logs a 5xx, whose cause the client does not see.
func (h *resolver) fail(w http.ResponseWriter, r *http.Request, err error) {
	if status := format.WriteError(w, err); status >= http.StatusInternalServerError {
		h.deps.Log.Log(r.Context(), server.FailLevel(r), "maven: request failed", "method", r.Method, "path", r.URL.EscapedPath(),
			"status", status, "error", err.Error())
	}
}

// pathNotFound is the error for a path the repo does not have, with why when there is a reason:
// index.ErrNotFound, and never the format.ErrInvalid of a path that does not parse, which would
// be a 400 (Maven and Gradle take a 404 for a file that is not there).
func pathNotFound(repo index.Repo, p string, why error) error {
	if why == nil {
		return fmt.Errorf("%w: maven/%s has no %s", index.ErrNotFound, repo.Name, p)
	}
	return fmt.Errorf("%w: maven/%s has no %s: %s", index.ErrNotFound, repo.Name, p, strings.ReplaceAll(why.Error(), format.ErrInvalid.Error()+": ", ""))
}

// contentTypeOf returns the Content-Type of a file of a version by its extension.
func contentTypeOf(extension string) string {
	if strings.HasSuffix(extension, signatureExtension) || extension == "asc" {
		return contentTypeSignature
	}
	if ct, ok := contentTypes[extension]; ok {
		return ct
	}
	return "application/octet-stream"
}

// registryOf returns the provider of the repo's registry.
func registryOf(deps format.Deps, repo index.Repo) (provider.Provider, error) {
	if deps.Providers != nil {
		if p, ok := deps.Providers.Provider(repo.Registry); ok && p != nil {
			return p, nil
		}
	}
	return nil, fmt.Errorf("maven: repo maven/%s: registry %q is not configured", repo.Name, repo.Registry)
}

// ociName is the OCI repository of a package as the provider names it: repo.Prefix, or
// maven/<repo> when it is empty, and the package path, as ingest computes it.
func ociName(repo index.Repo, pkgPath string) string {
	base := strings.Trim(repo.Prefix, "/")
	if base == "" {
		base = repo.Format + "/" + repo.Name
	}
	return base + "/" + pkgPath
}
