package profile

import (
	"regexp"
	"strings"
	"testing"

	"github.com/opencontainers/go-digest"
)

// TestKeepTag: the keep-tag of a digest is a valid tag of at most 128 characters, of its own
// form, clear of the referrers tag schema (docs/spec/proxy-and-group.md §11.6).
func TestKeepTag(t *testing.T) {
	grammar := regexp.MustCompile(`^[a-zA-Z0-9_][a-zA-Z0-9._-]{0,127}$`)
	d256 := digest.FromString("manifest")
	if got, want := KeepTag(d256), "_keep.sha256."+d256.Encoded(); got != want {
		t.Errorf("KeepTag = %q, want %q", got, want)
	}
	for _, d := range []digest.Digest{d256, digest.SHA512.FromString("manifest")} {
		tag := KeepTag(d)
		if !grammar.MatchString(tag) || !IsKeepTag(tag) {
			t.Errorf("KeepTag(%s) = %q: not a tag, or not a keep-tag", d, tag)
		}
		if _, _, cosign := CosignTag(tag); cosign || strings.HasPrefix(tag, "sha256-") {
			t.Errorf("KeepTag(%s) = %q has the referrers schema's form", d, tag)
		}
	}
	for _, tag := range []string{"latest", "1.27", "sha256-" + d256.Encoded(), "keep.sha256.x", "_j1"} {
		if IsKeepTag(tag) {
			t.Errorf("IsKeepTag(%q) = true", tag)
		}
	}
}
