package maven

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/lajstrom/lajstrom/internal/format"
)

// The checksum algorithms of a Maven 2 repository: the extension of a checksum file, a key of
// format.UploadedFile.Checksums and a field of a file in the metadata layer.
const (
	MD5    = "md5"
	SHA1   = "sha1"
	SHA256 = "sha256"
	SHA512 = "sha512"
)

// algorithms are the checksum algorithms with the number of lowercase hex digits of a digest.
var algorithms = []struct {
	name   string
	digits int
}{{MD5, 32}, {SHA1, 40}, {SHA256, 64}, {SHA512, 128}}

// MetadataFile is the file name of the metadata of an artifact, the list of its versions.
const MetadataFile = "maven-metadata.xml"

// signatureExtension ends the extension of an OpenPGP signature: lib-core-1.2.0.jar.asc.
const signatureExtension = ".asc"

// Kind is what a path below /maven/<repo>/ names.
type Kind int

const (
	// KindFile is a file of a version: an artifact, the POM or a signature (ParsePath).
	KindFile Kind = iota + 1
	// KindChecksum is the checksum file of another path (ChecksumOf).
	KindChecksum
	// KindMetadata is a maven-metadata.xml (ParseMetadataPath).
	KindMetadata
)

// Classify says what a path names by its last element alone: a checksum file by its extension,
// a maven-metadata.xml by its name, and any other path a file of a version. It does not check
// the path; ParsePath, ChecksumOf and ParseMetadataPath do.
func Classify(p string) Kind {
	if _, _, ok := ChecksumOf(p); ok {
		return KindChecksum
	}
	if p == MetadataFile || strings.HasSuffix(p, "/"+MetadataFile) {
		return KindMetadata
	}
	return KindFile
}

// ChecksumOf reads the path or file name of a checksum file: p with .md5, .sha1, .sha256 or
// .sha512 appended to the path of the file it is the checksum of, the target. ok is false when
// p has none of these extensions or nothing before it. The target can itself be a signature
// (lib-core-1.2.0.jar.asc.sha1) or a maven-metadata.xml.
func ChecksumOf(p string) (target, algorithm string, ok bool) {
	for _, a := range algorithms {
		t, found := strings.CutSuffix(p, "."+a.name)
		if found && t != "" && !strings.HasSuffix(t, "/") {
			return t, a.name, true
		}
	}
	return "", "", false
}

// Coordinates name a version of a maven artifact. The JSON keys are the POM's.
type Coordinates struct {
	// GroupID is the groupId, dot-separated: com.example.
	GroupID string `json:"groupId"`
	// ArtifactID is the artifactId: lib-core.
	ArtifactID string `json:"artifactId"`
	// Version is the version, exactly as written: 1.2.0.
	Version string `json:"version"`
}

// Name returns the package name of the coordinates, the value of lajstrom.name:
// groupId:artifactId.
func (c Coordinates) Name() string { return c.GroupID + ":" + c.ArtifactID }

// Dir returns the directory of the version in a Maven 2 repository, the groupId with "/" for
// ".", then the artifactId and the version, the SNAPSHOT version of a build:
// com/example/lib-core/1.2.0, com/example/lib-core/1.0-SNAPSHOT for 1.0-20261114.101112-3.
func (c Coordinates) Dir() string {
	return strings.ReplaceAll(c.GroupID, ".", "/") + "/" + c.ArtifactID + "/" + versionDir(c.Version)
}

// String returns groupId:artifactId:version.
func (c Coordinates) String() string { return c.Name() + ":" + c.Version }

// File is a file of a version in a Maven 2 repository: an artifact, the POM, or the OpenPGP
// signature of one of them.
type File struct {
	Coordinates
	// Classifier is the classifier of the file name (sources, javadoc, linux-x86_64); empty
	// when the file has none.
	Classifier string
	// Extension is the rest of the file name after the dot that ends the version or the
	// classifier: jar, pom, tar.gz, and jar.asc for the signature of a jar.
	Extension string
}

