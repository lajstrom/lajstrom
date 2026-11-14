package profile

import (
	"errors"
	"maps"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
)

// yankEntry is the yank of requests 2.32.0 in the repo internal, the pypi example of §8.
func yankEntry() JournalEntry {
	return JournalEntry{JournalChange: JournalChange{ArtifactTypePypiYank, JournalYank, "", "2.32.0", "breaks proxies with HTTPS_PROXY set"},
		Format: "pypi", Repo: "internal", Name: "requests", Seq: 3,
		UploadedBy: "user:019a6b1e-5c3d-7f00-8a11-2b3c4d5e6f70", Created: time.Date(2026, 11, 14, 10, 0, 0, 0, time.UTC)}
}

// TestJournalPypi checks the changes of a pypi package's journal (§8, #81): the yank of a
// version with a reason or without one, its unyank, and what is refused.
func TestJournalPypi(t *testing.T) {
	long := strings.Repeat("r", MaxJournalMessage)
	checks := []struct {
		name string
		c    JournalChange
		ok   bool
	}{
		{"yank with a reason", JournalChange{ArtifactTypePypiYank, JournalYank, "", "2.32.0", "broken"}, true},
		{"yank without a reason", JournalChange{ArtifactTypePypiYank, JournalYank, "", "2.32.0", ""}, true},
		{"yank with the longest reason", JournalChange{ArtifactTypePypiYank, JournalYank, "", "2.32.0", long}, true},
		{"unyank", JournalChange{ArtifactTypePypiYank, JournalUnyank, "", "2.32.0", ""}, true},
		{"yank without a version", JournalChange{ArtifactTypePypiYank, JournalYank, "", "", "broken"}, false},
		{"unyank with a reason", JournalChange{ArtifactTypePypiYank, JournalUnyank, "", "2.32.0", "broken"}, false},
		{"yank with a dist-tag", JournalChange{ArtifactTypePypiYank, JournalYank, "beta", "2.32.0", ""}, false},
		{"a reason too long", JournalChange{ArtifactTypePypiYank, JournalYank, "", "2.32.0", long + "r"}, false},
		{"a reason that is not UTF-8", JournalChange{ArtifactTypePypiYank, JournalYank, "", "2.32.0", "\xff"}, false},
		{"an npm op of the yank type", JournalChange{ArtifactTypePypiYank, JournalDeprecate, "", "2.32.0", "m"}, false},
		{"a yank of an npm type", JournalChange{ArtifactTypeNpmDeprecation, JournalYank, "", "1.0.0", ""}, false},
	}
	for _, tt := range checks {
		t.Run(tt.name, func(t *testing.T) {
			if err := tt.c.Check(); (err == nil) != tt.ok || (err != nil && !errors.Is(err, ErrJournal)) {
				t.Errorf("Check = %v, want ok %v", err, tt.ok)
			}
		})
	}
}

// TestBuildJournalPypi builds the yank with a reason, the yank without one and the unyank, and
// reads each back from its manifest and from the attrs the index keeps.
func TestBuildJournalPypi(t *testing.T) {
	for _, e := range []JournalEntry{yankEntry(), withChange(yankEntry(), JournalYank, ""), withChange(yankEntry(), JournalUnyank, "")} {
		m, err := BuildJournal(e)
		if err != nil {
			t.Fatalf("BuildJournal(%s %q): %v", e.Op, e.Message, err)
		}
		want := map[string]string{"lajstrom.schema": "1", "lajstrom.format": "pypi", "lajstrom.repo": "internal", "lajstrom.name": "requests",
			"lajstrom.pypi.seq": "3", "lajstrom.pypi.op": e.Op, "lajstrom.pypi.version": "2.32.0",
			"lajstrom.uploaded-by": "user:019a6b1e-5c3d-7f00-8a11-2b3c4d5e6f70", "org.opencontainers.image.created": "2026-11-14T10:00:00Z"}
		if e.Message != "" {
			want["lajstrom.pypi.reason"] = e.Message
		}
		if d := cmp.Diff(want, m.Annotations); d != "" || m.ArtifactType != ArtifactTypePypiYank {
			t.Errorf("the manifest of %s %q: artifactType %s (-want +got)\n%s", e.Op, e.Message, m.ArtifactType, d)
		}
		back, err := ParseJournal(m)
		if d := cmp.Diff(e, back); err != nil || d != "" {
			t.Errorf("ParseJournal: %v (-want +got)\n%s", err, d)
		}
		c, seq, err := JournalChangeOf(m.ArtifactType, e.Attrs())
		if err != nil || seq != 3 || c != e.JournalChange {
			t.Errorf("JournalChangeOf(the attrs) = %+v, %d, %v", c, seq, err)
		}
	}
}

