// Checked once with mvn in a container on 2026-11-07, not in CI (#22). From the repository
// root, with the fixtures of TestWriteFixtures: lib-core 1.2.0 depends on lib-api 1.0.0, and
// both are a jar and a POM with their checksum files in a Maven 2 layout. Step 1 resolves
// them from a file-based remote repository with strict checksums (a wrong .sha512 fails it
// with "Checksum validation failed"); step 2 offline from a local repository laid out the same
// way, the plugins of step 1 next to them. Maven fetches its plugins from Maven Central.
//
//	export LAJSTROM_FIXTURES_OUT=$(mktemp -d)
//	go test -count=1 -run TestWriteFixtures ./internal/testutil
//	docker run --rm --name mvn-22 -v "$LAJSTROM_FIXTURES_OUT:/fixtures:ro" maven:3.9-eclipse-temurin-21 sh -ec '
//	  cd /root
//	  mvn -B -C dependency:get -Dartifact=com.example:lib-core:1.2.0 \
//	    -DremoteRepositories=fixtures::default::file:///fixtures/maven \
//	    -Daether.checksums.algorithms=SHA-512,SHA-256,SHA-1,MD5 > get.log 2>&1 || { cat get.log; exit 1; }
//	  grep -e fixtures -e BUILD -e WARN -e ERROR get.log
//	  cp -r /root/.m2/repository /repo && rm -rf /repo/com/example
//	  cp -r /fixtures/maven/com/example /repo/com/example
//	  mvn -B -o dependency:get -Dartifact=com.example:lib-core:1.2.0 -Dmaven.repo.local=/repo \
//	    > offline.log 2>&1 || { cat offline.log; exit 1; }
//	  grep -e Resolving -e BUILD -e WARN -e ERROR offline.log'
//
// Output of docker run:
//
//	[INFO] Downloading from fixtures: file:///fixtures/maven/com/example/lib-core/1.2.0/lib-core-1.2.0.pom
//	[INFO] Downloaded from fixtures: file:///fixtures/maven/com/example/lib-core/1.2.0/lib-core-1.2.0.pom (441 B at 88 kB/s)
//	[INFO] Downloading from fixtures: file:///fixtures/maven/com/example/lib-api/1.0.0/lib-api-1.0.0.pom
//	[INFO] Downloaded from fixtures: file:///fixtures/maven/com/example/lib-api/1.0.0/lib-api-1.0.0.pom (295 B at 59 kB/s)
//	[INFO] Downloading from fixtures: file:///fixtures/maven/com/example/lib-core/1.2.0/lib-core-1.2.0.jar
//	[INFO] Downloaded from fixtures: file:///fixtures/maven/com/example/lib-core/1.2.0/lib-core-1.2.0.jar (518 B at 86 kB/s)
//	[INFO] Downloading from fixtures: file:///fixtures/maven/com/example/lib-api/1.0.0/lib-api-1.0.0.jar
//	[INFO] Downloaded from fixtures: file:///fixtures/maven/com/example/lib-api/1.0.0/lib-api-1.0.0.jar (518 B at 259 kB/s)
//	[INFO] BUILD SUCCESS
//	[INFO] Resolving com.example:lib-core:jar:1.2.0 with transitive dependencies
//	[INFO] BUILD SUCCESS

package testutil

import (
	"bytes"
	"encoding/xml"
	"slices"
	"strings"
	"testing"
)

func TestJar(t *testing.T) {
	for _, tc := range []struct {
		name        string
		opts        JarOptions
		path        string
		packagePath string
	}{
		{
			name:        "defaults",
			path:        "com/example/lib-core/1.2.0/lib-core-1.2.0.jar",
			packagePath: "com.example/lib-core",
		},
		{
			name:        "classifier, mixed-case coordinates",
			opts:        JarOptions{GroupID: "org.Acme.tools", ArtifactID: "Lib.Util", Version: "3.0.0-RC1", Classifier: "sources"},
			path:        "org/Acme/tools/Lib.Util/3.0.0-RC1/Lib.Util-3.0.0-RC1-sources.jar",
			packagePath: "org.acme.tools/lib.util",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b, want := Jar(tc.opts)
			if want.Path != tc.path || want.PackagePath != tc.packagePath {
				t.Errorf("Path, PackagePath = %q, %q, want %q, %q", want.Path, want.PackagePath, tc.path, tc.packagePath)
			}
			if want.Extension != "jar" || !strings.HasSuffix(want.Path, "/"+want.Filename) {
				t.Errorf("Extension, Filename = %q, %q", want.Extension, want.Filename)
			}
			files := readZip(t, b)
			if got := zipNames(files); !slices.Equal(got, []string{"META-INF/", "META-INF/MANIFEST.MF", "com/example/Hello.class"}) {
				t.Fatalf("members %q, want META-INF/, META-INF/MANIFEST.MF, com/example/Hello.class", got)
			}
			if !bytes.Equal(files[1].body, want.Manifest) || !bytes.HasPrefix(want.Manifest, []byte("Manifest-Version: 1.0\r\n")) {
				t.Errorf("MANIFEST.MF = %q, want Manifest %q", files[1].body, want.Manifest)
			}
			if !bytes.HasPrefix(files[2].body, []byte{0xca, 0xfe, 0xba, 0xbe}) {
				t.Errorf("Hello.class = % x, want the class file magic", files[2].body)
			}
			checkWhole(t, b, want.Size, want.Checksums)
		})
	}
}

