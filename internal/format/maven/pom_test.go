package maven

import (
	"errors"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/lajstrom/lajstrom/internal/format"
	"github.com/lajstrom/lajstrom/internal/testutil"
)

// project returns a POM in the POM 4.0.0 namespace with body inside project.
func project(body string) []byte {
	return []byte(`<?xml version="1.0" encoding="UTF-8"?>
<project xmlns="http://maven.apache.org/POM/4.0.0" xmlns:xsi="http://www.w3.org/2001/XMLSchema-instance"
    xsi:schemaLocation="http://maven.apache.org/POM/4.0.0 https://maven.apache.org/xsd/maven-4.0.0.xsd">
  <modelVersion>4.0.0</modelVersion>
` + body + `
</project>
`)
}

// parent is the parent element of the POMs of TestParsePOM.
const parent = `<parent><groupId>com.example</groupId><artifactId>parent</artifactId><version>7</version></parent>`

// TestParsePOM reads the POMs of testutil and hand-written ones: parent inheritance of groupId
// and version, ${project.*} in the coordinates and the dependencies, a property of a parent kept
// in a dependency, a POM without namespace, one in ISO-8859-1, pom packaging.
func TestParsePOM(t *testing.T) {
	deps := []testutil.DependencyOptions{
		{GroupID: "com.example", ArtifactID: "lib-api", Version: "1.0.0"},
		{GroupID: "org.junit.jupiter", ArtifactID: "junit-jupiter", Version: "5.11.3", Scope: "test"},
	}
	plain, _ := testutil.POM(testutil.POMOptions{})
	withDeps, _ := testutil.POM(testutil.POMOptions{GroupID: "org.example", ArtifactID: "app", Version: "2.0", Packaging: "war", Dependencies: deps})
	bom, _ := testutil.POM(testutil.POMOptions{ArtifactID: "bom", Packaging: "pom"})
	p := &Coordinates{GroupID: "com.example", ArtifactID: "parent", Version: "7"}
	for _, tt := range []struct {
		name string
		pom  []byte
		want POM
	}{
		{"testutil defaults", plain, POM{Coordinates: Coordinates{"com.example", "lib-core", "1.2.0"}, Packaging: "jar"}},
		{"testutil with dependencies", withDeps, POM{Coordinates: Coordinates{"org.example", "app", "2.0"}, Packaging: "war",
			Dependencies: []Dependency{
				{GroupID: "com.example", ArtifactID: "lib-api", Version: "1.0.0"},
				{GroupID: "org.junit.jupiter", ArtifactID: "junit-jupiter", Version: "5.11.3", Scope: "test"},
			}}},
		{"pom packaging", bom, POM{Coordinates: Coordinates{"com.example", "bom", "1.2.0"}, Packaging: "pom"}},
		{"groupId and version from the parent", project(parent + `<artifactId>lib-core</artifactId>`),
			POM{Coordinates: Coordinates{"com.example", "lib-core", "7"}, Packaging: "jar", Parent: p}},
		{"own version, groupId from the parent", project(parent + `<artifactId>lib-core</artifactId><version>1.2.0</version>`),
			POM{Coordinates: Coordinates{"com.example", "lib-core", "1.2.0"}, Packaging: "jar", Parent: p}},
		{"${project.parent.*} and ${project.*}", project(parent + `
  <groupId>${project.parent.groupId}.libs</groupId>
  <artifactId>  lib-core  </artifactId>
  <version>${project.parent.version}.1</version>
  <packaging>bundle</packaging>
  <dependencies>
    <dependency><groupId>${project.groupId}</groupId><artifactId>lib-api</artifactId><version>${project.version}</version></dependency>
    <dependency>
      <groupId>com.fasterxml.jackson.core</groupId><artifactId>jackson-databind</artifactId><version>${jackson.version}</version>
      <type>jar</type><classifier>tests</classifier><optional>true</optional>
    </dependency>
  </dependencies>
  <dependencyManagement><dependencies><dependency><groupId>x</groupId><artifactId>managed</artifactId></dependency></dependencies></dependencyManagement>
  <build><plugins><plugin><artifactId>p</artifactId><dependencies><dependency><groupId>x</groupId><artifactId>plugin-dep</artifactId></dependency></dependencies></plugin></plugins></build>`),
			POM{Coordinates: Coordinates{"com.example.libs", "lib-core", "7.1"}, Packaging: "bundle", Parent: p, Dependencies: []Dependency{
				{GroupID: "com.example.libs", ArtifactID: "lib-api", Version: "7.1"},
				{GroupID: "com.fasterxml.jackson.core", ArtifactID: "jackson-databind", Version: "${jackson.version}", Type: "jar", Classifier: "tests", Optional: "true"},
			}}},
		// Four rounds resolve the groupId; a dependency that refers to it took a fifth before #51.
		{"a dependency sees the coordinates resolved", project(`<parent><groupId>g</groupId><artifactId>parent</artifactId>` +
			`<version>${project.parent.groupId}</version></parent><groupId>${project.version}</groupId><artifactId>a</artifactId>` +
			`<version>${project.packaging}</version><packaging>${project.parent.version}</packaging>` +
			`<dependencies><dependency><groupId>${project.groupId}</groupId><artifactId>x</artifactId></dependency></dependencies>`),
			POM{Coordinates: Coordinates{"g", "a", "g"}, Packaging: "g", Parent: &Coordinates{"g", "parent", "g"},
				Dependencies: []Dependency{{GroupID: "g", ArtifactID: "x"}}}},
		{"no namespace", []byte(`<project><modelVersion>4.0.0</modelVersion><groupId>log4j</groupId><artifactId>log4j</artifactId>` +
			`<version>1.2.17</version><packaging>bundle</packaging></project>`),
			POM{Coordinates: Coordinates{"log4j", "log4j", "1.2.17"}, Packaging: "bundle"}},
		{"ISO-8859-1", append([]byte(`<?xml version="1.0" encoding="ISO-8859-1"?><project><groupId>org.example</groupId>`+
			`<artifactId>caf`), append([]byte{0xe9}, []byte(`</artifactId><version>1.0</version></project>`)...)...), POM{}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParsePOM(tt.pom)
			if tt.name == "ISO-8859-1" {
				// é is not in the artifactId grammar: the error shows that it was decoded.
				if !errors.Is(err, format.ErrInvalid) || !strings.Contains(err.Error(), `"café"`) {
					t.Fatalf("ParsePOM: %v, want the artifactId café refused", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if d := cmp.Diff(&tt.want, got); d != "" {
				t.Errorf("ParsePOM (-want +got)\n%s", d)
			}
		})
	}
}

// TestParsePOMRefuses lists the POMs ParsePOM refuses, each with a part of the message that
// names the rule; ${revision} is refused as a CI-friendly version.
func TestParsePOMRefuses(t *testing.T) {
	big, _ := testutil.POM(testutil.POMOptions{})
	big = append(big, strings.Repeat(" ", maxPOM)...)
	for _, tt := range []struct {
		name string
		pom  []byte
		msg  string
	}{
		{"${revision}", project(`<groupId>com.example</groupId><artifactId>lib-core</artifactId><version>${revision}</version>`),
			`the version of the POM is "${revision}": ${revision} is a CI-friendly version`},
		{"${revision}${changelist}", project(`<groupId>com.example</groupId><artifactId>lib-core</artifactId><version>${revision}${changelist}</version>`),
			"CI-friendly version"},
		{"${revision} inherited", project(`<parent><groupId>com.example</groupId><artifactId>parent</artifactId><version>${revision}</version></parent>` +
			`<artifactId>lib-core</artifactId>`), "CI-friendly version"},
		{"${revision} in the parent", project(`<parent><groupId>com.example</groupId><artifactId>parent</artifactId><version>${revision}</version></parent>` +
			`<artifactId>lib-core</artifactId><version>1.0</version>`), `the parent version of the POM is "${revision}"`},
		{"another property", project(`<groupId>${group}</groupId><artifactId>lib-core</artifactId><version>1.0</version>`),
			"with the property reference ${group}; Lajstrom resolves only ${project.*}"},
		{"the version as itself", project(`<groupId>com.example</groupId><artifactId>lib-core</artifactId><version>${project.version}</version>`),
			"${project.version}"},
		{"an unclosed reference", project(`<groupId>com.example</groupId><artifactId>lib-core</artifactId><version>${revision</version>`),
			"${ without }"},
		{"SNAPSHOT", project(`<groupId>com.example</groupId><artifactId>lib-core</artifactId><version>1.0-SNAPSHOT</version>`), "SNAPSHOT"},
		{"no groupId", project(`<artifactId>lib-core</artifactId><version>1.0</version>`), "no groupId"},
		{"no artifactId", project(`<groupId>com.example</groupId><version>1.0</version>`), "no artifactId"},
		{"no version", project(`<groupId>com.example</groupId><artifactId>lib-core</artifactId>`), "no version"},
		{"a bad groupId", project(`<groupId>com/example</groupId><artifactId>lib-core</artifactId><version>1.0</version>`), "groupId"},
		{"a bad packaging", project(`<groupId>com.example</groupId><artifactId>lib-core</artifactId><version>1.0</version><packaging>a b</packaging>`), "packaging"},
		{"modelVersion 4.1.0", []byte(`<project><modelVersion>4.1.0</modelVersion></project>`), "modelVersion"},
		{"not project", []byte(`<settings><groupId>com.example</groupId></settings>`), "root element"},
		{"not XML", []byte("PK\x03\x04"), "not a POM"},
		{"an entity", project(`<groupId>&ent;</groupId>`), "not a POM"},
		{"windows-1252", []byte(`<?xml version="1.0" encoding="windows-1252"?><project/>`), "the encoding windows-1252"},
		{"too large", big, "at most"},
		{"references that refer to each other", referencesEachOther(64), "references of the POM expand to more than"},
		{"one long value referred to often", project(`<groupId>com.example</groupId><artifactId>lib-core</artifactId>` +
			`<version>1.` + strings.Repeat("0", 4<<10) + `</version><dependencies>` +
			strings.Repeat(`<dependency><version>${project.version}</version></dependency>`, 64) + `</dependencies>`),
			"references of the POM expand to more than"},
	} {
		_, err := ParsePOM(tt.pom)
		if !errors.Is(err, format.ErrInvalid) || !strings.Contains(err.Error(), tt.msg) {
			t.Errorf("%s: ParsePOM = %v, want ErrInvalid with %q", tt.name, err, tt.msg)
		}
	}
}

// referencesEachOther returns a POM whose groupId is k references to the version and whose
// version is k references to the groupId: each round of the interpolation multiplied its length
// by k, and before #51 a POM of 938 bytes (k = 16) allocated 186 MB, one of 2 KiB (k = 64)
// would have needed hundreds of GB.
func referencesEachOther(k int) []byte {
	return project(`<groupId>` + strings.Repeat("${project.version}", k) + `</groupId><artifactId>a</artifactId>` +
		`<version>` + strings.Repeat("${project.groupId}", k) + `</version>`)
}

// pomAllocBound is the most ParsePOM may allocate for a POM of n bytes: encoding/xml allocates
// a few hundred bytes per element and attribute, the interpolation at most the POM and
// expansionSlack.
func pomAllocBound(n int) int { return testutil.AllocSlack + 256*n }

// TestParsePOMExpansionBounded checks that what ParsePOM allocates for references that refer to
// each other grows with the size of the POM, not with the length the references expand to.
func TestParsePOMExpansionBounded(t *testing.T) {
	for _, k := range []int{8, 16, 64, 1024} {
		b := referencesEachOther(k)
		if n := testutil.Allocated(func() { _, _ = ParsePOM(b) }); n > pomAllocBound(len(b)) {
			t.Errorf("k = %d: ParsePOM of %d bytes allocated %d bytes, at most %d", k, len(b), n, pomAllocBound(len(b)))
		}
	}
}
