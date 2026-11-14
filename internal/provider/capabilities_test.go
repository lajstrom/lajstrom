package provider

import (
	"strings"
	"testing"
)

// TestCheckName: a name deeper than MaxNameDepth is refused with the limit named; no limit, or a
// name within it, passes (#599).
func TestCheckName(t *testing.T) {
	tests := []struct {
		depth int
		name  string
		want  string // a part of the error, "" for nil
	}{
		{0, "raw/assets/a/b/c/d/e/f.txt", ""},
		{4, "maven/libs/com.example/lib", ""},
		{4, "raw/assets/ci/app.tar", ""},
		{4, "raw/assets/ci/build/app.tar", "the OCI repository raw/assets/ci/build/app.tar has 5 components, and the registry takes at most 4 below its prefix"},
		{3, "pypi/internal/requests", ""},
		{3, "npm/internal/types/node", "has 4 components, and the registry takes at most 3"},
		{1, "pypi/internal/requests", "has 3 components, and the registry takes at most 1"},
	}
	for _, tt := range tests {
		err := Capabilities{MaxNameDepth: tt.depth}.CheckName(tt.name)
		switch {
		case tt.want == "" && err != nil:
			t.Errorf("CheckName(%q) with depth %d = %v, want nil", tt.name, tt.depth, err)
		case tt.want != "" && (err == nil || !strings.Contains(err.Error(), tt.want)):
			t.Errorf("CheckName(%q) with depth %d = %v, want an error containing %q", tt.name, tt.depth, err, tt.want)
		case tt.want != "" && !strings.Contains(err.Error(), "capabilities.maxNameDepth"):
			t.Errorf("CheckName(%q) = %v: the error does not name the key", tt.name, err)
		}
	}
}

// TestCheckNameLength: a name longer than MaxNameLength is refused with its length and the
// limit, and without the instance prefix, which clients never see; no limit, or a name within
// it, passes; a name too deep is refused for its depth first (#661).
func TestCheckNameLength(t *testing.T) {
	name := func(n int) string { return "raw/assets/" + strings.Repeat("a", n-len("raw/assets/")) }
	tests := []struct {
		caps Capabilities
		name string
		want string // a part of the error, "" for nil
	}{
		{Capabilities{}, name(1000), ""},
		{Capabilities{MaxNameLength: 255}, name(255), ""},
		{Capabilities{MaxNameLength: 255}, name(256), "the OCI repository " + name(256) + " is 256 bytes long, and the registry takes at most 255"},
		{Capabilities{MaxNameLength: 246}, name(246), ""},
		{Capabilities{MaxNameLength: 246}, name(247), "is 247 bytes long, and the registry takes at most 246"},
		{Capabilities{MaxNameDepth: 3, MaxNameLength: 10}, "raw/assets/a/b", "has 4 components, and the registry takes at most 3"},
	}
	for _, tt := range tests {
		err := tt.caps.CheckName(tt.name)
		switch {
		case tt.want == "" && err != nil:
			t.Errorf("CheckName(%d bytes) with %+v = %v, want nil", len(tt.name), tt.caps, err)
		case tt.want != "" && (err == nil || !strings.Contains(err.Error(), tt.want)):
			t.Errorf("CheckName(%d bytes) with %+v = %v, want an error containing %q", len(tt.name), tt.caps, err, tt.want)
		case err != nil && tt.caps.MaxNameDepth == 0 && strings.Contains(err.Error(), "prefix"):
			t.Errorf("CheckName(%d bytes) = %v: the error names the prefix", len(tt.name), err)
		}
	}
}