// Filename returns the file name, <artifactId>-<version>[-<classifier>].<extension>.
func (f File) Filename() string {
	name := f.ArtifactID + "-" + f.Version
	if f.Classifier != "" {
		name += "-" + f.Classifier
	}
	return name + "." + f.Extension
}

// Path returns the path of the file in a Maven 2 repository: Dir()/Filename().
func (f File) Path() string { return f.Dir() + "/" + f.Filename() }

// IsPOM reports whether the file is the POM of its version: the extension pom and no
// classifier.
func (f File) IsPOM() bool { return f.Extension == "pom" && f.Classifier == "" }

// The grammars of the coordinates. A groupId is dot-separated parts, so that it has exactly one
// directory form; an artifactId does not start with ".", so that it is never "." or ".."; a
// version and a classifier are letters, digits, ".", "_", "+" and "-", which every release on
// Maven Central keeps to and which leaves no room for "${", a version range or a path. An
// extension is dot-separated parts.
var (
	groupIDPattern    = regexp.MustCompile(`^[A-Za-z0-9_-]+(?:\.[A-Za-z0-9_-]+)*$`)
	artifactIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-][A-Za-z0-9_.-]*$`)
	tokenPattern      = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9._+-]*$`)
	extensionPattern  = regexp.MustCompile(`^[A-Za-z0-9_-]+(?:\.[A-Za-z0-9_-]+)*$`)
)

// IsSnapshot reports whether Maven reads version as a SNAPSHOT: it ends with SNAPSHOT, in any
// case, or with the <yyyyMMdd.HHmmss>-<build> of a build (looksLikeBuild).
func IsSnapshot(version string) bool { return endsInSnapshot(version) || looksLikeBuild(version) }

// checkGroupArtifact checks the grammar of a groupId and an artifactId.
func checkGroupArtifact(groupID, artifactID string) error {
	if !groupIDPattern.MatchString(groupID) {
		return fmt.Errorf("%w: groupId %q is not dot-separated parts of letters, digits, \"_\" and \"-\"", format.ErrInvalid, groupID)
	}
	if !artifactIDPattern.MatchString(artifactID) {
		return fmt.Errorf("%w: artifactId %q is not letters, digits, \"_\", \"-\" and \".\", the first not \".\"", format.ErrInvalid, artifactID)
	}
	return nil
}

// checkVersion checks a version of the profile (docs/spec/manifest-profile.md §4.2): its grammar,
// and a release version or a well-formed build. A SNAPSHOT version itself is refused: each
// deploy of it is a build, named with its timestamp and build number.
func checkVersion(version string) error {
	if !tokenPattern.MatchString(version) {
		return fmt.Errorf("%w: version %q is not letters, digits, \".\", \"_\", \"+\" and \"-\", the first a letter, a digit or \"_\"",
			format.ErrInvalid, version)
	}
	if endsInSnapshot(version) {
		return fmt.Errorf("%w: version %q is a SNAPSHOT version, which is never a version of its own: each deploy of it is a build, "+
			"its files named with its timestamp and build number (unique snapshot versions, as Maven 3 and Gradle deploy them)", format.ErrInvalid, version)
	}
	if looksLikeBuild(version) {
		_, err := parseBuild(version)
		return err
	}
	return nil
}

// check checks the grammar of the coordinates: a release version or a build.
func (c Coordinates) check() error {
	if err := checkGroupArtifact(c.GroupID, c.ArtifactID); err != nil {
		return err
	}
	return checkVersion(c.Version)
}

