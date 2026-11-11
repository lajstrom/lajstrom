package testutil

import (
	"encoding/xml"
	"slices"
	"strings"

	"github.com/lajstrom/lajstrom/internal/oci/profile"
)

// Coordinates are the defaults of JarOptions and POMOptions.
const (
	defaultGroupID    = "com.example"
	defaultArtifactID = "lib-core"
	defaultVersion    = "1.2.0"
)

// JarOptions are the options of Jar. Every empty field but Classifier takes its default, so
// JarOptions{} is a valid jar.
type JarOptions struct {
	// GroupID, ArtifactID and Version are the coordinates; defaults com.example, lib-core,
	// 1.2.0.
	GroupID    string
	ArtifactID string
	Version    string
	// Classifier is the classifier of the file name (sources, javadoc); none by default.
	Classifier string
}

// JarInfo is what a parser finds in the jar Jar builds and in its path.
type JarInfo struct {
	// GroupID, ArtifactID, Version and Classifier are the coordinates of the file.
	GroupID    string
	ArtifactID string
	Version    string
	Classifier string
	// Extension is "jar".
	Extension string
	// Filename is <artifactId>-<version>[-<classifier>].jar.
	Filename string
	// Path is the path of the file in a Maven 2 repository:
	// com/example/lib-core/1.2.0/lib-core-1.2.0.jar.
	Path string
	// PackagePath is the OCI package path of the coordinates
	// (docs/spec/manifest-profile.md §4.1): com.example/lib-core.
	PackagePath string
	// Manifest is META-INF/MANIFEST.MF.
	Manifest []byte
	// Size is the size of the whole file in bytes.
	Size int64
	// Checksums are the digests of the whole file, the content of the .md5, .sha1, .sha256
	// and .sha512 files next to it.
	Checksums
}

// Jar returns a jar, a zip archive with META-INF/, META-INF/MANIFEST.MF (Manifest-Version,
// Created-By) and com/example/Hello.class, a placeholder of a few bytes that starts like a
// class file and is not one (nothing compiles it), and the facts a parser finds in it and in
// its path. The same options give the same bytes: every member is dated 2026-01-01 00:00 UTC.
func Jar(opts JarOptions) ([]byte, JarInfo) {
	info := JarInfo{Classifier: opts.Classifier, Extension: "jar"}
	info.GroupID, info.ArtifactID, info.Version = mavenDefaults(opts.GroupID, opts.ArtifactID, opts.Version)
	info.Filename, info.Path, info.PackagePath = mavenPaths(info.GroupID, info.ArtifactID, info.Version, info.Classifier, info.Extension)
	info.Manifest = []byte("Manifest-Version: 1.0\r\nCreated-By: lajstrom testutil\r\n\r\n")
	b := zipOf([]member{
		{name: "META-INF/"},
		{name: "META-INF/MANIFEST.MF", body: info.Manifest},
		// The class file magic and major version 65 (Java 21), and no class.
		{name: "com/example/Hello.class", body: []byte{0xca, 0xfe, 0xba, 0xbe, 0x00, 0x00, 0x00, 0x41}},
	})
	info.Size = int64(len(b))
	info.Checksums = checksumsOf(b)
	return b, info
}

// DependencyOptions is a dependency of a POM.
type DependencyOptions struct {
	// GroupID, ArtifactID and Version are the coordinates of the dependency; an empty Version
	// writes no version element.
	GroupID    string
	ArtifactID string
	Version    string
	// Scope is the scope (compile, test, …); empty writes no scope element, which Maven reads
	// as compile.
	Scope string
}

// POMOptions are the options of POM. Every empty field takes its default, so POMOptions{} is a
// valid POM.
type POMOptions struct {
	// GroupID, ArtifactID and Version are the coordinates; defaults com.example, lib-core,
	// 1.2.0.
	GroupID    string
	ArtifactID string
	Version    string
	// Packaging is the packaging; default "jar".
	Packaging string
	// Dependencies are written in order; none by default.
	Dependencies []DependencyOptions
}

