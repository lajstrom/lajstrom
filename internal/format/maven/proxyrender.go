package maven

import (
	"context"
	"fmt"
	"slices"

	"github.com/lajstrom/lajstrom/internal/format"
	"github.com/lajstrom/lajstrom/internal/index"
)

// RenderProxy renders the maven-metadata.xml of kind of name for a proxy repo (docs/spec/
// http-routing.md, maven, Proxy). From kept, the upstream's document: the Maven metadata model
// as Lajstrom writes it, an artifact's without the versions the repo's policy.maven.versionPolicy
// does not take (takes), latest and release the upstream's while listed. Without kept, what the
// repo holds, as a hosted repo renders it (RenderIndex). A SNAPSHOT version's document in a
// release repo, and a file's document, which no route serves, wrap index.ErrNotFound. A
// maven-metadata.xml has no link, so base is not used.
func (Plugin) RenderProxy(ctx context.Context, repo index.Repo, name, kind string, kept []byte, st index.Reader, _ string) (format.Document, error) {
	c, err := artifactOf(name)
	if err != nil {
		return format.Document{}, err
	}
	snapshot, file, err := proxyKind(kind)
	switch {
	case err != nil:
		return format.Document{}, err
	case file != "":
		return format.Document{}, fmt.Errorf("%w: maven serves no document of a file, %s", index.ErrNotFound, file)
	case snapshot != "" && versionPolicy(repo) != PolicySnapshot:
		return format.Document{}, fmt.Errorf("%w: maven/%s takes release versions, and %s is a SNAPSHOT version", index.ErrNotFound,
			repo.Name, snapshot)
	}
	c.Version = snapshot
	path := artifactDir(c) + MetadataFile
	if snapshot != "" {
		path = c.Dir() + "/" + MetadataFile
	}
	if kept == nil {
		return renderHeld(ctx, repo, c, kind, path, st)
	}
	doc, err := readRepoMetadata(kept)
	if err != nil {
		return format.Document{}, fmt.Errorf("maven: the kept %s of %s: %w", MetadataFile, name, err)
	}
	if snapshot == "" {
		doc.keep(func(v string) bool { return takes(repo, v) })
	}
	body, err := doc.bytes()
	if err != nil {
		return format.Document{}, err
	}
	return format.Document{Kind: kind, Path: path, ContentType: contentTypeXML, Content: body}, nil
}

// renderHeld renders the maven-metadata.xml of the artifact c, or of its SNAPSHOT version
// c.Version, from what the repo holds, as RenderIndex does for a hosted repo, as the document of
// kind at path. index.ErrNotFound when the repo holds nothing of it.
func renderHeld(ctx context.Context, repo index.Repo, c Coordinates, kind, path string, st index.Reader) (format.Document, error) {
	docs, err := Plugin{}.RenderIndex(ctx, repo, metadataScope(c), st)
	if err != nil {
		return format.Document{}, err
	}
	for _, d := range docs {
		if d.Kind == kindMetadata {
			return format.Document{Kind: kind, Path: path, ContentType: contentTypeXML, Content: d.Content}, nil
		}
	}
	return format.Document{}, fmt.Errorf("%w: maven/%s holds nothing of %s", index.ErrNotFound, repo.Name, path)
}

// takes reports whether the repo's policy.maven.versionPolicy takes the version an artifact's
// maven-metadata.xml lists: a SNAPSHOT version in a snapshot repo, any other in a release repo.
func takes(repo index.Repo, version string) bool {
	return endsInSnapshot(version) == (versionPolicy(repo) == PolicySnapshot)
}

// keep leaves in the versions of the document those that listed reports, in their order, and
// keeps latest and release while listed; otherwise latest is the greatest version listed in
// Maven's version order, and release the greatest that is not a pre-release or a SNAPSHOT
// version, as a hosted repo chooses them, each absent when there is none.
func (doc *repoMetadata) keep(listed func(string) bool) {
	v := doc.Versioning
	if v == nil {
		return
	}
	v.Versions = slices.DeleteFunc(v.Versions, func(s string) bool { return !listed(s) })
	sorted := slices.Clone(v.Versions)
	sortVersions(sorted)
	if !slices.Contains(v.Versions, v.Latest) {
		v.Latest = ""
		if n := len(sorted); n > 0 {
			v.Latest = sorted[n-1]
		}
	}
	if !slices.Contains(v.Versions, v.Release) || endsInSnapshot(v.Release) {
		v.Release = ""
		for i := len(sorted) - 1; i >= 0 && v.Release == ""; i-- {
			if !isPrerelease(sorted[i]) && !endsInSnapshot(sorted[i]) {
				v.Release = sorted[i]
			}
		}
	}
}
