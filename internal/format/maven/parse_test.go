package maven

import (
	"errors"
	"strings"
	"testing"

	"github.com/lajstrom/lajstrom/internal/format"
	"github.com/lajstrom/lajstrom/internal/testutil"
)

// TestParsePath reads a table of paths as Maven Central and Gradle lay them out: classifiers
// (with "-", "_" and a dot), dotted artifactIds, a one-part groupId, a tar.gz extension, a
// version with a qualifier, the POM of a pom-packaging project, signatures and Gradle module
// metadata. Path() writes each back.
func TestParsePath(t *testing.T) {
	_, jar := testutil.Jar(testutil.JarOptions{Classifier: "sources"})
	_, pom := testutil.POM(testutil.POMOptions{Packaging: "pom"})
	for _, tt := range []struct {
		path                     string
		g, a, v, classifier, ext string
		pom                      bool
	}{
		{jar.Path, jar.GroupID, jar.ArtifactID, jar.Version, "sources", "jar", false},
		{pom.Path, pom.GroupID, pom.ArtifactID, pom.Version, "", "pom", true},
		{"com/example/lib-core/1.2.0/lib-core-1.2.0.jar", "com.example", "lib-core", "1.2.0", "", "jar", false},
		{"com/example/lib-core/1.2.0/lib-core-1.2.0-javadoc.jar", "com.example", "lib-core", "1.2.0", "javadoc", "jar", false},
		{"junit/junit/4.13.2/junit-4.13.2.jar", "junit", "junit", "4.13.2", "", "jar", false},
		{"org/eclipse/jdt/org.eclipse.jdt.core/3.33.0/org.eclipse.jdt.core-3.33.0.jar", "org.eclipse.jdt", "org.eclipse.jdt.core", "3.33.0", "", "jar", false},
		{"org/eclipse/jdt/org.eclipse.jdt.core/3.33.0/org.eclipse.jdt.core-3.33.0-sources.jar", "org.eclipse.jdt", "org.eclipse.jdt.core", "3.33.0", "sources", "jar", false},
		{"org/apache/maven/apache-maven/3.9.9/apache-maven-3.9.9-bin.tar.gz", "org.apache.maven", "apache-maven", "3.9.9", "bin", "tar.gz", false},
		{"org/apache/maven/apache-maven/3.9.9/apache-maven-3.9.9-bin.tar.gz.asc", "org.apache.maven", "apache-maven", "3.9.9", "bin", "tar.gz.asc", false},
		{"org/apache/maven/apache-maven/3.9.9/apache-maven-3.9.9-src.zip", "org.apache.maven", "apache-maven", "3.9.9", "src", "zip", false},
		{"com/example/dist/1.0/dist-1.0.tar.gz", "com.example", "dist", "1.0", "", "tar.gz", false},
		{"io/netty/netty-transport-native-epoll/4.1.115.Final/netty-transport-native-epoll-4.1.115.Final-linux-x86_64.jar",
			"io.netty", "netty-transport-native-epoll", "4.1.115.Final", "linux-x86_64", "jar", false},
		{"org/lwjgl/lwjgl/3.3.4/lwjgl-3.3.4-natives-windows-x86.jar", "org.lwjgl", "lwjgl", "3.3.4", "natives-windows-x86", "jar", false},
		{"net/sf/json-lib/json-lib/2.4/json-lib-2.4-jdk1.5.jar", "net.sf.json-lib", "json-lib", "2.4", "jdk1.5", "jar", false},
		{"com/google/guava/guava/33.3.1-jre/guava-33.3.1-jre.jar", "com.google.guava", "guava", "33.3.1-jre", "", "jar", false},
		{"com/google/guava/guava/33.3.1-jre/guava-33.3.1-jre.module", "com.google.guava", "guava", "33.3.1-jre", "", "module", false},
		{"org/springframework/boot/spring-boot-dependencies/3.4.0/spring-boot-dependencies-3.4.0.pom",
			"org.springframework.boot", "spring-boot-dependencies", "3.4.0", "", "pom", true},
		{"com/example/lib-core/1.2.0/lib-core-1.2.0.jar.asc", "com.example", "lib-core", "1.2.0", "", "jar.asc", false},
		{"com/example/lib-core/1.2.0/lib-core-1.2.0-sources.jar.asc", "com.example", "lib-core", "1.2.0", "sources", "jar.asc", false},
		{"com/example/lib-core/1.2.0/lib-core-1.2.0.pom.asc", "com.example", "lib-core", "1.2.0", "", "pom.asc", false},
		{"com/example/lib-core/1.2.0/lib-core-1.2.0-tests.asc", "com.example", "lib-core", "1.2.0", "tests", "asc", false},
		{"com/example/Lib_Core/1.0.0+build.7/Lib_Core-1.0.0+build.7.jar", "com.example", "Lib_Core", "1.0.0+build.7", "", "jar", false},
		{"com/example/lib-core/1.2.0-SNAPSHOT/lib-core-1.2.0-20261109.101500-1.jar", "com.example", "lib-core", "1.2.0-20261109.101500-1", "", "jar", false},
		{"com/example/lib-core/1.2.0-SNAPSHOT/lib-core-1.2.0-20261109.101500-12-sources.jar", "com.example", "lib-core", "1.2.0-20261109.101500-12", "sources", "jar", false},
		{"com/example/lib-core/1.2.0-SNAPSHOT/lib-core-1.2.0-20261109.101500-3-1.jar", "com.example", "lib-core", "1.2.0-20261109.101500-3", "1", "jar", false},
		{"com/example/lib-core/1.2.0-SNAPSHOT/lib-core-1.2.0-20261109.101500-3.pom.asc", "com.example", "lib-core", "1.2.0-20261109.101500-3", "", "pom.asc", false},
		{"com/example/lib-core/1.0-rc1-SNAPSHOT/lib-core-1.0-rc1-20261109.101500-1.pom", "com.example", "lib-core", "1.0-rc1-20261109.101500-1", "", "pom", true},
	} {
		f, err := ParsePath(tt.path)
		if err != nil {
			t.Errorf("ParsePath(%q): %v", tt.path, err)
			continue
		}
		want := File{Coordinates: Coordinates{GroupID: tt.g, ArtifactID: tt.a, Version: tt.v}, Classifier: tt.classifier, Extension: tt.ext}
		if f != want || f.IsPOM() != tt.pom {
			t.Errorf("ParsePath(%q) = %+v (POM %t), want %+v (POM %t)", tt.path, f, f.IsPOM(), want, tt.pom)
		}
		if f.Path() != tt.path || Classify(tt.path) != KindFile {
			t.Errorf("%q: Path() = %q, Classify %d", tt.path, f.Path(), Classify(tt.path))
		}
	}
}

