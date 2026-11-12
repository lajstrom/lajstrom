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
