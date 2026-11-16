package maven

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/lajstrom/lajstrom/internal/format"
	"github.com/lajstrom/lajstrom/internal/index"
	"github.com/lajstrom/lajstrom/internal/testutil"
)

// groupEnv is the proxy env with the hosted repo maven/releases and the group maven/all of
// releases and central, in that order, with merge as its group.merge.
type groupEnv struct {
	*proxyEnv
	hosted, group index.Repo
}

func newGroupEnv(t *testing.T, merge string) *groupEnv {
	t.Helper()
	ctx := t.Context()
	e := &groupEnv{proxyEnv: newProxyEnv(t, "")}
	e.hosted = index.Repo{Format: Name, Name: "releases", Registry: "local", Prefix: "maven/releases", Kind: index.KindHosted}
	must(t, e.st.CreateRepo(ctx, &e.hosted))
	e.group = index.Repo{Format: Name, Name: "all", Kind: index.KindGroup, Group: json.RawMessage(`{"merge": [` + merge + `]}`)}
	must(t, e.st.CreateRepo(ctx, &e.group))
	must(t, e.st.SetGroupMembers(ctx, e.group.ID, []int64{e.hosted.ID, e.repo.ID}))
	return e
}

// put PUTs body to /maven/<repo>/<path> and returns the status.
func (e *groupEnv) put(repo, path string, body []byte) int {
	rr := httptest.NewRecorder()
	e.h.ServeHTTP(rr, httptest.NewRequest(http.MethodPut, "/maven/"+repo+"/"+path, bytes.NewReader(body)))
	return rr.Code
}

// deploy deploys the release version of com.example:lib-core through the group, as mvn does
// without its checksums, and returns its files.
func (e *groupEnv) deploy(t *testing.T, version string) []deployFile {
	t.Helper()
	files := release(version, "hosted")
	for _, f := range files {
		if code := e.put("all", f.path, f.body); code != http.StatusCreated {
			t.Fatalf("PUT %s through the group: %d", f.path, code)
		}
	}
	if code := e.put("all", metadataPath, metadataDoc(version)); code != http.StatusCreated {
		t.Fatalf("the commit through the group: %d", code)
	}
	return files
}

// versionsOf returns the versions of the artifact's maven-metadata.xml that repo serves at path.
func versionsOf(t *testing.T, h http.Handler, repo, path string) *repoVersioning {
	t.Helper()
	rr := get(h, repo, path)
	if rr.Code != http.StatusOK {
		t.Fatalf("GET %s of %s: %d %s", path, repo, rr.Code, rr.Body)
	}
	doc, err := readRepoMetadata(rr.Body.Bytes())
	must(t, err)
	if doc.Versioning == nil {
		t.Fatalf("GET %s of %s: no versioning in %s", path, repo, rr.Body)
	}
	return doc.Versioning
}

// TestGroupResolve: a deploy through a group goes to its hosted member; the artifact the hosted
// member holds hides the upstream's versions of it, so the group's metadata is the hosted
// member's, its files come from there (a hosted version holds them without a variant), and a
// version only the upstream has is 404 with no request; another artifact comes from the proxy
// member, fetched on its first request. A group without a hosted member answers a deploy 405.
func TestGroupResolve(t *testing.T) {
	e := newGroupEnv(t, "")
	files := e.deploy(t, "1.2.0")
	other, oi := testutil.Jar(testutil.JarOptions{GroupID: "com.other", ArtifactID: "tool", Version: "2.0"})
	e.up.put(metadataPath, upstreamDoc("1.3.0", "1.3.0", "1.1.0", "1.3.0"), "")
	e.up.put("com/example/lib-core/1.3.0/lib-core-1.3.0.jar", []byte("public"), "")
	e.up.put(oi.Path, other, "")
	e.up.put("com/other/tool/maven-metadata.xml", []byte(`<metadata><groupId>com.other</groupId><artifactId>tool</artifactId>`+
		`<versioning><versions><version>2.0</version></versions></versioning></metadata>`), "")

	if v := versionsOf(t, e.h, "all", metadataPath); !slices.Equal(v.Versions, []string{"1.2.0"}) || v.Latest != "1.2.0" {
		t.Errorf("the metadata of a hosted artifact through the group: %+v", v)
	}
	jar := files[0]
	served(t, get(e.h, "all", jar.path), jar.body, "the hosted jar through the group")
	served(t, get(e.h, "all", jar.path+".sha256"), []byte(sumsOf(jar.body)[SHA256]), "the hosted jar's sha256 through the group")
	if rr := get(e.h, "all", "com/example/lib-core/1.3.0/lib-core-1.3.0.jar"); rr.Code != http.StatusNotFound {
		t.Errorf("a version only the upstream has of a hosted artifact: %d %s", rr.Code, rr.Body)
	}
	expect(t, "requests upstream for the hosted artifact", []int32{e.up.docs.Load(), e.up.sha1s.Load(), e.up.gets.Load()}, []int32{0, 0, 0})

	if v := versionsOf(t, e.h, "all", "com/other/tool/maven-metadata.xml"); !slices.Equal(v.Versions, []string{"2.0"}) {
		t.Errorf("the metadata of an upstream artifact through the group: %+v", v)
	}
	served(t, get(e.h, "all", oi.Path), other, "an upstream jar through the group")
	served(t, get(e.h, "all", oi.Path), other, "an upstream jar through the group again")
	expect(t, "file requests upstream", e.up.gets.Load(), int32(1))

	lone := index.Repo{Format: Name, Name: "lone", Kind: index.KindGroup, Group: json.RawMessage(`{"merge": []}`)}
	must(t, e.st.CreateRepo(t.Context(), &lone))
	must(t, e.st.SetGroupMembers(t.Context(), lone.ID, []int64{e.repo.ID}))
	if code := e.put("lone", jar.path, jar.body); code != http.StatusMethodNotAllowed {
		t.Errorf("a deploy to a group without a hosted member: %d, want 405", code)
	}
}