// POMInfo is what a parser finds in the POM that POM builds and in its path.
type POMInfo struct {
	// GroupID, ArtifactID, Version and Packaging are as the POM has them.
	GroupID    string
	ArtifactID string
	Version    string
	Packaging  string
	// Dependencies are the dependencies, in order.
	Dependencies []DependencyOptions
	// Filename is <artifactId>-<version>.pom.
	Filename string
	// Path is the path of the file in a Maven 2 repository:
	// com/example/lib-core/1.2.0/lib-core-1.2.0.pom.
	Path string
	// PackagePath is the OCI package path of the coordinates
	// (docs/spec/manifest-profile.md §4.1): com.example/lib-core.
	PackagePath string
	// Size is the size of the whole file in bytes.
	Size int64
	// Checksums are the digests of the whole file, the content of the .md5, .sha1, .sha256
	// and .sha512 files next to it.
	Checksums
}

// pomProject is the POM POM writes. The namespace of project is the default namespace of the
// document, so the elements below it are in it too.
type pomProject struct {
	XMLName      xml.Name        `xml:"http://maven.apache.org/POM/4.0.0 project"`
	ModelVersion string          `xml:"modelVersion"`
	GroupID      string          `xml:"groupId"`
	ArtifactID   string          `xml:"artifactId"`
	Version      string          `xml:"version"`
	Packaging    string          `xml:"packaging"`
	Dependencies []pomDependency `xml:"dependencies>dependency,omitempty"`
}

// pomDependency is a dependency element of pomProject.
type pomDependency struct {
	GroupID    string `xml:"groupId"`
	ArtifactID string `xml:"artifactId"`
	Version    string `xml:"version,omitempty"`
	Scope      string `xml:"scope,omitempty"`
}

// POM returns a minimal POM, a project with modelVersion 4.0.0, the coordinates, packaging and
// the dependencies, and the facts a parser finds in it and in its path. The same options give
// the same bytes.
func POM(opts POMOptions) ([]byte, POMInfo) {
	info := POMInfo{Packaging: opts.Packaging, Dependencies: slices.Clone(opts.Dependencies)}
	info.GroupID, info.ArtifactID, info.Version = mavenDefaults(opts.GroupID, opts.ArtifactID, opts.Version)
	if info.Packaging == "" {
		info.Packaging = "jar"
	}
	info.Filename, info.Path, info.PackagePath = mavenPaths(info.GroupID, info.ArtifactID, info.Version, "", "pom")
	p := pomProject{
		ModelVersion: "4.0.0",
		GroupID:      info.GroupID,
		ArtifactID:   info.ArtifactID,
		Version:      info.Version,
		Packaging:    info.Packaging,
	}
	for _, d := range info.Dependencies {
		p.Dependencies = append(p.Dependencies, pomDependency(d))
	}
	out, err := xml.MarshalIndent(p, "", "  ")
	must(err)
	b := append([]byte(xml.Header), out...)
	b = append(b, '\n')
	info.Size = int64(len(b))
	info.Checksums = checksumsOf(b)
	return b, info
}

// mavenDefaults returns the coordinates, each set to its default when empty.
func mavenDefaults(groupID, artifactID, version string) (string, string, string) {
	if groupID == "" {
		groupID = defaultGroupID
	}
	if artifactID == "" {
		artifactID = defaultArtifactID
	}
	if version == "" {
		version = defaultVersion
	}
	return groupID, artifactID, version
}

// mavenPaths returns the file name <artifactId>-<version>[-<classifier>].<extension>, its path
// in a Maven 2 repository (the groupId with '.' as '/', the artifactId, the version, the file
// name) and the OCI package path of §4.1.
func mavenPaths(groupID, artifactID, version, classifier, extension string) (string, string, string) {
	name := artifactID + "-" + version
	if classifier != "" {
		name += "-" + classifier
	}
	name += "." + extension
	repoPath := strings.Join([]string{strings.ReplaceAll(groupID, ".", "/"), artifactID, version, name}, "/")
	return name, repoPath, profile.PackagePath(profile.EscapeComponent(groupID), profile.EscapeComponent(artifactID))
}
