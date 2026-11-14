package maven

import (
	"bytes"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/lajstrom/lajstrom/internal/format"
)

// snapshotDoc is the maven-metadata.xml of the SNAPSHOT version that mvn PUTs after the files of
// the build timestamp-number.
func snapshotDoc(timestamp, number string) []byte {
	return []byte(`<?xml version="1.0" encoding="UTF-8"?>
<metadata modelVersion="1.1.0"><groupId>com.example</groupId><artifactId>lib-core</artifactId><version>1.0-SNAPSHOT</version>` +
		`<versioning><snapshot><timestamp>` + timestamp + `</timestamp><buildNumber>` + number + `</buildNumber></snapshot>` +
		`<lastUpdated>` + strings.Replace(timestamp, ".", "", 1) + `</lastUpdated><snapshotVersions><snapshotVersion><extension>jar</extension>` +
		`<value>1.0-` + timestamp + `-` + number + `</value></snapshotVersion></snapshotVersions></versioning></metadata>`)
}

// artifactDoc is the artifact's maven-metadata.xml that mvn PUTs last after a SNAPSHOT deploy:
// latest the SNAPSHOT version, no release.
var artifactDoc = []byte(`<metadata><groupId>com.example</groupId><artifactId>lib-core</artifactId><versioning>` +
	`<latest>1.0-SNAPSHOT</latest><versions><version>1.0-SNAPSHOT</version></versions><lastUpdated>20261114101112</lastUpdated></versioning></metadata>`)

// deploySnapshot PUTs a build as mvn does: each file and its .sha1 and .md5, the SNAPSHOT
// version's maven-metadata.xml, which commits the build, then the artifact's. It returns the
// statuses of the two documents.
func (e *deployEnv) deploySnapshot(files []deployFile, timestamp, number string) (version, artifact int) {
	e.t.Helper()
	for _, f := range files {
		e.wantPut(f.path, f.body, http.StatusCreated)
		for _, a := range []string{SHA1, MD5} {
			e.wantPut(f.path+"."+a, []byte(sumsOf(f.body)[a]), http.StatusCreated)
		}
	}
	version = e.put("alice", snapshotDir+MetadataFile, snapshotDoc(timestamp, number)).Code
	artifact = e.put("alice", metadataPath, artifactDoc).Code
	return version, artifact
}

// get GETs /maven/releases/<path> as alice.
func (e *deployEnv) get(path string) *httptest.ResponseRecorder {
	e.t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/maven/releases/"+path, nil)
	req.Header.Set("X-Test-User", "alice")
	rr := httptest.NewRecorder()
	e.h.ServeHTTP(rr, req)
	return rr
}

// wantSnapshotDeploy deploys a build (deploySnapshot) and fails the test unless the SNAPSHOT
// version's maven-metadata.xml publishes it (201) and the artifact's has nothing left to (200).
func (e *deployEnv) wantSnapshotDeploy(files []deployFile, timestamp, number string) {
	e.t.Helper()
	if v, a := e.deploySnapshot(files, timestamp, number); v != http.StatusCreated || a != http.StatusOK {
		e.t.Fatalf("the build %s-%s: %d and %d, want 201 and 200", timestamp, number, v, a)
	}
}

// TestDeploySnapshotTwice: two deploys of 1.0-SNAPSHOT, as mvn sends them, are two builds, two
// versions under two tags, each committed by the SNAPSHOT version's maven-metadata.xml (201) and
// followed by the artifact's (200: nothing left to commit); the first build's manifest stays as
// it was. The same build again is 200, other bytes under it 409.
func TestDeploySnapshotTwice(t *testing.T) {
	e := newDeployEnv(t, nil)
	e.snapshotRepo()
	e.wantSnapshotDeploy(snapshotBuild("1.0-20261114.101112-1", "", true), "20261114.101112", "1")
	first := e.versions()
	if len(first) != 1 || first[0].Version != "1.0-20261114.101112-1" || first[0].Tag != "1.0-20261114.101112-1" {
		t.Fatalf("versions %+v, want the build 1.0-20261114.101112-1", first)
	}
	second := snapshotBuild("1.0-20261114.111500-2", "second", false)
	e.wantSnapshotDeploy(second, "20261114.111500", "2")
	vs := e.versions()
	if len(vs) != 2 {
		t.Fatalf("versions %+v, want two builds", vs)
	}
	for _, v := range vs {
		if v.Version == first[0].Version && (v.Digest != first[0].Digest || v.Tag != first[0].Tag) {
			t.Errorf("the first build changed: %+v, was %+v", v, first[0])
		}
		if m := e.manifest(v); m.Annotations["lajstrom.version"] != v.Version {
			t.Errorf("the manifest of %s says version %q", v.Version, m.Annotations["lajstrom.version"])
		}
	}

	if v, _ := e.deploySnapshot(second, "20261114.111500", "2"); v != http.StatusOK {
		t.Errorf("the second build again: %d, want 200", v)
	}
	if v, _ := e.deploySnapshot(snapshotBuild("1.0-20261114.111500-2", "changed", false), "20261114.111500", "2"); v != http.StatusConflict {
		t.Errorf("other bytes under the second build: %d, want 409", v)
	}
	if n := len(e.versions()); n != 2 {
		t.Errorf("%d versions, want 2", n)
	}
}

