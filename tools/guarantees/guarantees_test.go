package main

import (
	"bytes"
	"os"
	"slices"
	"strings"
	"testing"
	"testing/fstest"
)

// TestDocument is the check of make test and CI: docs/spec/guarantees.md against this tree.
// Renaming or deleting a test that a guarantee names fails here, with the guarantee's id.
func TestDocument(t *testing.T) {
	_, _, probs, err := checkRoot(os.DirFS("../.."))
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range probs {
		t.Error(p)
	}
}

// The fixture tree: one Go test of each kind, a conformance suite and a script.
const (
	fixtureTests = `package raw_test

import "testing"

func TestPut(t *testing.T) {}

func TestSkipped(t *testing.T) {
	t.Skip("never runs")
}

func TestSometimes(t *testing.T) {
	if testing.Short() {
		t.Skip("short")
	}
}

func FuzzParse(f *testing.F) {}

func Testify(t *testing.T) {}

func helper(t *testing.T) {}
`
	fixtureIngest = `//go:build integration

package ingest

import "testing"

func TestPut(t *testing.T) {}
`
	fixtureStoretest = `package storetest

func Run() {
	tests := []struct {
		name string
		run  func()
	}{
		{"Sweep", testSweep},
		{"TagClaims", testTagClaims},
	}
	_ = tests
}

func testSweep()     {}
func testTagClaims() {}
`
	fixtureScript = "#!/usr/bin/env bash\n# R-01, then R-07.\nok \"R-01 a PUT\"\n"
)

// fixture returns the tree with a document of the entries given and a matching summary.
func fixture(entries ...string) fstest.MapFS {
	var doc, summary strings.Builder
	doc.WriteString("# Spec: guarantees\n\n## Storage\n\n")
	summary.WriteString("## Summary\n\n| Id | Guarantee | Status | Markers |\n| --- | --- | --- | --- |\n")
	for _, e := range entries {
		doc.WriteString(e + "\n")
		id, _, _ := strings.Cut(strings.TrimPrefix(e, "### "), ".")
		status := codeRE.FindStringSubmatch(e[strings.Index(e, "**Status.**"):])[1]
		if status == statusStrong {
			status = "strongly consistent only"
		}
		summary.WriteString("| " + id + " | a guarantee | " + status + " | |\n")
	}
	doc.WriteString(summary.String() + "\n## Not guaranteed\n")
	return fstest.MapFS{
		docPath:                                    {Data: []byte(doc.String())},
		"internal/format/raw/raw_test.go":          {Data: []byte(fixtureTests)},
		"internal/ingest/ingest_test.go":           {Data: []byte(fixtureIngest)},
		"internal/index/storetest/storetest.go":    {Data: []byte(fixtureStoretest)},
		"test/e2e/raw.sh":                          {Data: []byte(fixtureScript)},
		".claude/worktrees/x/internal/a_test.go":   {Data: []byte("package a\n\nimport \"testing\"\n\nfunc TestHidden(t *testing.T) {}\n")},
		"internal/format/raw/testdata/x_test.go":   {Data: []byte("not Go")},
		"internal/format/raw/node_modules/y.sh":    {Data: []byte("")},
		"internal/format/raw/other_test_helper.go": {Data: []byte("package raw\n")},
	}
}

// guarantee is an entry with the Tests and Status fields given.
func guarantee(id, tests, status string) string {
	return "### " + id + ". A guarantee\n\n- **Statement.** Something holds.\n- **Tests.** " + tests +
		"\n- **When violated.** Not observed.\n- **Status.** " + status + "\n"
}

