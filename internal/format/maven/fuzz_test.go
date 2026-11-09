package maven

import (
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/lajstrom/lajstrom/internal/format"
	"github.com/lajstrom/lajstrom/internal/testutil"
)

// FuzzPOM feeds ParsePOM a POM: the small ones here and the real-looking ones of
// testdata/fuzz/FuzzPOM, which TestFuzzCorpus writes. eachRef must find the references
// propertyRef finds. Every error must be format.ErrInvalid, and what ParsePOM allocates must
// grow with the size of the POM. A POM it accepts has
// coordinates, packaging and parent that pass check and dependency fields without white space
// around them, and written back with what was resolved, it parses to the same POM.
func FuzzPOM(f *testing.F) {
	for _, s := range []string{
		`<project/>`,
		`<project><groupId>g</groupId><artifactId>a</artifactId><version>1</version></project>`,
		`<project><parent><groupId>g</groupId><artifactId>p</artifactId><version>1</version></parent><artifactId>a</artifactId>` +
			`<dependencies><dependency><groupId>${project.groupId}</groupId><artifactId>b</artifactId>` +
			`<version>${project.parent.version}</version><scope> test </scope></dependency></dependencies></project>`,
		`<project><groupId>g</groupId><artifactId>a</artifactId><version>${project.version}</version></project>`,
		`<project><groupId>${project.version}${project.version}</groupId><artifactId>a</artifactId>` +
			`<version>${project.groupId}${project.groupId}</version></project>`,
		`<?xml version="1.0" encoding="ISO-8859-1"?><project><groupId>g</groupId><artifactId>caf` + "\xe9" + `</artifactId></project>`,
		`<project><groupId><![CDATA[g]]></groupId><!-- c --><artifactId>a&#x2d;b</artifactId><version>1&amp;</version></project>`,
	} {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, b []byte) {
		var refs [][]int
		eachRef(string(b), func(start, end int) { refs = append(refs, []int{start, end}) })
		if want := propertyRef.FindAllStringIndex(string(b), -1); !reflect.DeepEqual(refs, want) {
			t.Fatalf("eachRef finds %v, propertyRef %v", refs, want)
		}
		var (
			pom *POM
			err error
		)
		testutil.CheckAllocated(t, fmt.Sprintf("ParsePOM of %d bytes", len(b)), pomAllocBound(len(b)),
			func() { pom, err = ParsePOM(b) })
		if err != nil {
			if !errors.Is(err, format.ErrInvalid) {
				t.Fatalf("ParsePOM = %v, want ErrInvalid", err)
			}
			return
		}
		if err := pom.check(); err != nil {
			t.Fatalf("ParsePOM accepted a POM that check refuses: %v", err)
		}
		for _, d := range pom.Dependencies {
			for _, v := range []string{d.GroupID, d.ArtifactID, d.Version, d.Type, d.Classifier, d.Scope, d.Optional} {
				if strings.TrimSpace(v) != v {
					t.Fatalf("dependency %+v: %q has white space around it", d, v)
				}
			}
		}
		back := pomOf(pom)
		again, err := ParsePOM(back)
		if err != nil {
			t.Fatalf("ParsePOM of the POM written back:\n%s\n%v", back, err)
		}
		if d := cmp.Diff(pom, again); d != "" {
			t.Fatalf("the POM written back parses to another POM (-first +again)\n%s", d)
		}
	})
}

// pomOf writes p as a POM without namespace: the coordinates, the packaging, the parent and the
// dependencies, the elements of empty values left out. Every character but printable ASCII is a
// character reference, so that the value comes back as it is.
func pomOf(p *POM) []byte {
	var b strings.Builder
	el := func(name, v string) {
		if v == "" {
			return
		}
		b.WriteString("<" + name + ">")
		for _, r := range v {
			if r < 0x20 || r > 0x7e || strings.ContainsRune(`<>&'"`, r) {
				fmt.Fprintf(&b, "&#x%x;", r)
			} else {
				b.WriteRune(r)
			}
		}
		b.WriteString("</" + name + ">")
	}
	b.WriteString("<project><modelVersion>4.0.0</modelVersion>")
	if q := p.Parent; q != nil {
		b.WriteString("<parent>")
		el("groupId", q.GroupID)
		el("artifactId", q.ArtifactID)
		el("version", q.Version)
		b.WriteString("</parent>")
	}
	el("groupId", p.GroupID)
	el("artifactId", p.ArtifactID)
	el("version", p.Version)
	el("packaging", p.Packaging)
	if len(p.Dependencies) > 0 {
		b.WriteString("<dependencies>")
		for _, d := range p.Dependencies {
			b.WriteString("<dependency>")
			el("groupId", d.GroupID)
			el("artifactId", d.ArtifactID)
			el("version", d.Version)
			el("type", d.Type)
			el("classifier", d.Classifier)
			el("scope", d.Scope)
			el("optional", d.Optional)
			b.WriteString("</dependency>")
		}
		b.WriteString("</dependencies>")
	}
	b.WriteString("</project>")
	return []byte(b.String())
}

