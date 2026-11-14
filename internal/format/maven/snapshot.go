package maven

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/lajstrom/lajstrom/internal/format"
	"github.com/lajstrom/lajstrom/internal/index"
)

// SNAPSHOT builds (docs/spec/manifest-profile.md §4.2, #80). Each deploy of a SNAPSHOT version,
// 1.0-SNAPSHOT, is a build: a version of its own whose version is the timestamped version its
// files are named with, 1.0-20261114.101112-3, the UTC time of the deploy and its build number,
// as Maven 3 and Gradle write unique snapshot versions. The SNAPSHOT version is never a version
// of the profile nor a tag: what a build has besides its version is read from it.

// snapshotSuffix ends a SNAPSHOT version: 1.0-SNAPSHOT.
const snapshotSuffix = "-SNAPSHOT"

// timestampLayout is the timestamp of a build, yyyyMMdd.HHmmss in UTC.
const timestampLayout = "20060102.150405"

// maxBuildNumber is the largest build number: Maven reads it as an int.
const maxBuildNumber = 999999999

// buildPattern is how Maven tells a build: its version ends with <yyyyMMdd.HHmmss>-<n> at its
// start or after a "-". The groups are the base with its "-", the timestamp and the number.
var buildPattern = regexp.MustCompile(`^(.*-)?([0-9]{8}\.[0-9]{6})-([0-9]+)$`)

// build is a SNAPSHOT build, read from its version.
type build struct {
	base      string // 1.0
	timestamp string // 20261114.101112
	number    int    // 3
}

// snapshot returns the SNAPSHOT version of the build, <base>-SNAPSHOT.
func (b build) snapshot() string { return b.base + snapshotSuffix }

// looksLikeBuild reports whether Maven reads version as a build: it ends with a timestamp and a
// number. A version that does may still break the rules of a build (parseBuild).
func looksLikeBuild(version string) bool { return buildPattern.MatchString(version) }

// parseBuild reads version as a build. A version that does not look like one is refused, and so
// is one that breaks the rules: the base is empty, is a build itself or ends in SNAPSHOT; the
// timestamp is not a date and time; the number is 0, has a leading zero or is above
// maxBuildNumber. Errors wrap format.ErrInvalid.
func parseBuild(version string) (build, error) {
	m := buildPattern.FindStringSubmatch(version)
	if m == nil {
		return build{}, fmt.Errorf("%w: %q is not the version of a SNAPSHOT build, <base>-<yyyyMMdd.HHmmss>-<n>", format.ErrInvalid, version)
	}
	b := build{base: strings.TrimSuffix(m[1], "-"), timestamp: m[2]}
	n, err := strconv.Atoi(m[3])
	switch {
	case b.base == "":
		return build{}, fmt.Errorf("%w: the SNAPSHOT build %q has no base version before its timestamp", format.ErrInvalid, version)
	case looksLikeBuild(b.base) || endsInSnapshot(b.base):
		return build{}, fmt.Errorf("%w: the SNAPSHOT build %q has the base %q, itself a SNAPSHOT version", format.ErrInvalid, version, b.base)
	case err != nil || n < 1 || n > maxBuildNumber || m[3][0] == '0':
		return build{}, fmt.Errorf("%w: the SNAPSHOT build %q has the build number %q, not a number from 1 to %d without a leading zero",
			format.ErrInvalid, version, m[3], maxBuildNumber)
	}
	if _, err := time.Parse(timestampLayout, b.timestamp); err != nil {
		return build{}, fmt.Errorf("%w: the SNAPSHOT build %q has the timestamp %q, not a time yyyyMMdd.HHmmss", format.ErrInvalid, version, b.timestamp)
	}
	b.number = n
	return b, nil
}

// SnapshotOf returns the SNAPSHOT version of the build version, 1.0-SNAPSHOT of
// 1.0-20261114.101112-3, and false when version is not a well-formed build: a release.
func SnapshotOf(version string) (string, bool) {
	b, err := parseBuild(version)
	if err != nil {
		return "", false
	}
	return b.snapshot(), true
}

// versionDir returns the directory of a version in a Maven 2 repository: its SNAPSHOT version
// for a build, the version itself otherwise.
func versionDir(version string) string {
	if s, ok := SnapshotOf(version); ok {
		return s
	}
	return version
}

// endsInSnapshot reports whether version ends with SNAPSHOT in any case.
func endsInSnapshot(version string) bool {
	n := len("SNAPSHOT")
	return len(version) >= n && strings.EqualFold(version[len(version)-n:], "SNAPSHOT")
}

// isSnapshotVersion reports whether version is a SNAPSHOT version as Maven 3 deploys one:
// <base>-SNAPSHOT, the suffix in capitals, the base a release version.
func isSnapshotVersion(version string) bool {
	base, ok := strings.CutSuffix(version, snapshotSuffix)
	return ok && base != "" && checkVersion(base) == nil && !looksLikeBuild(base)
}

// The values of policy.maven.versionPolicy (docs/api.md, Policy).
const (
	// PolicyRelease is a repo of release versions, the default.
	PolicyRelease = "release"
	// PolicySnapshot is a repo of SNAPSHOT builds.
	PolicySnapshot = "snapshot"
)

// versionPolicy returns the repo policy's maven.versionPolicy: PolicySnapshot when it says so,
// PolicyRelease otherwise. The policy is read leniently; the API checks it.
func versionPolicy(repo index.Repo) string {
	var p struct {
		Maven struct {
			VersionPolicy string `json:"versionPolicy"`
		} `json:"maven"`
	}
	if len(repo.Policy) > 0 && json.Unmarshal(repo.Policy, &p) == nil && p.Maven.VersionPolicy == PolicySnapshot {
		return PolicySnapshot
	}
	return PolicyRelease
}

var _ format.Admitter = Plugin{}

// Admit refuses a version the repo's policy.maven.versionPolicy does not take
// (docs/spec/manifest-profile.md §5): a SNAPSHOT build in a release repo, a release in a snapshot
// repo. Ingest asks it on every publish and never on reindex. The error wraps format.ErrInvalid.
func (Plugin) Admit(repo index.Repo, e format.Entry) error { return admit(repo, e.Version) }

// admit is Admit for a version: the deploy path asks it at the first file of a build, before
// the file is stored.
func admit(repo index.Repo, version string) error {
	snapshot, build := SnapshotOf(version)
	switch policy := versionPolicy(repo); {
	case build && policy != PolicySnapshot:
		return fmt.Errorf("%w: maven/%s takes release versions, and %s is a build of the SNAPSHOT version %s: "+
			"deploy SNAPSHOT versions to a repo whose policy.maven.versionPolicy is %s", format.ErrInvalid, repo.Name, version, snapshot, PolicySnapshot)
	case !build && policy == PolicySnapshot:
		return fmt.Errorf("%w: maven/%s takes the builds of SNAPSHOT versions (policy.maven.versionPolicy %s), and %s is a release version: "+
			"deploy it to a release repo", format.ErrInvalid, repo.Name, PolicySnapshot, version)
	}
	return nil
}
