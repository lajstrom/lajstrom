package maven

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/lajstrom/lajstrom/internal/format"
	"github.com/lajstrom/lajstrom/internal/index"
)

// maven has group repos (ADR 0013, docs/spec/http-routing.md, maven, Group): the core asks the
// members for their maven-metadata.xml and merges them here; a file comes from the first member
// that holds it, a hosted member's version holding it without a variant.
var _ format.Grouper = Plugin{}

// MergeGroup merges the maven-metadata.xml of kind of name of the members of a group that take
// part, in member order (docs/spec/http-routing.md, maven, Group): an artifact's (mergeArtifact)
// or a SNAPSHOT version's (mergeSnapshot), written as RenderProxy writes a proxy's. A
// maven-metadata.xml has no link, so base is not used. A document that does not read wraps
// format.ErrInvalid.
func (Plugin) MergeGroup(_ context.Context, _ index.Repo, name, kind string, docs []format.MemberDocument, _ string) (format.Document, error) {
	c, err := artifactOf(name)
	if err != nil {
		return format.Document{}, err
	}
	snapshot, file, err := proxyKind(kind)
	switch {
	case err != nil:
		return format.Document{}, err
	case file != "":
		return format.Document{}, fmt.Errorf("%w: maven serves no document of a file, %s", format.ErrInvalid, file)
	}
	parsed := make([]*repoMetadata, len(docs))
	for i, d := range docs {
		if parsed[i], err = readRepoMetadata(d.Content); err != nil {
			return format.Document{}, fmt.Errorf("maven: the %s of %s in %s/%s: %w", MetadataFile, name, d.Repo.Format, d.Repo.Name, err)
		}
	}
	merged, path := mergeArtifact(parsed), artifactDir(c)+MetadataFile
	if snapshot != "" {
		c.Version = snapshot
		merged, path = mergeSnapshot(parsed), c.Dir()+"/"+MetadataFile
	}
	body, err := merged.bytes()
	if err != nil {
		return format.Document{}, err
	}
	return format.Document{Kind: kind, Path: path, ContentType: contentTypeXML, Content: body}, nil
}

// head returns a document with the root of the first of docs that has each: modelVersion,
// groupId, artifactId and version.
func head(docs []*repoMetadata) *repoMetadata {
	out := &repoMetadata{}
	for _, d := range docs {
		out.ModelVersion = cmp.Or(out.ModelVersion, d.ModelVersion)
		out.GroupID = cmp.Or(out.GroupID, d.GroupID)
		out.ArtifactID = cmp.Or(out.ArtifactID, d.ArtifactID)
		out.Version = cmp.Or(out.Version, d.Version)
	}
	return out
}

// mergeArtifact merges the maven-metadata.xml of an artifact of several members: versions their
// union in Maven's version order, latest the greatest of their latest in that order, release
// likewise of their release, lastUpdated the latest, the plugins their union by prefix, a prefix
// the first member's.
func mergeArtifact(docs []*repoMetadata) *repoMetadata {
	out := head(docs)
	var v *repoVersioning
	plugins := map[string]bool{}
	for _, d := range docs {
		for _, p := range d.Plugins {
			if !plugins[p.Prefix] {
				plugins[p.Prefix] = true
				out.Plugins = append(out.Plugins, p)
			}
		}
		if d.Versioning == nil {
			continue
		}
		if v == nil {
			v = &repoVersioning{}
		}
		for _, s := range d.Versioning.Versions {
			if !slices.Contains(v.Versions, s) {
				v.Versions = append(v.Versions, s)
			}
		}
		v.Latest, v.Release = greater(v.Latest, d.Versioning.Latest), greater(v.Release, d.Versioning.Release)
		v.LastUpdated = max(v.LastUpdated, d.Versioning.LastUpdated)
	}
	if v != nil {
		sortVersions(v.Versions)
	}
	out.Versioning = v
	return out
}

// greater returns the greater of two versions in Maven's version order, either when the other
// is "".
func greater(a, b string) string {
	if a == "" || (b != "" && compareVersions(b, a) > 0) {
		return b
	}
	return a
}

// mergeSnapshot merges the maven-metadata.xml of a SNAPSHOT version of several members: snapshot
// and lastUpdated of the member whose latest build is the newest, by its timestamp and then its
// build number, the first member's on a tie; snapshotVersions their union by classifier and
// extension, each the one with the newest updated, the first member's on a tie, sorted as a
// hosted repo sorts them.
func mergeSnapshot(docs []*repoMetadata) *repoMetadata {
	out := head(docs)
	var newest *repoVersioning
	var files []snapshotVersion
	for _, d := range docs {
		v := d.Versioning
		if v == nil {
			continue
		}
		if newest == nil || newerBuild(v.Snapshot, newest.Snapshot) {
			newest = v
		}
		for _, sv := range v.SnapshotVersions {
			i := slices.IndexFunc(files, func(f snapshotVersion) bool { return f.Classifier == sv.Classifier && f.Extension == sv.Extension })
			switch {
			case i < 0:
				files = append(files, sv)
			case sv.Updated > files[i].Updated:
				files[i] = sv
			}
		}
	}
	if newest == nil {
		return out
	}
	slices.SortFunc(files, func(a, b snapshotVersion) int {
		return cmp.Or(strings.Compare(a.Extension, b.Extension), strings.Compare(a.Classifier, b.Classifier))
	})
	out.Versioning = &repoVersioning{Snapshot: newest.Snapshot, LastUpdated: newest.LastUpdated, SnapshotVersions: files}
	return out
}

// newerBuild reports whether the latest build a is newer than b: by timestamp, then build
// number; a build is newer than none.
func newerBuild(a, b *repoSnapshot) bool {
	switch {
	case a == nil:
		return false
	case b == nil:
		return true
	}
	return cmp.Or(strings.Compare(a.Timestamp, b.Timestamp), cmp.Compare(a.BuildNumber, b.BuildNumber)) > 0
}