// TestParsePathRefuses lists the paths that are not a file of a version, each with a part of
// the message that names the rule.
func TestParsePathRefuses(t *testing.T) {
	for _, tt := range []struct{ path, msg string }{
		{"com/example/lib-core/1.2.0/lib-core-1.2.0.jar.sha1", "sha1 checksum file"},
		{"com/example/lib-core/1.2.0/lib-core-1.2.0.pom.md5", "md5 checksum file"},
		{"com/example/lib-core/maven-metadata.xml", "is a maven-metadata.xml"},
		{"com/example/lib-core/1.2.0/maven-metadata.xml", "is a maven-metadata.xml"},
		{"com/example/lib-core/1.2.0-SNAPSHOT/lib-core-1.2.0-SNAPSHOT.jar", "named with the SNAPSHOT version 1.2.0-SNAPSHOT"},
		{"com/example/lib-core/1.2.0-SNAPSHOT/lib-core-1.2.0-SNAPSHOT-sources.jar", "named with the SNAPSHOT version"},
		{"com/example/lib-core/1.2.0-snapshot/lib-core-1.2.0-snapshot.jar", "SNAPSHOT version"},
		{"com/example/lib-core/1.2.0-snapshot/lib-core-1.2.0-20261109.101500-1.jar", "SNAPSHOT version"},
		{"com/example/lib-core/1.2.0-20261109.101500-1/lib-core-1.2.0-20261109.101500-1.jar", "in the directory of its SNAPSHOT version, com/example/lib-core/1.2.0-SNAPSHOT"},
		{"com/example/lib-core/1.2.0-SNAPSHOT/lib-core-1.2.0-20261109.101500-0.jar", "the build number \"0\""},
		{"com/example/lib-core/1.2.0-SNAPSHOT/lib-core-1.2.0-20261109.101500-007.jar", "without a leading zero"},
		{"com/example/lib-core/1.2.0-SNAPSHOT/lib-core-1.2.0-20261109.101500-1234567890.jar", "from 1 to 999999999"},
		{"com/example/lib-core/1.2.0-SNAPSHOT/lib-core-1.2.0-20261339.101500-1.jar", "not a time yyyyMMdd.HHmmss"},
		{"com/example/lib-core/1.2.0-SNAPSHOT/lib-core-1.3.0-20261109.101500-1.jar", "does not start with <artifactId>-<version>"},
		{"com/example/lib-core/1.2.0-SNAPSHOT/lib-core-1.2.0-20261109.101500.jar", "does not start with <artifactId>-<version>"},
		{"lib-core/1.2.0/lib-core-1.2.0.jar", "<groupPath>/<artifactId>/<version>/<file>"},
		{"com/example/lib-core/1.2.0/lib-api-1.2.0.jar", "does not start with <artifactId>-<version>"},
		{"com/example/lib-core/1.2.0/lib-core-1.3.0.jar", "does not start with <artifactId>-<version>"},
		{"com/example/lib-core/1.2.0/lib-core-1.2.0", "not <artifactId>-<version>[-<classifier>].<extension>"},
		{"com/example/lib-core/1.2.0/lib-core-1.2.0x.jar", "not <artifactId>-<version>[-<classifier>].<extension>"},
		{"com/example/lib-core/1.2.0/lib-core-1.2.0-sources", "extension"},
		{"com/example/lib-core/1.2.0/lib-core-1.2.0.", "extension"},
		{"com/example/lib-core/1.2.0/lib-core-1.2.0.jar..asc", "extension"},
		{"com/example/lib-core/1.2.0/lib-core-1.2.0-.jar", "classifier"},
		{"com/exa.mple/lib-core/1.2.0/lib-core-1.2.0.jar", "groupId"},
		{"com//lib-core/1.2.0/lib-core-1.2.0.jar", "groupId"},
		{"/com/example/lib-core/1.2.0/lib-core-1.2.0.jar", "groupId"},
		{"com/example/../1.2.0/..-1.2.0.jar", "artifactId"},
		{"com/example/lib-core/../lib-core-...jar", "version"},
		{"com/example/lib-core/${revision}/lib-core-${revision}.jar", "version"},
		{"com/example/lib-core/[1.0,2.0)/lib-core-[1.0,2.0).jar", "version"},
	} {
		_, err := ParsePath(tt.path)
		if !errors.Is(err, format.ErrInvalid) || !strings.Contains(err.Error(), tt.msg) {
			t.Errorf("ParsePath(%q) = %v, want ErrInvalid with %q", tt.path, err, tt.msg)
		}
	}
}