// TestParseJournalPypi: what ParseJournal refuses of a yank's manifest.
func TestParseJournalPypi(t *testing.T) {
	refused := map[string]func(map[string]string){
		"lajstrom.format npm":           func(a map[string]string) { a[AnnotationFormat] = "npm" },
		"the npm keys":                  func(a map[string]string) { renameKeys(a, "lajstrom.pypi.", "lajstrom.npm.") },
		"without lajstrom.pypi.seq":     func(a map[string]string) { delete(a, AnnotationPypiSeq) },
		"a reason on an unyank":         func(a map[string]string) { a[AnnotationPypiOp] = JournalUnyank },
		"lajstrom.pypi.seq 03":          func(a map[string]string) { a[AnnotationPypiSeq] = "03" },
		"without lajstrom.pypi.version": func(a map[string]string) { delete(a, AnnotationPypiVersion) },
	}
	for name, change := range refused {
		t.Run(name, func(t *testing.T) {
			m, err := BuildJournal(yankEntry())
			if err != nil {
				t.Fatal(err)
			}
			m.Annotations = maps.Clone(m.Annotations)
			change(m.Annotations)
			if _, err := ParseJournal(m); !errors.Is(err, ErrJournal) {
				t.Errorf("ParseJournal = %v, want ErrJournal", err)
			}
		})
	}
}

// TestJournalStatePypi folds yanks and an unyank, and tells the formats that have a journal and
// the format of each journal type.
func TestJournalStatePypi(t *testing.T) {
	s := JournalState{}
	for i, c := range []JournalChange{
		{ArtifactTypePypiYank, JournalYank, "", "1.0", "broken"},
		{ArtifactTypePypiYank, JournalYank, "", "1.1", ""},
		{ArtifactTypePypiYank, JournalYank, "", "1.0", "really broken"},
		{ArtifactTypePypiYank, JournalUnyank, "", "1.1", ""},
	} {
		s.Apply(int64(i+1), c)
	}
	if d := cmp.Diff(JournalState{{JournalKindYanked, "1.0"}: {"really broken", 3}}, s); d != "" {
		t.Errorf("the fold (-want +got)\n%s", d)
	}

	for f, want := range map[string]bool{"npm": true, "pypi": true, "raw": false, "maven": false, "": false} {
		if HasJournal(f) != want {
			t.Errorf("HasJournal(%q) = %v", f, !want)
		}
	}
	for at, want := range map[string]string{ArtifactTypePypiYank: "pypi", ArtifactTypeNpmDistTags: "npm", ArtifactTypeNpmDeprecation: "npm", ArtifactType("pypi"): ""} {
		if f, ok := JournalFormat(at); f != want || ok != (want != "") {
			t.Errorf("JournalFormat(%s) = %q, %v", at, f, ok)
		}
	}
}

// withChange is e with another op and reason.
func withChange(e JournalEntry, op, reason string) JournalEntry {
	e.Op, e.Message = op, reason
	return e
}

// renameKeys moves the keys of a that start with from to the same name after to.
func renameKeys(a map[string]string, from, to string) {
	for k, v := range maps.Clone(a) {
		if rest, ok := strings.CutPrefix(k, from); ok {
			delete(a, k)
			a[to+rest] = v
		}
	}
}
