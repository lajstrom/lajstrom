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