// ParsePath reads the path of a file of a version below /maven/<repo>/,
// <groupPath>/<artifactId>/<version>/<file>: com/example/lib-core/1.2.0/lib-core-1.2.0-sources.jar
// is groupId com.example, artifactId lib-core, version 1.2.0, classifier sources, extension
// jar. The groupId is the directories before the artifactId joined with "."; the file name is
// read with ParseFilename. A file of a SNAPSHOT build is in the directory of its SNAPSHOT version:
// com/example/lib-core/1.0-SNAPSHOT/lib-core-1.0-20261114.101112-3.jar is the version
// 1.0-20261114.101112-3. A checksum file, a maven-metadata.xml, a build outside the directory of
// its SNAPSHOT version and the non-unique name of a SNAPSHOT version (lib-core-1.0-SNAPSHOT.jar)
// are refused. Errors wrap format.ErrInvalid.
func ParsePath(p string) (File, error) {
	f, err := parseFilePath(p)
	if err == nil && isSnapshotVersion(f.Version) {
		return File{}, fmt.Errorf("%w: %q is named with the SNAPSHOT version %s: a build is deployed as a unique snapshot version, "+
			"its files named with its timestamp and build number (Maven 3, Gradle), never as %s", format.ErrInvalid, p, f.Version, f.Filename())
	}
	return f, err
}

// parseFilePath reads p as ParsePath does, and takes the non-unique name of a file of a SNAPSHOT
// version too: its File has the SNAPSHOT version, which the resolver reads as the latest build.
func parseFilePath(p string) (File, error) {
	if target, alg, ok := ChecksumOf(p); ok {
		return File{}, fmt.Errorf("%w: %q is the %s checksum file of %q, not a file of a version", format.ErrInvalid, p, alg, target)
	}
	elems := strings.Split(p, "/")
	n := len(elems)
	if n < 4 {
		return File{}, fmt.Errorf("%w: %q is not the path of a file of a version, <groupPath>/<artifactId>/<version>/<file>", format.ErrInvalid, p)
	}
	if elems[n-1] == MetadataFile {
		return File{}, fmt.Errorf("%w: %q is a %s, not a file of a version", format.ErrInvalid, p, MetadataFile)
	}
	c := Coordinates{GroupID: groupOf(elems[:n-3]), ArtifactID: elems[n-3], Version: elems[n-2]}
	if isSnapshotVersion(c.Version) {
		return parseSnapshotFile(c, p, elems[n-1])
	}
	if err := c.check(); err != nil {
		return File{}, fmt.Errorf("%q: %w", p, err)
	}
	if looksLikeBuild(c.Version) {
		return File{}, fmt.Errorf("%w: %q: a SNAPSHOT build is in the directory of its SNAPSHOT version, %s", format.ErrInvalid, p, c.Dir())
	}
	return ParseFilename(c, elems[n-1])
}

// buildPrefix is the timestamp and build number at the start of the rest of a build's file name.
var buildPrefix = regexp.MustCompile(`^[0-9]{8}\.[0-9]{6}-[0-9]+`)

// parseSnapshotFile reads the name of a file in the directory of the SNAPSHOT version c.Version:
// <artifactId>-<base>-<timestamp>-<n>…, a file of that build, or <artifactId>-<base>-SNAPSHOT…,
// the non-unique name, whose File has the SNAPSHOT version.
func parseSnapshotFile(c Coordinates, p, name string) (File, error) {
	if err := checkGroupArtifact(c.GroupID, c.ArtifactID); err != nil {
		return File{}, fmt.Errorf("%q: %w", p, err)
	}
	base := strings.TrimSuffix(c.Version, snapshotSuffix)
	rest, ok := strings.CutPrefix(name, c.ArtifactID+"-"+base+"-")
	if stamp := buildPrefix.FindString(rest); ok && stamp != "" {
		c.Version = base + "-" + stamp
		if err := checkVersion(c.Version); err != nil {
			return File{}, fmt.Errorf("%q: %w", p, err)
		}
	}
	return ParseFilename(c, name)
}

// groupOf returns the groupId of the directories of a group path, joined with ".". A directory
// with a dot in it gives an invalid groupId: the directories of com.example are com/example, and
// com/exa.mple would be a second spelling of com.exa.mple.
func groupOf(dirs []string) string {
	for _, d := range dirs {
		if strings.Contains(d, ".") {
			return strings.Join(dirs, "/")
		}
	}
	return strings.Join(dirs, ".")
}

