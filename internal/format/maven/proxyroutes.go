package maven

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/lajstrom/lajstrom/internal/format"
	"github.com/lajstrom/lajstrom/internal/index"
)

// The routes of a proxy repo and of a group of maven are the hosted repo's (docs/spec/
// http-routing.md §6): their maven-metadata.xml and their files come from the core of proxy and
// group repos, Deps.Proxy, which asks the upstream and fetches a file on its first request, and
// merges a group's members; the checksum files of a document are of its bytes, those of a file
// from the metadata layer of the version that holds it.

// proxied reports whether the documents and the files of repo come from the core: a proxy
// repo's and a group's.
func proxied(repo index.Repo) bool {
	return repo.Kind == index.KindProxy || repo.Kind == index.KindGroup
}

// proxyCore returns the core of proxy and group repos, or an error wrapping
// format.ErrUnsupported when the server has none.
func (h *resolver) proxyCore(repo index.Repo) (format.Proxy, error) {
	if h.deps.Proxy == nil {
		return nil, fmt.Errorf("%w: maven/%s is a %s repo, and this server serves none", format.ErrUnsupported, repo.Name, repo.Kind)
	}
	return h.deps.Proxy, nil
}

// proxyMetadata serves the maven-metadata.xml of the artifact c, or of its SNAPSHOT version
// c.Version, of a proxy repo or a group, for algorithm "", or its checksum file of algorithm,
// computed from the bytes served. A SNAPSHOT version's in a proxy repo whose policy takes
// releases is 404, and nothing is asked of the upstream for it.
func (h *resolver) proxyMetadata(w http.ResponseWriter, r *http.Request, repo index.Repo, c Coordinates, p, algorithm string) {
	if repo.Kind == index.KindProxy && c.Version != "" && !takes(repo, c.Version) {
		h.fail(w, r, pathNotFound(repo, p, fmt.Errorf("its policy.maven.versionPolicy is %s", versionPolicy(repo))))
		return
	}
	core, err := h.proxyCore(repo)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	kind := kindMetadata
	if c.Version != "" {
		kind = snapshotKind(c.Version)
	}
	doc, err := core.Document(r.Context(), repo, c.Name(), kind)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	if algorithm == "" {
		format.ServeDocument(w, r, doc, contentTypeXML)
		return
	}
	sums := newHashes()
	_, _ = sums.writer().Write(doc.Content) // a hash takes every write
	sum := sums.sums()[algorithm]
	etag := sha256.Sum256([]byte(sum))
	format.ServeDocument(w, r, &index.Cache{Content: []byte(sum), SHA256: hex.EncodeToString(etag[:]), RenderedAt: doc.RenderedAt},
		contentTypeChecksum)
}

// proxyLookup returns the file at path p of a proxy repo or a group, held or fetched by the core
// (format.Proxy.File), and what its path says: for the non-unique name of a SNAPSHOT version,
// the file of the build its maven-metadata.xml names (snapshotBuild). A file of a version that a
// proxy repo's policy does not take is index.ErrNotFound, and nothing is asked of the upstream.
func (h *resolver) proxyLookup(ctx context.Context, repo index.Repo, p string) (*index.FileMatch, File, error) {
	f, err := parseFilePath(p)
	if err != nil {
		return nil, File{}, pathNotFound(repo, p, err)
	}
	if repo.Kind == index.KindProxy && !takes(repo, versionDir(f.Version)) {
		return nil, File{}, pathNotFound(repo, p, fmt.Errorf("its policy.maven.versionPolicy is %s", versionPolicy(repo)))
	}
	core, err := h.proxyCore(repo)
	if err != nil {
		return nil, File{}, err
	}
	if isSnapshotVersion(f.Version) {
		build, err := h.snapshotBuild(ctx, core, repo, f, p)
		if err != nil {
			return nil, File{}, err
		}
		f.Version = build
	}
	m, err := core.File(ctx, repo, f.Path())
	return m, f, err
}

// snapshotBuild returns the build of the SNAPSHOT version f.Version that its maven-metadata.xml
// names for the classifier and extension of f: the value of that snapshotVersion, else the
// timestamp and build number of its snapshot. index.ErrNotFound when it names none.
func (h *resolver) snapshotBuild(ctx context.Context, core format.Proxy, repo index.Repo, f File, p string) (string, error) {
	doc, err := core.Document(ctx, repo, f.Name(), snapshotKind(f.Version))
	if err != nil {
		return "", err
	}
	md, err := readRepoMetadata(doc.Content)
	if err != nil {
		return "", fmt.Errorf("maven: the %s of %s in maven/%s: %w", MetadataFile, f.Coordinates, repo.Name, err)
	}
	if v := md.Versioning; v != nil {
		for _, sv := range v.SnapshotVersions {
			if b, err := parseBuild(sv.Value); err == nil && sv.Classifier == f.Classifier && sv.Extension == f.Extension && b.snapshot() == f.Version {
				return sv.Value, nil
			}
		}
		if s := v.Snapshot; s != nil && s.Timestamp != "" {
			build := strings.TrimSuffix(f.Version, snapshotSuffix) + "-" + s.Timestamp + "-" + strconv.Itoa(s.BuildNumber)
			if _, err := parseBuild(build); err == nil {
				return build, nil
			}
		}
	}
	return "", pathNotFound(repo, p, fmt.Errorf("the %s of %s names no build of it", MetadataFile, f.Version))
}

// holder returns the repo that holds the file m that repo answered with: repo itself, or the
// member of a group whose registry serves it.
func (h *resolver) holder(ctx context.Context, repo index.Repo, m *index.FileMatch) (index.Repo, error) {
	if m.Package.RepoID == repo.ID {
		return repo, nil
	}
	member, err := h.deps.Index.GetRepoByID(ctx, m.Package.RepoID)
	if err != nil {
		return index.Repo{}, fmt.Errorf("maven: the member of maven/%s that holds %s: %w", repo.Name, m.File.Name, err)
	}
	return *member, nil
}