// TestBuildRoute: the names Quay's rule matches, as quay/quay@77b369a refused them on 2026-11-14
// (Project Quay 3.18.2 with the commit's sources, docs/registries/quay.md, The build rule): a
// component build, not the first, followed by nothing, one component, or one and logs or status.
func TestBuildRoute(t *testing.T) {
	tests := []struct {
		name string
		want bool
	}{
		{"foo/build", true},
		{"foo/build/bar", true},
		{"foo/build/bar/logs", true},
		{"foo/build/bar/status", true},
		{"foo/bar/build", true},
		{"foo/bar/build/baz", true},
		{"foo/build/build", true},
		{"foo/build/logs", true},
		{"foo/build/status", true},
		{"a/b/c/d/build", true},
		{"pypi/internal/build", true},
		{"npm/internal/netlify/build", true},
		{"npm/internal/build/cli", true},
		{"raw/assets/build/app.tar.gz", true},
		{"raw/assets/ci/build", true},
		{"raw/build/readme.txt", true},
		{"raw/build/logs", true},
		{"maven/releases/com.example/build", true},
		{"oci/hub/paketobuildpacks/build", true},
		{"build", false},
		{"build/foo", false},
		{"build/foo/bar", false},
		{"foo/building/bar", false},
		{"foo/mybuild", false},
		{"foo/builds", false},
		{"foo/rebuild/bar", false},
		{"foo/build-x", false},
		{"foo/build.x", false},
		{"foo/build_x", false},
		{"foo/x_build", false},
		{"foo/build/bar/extra", false},
		{"foo/build/bar/logs/x", false},
		{"foo/build/bar/baz", false},
		{"foo/build/logs/x", false},
		{"raw/assets/build/linux/app.tar.gz", false},
		{"raw/assets/buil_64/app.tar.gz", false},
		{"npm/internal/netlify/buil_64", false},
		{"foo/logs", false},
		{"maven/releases/com.android.tools.build/gradle", false},
		// Components Lajstrom never writes, as Quay reads them: an empty one before build does
		// not count, an empty one after it ends no route.
		{"/build", false},
		{"a//build", false},
		{"a/build/", false},
		{"a/build//logs", false},
	}
	for _, tt := range tests {
		if got := BuildRoute(tt.name); got != tt.want {
			t.Errorf("BuildRoute(%q) = %v, want %v", tt.name, got, tt.want)
		}
	}
}

// TestCheckNew: a new name that ends in a build route is refused where the registry creates none,
// with the rule and the key named; CheckName, for a name that exists, does not apply it; the
// limits of CheckName come first (#662).
func TestCheckNew(t *testing.T) {
	quay := Capabilities{BuildRoutes: true, MaxNameLength: 243}
	tests := []struct {
		caps Capabilities
		name string
		want string // a part of CheckNew's error, "" for nil
	}{
		{quay, "pypi/internal/build", "the OCI repository pypi/internal/build ends in a build route of the registry, …/build, …/build/<x> or …/build/<x>/logs|status"},
		{quay, "raw/assets/build/app.tar.gz", "(capabilities.buildRoutes)"},
		{quay, "raw/assets/build/linux/app.tar.gz", ""},
		{quay, "pypi/internal/requests", ""},
		{Capabilities{}, "pypi/internal/build", ""},
		{Capabilities{BuildRoutes: true, MaxNameDepth: 3}, "npm/internal/netlify/build", "has 4 components, and the registry takes at most 3"},
	}
	for _, tt := range tests {
		err := tt.caps.CheckNew(tt.name)
		switch {
		case tt.want == "" && err != nil:
			t.Errorf("CheckNew(%q) with %+v = %v, want nil", tt.name, tt.caps, err)
		case tt.want != "" && (err == nil || !strings.Contains(err.Error(), tt.want)):
			t.Errorf("CheckNew(%q) with %+v = %v, want an error containing %q", tt.name, tt.caps, err, tt.want)
		}
		if tt.caps.MaxNameDepth == 0 {
			if err := tt.caps.CheckName(tt.name); err != nil {
				t.Errorf("CheckName(%q) with %+v = %v, want nil: an existing repository is exempt", tt.name, tt.caps, err)
			}
		}
	}
}