func TestChecksumOf(t *testing.T) {
	for _, tt := range []struct {
		path, target, algorithm string
		ok                      bool
	}{
		{"com/example/lib-core/1.2.0/lib-core-1.2.0.jar.md5", "com/example/lib-core/1.2.0/lib-core-1.2.0.jar", MD5, true},
		{"com/example/lib-core/1.2.0/lib-core-1.2.0.jar.sha1", "com/example/lib-core/1.2.0/lib-core-1.2.0.jar", SHA1, true},
		{"lib-core-1.2.0.pom.sha256", "lib-core-1.2.0.pom", SHA256, true},
		{"lib-core-1.2.0.pom.sha512", "lib-core-1.2.0.pom", SHA512, true},
		{"lib-core-1.2.0.jar.asc.sha1", "lib-core-1.2.0.jar.asc", SHA1, true},
		{"com/example/lib-core/maven-metadata.xml.sha512", "com/example/lib-core/maven-metadata.xml", SHA512, true},
		{"lib-core-1.2.0.jar", "", "", false},
		{"lib-core-1.2.0.jar.asc", "", "", false},
		{"lib-core-1.2.0.jar.SHA1", "", "", false},
		{"lib-core-1.2.0.jar.sha384", "", "", false},
		{".sha1", "", "", false},
		{"com/example/.md5", "", "", false},
	} {
		target, alg, ok := ChecksumOf(tt.path)
		if target != tt.target || alg != tt.algorithm || ok != tt.ok {
			t.Errorf("ChecksumOf(%q) = %q, %q, %t; want %q, %q, %t", tt.path, target, alg, ok, tt.target, tt.algorithm, tt.ok)
		}
		if ok && Classify(tt.path) != KindChecksum {
			t.Errorf("Classify(%q) = %d, want KindChecksum", tt.path, Classify(tt.path))
		}
	}
}

