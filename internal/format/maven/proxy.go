package maven

import (
	"bytes"
	"encoding/hex"
	"encoding/xml"
	"fmt"
	"strings"

	"github.com/lajstrom/lajstrom/internal/format"
	"github.com/lajstrom/lajstrom/internal/index"
)

// The proxy hooks of maven (ADR 0013 and its Amendment, docs/spec/http-routing.md, maven,
// Proxy): a proxy repo keeps the upstream's maven-metadata.xml of an artifact and of a SNAPSHOT
// version in proxy_meta, and fetches a file on its first request, alone. A Maven 2 repository
// lists no files of a version, so the document that lists a file is the upstream's .sha1 of it,
// a document of its own; a signature without one is fetched unchecked (UnlistedEntry).
// RenderProxy is in proxyrender.go, the routes in proxyroutes.go.

var _ format.Proxier = Plugin{}

// The document kinds of a proxy repo besides kindMetadata, the maven-metadata.xml of an
// artifact: kindSnapshotPrefix and the SNAPSHOT version, the maven-metadata.xml of that version;
// kindFilePrefix and <version directory>/<file name>, the upstream's .sha1 of the file.
const (
	kindSnapshotPrefix = kindMetadata + ":"
	kindFilePrefix     = "file:"
)

// Key returns groupId:artifactId as it is spelt, which the patterns of the allow and deny lists
// see ignoring case. A name that is not well-formed coordinates wraps format.ErrInvalid.
func (Plugin) Key(name string) (string, error) {
	if _, err := artifactOf(name); err != nil {
		return "", err
	}
	return name, nil
}

// artifactOf returns the groupId and artifactId of name, groupId:artifactId.
func artifactOf(name string) (Coordinates, error) {
	g, a, ok := strings.Cut(name, ":")
	if !ok {
		return Coordinates{}, fmt.Errorf("%w: %q is not groupId:artifactId", format.ErrInvalid, name)
	}
	if err := checkGroupArtifact(g, a); err != nil {
		return Coordinates{}, fmt.Errorf("%q: %w", name, err)
	}
	return Coordinates{GroupID: g, ArtifactID: a}, nil
}

// artifactDir returns the directory of the artifact c in a Maven 2 repository, with a trailing
// slash: com/example/lib-core/.
func artifactDir(c Coordinates) string {
	return strings.ReplaceAll(c.GroupID, ".", "/") + "/" + c.ArtifactID + "/"
}

// snapshotKind and fileKind are the kinds of the document of the SNAPSHOT version snapshot and
// of the file f.
func snapshotKind(snapshot string) string { return kindSnapshotPrefix + snapshot }
func fileKind(f File) string              { return kindFilePrefix + versionDir(f.Version) + "/" + f.Filename() }

// proxyKind is what a document kind names: the artifact's maven-metadata.xml (both ""), a
// SNAPSHOT version's (snapshot), or a file's .sha1 (file, its path below the artifact's
// directory). Another kind wraps format.ErrInvalid.
func proxyKind(kind string) (snapshot, file string, err error) {
	switch {
	case kind == kindMetadata:
		return "", "", nil
	case strings.HasPrefix(kind, kindSnapshotPrefix) && isSnapshotVersion(strings.TrimPrefix(kind, kindSnapshotPrefix)):
		return strings.TrimPrefix(kind, kindSnapshotPrefix), "", nil
	case strings.HasPrefix(kind, kindFilePrefix) && strings.Count(kind, "/") == 1:
		return "", strings.TrimPrefix(kind, kindFilePrefix), nil
	}
	return "", "", fmt.Errorf("%w: maven has no document of kind %q", format.ErrInvalid, kind)
}

// MetadataRequest is the GET of the document of kind of name below the upstream's URL, the
// layout of a Maven 2 repository: <groupPath>/<artifactId>/maven-metadata.xml,
// <groupPath>/<artifactId>/<base>-SNAPSHOT/maven-metadata.xml, or the file's path and .sha1.
func (Plugin) MetadataRequest(name, kind string) (format.UpstreamRequest, error) {
	c, err := artifactOf(name)
	if err != nil {
		return format.UpstreamRequest{}, err
	}
	snapshot, file, err := proxyKind(kind)
	switch {
	case err != nil:
		return format.UpstreamRequest{}, err
	case file != "":
		return format.UpstreamRequest{Path: artifactDir(c) + file + "." + SHA1}, nil
	case snapshot != "":
		return format.UpstreamRequest{Path: artifactDir(c) + snapshot + "/" + MetadataFile, Accept: contentTypeXML}, nil
	}
	return format.UpstreamRequest{Path: artifactDir(c) + MetadataFile, Accept: contentTypeXML}, nil
}

