package maven

import (
	"cmp"
	"context"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"slices"
	"strings"

	"github.com/lajstrom/lajstrom/internal/format"
	"github.com/lajstrom/lajstrom/internal/index"
)

// snapshotMetadata is the maven-metadata.xml of a SNAPSHOT version, as Maven 3 writes it
// (docs/spec/http-routing.md §3, maven).
type snapshotMetadata struct {
	XMLName      xml.Name           `xml:"metadata"`
	ModelVersion string             `xml:"modelVersion,attr"`
	GroupID      string             `xml:"groupId"`
	ArtifactID   string             `xml:"artifactId"`
	Version      string             `xml:"version"`
	Versioning   snapshotVersioning `xml:"versioning"`
}

// snapshotVersioning is the versioning of a SNAPSHOT version: the latest build, and per
// classifier and extension the newest build that has such a file.
type snapshotVersioning struct {
	Snapshot struct {
		Timestamp   string `xml:"timestamp"`
		BuildNumber int    `xml:"buildNumber"`
	} `xml:"snapshot"`
	LastUpdated      string            `xml:"lastUpdated"`
	SnapshotVersions []snapshotVersion `xml:"snapshotVersions>snapshotVersion"`
}

// snapshotVersion is a file of a build in the metadata of its SNAPSHOT version.
type snapshotVersion struct {
	Classifier string `xml:"classifier,omitempty"`
	Extension  string `xml:"extension"`
	Value      string `xml:"value"`
	Updated    string `xml:"updated"`
}

// renderSnapshot renders the maven-metadata.xml of the SNAPSHOT version snapshot of
// groupId:artifactId from the builds the index has of it, and its checksum files; none when it
// has no build.
func renderSnapshot(ctx context.Context, st index.Reader, pkg *index.Package, c Coordinates) ([]format.Document, error) {
	vs, err := st.ListVersions(ctx, pkg.ID)
	if err != nil {
		return nil, err
	}
	builds := buildsOf(vs, c.Version)
	if len(builds) == 0 {
		return nil, nil
	}
	latest := builds[len(builds)-1]
	doc := snapshotMetadata{ModelVersion: "1.1.0", GroupID: c.GroupID, ArtifactID: c.ArtifactID, Version: c.Version}
	doc.Versioning.Snapshot.Timestamp, doc.Versioning.Snapshot.BuildNumber = latest.timestamp, latest.number
	doc.Versioning.LastUpdated = latest.updated()
	seen := map[[2]string]bool{}
	for i := len(builds) - 1; i >= 0; i-- {
		b := builds[i]
		files, err := buildFiles(ctx, st, c, b)
		if err != nil {
			return nil, err
		}
		for _, f := range files {
			if key := [2]string{f.Classifier, f.Extension}; !seen[key] {
				seen[key] = true
				doc.Versioning.SnapshotVersions = append(doc.Versioning.SnapshotVersions,
					snapshotVersion{Classifier: f.Classifier, Extension: f.Extension, Value: b.version(), Updated: b.updated()})
			}
		}
	}
	slices.SortFunc(doc.Versioning.SnapshotVersions, func(a, b snapshotVersion) int {
		return cmp.Or(strings.Compare(a.Extension, b.Extension), strings.Compare(a.Classifier, b.Classifier))
	})
	body, err := xml.MarshalIndent(doc, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("maven: the %s of %s: %w", MetadataFile, c, err)
	}
	return withChecksums(c.Dir()+"/"+MetadataFile, append(append([]byte(xml.Header), body...), '\n')), nil
}

// buildFiles returns the classifier and extension of each file of the build b of c: from the
// metadata layer in its row, or, for a row without it (degraded), from the names of its files.
func buildFiles(ctx context.Context, st index.Reader, c Coordinates, b builtVersion) ([]File, error) {
	var md metadata
	if b.row.Metadata != nil && json.Unmarshal(b.row.Metadata, &md) == nil && len(md.Files) > 0 {
		out := make([]File, len(md.Files))
		for i, f := range md.Files {
			out[i] = File{Classifier: f.Classifier, Extension: f.Extension}
		}
		return out, nil
	}
	rows, err := st.ListFiles(ctx, b.row.ID)
	if err != nil {
		return nil, fmt.Errorf("maven: the files of %s:%s: %w", c.Name(), b.version(), err)
	}
	var out []File
	for _, r := range rows {
		if f, err := ParseFilename(Coordinates{GroupID: c.GroupID, ArtifactID: c.ArtifactID, Version: b.version()}, r.Name); err == nil {
			out = append(out, f)
		}
	}
	return out, nil
}