// pomTest is a POM as the test reads it with encoding/xml, apart from the builder's types.
type pomTest struct {
	XMLName      xml.Name
	ModelVersion string `xml:"modelVersion"`
	GroupID      string `xml:"groupId"`
	ArtifactID   string `xml:"artifactId"`
	Version      string `xml:"version"`
	Packaging    string `xml:"packaging"`
	Dependencies []struct {
		GroupID    string `xml:"groupId"`
		ArtifactID string `xml:"artifactId"`
		Version    string `xml:"version"`
		Scope      string `xml:"scope"`
	} `xml:"dependencies>dependency"`
}

func TestPOM(t *testing.T) {
	for _, tc := range []struct {
		name string
		opts POMOptions
		// want are the options with the defaults filled in.
		want POMOptions
		path string
	}{
		{
			name: "defaults",
			want: POMOptions{GroupID: "com.example", ArtifactID: "lib-core", Version: "1.2.0", Packaging: "jar"},
			path: "com/example/lib-core/1.2.0/lib-core-1.2.0.pom",
		},
		{
			name: "pom packaging, dependencies",
			opts: POMOptions{
				GroupID: "org.acme", ArtifactID: "parent", Version: "2.0", Packaging: "pom",
				Dependencies: []DependencyOptions{
					{GroupID: "com.example", ArtifactID: "lib-core", Version: "[1.2,2.0)"},
					{GroupID: "org.junit.jupiter", ArtifactID: "junit-jupiter", Version: "5.11.0", Scope: "test"},
					{GroupID: "a&b", ArtifactID: "<c>"},
				},
			},
			want: POMOptions{
				GroupID: "org.acme", ArtifactID: "parent", Version: "2.0", Packaging: "pom",
				Dependencies: []DependencyOptions{
					{GroupID: "com.example", ArtifactID: "lib-core", Version: "[1.2,2.0)"},
					{GroupID: "org.junit.jupiter", ArtifactID: "junit-jupiter", Version: "5.11.0", Scope: "test"},
					{GroupID: "a&b", ArtifactID: "<c>"},
				},
			},
			path: "org/acme/parent/2.0/parent-2.0.pom",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b, want := POM(tc.opts)
			if !bytes.HasPrefix(b, []byte(xml.Header)) {
				t.Errorf("no XML declaration: %q", b[:min(40, len(b))])
			}
			var p pomTest
			if err := xml.Unmarshal(b, &p); err != nil {
				t.Fatalf("encoding/xml: %v\n%s", err, b)
			}
			if p.XMLName != (xml.Name{Space: "http://maven.apache.org/POM/4.0.0", Local: "project"}) || p.ModelVersion != "4.0.0" {
				t.Errorf("root %v, modelVersion %q, want project of the POM 4.0.0 namespace, 4.0.0", p.XMLName, p.ModelVersion)
			}
			got := POMOptions{GroupID: p.GroupID, ArtifactID: p.ArtifactID, Version: p.Version, Packaging: p.Packaging}
			for _, d := range p.Dependencies {
				got.Dependencies = append(got.Dependencies, DependencyOptions(d))
			}
			if !pomEqual(got, tc.want) {
				t.Errorf("parsed %+v, want %+v", got, tc.want)
			}
			fromInfo := POMOptions{
				GroupID: want.GroupID, ArtifactID: want.ArtifactID, Version: want.Version,
				Packaging: want.Packaging, Dependencies: want.Dependencies,
			}
			if !pomEqual(fromInfo, tc.want) {
				t.Errorf("POMInfo %+v, want %+v", fromInfo, tc.want)
			}
			if want.Path != tc.path || !strings.HasSuffix(want.Path, "/"+want.Filename) {
				t.Errorf("Path, Filename = %q, %q, want %q", want.Path, want.Filename, tc.path)
			}
			checkWhole(t, b, want.Size, want.Checksums)
		})
	}
}

// pomEqual reports whether two POMOptions are equal.
func pomEqual(a, b POMOptions) bool {
	return a.GroupID == b.GroupID && a.ArtifactID == b.ArtifactID && a.Version == b.Version &&
		a.Packaging == b.Packaging && slices.Equal(a.Dependencies, b.Dependencies)
}

func TestMavenDeterministic(t *testing.T) {
	j1, _ := Jar(JarOptions{})
	j2, _ := Jar(JarOptions{})
	opts := POMOptions{Dependencies: []DependencyOptions{{GroupID: "g", ArtifactID: "a", Version: "1"}}}
	p1, _ := POM(opts)
	p2, _ := POM(opts)
	if !bytes.Equal(j1, j2) || !bytes.Equal(p1, p2) {
		t.Fatal("two calls with the same options give different bytes")
	}
}