// TestParseMetadataPath reads the maven-metadata.xml of an artifact and refuses that of a
// SNAPSHOT version.
func TestParseMetadataPath(t *testing.T) {
	for _, tt := range []struct {
		path string
		want Coordinates
	}{
		{"com/example/lib-core/maven-metadata.xml", Coordinates{GroupID: "com.example", ArtifactID: "lib-core"}},
		{"junit/junit/maven-metadata.xml", Coordinates{GroupID: "junit", ArtifactID: "junit"}},
		{"org/eclipse/jdt/org.eclipse.jdt.core/maven-metadata.xml", Coordinates{GroupID: "org.eclipse.jdt", ArtifactID: "org.eclipse.jdt.core"}},
	} {
		got, err := ParseMetadataPath(tt.path)
		if err != nil || got != tt.want || Classify(tt.path) != KindMetadata {
			t.Errorf("ParseMetadataPath(%q) = %+v, %v (Classify %d); want %+v", tt.path, got, err, Classify(tt.path), tt.want)
		}
	}
	for _, tt := range []struct{ path, msg string }{
		{"com/example/lib-core/1.2.0-SNAPSHOT/maven-metadata.xml", "SNAPSHOT"},
		{"lib-core/maven-metadata.xml", "is not <groupPath>/<artifactId>/maven-metadata.xml"},
		{"com/example/lib-core/metadata.xml", "is not <groupPath>/<artifactId>/maven-metadata.xml"},
		{"com/../maven-metadata.xml", "artifactId"},
		{"com/exa.mple/lib-core/maven-metadata.xml", "groupId"},
	} {
		if _, err := ParseMetadataPath(tt.path); !errors.Is(err, format.ErrInvalid) || !strings.Contains(err.Error(), tt.msg) {
			t.Errorf("ParseMetadataPath(%q) = %v, want ErrInvalid with %q", tt.path, err, tt.msg)
		}
	}
}

func TestIsSnapshot(t *testing.T) {
	for v, want := range map[string]bool{
		"1.0-SNAPSHOT": true, "1.0-snapshot": true, "SNAPSHOT": true, "1.0-20261109.101500-3": true, "20261109.101500-3": true,
		"1.0": false, "1.0-SNAP": false, "1.0-20261109": false, "SNAPSHOT-1.0": false,
	} {
		if got := IsSnapshot(v); got != want {
			t.Errorf("IsSnapshot(%q) = %t, want %t", v, got, want)
		}
	}
}