// TestFuzzCorpus writes testdata/fuzz/FuzzPOM when LAJSTROM_FUZZ_CORPUS=write: POMs of testutil,
// POMs written here as Maven Central has them, and the POM that took gigabytes before #51.
func TestFuzzCorpus(t *testing.T) {
	plain, _ := testutil.POM(testutil.POMOptions{})
	deps, _ := testutil.POM(testutil.POMOptions{GroupID: "org.example", ArtifactID: "app", Version: "2.0", Packaging: "war",
		Dependencies: []testutil.DependencyOptions{
			{GroupID: "com.example", ArtifactID: "lib-api", Version: "1.0.0"},
			{GroupID: "org.junit.jupiter", ArtifactID: "junit-jupiter", Version: "5.11.3", Scope: "test"},
			{GroupID: "org.slf4j", ArtifactID: "slf4j-api"},
		}})
	bom, _ := testutil.POM(testutil.POMOptions{ArtifactID: "bom", Packaging: "pom"})
	testutil.WriteFuzzCorpus(t, "FuzzPOM", map[string]any{
		"testutil-default":          plain,
		"testutil-war-dependencies": deps,
		"testutil-bom":              bom,
		"parent-properties": project(`<parent><groupId>org.springframework.boot</groupId>` +
			`<artifactId>spring-boot-starter-parent</artifactId><version>3.3.5</version><relativePath/></parent>
  <groupId>com.example</groupId><artifactId>demo</artifactId><version>0.0.1</version><name>demo</name>
  <properties><java.version>21</java.version></properties>
  <dependencies>
    <dependency><groupId>org.springframework.boot</groupId><artifactId>spring-boot-starter-web</artifactId></dependency>
    <dependency><groupId>${project.groupId}</groupId><artifactId>demo-api</artifactId><version>${project.version}</version></dependency>
    <dependency><groupId>org.springframework.boot</groupId><artifactId>spring-boot-starter-test</artifactId><scope>test</scope></dependency>
  </dependencies>
  <build><plugins><plugin><groupId>org.springframework.boot</groupId><artifactId>spring-boot-maven-plugin</artifactId></plugin></plugins></build>`),
		"inherited-coordinates": project(parent + `<artifactId>lib-core</artifactId><packaging>bundle</packaging>
  <dependencyManagement><dependencies><dependency><groupId>x</groupId><artifactId>managed</artifactId></dependency></dependencies></dependencyManagement>
  <dependencies><dependency><groupId>com.fasterxml.jackson.core</groupId><artifactId>jackson-databind</artifactId>` +
			`<version>${jackson.version}</version><type>jar</type><classifier>tests</classifier><optional>true</optional></dependency></dependencies>
  <profiles><profile><id>p</id><dependencies><dependency><groupId>y</groupId><artifactId>in-profile</artifactId></dependency></dependencies></profile></profiles>`),
		"no-namespace-latin1": []byte("<?xml version=\"1.0\" encoding=\"ISO-8859-1\"?>\n<project>\n  <modelVersion>4.0.0</modelVersion>\n" +
			"  <groupId>log4j</groupId>\n  <artifactId>log4j</artifactId>\n  <version>1.2.17</version>\n  <packaging>bundle</packaging>\n" +
			"  <name>Apache Log4j</name>\n  <description>Gr\xfc\xdfe aus K\xf6ln</description>\n</project>\n"),
		"references-each-other": referencesEachOther(64),
	})
}