// TestResolveSnapshot: after two builds of 1.0-SNAPSHOT, the SNAPSHOT version's
// maven-metadata.xml names the second, and the sources of the first, which the second does not
// have; the artifact's lists 1.0-SNAPSHOT once, as latest, with no release; the non-unique name of
// the jar is the second build's, that of the sources the first build's.
func TestResolveSnapshot(t *testing.T) {
	e := newDeployEnv(t, nil)
	e.snapshotRepo()
	e.deploySnapshot(snapshotBuild("1.0-20261114.101112-1", "", true), "20261114.101112", "1")
	second := snapshotBuild("1.0-20261114.111500-2", "second", false)
	e.deploySnapshot(second, "20261114.111500", "2")

	body := e.get(snapshotDir + MetadataFile).Body.String()
	for _, want := range []string{"<version>1.0-SNAPSHOT</version>", "<timestamp>20261114.111500</timestamp>", "<buildNumber>2</buildNumber>",
		"<classifier>sources</classifier>\n        <extension>jar</extension>\n        <value>1.0-20261114.101112-1</value>"} {
		if !strings.Contains(body, want) {
			t.Errorf("the SNAPSHOT version's %s has no %q:\n%s", MetadataFile, want, body)
		}
	}
	if body := e.get(metadataPath).Body.String(); strings.Count(body, "1.0-SNAPSHOT") != 2 || strings.Contains(body, "<release>") {
		t.Errorf("the artifact's %s, want 1.0-SNAPSHOT as latest and in versions once, no release:\n%s", MetadataFile, body)
	}
	if got := e.get(snapshotDir + "lib-core-1.0-SNAPSHOT.jar"); got.Code != http.StatusOK || !bytes.Equal(got.Body.Bytes(), second[0].body) {
		t.Errorf("GET of the non-unique name: %d, %d bytes, want 200 and the second build's jar", got.Code, got.Body.Len())
	}
	if got := e.get(snapshotDir + "lib-core-1.0-SNAPSHOT-sources.jar"); got.Code != http.StatusOK {
		t.Errorf("GET of the non-unique name of the sources, which the first build has only: %d, want 200", got.Code)
	}

}

