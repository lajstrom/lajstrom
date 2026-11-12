package npm

import (
	"errors"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/lajstrom/lajstrom/internal/format"
	"github.com/lajstrom/lajstrom/internal/oci/profile"
)

// TestAdoptOps: a converted proxy gets an add per dist-tag whose version it holds and a
// deprecate per held version with a message, in a fixed order; a dist-tag of a version not held,
// a target that is not a version, and an op the journal refuses are skipped and said; a body
// that is not a packument of the name is ErrInvalid (docs/spec/proxy-and-group.md §2.6).
func TestAdoptOps(t *testing.T) {
	if got := (Plugin{}).AdoptKind(); got != kindPackument {
		t.Errorf("AdoptKind = %q, want %q", got, kindPackument)
	}
	kept := `{"name": "@acme/w", "dist-tags": {"next": "2.0.0-rc.1", "latest": "1.0.0", "beta": "3.0.0-beta.1",
		"1.x": "1.1.0", "odd": 7},
		"versions": {
			"1.0.0": {"deprecated": "use 1.1.0"},
			"1.1.0": {"deprecated": false},
			"2.0.0-rc.1": {},
			"0.9.0": {"deprecated": "` + strings.Repeat("x", profile.MaxJournalMessage+1) + `"},
			"3.0.0-beta.1": {"deprecated": "not held"}}}`
	ops, skipped, err := Plugin{}.AdoptOps("@acme/w", []byte(kept), []string{"2.0.0-rc.1", "1.1.0", "1.0.0", "0.9.0"})
	must(t, err)
	dt, dep := profile.ArtifactTypeNpmDistTags, profile.ArtifactTypeNpmDeprecation
	want := []format.JournalOp{
		{ArtifactType: dt, Op: profile.JournalAdd, Tag: "latest", Version: "1.0.0"},
		{ArtifactType: dt, Op: profile.JournalAdd, Tag: "next", Version: "2.0.0-rc.1"},
		{ArtifactType: dep, Op: profile.JournalDeprecate, Version: "1.0.0", Message: "use 1.1.0"},
	}
	if d := cmp.Diff(want, ops); d != "" {
		t.Errorf("ops (-want +got)\n%s", d)
	}
	for i, prefix := range []string{"dist-tag 1.x: ", "dist-tag beta: 3.0.0-beta.1 is not held", "dist-tag odd: not a version",
		"the deprecation of 0.9.0: "} {
		if i >= len(skipped) || !strings.HasPrefix(skipped[i], prefix) {
			t.Errorf("skipped %q, want %d: %q…", skipped, i, prefix)
		}
	}
	if len(skipped) != 4 {
		t.Errorf("skipped %q, want 4", skipped)
	}

	if ops, skipped, err := (Plugin{}).AdoptOps("@acme/w", []byte(`{"name": "@acme/w"}`), []string{"1.0.0"}); err != nil ||
		len(ops) != 0 || len(skipped) != 0 {
		t.Errorf("a packument without dist-tags: %v, %v, %v; want nothing", ops, skipped, err)
	}
	for _, body := range []string{`[]`, `{`, `{"name": "other"}`} {
		if _, _, err := (Plugin{}).AdoptOps("@acme/w", []byte(body), nil); !errors.Is(err, format.ErrInvalid) {
			t.Errorf("AdoptOps(%s) = %v, want ErrInvalid", body, err)
		}
	}
}
