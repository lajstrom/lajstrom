package maven

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/lajstrom/lajstrom/internal/auth"
	"github.com/lajstrom/lajstrom/internal/format"
	"github.com/lajstrom/lajstrom/internal/index"
	"github.com/lajstrom/lajstrom/internal/oci/profile"
	"github.com/lajstrom/lajstrom/internal/testutil"
)

// TestSnapshotOf reads versions as builds by the rules of docs/spec/manifest-profile.md §4.2:
// a build's SNAPSHOT version, and false for a release and for a version that looks like a build
// and breaks a rule; checkVersion takes the releases and the builds, and refuses a SNAPSHOT
// version and those builds.
func TestSnapshotOf(t *testing.T) {
	for _, tt := range []struct {
		version, snapshot string // "" for no build
		valid             bool   // checkVersion takes it
	}{
		{"1.0-20261114.101112-3", "1.0-SNAPSHOT", true},
		{"1.0-rc1-20261114.101112-12", "1.0-rc1-SNAPSHOT", true},
		{"2-20261231.235959-999999999", "2-SNAPSHOT", true},
		{"1.0", "", true},
		{"1.0-20261114", "", true},
		{"1.0-20261114.101112", "", true},
		{"1.0-SNAPSHOT", "", false},
		{"1.0-snapshot", "", false},
		{"20261114.101112-1", "", false},                       // no base
		{"1.0-SNAPSHOT-20261114.101112-1", "", false},          // a SNAPSHOT base
		{"1.0-20261114.101112-1-20261114.101112-2", "", false}, // a build as the base
		{"1.0-20261114.101112-0", "", false},
		{"1.0-20261114.101112-01", "", false},
		{"1.0-20261114.101112-1000000000", "", false},
		{"1.0-20261314.101112-1", "", false}, // month 13
		{"1.0-20261114.246000-1", "", false},
	} {
		got, ok := SnapshotOf(tt.version)
		if got != tt.snapshot || ok != (tt.snapshot != "") {
			t.Errorf("SnapshotOf(%q) = %q, %t; want %q", tt.version, got, ok, tt.snapshot)
		}
		if err := checkVersion(tt.version); (err == nil) != tt.valid || (err != nil && !errors.Is(err, format.ErrInvalid)) {
			t.Errorf("checkVersion(%q) = %v, want valid %t", tt.version, err, tt.valid)
		}
	}
	if d := (Coordinates{GroupID: "com.example", ArtifactID: "lib-core", Version: "1.0-20261114.101112-3"}).Dir(); d != "com/example/lib-core/1.0-SNAPSHOT" {
		t.Errorf("the directory of a build: %q, want that of its SNAPSHOT version", d)
	}
}

// snapshotDir is the directory of the SNAPSHOT version of the deploy tests.
const snapshotDir = "com/example/lib-core/1.0-SNAPSHOT/"

// snapshotBuild is the files of the build of com.example:lib-core 1.0-SNAPSHOT at the timestamp
// and number of version, its jar, sources jar (when sources) and POM, as Maven 3 names them; seed
// changes the bytes of the jars. The POM keeps the SNAPSHOT version, as Maven's does.
func snapshotBuild(version, seed string, sources bool) []deployFile {
	classifiers := []string{""}
	if sources {
		classifiers = append(classifiers, "sources")
	}
	var files []deployFile
	for _, c := range classifiers {
		b, _ := testutil.Jar(testutil.JarOptions{Version: "1.0-SNAPSHOT", Classifier: c})
		name := "lib-core-" + version
		if c != "" {
			name += "-" + c
		}
		files = append(files, deployFile{snapshotDir + name + ".jar", append(b, seed...)})
	}
	pom, _ := testutil.POM(testutil.POMOptions{Version: "1.0-SNAPSHOT"})
	return append(files, deployFile{snapshotDir + "lib-core-" + version + ".pom", pom})
}

// snapshotRepo makes the env's repo a repo of SNAPSHOT builds.
func (e *deployEnv) snapshotRepo() {
	e.t.Helper()
	e.repo.Policy = []byte(`{"maven":{"versionPolicy":"snapshot"}}`)
	must(e.t, e.st.UpdateRepo(context.Background(), &e.repo))
}

