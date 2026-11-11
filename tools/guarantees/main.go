// Command guarantees checks docs/spec/guarantees.md against the tree (#447): every guarantee
// whose status is holds names a test, every other status names an issue, and every test an
// entry names exists and is not skipped unconditionally: a Go test function (in the package the
// entry names, when it names one), a case of the store conformance suite, an end-to-end script
// of test/e2e and the acceptance steps the entry says that script covers. Each problem is one
// line that starts with the guarantee's id.
//
// make test runs the check (TestDocument); make guarantees prints the report of what each
// guarantee proves, for the release notes:
//
//	go run ./tools/guarantees [-root DIR] [-report]
package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// run is the command: 0 when the document checks, 1 when it does not, 2 on a usage error or
// one of reading.
func run(args []string, stdout, stderr io.Writer) int {
	fl := flag.NewFlagSet("guarantees", flag.ContinueOnError)
	fl.SetOutput(stderr)
	root := fl.String("root", ".", "the repository's root")
	report := fl.Bool("report", false, "print the report of every guarantee as a Markdown table")
	if err := fl.Parse(args); err != nil {
		return 2
	}
	doc, t, probs, err := checkRoot(os.DirFS(*root))
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "guarantees:", err)
		return 2
	}
	if *report {
		writeReport(stdout, doc, t)
	}
	for _, p := range probs {
		_, _ = fmt.Fprintln(stderr, p)
	}
	if len(probs) > 0 {
		_, _ = fmt.Fprintf(stderr, "guarantees: %d problems in %s\n", len(probs), docPath)
		return 1
	}
	return 0
}

// writeReport writes one row per guarantee: its id, title and status, how many Go tests and
// conformance cases it names, and its end-to-end scripts with their steps.
func writeReport(w io.Writer, doc *document, t *tree) {
	_, _ = fmt.Fprintln(w, "| Id | Guarantee | Status | Go tests | Conformance cases | End-to-end |")
	_, _ = fmt.Fprintln(w, "| --- | --- | --- | --- | --- | --- |")
	for _, e := range doc.entries {
		r := parseTests(e.fields["Tests"], t)
		_, _ = fmt.Fprintf(w, "| %s | %s | %s | %d | %d | %s |\n", e.id, e.title, e.status(), len(r.goTests),
			len(r.cases), e2eCell(r))
	}
}

// e2eCell is "ha.sh (H-02, H-03), oras.sh", or "" without a script.
func e2eCell(r refs) string {
	var cells []string
	for _, s := range r.scripts {
		var ids []string
		for _, st := range r.steps {
			if st.script == s {
				ids = append(ids, st.id)
			}
		}
		if len(ids) > 0 {
			s += " (" + strings.Join(ids, ", ") + ")"
		}
		cells = append(cells, s)
	}
	return strings.Join(cells, ", ")
}
