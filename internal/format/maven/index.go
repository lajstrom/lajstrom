package maven

import (
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"strings"

	"github.com/lajstrom/lajstrom/internal/format"
	"github.com/lajstrom/lajstrom/internal/index"
)

// The maven-metadata.xml of an artifact in index_cache (docs/spec/index-schema.md): its scope is
// <groupId>/<artifactId>, the coordinates as they are spelt (com.example/lib-core), and that of a
// SNAPSHOT version <groupId>/<artifactId>/<version> (com.example/lib-core/1.0-SNAPSHOT); its kind
// is maven-metadata, and each of its checksum files is a document of the same render, of the kind
// maven-metadata.<algorithm>, so that a checksum is always that of the bytes served.
const kindMetadata = "maven-metadata"

// The Content-Types of the files a client reads: XML (the POM and maven-metadata.xml), and a
// checksum file, its lowercase hex alone.
const (
	contentTypeXML      = "application/xml"
	contentTypeChecksum = "text/plain"
)

// lastUpdatedLayout is the format of lastUpdated in a maven-metadata.xml, yyyyMMddHHmmss in UTC.
const lastUpdatedLayout = "20060102150405"

// metadataScope returns the index_cache scope of the maven-metadata.xml of an artifact, or of
// its SNAPSHOT version c.Version when it has one.
func metadataScope(c Coordinates) format.IndexScope {
	if c.Version != "" {
		return format.IndexScope(c.GroupID + "/" + c.ArtifactID + "/" + c.Version)
	}
	return format.IndexScope(c.GroupID + "/" + c.ArtifactID)
}

// metadataKind returns the index_cache kind of the maven-metadata.xml, for algorithm "", or of
// its checksum file of algorithm.
func metadataKind(algorithm string) string {
	if algorithm == "" {
		return kindMetadata
	}
	return kindMetadata + "." + algorithm
}

// RenderIndex renders the maven-metadata.xml of an artifact, the scope <groupId>/<artifactId>,
// from the index: groupId, artifactId, and versioning with latest, the greatest version in
// Maven's version order, release, the greatest that is not a pre-release (1.0-rc1, 2.0.0-M3;
// docs/acceptance.md E-08) and absent when every version is one, the versions in that order, and
// lastUpdated, the newest created of the versions as yyyyMMddHHmmss in UTC; the builds of a
// SNAPSHOT version are one version there, the SNAPSHOT version. The scope
// <groupId>/<artifactId>/<base>-SNAPSHOT is the maven-metadata.xml of that SNAPSHOT version
// (renderSnapshot). It returns the document and its .md5, .sha1, .sha256 and .sha512 files,
// computed from its bytes. The render depends on the index alone, so that two renders of the
// same versions, on any replica, are the same bytes with the same checksums. An artifact that is
// not in the index, or has no version (no build), renders no document, which
// format.CachedDocument answers with index.ErrNotFound; another scope is index.ErrNotFound.
func (Plugin) RenderIndex(ctx context.Context, repo index.Repo, scope format.IndexScope, st index.Reader) ([]format.Document, error) {
	parts := strings.Split(string(scope), "/")
	if len(parts) < 2 || len(parts) > 3 || checkGroupArtifact(parts[0], parts[1]) != nil || (len(parts) == 3 && !isSnapshotVersion(parts[2])) {
		return nil, fmt.Errorf("%w: maven scope %q is not <groupId>/<artifactId>[/<base>-SNAPSHOT]", index.ErrNotFound, scope)
	}
	c := Coordinates{GroupID: parts[0], ArtifactID: parts[1]}
	pkg, err := st.GetPackage(ctx, repo.ID, c.Name())
	if errors.Is(err, index.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if len(parts) == 3 {
		c.Version = parts[2]
		return renderSnapshot(ctx, st, pkg, c)
	}
	groupID, artifactID := c.GroupID, c.ArtifactID
	vs, err := st.ListVersions(ctx, pkg.ID)
	if err != nil || len(vs) == 0 {
		return nil, err
	}
	return metadataDocuments(groupID, artifactID, vs)
}

// artifactMetadata is the maven-metadata.xml of an artifact.
type artifactMetadata struct {
	XMLName    xml.Name   `xml:"metadata"`
	GroupID    string     `xml:"groupId"`
	ArtifactID string     `xml:"artifactId"`
	Versioning versioning `xml:"versioning"`
}

// versioning is the versioning element of a maven-metadata.xml.
type versioning struct {
	Latest      string   `xml:"latest"`
	Release     string   `xml:"release,omitempty"`
	Versions    []string `xml:"versions>version"`
	LastUpdated string   `xml:"lastUpdated"`
}

// metadataDocuments renders the maven-metadata.xml of the versions vs of an artifact, at least
// one, and its checksum files. The builds of a SNAPSHOT version are listed once, as it.
func metadataDocuments(groupID, artifactID string, vs []index.Version) ([]format.Document, error) {
	var versions []string
	seen := map[string]bool{}
	updated := vs[0].CreatedAt
	for _, v := range vs {
		if d := versionDir(v.Version); !seen[d] {
			seen[d] = true
			versions = append(versions, d)
		}
		if v.CreatedAt.After(updated) {
			updated = v.CreatedAt
		}
	}
	sortVersions(versions)
	doc := artifactMetadata{GroupID: groupID, ArtifactID: artifactID, Versioning: versioning{
		Latest: versions[len(versions)-1], Versions: versions, LastUpdated: updated.UTC().Format(lastUpdatedLayout),
	}}
	for i := len(versions) - 1; i >= 0 && doc.Versioning.Release == ""; i-- {
		if !isPrerelease(versions[i]) {
			doc.Versioning.Release = versions[i]
		}
	}
	body, err := xml.MarshalIndent(doc, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("maven: the %s of %s:%s: %w", MetadataFile, groupID, artifactID, err)
	}
	body = append(append([]byte(xml.Header), body...), '\n')
	return withChecksums(strings.ReplaceAll(groupID, ".", "/")+"/"+artifactID+"/"+MetadataFile, body), nil
}

// withChecksums returns the maven-metadata.xml body at path and its four checksum files,
// computed from its bytes.
func withChecksums(path string, body []byte) []format.Document {
	docs := []format.Document{{Kind: kindMetadata, Path: path, ContentType: contentTypeXML, Content: body}}
	h := newHashes()
	_, _ = h.writer().Write(body) // a hash takes every write
	sums := h.sums()
	for _, a := range algorithms {
		docs = append(docs, format.Document{Kind: metadataKind(a.name), Path: path + "." + a.name, ContentType: contentTypeChecksum,
			Content: []byte(sums[a.name])})
	}
	return docs
}