// TestDeploySnapshotRefused: the policy decides which kind a deploy may add, at its first file
// and before anything is stored (400 naming the key); a non-unique file name, and a build that
// breaks the rules, are 400; the SNAPSHOT version's maven-metadata.xml naming no build, or a
// build without files, is 400, and the artifact's naming a SNAPSHOT version without a build too.
func TestDeploySnapshotRefused(t *testing.T) {
	e := newDeployEnv(t, nil)
	build := snapshotBuild("1.0-20261114.101112-1", "", false)
	if rr := e.put("alice", build[0].path, build[0].body); rr.Code != http.StatusBadRequest || !strings.Contains(rr.Body.String(), "policy.maven.versionPolicy") {
		t.Errorf("a build in a release repo: %d %s, want 400 naming the policy", rr.Code, rr.Body)
	}
	e.snapshotRepo()
	rel := release("1.2.0", "")
	if rr := e.put("alice", rel[0].path, rel[0].body); rr.Code != http.StatusBadRequest || !strings.Contains(rr.Body.String(), "policy.maven.versionPolicy") {
		t.Errorf("a release in a snapshot repo: %d %s, want 400 naming the policy", rr.Code, rr.Body)
	}
	for _, p := range []string{
		snapshotDir + "lib-core-1.0-SNAPSHOT.jar",                                       // non-unique
		snapshotDir + "lib-core-1.0-20261114.101112-0.jar",                              // build number 0
		snapshotDir + "lib-core-1.0-20261114.101112-01.jar",                             // a leading zero
		snapshotDir + "lib-core-1.0-20261399.101112-1.jar",                              // not a date
		"com/example/lib-core/1.0-20261114.101112-1/lib-core-1.0-20261114.101112-1.jar", // not in its SNAPSHOT version's directory
		"com/example/lib-core/1.0-snapshot/lib-core-1.0-20261114.101112-1.jar",
	} {
		e.wantPut(p, build[0].body, http.StatusBadRequest)
	}
	rr, err := e.fake.Repository(t.Context(), "maven/releases/com.example/lib-core")
	must(t, err)
	if stored, err := rr.Exists(t.Context(), ocispec.Descriptor{Digest: digest.FromBytes(build[0].body), Size: int64(len(build[0].body))}); err != nil || stored {
		t.Errorf("a refused file was stored: %t, %v", stored, err)
	}
	e.wantPut(snapshotDir+MetadataFile, snapshotDoc("20261114.101112", "1"), http.StatusBadRequest) // no files
	e.wantPut(snapshotDir+MetadataFile, []byte(`<metadata><version>1.0-SNAPSHOT</version></metadata>`), http.StatusBadRequest)
	e.wantPut(snapshotDir+MetadataFile, bytes.Replace(snapshotDoc("20261114.101112", "1"), []byte("<version>1.0-SNAPSHOT"), []byte("<version>2.0-SNAPSHOT"), 1), http.StatusBadRequest)
	e.wantPut(metadataPath, artifactDoc, http.StatusBadRequest) // no build of 1.0-SNAPSHOT
	if vs := e.versions(); len(vs) != 0 {
		t.Errorf("versions %+v, want none", vs)
	}
}

// TestSnapshotBuildOfDocument: the build a SNAPSHOT version's maven-metadata.xml is for is the one
// its snapshot names, else the newest of its snapshotVersions of that SNAPSHOT version; a
// document of another version, one that names a build breaking the rules, and one that names
// none are ErrInvalid.
func TestSnapshotBuildOfDocument(t *testing.T) {
	const sv = `<snapshotVersions><snapshotVersion><value>1.0-20261114.101112-1</value></snapshotVersion>` +
		`<snapshotVersion><value>1.0-20261114.121500-3</value></snapshotVersion><snapshotVersion><value>1.0-20261114.111500-2</value></snapshotVersion>` +
		`<snapshotVersion><value>2.0-20261115.000000-9</value></snapshotVersion></snapshotVersions>`
	for _, tt := range []struct{ name, doc, want, err string }{
		{"snapshot", `<metadata><version>1.0-SNAPSHOT</version><versioning><snapshot><timestamp>20261114.101112</timestamp><buildNumber>7</buildNumber></snapshot>` + sv + `</versioning></metadata>`,
			"1.0-20261114.101112-7", ""},
		{"snapshotVersions", `<metadata><versioning>` + sv + `</versioning></metadata>`, "1.0-20261114.121500-3", ""},
		{"another version", `<metadata><version>2.0-SNAPSHOT</version><versioning>` + sv + `</versioning></metadata>`, "", "the metadata of the version 2.0-SNAPSHOT"},
		{"a build number 0", `<metadata><versioning><snapshot><timestamp>20261114.101112</timestamp><buildNumber>0</buildNumber></snapshot></versioning></metadata>`,
			"", "the build number"},
		{"a build of a local repository", `<metadata><versioning><snapshot><localCopy>true</localCopy></snapshot></versioning></metadata>`, "", "names no build"},
		{"no build", `<metadata><version>1.0-SNAPSHOT</version></metadata>`, "", "names no build"},
	} {
		md, err := readMetadataXML(strings.NewReader(tt.doc), snapshotDir+MetadataFile)
		must(t, err)
		got, err := md.snapshotBuild("1.0-SNAPSHOT", snapshotDir+MetadataFile)
		if got != tt.want || (tt.err == "") != (err == nil) || (err != nil && (!errors.Is(err, format.ErrInvalid) || !strings.Contains(err.Error(), tt.err))) {
			t.Errorf("%s: %q, %v; want %q, an error with %q", tt.name, got, err, tt.want, tt.err)
		}
	}
}