// TestGroupMerge: with a merge pattern, the group's metadata of an artifact the hosted member
// holds is the union of the members' versions, latest and release the greatest of theirs, and a
// version only the upstream has is fetched through the proxy member.
func TestGroupMerge(t *testing.T) {
	e := newGroupEnv(t, `"com.example:*"`)
	e.deploy(t, "1.2.0")
	e.up.put(metadataPath, upstreamDoc("1.3.0", "1.3.0", "1.1.0", "1.3.0"), "")
	e.up.put("com/example/lib-core/1.3.0/lib-core-1.3.0.jar", []byte("public"), "")
	v := versionsOf(t, e.h, "all", metadataPath)
	if got := []string{strings.Join(v.Versions, " "), v.Latest, v.Release}; !slices.Equal(got, []string{"1.1.0 1.2.0 1.3.0", "1.3.0", "1.3.0"}) {
		t.Errorf("the merged metadata: %q", got)
	}
	rr := get(e.h, "all", metadataPath)
	served(t, get(e.h, "all", metadataPath+".md5"), []byte(sumsOf(rr.Body.Bytes())[MD5]), "the md5 of the merged metadata")
	served(t, get(e.h, "all", "com/example/lib-core/1.3.0/lib-core-1.3.0.jar"), []byte("public"), "a version the upstream has")
}

// TestMergeSnapshot: the merged document of a SNAPSHOT version takes snapshot and lastUpdated
// from the member with the newest build, and per classifier and extension the newest file; the
// merged plugins of a group's metadata are their union by prefix, the first member's first.
func TestMergeSnapshot(t *testing.T) {
	sv := func(classifier, value, updated string) string {
		return `<snapshotVersion><classifier>` + classifier + `</classifier><extension>jar</extension><value>` + value +
			`</value><updated>` + updated + `</updated></snapshotVersion>`
	}
	member := func(timestamp, build string, files ...string) format.MemberDocument {
		return format.MemberDocument{Content: []byte(`<metadata modelVersion="1.1.0"><groupId>com.example</groupId><artifactId>lib-core</artifactId>` +
			`<version>1.0-SNAPSHOT</version><versioning><snapshot><timestamp>` + timestamp + `</timestamp><buildNumber>` + build +
			`</buildNumber></snapshot><lastUpdated>` + strings.Replace(timestamp, ".", "", 1) + `</lastUpdated><snapshotVersions>` +
			strings.Join(files, "") + `</snapshotVersions></versioning></metadata>`)}
	}
	d, err := Plugin{}.MergeGroup(t.Context(), index.Repo{}, "com.example:lib-core", "maven-metadata:1.0-SNAPSHOT", []format.MemberDocument{
		member("20261114.101112", "3", sv("", "1.0-20261114.101112-3", "20261114101112"), sv("sources", "1.0-20261114.101112-3", "20261114101112")),
		member("20261115.090000", "4", sv("", "1.0-20261115.090000-4", "20261115090000")),
	}, "")
	must(t, err)
	doc, err := readRepoMetadata(d.Content)
	must(t, err)
	v := doc.Versioning
	got := []string{v.Snapshot.Timestamp, v.LastUpdated}
	for _, f := range v.SnapshotVersions {
		got = append(got, f.Classifier+"="+f.Value)
	}
	expect(t, "the merged SNAPSHOT version", got, []string{"20261115.090000", "20261115090000", "=1.0-20261115.090000-4", "sources=1.0-20261114.101112-3"})
	if d.Path != "com/example/lib-core/1.0-SNAPSHOT/maven-metadata.xml" || doc.Version != "1.0-SNAPSHOT" {
		t.Errorf("the merged document's path %s, version %s", d.Path, doc.Version)
	}

	plugin := func(prefix, artifactID string) string {
		return `<plugin><prefix>` + prefix + `</prefix><artifactId>` + artifactID + `</artifactId></plugin>`
	}
	d, err = Plugin{}.MergeGroup(t.Context(), index.Repo{}, "org.apache.maven:plugins", kindMetadata, []format.MemberDocument{
		{Content: []byte(`<metadata><plugins>` + plugin("dependency", "maven-dependency-plugin") + `</plugins></metadata>`)},
		{Content: []byte(`<metadata><plugins>` + plugin("dependency", "other") + plugin("help", "maven-help-plugin") + `</plugins></metadata>`)},
	}, "")
	must(t, err)
	doc, err = readRepoMetadata(d.Content)
	must(t, err)
	expect(t, "the merged plugins", doc.Plugins, []repoPlugin{{Prefix: "dependency", ArtifactID: "maven-dependency-plugin"},
		{Prefix: "help", ArtifactID: "maven-help-plugin"}})
	if doc.Versioning != nil {
		t.Errorf("a group's metadata merged with versioning: %s", d.Content)
	}
}