// ParseMetadata reads an upstream's document of kind of name. A maven-metadata.xml is kept as
// the upstream sent it and lists no entry: it must be the Maven metadata model, and when it
// names its groupId, artifactId or version, those of name and kind; a group's metadata, the
// plugin prefixes, names none of them. A file's .sha1 is kept as the lowercase hex of its first
// word, 40 hex digits (a file name may follow it), and lists the one entry of the file: its
// version, its Variant, the file at its path with that sha1, and the path as ParamPath. A body
// that is none of these wraps format.ErrInvalid.
func (Plugin) ParseMetadata(name, kind string, body []byte) (*format.UpstreamIndex, error) {
	c, err := artifactOf(name)
	if err != nil {
		return nil, err
	}
	snapshot, file, err := proxyKind(kind)
	if err != nil {
		return nil, err
	}
	if file != "" {
		return fileEntry(c, file, body)
	}
	doc, err := readRepoMetadata(body)
	if err != nil {
		return nil, fmt.Errorf("the %s of %s: %w", MetadataFile, name, err)
	}
	if err := doc.of(c, snapshot); err != nil {
		return nil, fmt.Errorf("the %s of %s: %w", MetadataFile, name, err)
	}
	return &format.UpstreamIndex{Keep: body, ContentType: contentTypeXML}, nil
}

// fileEntry reads the .sha1 of the file at path below the directory of the artifact c.
func fileEntry(c Coordinates, path string, body []byte) (*format.UpstreamIndex, error) {
	full := artifactDir(c) + path
	f, err := ParsePath(full)
	if err != nil {
		return nil, err
	}
	fields := strings.Fields(string(body))
	if len(fields) == 0 {
		return nil, fmt.Errorf("%w: the %s of %s is empty", format.ErrInvalid, SHA1, full)
	}
	sum := strings.ToLower(fields[0])
	if raw, err := hex.DecodeString(sum); err != nil || len(raw) != 20 {
		return nil, fmt.Errorf("%w: the %s of %s is not 40 hex digits", format.ErrInvalid, SHA1, full)
	}
	return &format.UpstreamIndex{Keep: []byte(sum), ContentType: contentTypeChecksum, Entries: []format.UpstreamEntry{{
		Version: f.Version, Variant: Variant(f), Params: map[string]string{ParamPath: full},
		Files: []format.UpstreamFile{{Name: f.Filename(), URL: full, Checksums: map[string]string{SHA1: sum}}},
	}}}, nil
}

var _ format.Unlisted = Plugin{}

// UnlistedEntry returns the entry of a signature, a file whose extension ends in .asc, when the
// upstream has no .sha1 of it, the document of ref.Kind: the one file at its path, Unchecked,
// without a checksum (docs/spec/http-routing.md, maven, Proxy; ADR 0013, Amendment). A forged
// signature fails its verification by itself. Any other file is false: without its .sha1 it is
// 404.
func (Plugin) UnlistedEntry(ref format.ArtifactRef) (format.UpstreamEntry, bool) {
	_, file, err := proxyKind(ref.Kind)
	if err != nil || file == "" {
		return format.UpstreamEntry{}, false
	}
	c, err := artifactOf(ref.Name)
	if err != nil {
		return format.UpstreamEntry{}, false
	}
	full := artifactDir(c) + file
	f, err := ParsePath(full)
	if err != nil || !strings.HasSuffix("."+f.Extension, signatureExtension) || f.Filename() != ref.File ||
		f.Version != ref.Version || Variant(f) != ref.Variant {
		return format.UpstreamEntry{}, false
	}
	return format.UpstreamEntry{Version: f.Version, Variant: Variant(f), Params: map[string]string{ParamPath: full},
		Files: []format.UpstreamFile{{Name: f.Filename(), URL: full, Unchecked: true}}}, true
}