func TestCheck(t *testing.T) {
	tests := []struct {
		name    string
		entries []string
		want    []string
	}{
		{"every kind named", []string{guarantee("INV-1",
			"`TestPut` (raw), `TestPut` of `internal/ingest`, `FuzzParse`, the store conformance cases\n  `Sweep` and `TagClaims`; R-01 in `raw.sh`, `raw.sh` (R-07), acceptance.md X-14.",
			"`holds`.")}, nil},
		{"a test renamed", []string{guarantee("INV-1", "`TestPut`, `TestGone`.", "`holds`.")},
			[]string{"INV-1: no Go test TestGone"}},
		{"in another package", []string{guarantee("INV-1", "`TestPut` (ingest, raw), `FuzzParse` (ingest).", "`holds`.")},
			[]string{"INV-1: no Go test FuzzParse in package ingest"}},
		{"words that are no package", []string{guarantee("INV-1", "`TestPut` (1 GiB up and down, heap under 64 MiB).", "`holds`.")}, nil},
		{"a test that is no test", []string{guarantee("INV-1", "`Testify`, `TestHidden`.", "`holds`.")},
			[]string{"INV-1: no Go test TestHidden"}},
		{"skipped always", []string{guarantee("INV-1", "`TestSkipped`, `TestSometimes`.", "`holds`.")},
			[]string{"INV-1: TestSkipped is skipped unconditionally"}},
		{"holds without a test", []string{guarantee("INV-1", "None.", "`holds`.")},
			[]string{"INV-1: status holds, and Tests names no test"}},
		{"not yet without an issue", []string{guarantee("INV-1", "None.", "`not yet`.")},
			[]string{"INV-1: status not yet names no issue"}},
		{"partly with an issue", []string{guarantee("INV-1", "None.", "`partly` ([#246](https://github.com/lajstrom/lajstrom/issues/246)).")}, nil},
		{"strongly consistent without an issue", []string{guarantee("INV-1", "`TestPut`.", "`"+statusStrong+"`.")},
			[]string{"INV-1: status " + statusStrong + " names no issue"}},
		{"an unknown status", []string{guarantee("INV-1", "`TestPut`.", "`fine`.")},
			[]string{"INV-1: unknown status \"fine\""}},
		{"a case, a script and a step missing", []string{guarantee("INV-1",
			"the store conformance case `Missing`; `npm.sh`, R-02 in `raw.sh`, `raw.sh` H-05.", "`holds`.")},
			[]string{"INV-1: no store conformance case Missing in internal/index/storetest",
				"INV-1: no end-to-end script test/e2e/npm.sh", "INV-1: test/e2e/raw.sh has no step R-02",
				"INV-1: test/e2e/raw.sh has no step H-05"}},
		{"a gap in the ids", []string{guarantee("INV-1", "`TestPut`.", "`holds`."), guarantee("INV-3", "`TestPut`.", "`holds`.")},
			[]string{"INV-3: line 12: the ids run from INV-1 without a gap: want INV-2"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, _, probs, err := checkRoot(fixture(tt.entries...))
			if err != nil {
				t.Fatal(err)
			}
			got := problems(probs)
			if !slices.Equal(got, tt.want) {
				t.Errorf("problems:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(tt.want, "\n"))
			}
		})
	}
}

// problems returns probs as lines, without those about awaitingIssue, whose ids the fixtures
// do not have.
func problems(probs []problem) []string {
	var lines []string
	for _, p := range probs {
		if !strings.Contains(p.msg, "awaitingIssue") {
			lines = append(lines, p.String())
		}
	}
	return lines
}

// TestSummary: an entry without a row, and a row whose status is not the entry's.
func TestSummary(t *testing.T) {
	fsys := fixture(guarantee("INV-1", "`TestPut`.", "`holds`."), guarantee("INV-2", "`TestPut`.", "`partly` (#1)."))
	doc := string(fsys[docPath].Data)
	doc = strings.Replace(doc, "| INV-1 | a guarantee | holds | |\n", "", 1)
	doc = strings.Replace(doc, "| INV-2 | a guarantee | partly | |", "| INV-2 | a guarantee | not yet (#1) | |", 1)
	fsys[docPath] = &fstest.MapFile{Data: []byte(doc)}
	_, _, probs, err := checkRoot(fsys)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"INV-1: no row in the summary", `INV-2: the summary says "not yet (#1)", the entry "partly"`}
	if got := problems(probs); !slices.Equal(got, want) {
		t.Errorf("problems:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

// TestAwaitingIssue: an entry of the list that links an issue now is a problem, and so is an
// id of the list the document does not have.
func TestAwaitingIssue(t *testing.T) {
	old := awaitingIssue
	t.Cleanup(func() { awaitingIssue = old })
	awaitingIssue = map[string]bool{"INV-1": true, "INV-2": true, "INV-9": true}
	_, _, probs, err := checkRoot(fixture(guarantee("INV-1", "None.", "`not yet` (new issue)."),
		guarantee("INV-2", "None.", "`not yet` (#7).")))
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"INV-2: the status names an issue now: remove INV-2 from awaitingIssue in tools/guarantees",
		"INV-9: listed in awaitingIssue, and the document has no such guarantee"}
	var got []string
	for _, p := range probs {
		got = append(got, p.String())
	}
	if !slices.Equal(got, want) {
		t.Errorf("problems:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

// TestRun: the exit statuses, the problems on standard error and the report on standard output.
func TestRun(t *testing.T) {
	dir := t.TempDir()
	fsys := fixture(guarantee("INV-1", "`TestPut` (raw); R-01 in `raw.sh`, `raw.sh` R-07.", "`holds`."),
		guarantee("INV-2", "`TestGone`.", "`holds`."))
	for name, f := range fsys {
		p := dir + "/" + name
		if err := os.MkdirAll(p[:strings.LastIndex(p, "/")], 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, f.Data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	var stdout, stderr bytes.Buffer
	if code := run([]string{"-root", dir, "-report"}, &stdout, &stderr); code != 1 {
		t.Errorf("exit status %d, want 1", code)
	}
	if !strings.Contains(stdout.String(), "| INV-1 | A guarantee | holds | 1 | 0 | raw.sh (R-01, R-07) |\n") {
		t.Errorf("report:\n%s", stdout.String())
	}
	if !strings.HasPrefix(stderr.String(), "INV-2: no Go test TestGone\n") {
		t.Errorf("stderr:\n%s", stderr.String())
	}
	if code := run([]string{"-root", t.TempDir()}, &stdout, &stderr); code != 2 {
		t.Errorf("exit status %d without a document, want 2", code)
	}
	if code := run([]string{"-nope"}, &stdout, &stderr); code != 2 {
		t.Errorf("exit status %d for an unknown flag, want 2", code)
	}
}