// ParseFilename reads the name of a file of the version c: <artifactId>-<version>, then
// -<classifier> or not, then .<extension>. Without a classifier the extension is the rest of
// the name after the dot. With one, the extension is the last dot-separated part, tar.<x> when
// the part before is tar, and .asc is kept on the extension of a signature: -bin.tar.gz is
// classifier bin and extension tar.gz, -sources.jar.asc classifier sources and extension
// jar.asc, -jdk1.5.jar classifier jdk1.5 and extension jar. Errors wrap format.ErrInvalid.
func ParseFilename(c Coordinates, name string) (File, error) {
	f := File{Coordinates: c}
	rest, ok := strings.CutPrefix(name, c.ArtifactID+"-"+c.Version)
	switch {
	case !ok:
		return File{}, fmt.Errorf("%w: the file name %q does not start with <artifactId>-<version>, %q", format.ErrInvalid, name, c.ArtifactID+"-"+c.Version)
	case strings.HasPrefix(rest, "."):
		f.Extension = rest[1:]
	case strings.HasPrefix(rest, "-"):
		f.Classifier, f.Extension = splitExtension(rest[1:])
		if !tokenPattern.MatchString(f.Classifier) {
			return File{}, fmt.Errorf("%w: the file name %q: classifier %q is not letters, digits, \".\", \"_\", \"+\" and \"-\"",
				format.ErrInvalid, name, f.Classifier)
		}
	default:
		return File{}, fmt.Errorf("%w: the file name %q is not <artifactId>-<version>[-<classifier>].<extension>", format.ErrInvalid, name)
	}
	if !extensionPattern.MatchString(f.Extension) {
		return File{}, fmt.Errorf("%w: the file name %q: extension %q is not dot-separated parts of letters, digits, \"_\" and \"-\"",
			format.ErrInvalid, name, f.Extension)
	}
	return f, nil
}

// splitExtension splits <classifier>.<extension> by the rule of ParseFilename. Without a dot the
// extension is empty; <classifier>.asc alone is the extension asc.
func splitExtension(s string) (classifier, extension string) {
	stem, sig := strings.CutSuffix(s, signatureExtension)
	if !strings.Contains(stem, ".") {
		stem, sig = s, false
	}
	i := strings.LastIndexByte(stem, '.')
	if i < 0 {
		return s, ""
	}
	if j := strings.LastIndexByte(stem[:i], '.'); j >= 0 && stem[j+1:i] == "tar" {
		i = j
	}
	extension = stem[i+1:]
	if sig {
		extension += signatureExtension
	}
	return stem[:i], extension
}

// ParseMetadataPath reads the path of a maven-metadata.xml below /maven/<repo>/. The metadata
// of an artifact, <groupPath>/<artifactId>/maven-metadata.xml, gives its groupId and
// artifactId, and Version is empty. Maven writes the metadata of a version only for a SNAPSHOT
// version, <groupPath>/<artifactId>/<version>-SNAPSHOT/maven-metadata.xml, so a directory that
// IsSnapshot is read as one and refused, SNAPSHOT versions being after the MVP. The metadata of
// a group (the plugin prefixes a maven-plugin deploys) has the shape of an artifact's and
// cannot be told from it by the path. Errors wrap format.ErrInvalid.
func ParseMetadataPath(p string) (Coordinates, error) {
	elems := strings.Split(p, "/")
	n := len(elems)
	if n < 3 || elems[n-1] != MetadataFile {
		return Coordinates{}, fmt.Errorf("%w: %q is not <groupPath>/<artifactId>/%s", format.ErrInvalid, p, MetadataFile)
	}
	if IsSnapshot(elems[n-2]) {
		return Coordinates{}, fmt.Errorf("%w: %q is the %s of the SNAPSHOT version %q; Lajstrom stores release versions only (SNAPSHOT versions are after the MVP)",
			format.ErrInvalid, p, MetadataFile, elems[n-2])
	}
	c := Coordinates{GroupID: groupOf(elems[:n-2]), ArtifactID: elems[n-2]}
	if err := checkGroupArtifact(c.GroupID, c.ArtifactID); err != nil {
		return Coordinates{}, fmt.Errorf("%q: %w", p, err)
	}
	return c, nil
}