// TestAdmit: policy.maven.versionPolicy decides which kind of version a repo takes, a release
// or a SNAPSHOT build, the release by default and for a policy it does not know (the API checks
// the values); the refusal is ErrInvalid naming the key.
func TestAdmit(t *testing.T) {
	for _, tt := range []struct {
		policy, version string
		ok              bool
	}{
		{"", "1.2.0", true},
		{"", "1.0-20261114.101112-3", false},
		{`{"maven":{"versionPolicy":"release"}}`, "1.0-20261114.101112-3", false},
		{`{"maven":{"versionPolicy":"release"}}`, "1.2.0", true},
		{`{"maven":{"versionPolicy":"snapshot"}}`, "1.0-20261114.101112-3", true},
		{`{"maven":{"versionPolicy":"snapshot"}}`, "1.2.0", false},
		{`{"maven":{"versionPolicy":"snapshot"}}`, "1.0-rc1", false},
		{`{"maven":{"versionPolicy":"mixed"}}`, "1.0-20261114.101112-3", false},
		{`{"maven":{"sessionTimeout":"1h"}}`, "1.2.0", true},
	} {
		repo := index.Repo{Format: Name, Name: "r", Policy: json.RawMessage(tt.policy)}
		err := Plugin{}.Admit(repo, format.Entry{Name: "com.example:lib-core", Version: tt.version})
		if tt.ok != (err == nil) || (err != nil && (!errors.Is(err, format.ErrInvalid) || !strings.Contains(err.Error(), "policy.maven.versionPolicy"))) {
			t.Errorf("Admit(%s, %s) = %v, want ok %t, or ErrInvalid naming the key", tt.policy, tt.version, err, tt.ok)
		}
	}
}

// TestPushSnapshotPolicy: a manifest pushed to /v2 meets the policy in ingest as a deploy does:
// a build in a release repo and a release in a snapshot repo are ErrInvalid naming the key, and
// nothing is pushed; a build in a snapshot repo is published.
func TestPushSnapshotPolicy(t *testing.T) {
	ctx := t.Context()
	e := newDeployEnv(t, nil)
	push := func(files []deployFile) error {
		t.Helper()
		var ups []format.UploadedFile
		for _, f := range files {
			ups = append(ups, upFile(f.path, f.body))
		}
		built, err := Plugin{}.Build(ctx, e.repo, format.Upload{Files: ups})
		must(t, err)
		rr, err := e.fake.Repository(ctx, built.RepoPath)
		must(t, err)
		for _, b := range built.Blobs {
			rc, err := b.Open(ctx)
			must(t, err)
			must(t, rr.Push(ctx, b.Descriptor, rc))
			_ = rc.Close()
		}
		raw, _, err := profile.Encode(built.Manifest)
		must(t, err)
		_, err = e.pipe.Ingest(ctx, e.repo, "com.example/lib-core", built.Tag, built.Manifest, raw, auth.Principal{Kind: auth.KindUser, Name: "alice"})
		return err
	}
	build := snapshotBuild("1.0-20261114.101112-1", "", false)
	if err := push(build); !errors.Is(err, format.ErrInvalid) || !strings.Contains(err.Error(), "policy.maven.versionPolicy") {
		t.Errorf("a build pushed to a release repo: %v, want ErrInvalid naming the policy", err)
	}
	e.snapshotRepo()
	if err := push(release("1.2.0", "")); !errors.Is(err, format.ErrInvalid) || !strings.Contains(err.Error(), "policy.maven.versionPolicy") {
		t.Errorf("a release pushed to a snapshot repo: %v, want ErrInvalid naming the policy", err)
	}
	if n := len(e.fake.PushReferences()); n != 0 {
		t.Errorf("%d manifests pushed, want none", n)
	}
	if err := push(build); err != nil {
		t.Errorf("a build pushed to a snapshot repo: %v", err)
	}
	if vs := e.versions(); len(vs) != 1 || vs[0].Version != "1.0-20261114.101112-1" {
		t.Errorf("versions %+v, want the build", vs)
	}
}