// FileRequest maps the path of a file of a version, a release's or a build's, to the artifact
// of that one file (docs/spec/manifest-profile.md §4.2): its version, its Variant, its file
// name, listed by the document of its .sha1. The non-unique name of a SNAPSHOT version, a
// checksum file, a maven-metadata.xml and any other path wrap index.ErrNotFound.
func (Plugin) FileRequest(path string) (format.ArtifactRef, error) {
	f, err := ParsePath(path)
	if err != nil {
		return format.ArtifactRef{}, fmt.Errorf("%w: maven: %s names no file a proxy fetches: %s", index.ErrNotFound, path,
			strings.ReplaceAll(err.Error(), format.ErrInvalid.Error()+": ", ""))
	}
	return format.ArtifactRef{Name: f.Name(), Version: f.Version, Variant: Variant(f), File: f.Filename(), Kind: fileKind(f)}, nil
}

// repoMetadata is the Maven repository metadata model as a proxy reads an upstream's
// maven-metadata.xml and writes its own: an artifact's versions, a SNAPSHOT version's builds, a
// group's plugin prefixes. An element the upstream's does not have stays out.
type repoMetadata struct {
	XMLName      xml.Name        `xml:"metadata"`
	ModelVersion string          `xml:"modelVersion,attr,omitempty"`
	GroupID      string          `xml:"groupId,omitempty"`
	ArtifactID   string          `xml:"artifactId,omitempty"`
	Version      string          `xml:"version,omitempty"`
	Versioning   *repoVersioning `xml:"versioning,omitempty"`
	Plugins      []repoPlugin    `xml:"plugins>plugin,omitempty"`
}

// repoVersioning is the versioning of a maven-metadata.xml, of an artifact or of a SNAPSHOT
// version.
type repoVersioning struct {
	Latest           string            `xml:"latest,omitempty"`
	Release          string            `xml:"release,omitempty"`
	Snapshot         *repoSnapshot     `xml:"snapshot,omitempty"`
	Versions         []string          `xml:"versions>version,omitempty"`
	LastUpdated      string            `xml:"lastUpdated,omitempty"`
	SnapshotVersions []snapshotVersion `xml:"snapshotVersions>snapshotVersion,omitempty"`
}

// repoSnapshot is the latest build of a SNAPSHOT version.
type repoSnapshot struct {
	Timestamp   string `xml:"timestamp,omitempty"`
	BuildNumber int    `xml:"buildNumber,omitempty"`
}

// repoPlugin is a plugin prefix of a group's metadata.
type repoPlugin struct {
	Name       string `xml:"name,omitempty"`
	Prefix     string `xml:"prefix"`
	ArtifactID string `xml:"artifactId"`
}

// of checks the coordinates a maven-metadata.xml names against those of its path, the artifact
// c and the SNAPSHOT version snapshot, "" for the artifact's: a groupId, an artifactId and the
// version of a SNAPSHOT version's document, each when it has one. The metadata of a group, the
// plugin prefixes, has the path of an artifact's document and may name the group's groupId,
// c's groupId and artifactId joined with "."; an artifact's may name a version, its last deploy.
func (doc *repoMetadata) of(c Coordinates, snapshot string) error {
	group := doc.GroupID == c.GroupID+"."+c.ArtifactID && doc.ArtifactID == ""
	switch {
	case doc.GroupID != "" && doc.GroupID != c.GroupID && !group:
		return fmt.Errorf("%w: it names groupId %q", format.ErrInvalid, doc.GroupID)
	case doc.ArtifactID != "" && doc.ArtifactID != c.ArtifactID:
		return fmt.Errorf("%w: it names artifactId %q", format.ErrInvalid, doc.ArtifactID)
	case snapshot != "" && doc.Version != "" && doc.Version != snapshot:
		return fmt.Errorf("%w: it names version %q", format.ErrInvalid, doc.Version)
	}
	return nil
}

// readRepoMetadata reads a maven-metadata.xml. One whose root is not metadata wraps
// format.ErrInvalid.
func readRepoMetadata(body []byte) (*repoMetadata, error) {
	var doc repoMetadata
	dec := xml.NewDecoder(bytes.NewReader(body))
	dec.CharsetReader = charsetReader
	if err := dec.Decode(&doc); err != nil {
		return nil, fmt.Errorf("%w: not a %s: %w", format.ErrInvalid, MetadataFile, err)
	}
	return &doc, nil
}

// bytes returns the document as Lajstrom writes a maven-metadata.xml: the XML declaration, the
// elements indented by two spaces, a final line break.
func (doc *repoMetadata) bytes() ([]byte, error) {
	body, err := xml.MarshalIndent(doc, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("maven: write a %s: %w", MetadataFile, err)
	}
	return append(append([]byte(xml.Header), body...), '\n'), nil
}
