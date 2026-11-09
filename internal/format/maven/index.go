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
// <groupId>/<artifactId>, the coordinates as they are spelt (com.example/lib-core), its kind
// maven-metadata, and each of its checksum files is a document of the same render, of the kind
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
// lastUpdated, the newest created of the versions as yyyyMMddHHmmss in UTC. It
// returns the document and its .md5, .sha1, .sha256 and .sha512 files, computed from its bytes.
// The render depends on the index alone, so that two renders of the same versions, on any
// replica, are the same bytes with the same checksums. An artifact that is not in the index, or
// has no version, renders no document, which format.CachedDocument answers with
// index.ErrNotFound; a scope that is not <groupId>/<artifactId> is index.ErrNotFound.
func (Plugin) RenderIndex(ctx context.Context, repo index.Repo, scope format.IndexScope, st index.Reader) ([]format.Document, error) {
	groupID, artifactID, ok := strings.Cut(string(scope), "/")
	if !ok || checkGroupArtifact(groupID, artifactID) != nil {
		return nil, fmt.Errorf("%w: maven scope %q is not <groupId>/<artifactId>", index.ErrNotFound, scope)
	}
	pkg, err := st.GetPackage(ctx, repo.ID, Coordinates{GroupID: groupID, ArtifactID: artifactID}.Name())
	if errors.Is(err, index.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
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
// one, and its checksum files.
func metadataDocuments(groupID, artifactID string, vs []index.Version) ([]format.Document, error) {
	versions := make([]string, len(vs))
	updated := vs[0].CreatedAt
	for i, v := range vs {
		versions[i] = v.Version
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
	path := strings.ReplaceAll(groupID, ".", "/") + "/" + artifactID + "/" + MetadataFile
	docs := []format.Document{{Kind: kindMetadata, Path: path, ContentType: contentTypeXML, Content: body}}
	h := newHashes()
	_, _ = h.writer().Write(body) // a hash takes every write
	sums := h.sums()
	for _, a := range algorithms {
		docs = append(docs, format.Document{Kind: metadataKind(a.name), Path: path + "." + a.name, ContentType: contentTypeChecksum,
			Content: []byte(sums[a.name])})
	}
	return docs, nil
}
