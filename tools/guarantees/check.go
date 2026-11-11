package main

import (
	"fmt"
	"io/fs"
	"regexp"
	"slices"
	"strings"
)

// awaitingIssue are the entries whose status is not holds and names no issue yet, because the
// issue is proposed and not opened (each says "new issue"); empty while every one is filed. An
// entry leaves the list in the pull request that links its issue; the check fails while one is
// listed and links one.
var awaitingIssue = map[string]bool{}

var (
	scriptRE  = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*\.sh$`)
	stepRE    = regexp.MustCompile(`\b[A-Z]-\d{2}\b`)
	issueRE   = regexp.MustCompile(`#\d+`)
	casesRE   = regexp.MustCompile("conformance cases? ((?:`[A-Za-z]+`(?:, and |, | and )?)+)")
	inParenRE = regexp.MustCompile(`^ \(([^()]*)\)`)
	ofRE      = regexp.MustCompile(`^ of ([^,;()]+)`)
	inRE      = regexp.MustCompile("^ in `([^`]+)`")
	itemSepRE = regexp.MustCompile(`, and |, | and `)
)

// refs is what the Tests field of an entry names.
type refs struct {
	goTests []goRef
	cases   []string
	scripts []string
	steps   []stepRef
}

// goRef is a Go test and the packages the text names for it: "`TestUpload` of deb and pypi",
// "`TestAuthorization` (facade, api, raw)". Words that are no package with tests are not packages.
type goRef struct {
	name string
	pkgs []string
}

// stepRef is an acceptance id the text says a script covers: "R-02 in `raw.sh`", or after the
// script, "`ha.sh` H-02, H-03". An id after "acceptance.md" is a reference, not a step.
type stepRef struct {
	id, script string
}

func (r refs) empty() bool {
	return len(r.goTests) == 0 && len(r.cases) == 0 && len(r.scripts) == 0
}

// parseTests reads the names of a Tests field.
func parseTests(text string, t *tree) refs {
	var r refs
	for _, m := range casesRE.FindAllStringSubmatch(text, -1) {
		for _, c := range codeRE.FindAllStringSubmatch(m[1], -1) {
			if isTestName(c[1]) {
				break
			}
			r.cases = append(r.cases, c[1])
		}
	}
	spans := codeRE.FindAllStringSubmatchIndex(text, -1)
	for _, s := range spans {
		name := text[s[2]:s[3]]
		switch {
		case isTestName(name):
			r.goTests = append(r.goTests, goRef{name: name, pkgs: qualifiers(text[s[1]:], t)})
		case scriptRE.MatchString(name) && !slices.Contains(r.scripts, name):
			r.scripts = append(r.scripts, name)
		}
	}
	for _, m := range stepRE.FindAllStringIndex(text, -1) {
		if script := stepScript(text, m, spans); script != "" {
			r.steps = append(r.steps, stepRef{id: text[m[0]:m[1]], script: script})
		}
	}
	return r
}

// qualifiers returns the packages that rest, the text after a Go test's name, names for it.
func qualifiers(rest string, t *tree) []string {
	m := inParenRE.FindStringSubmatch(rest)
	if m == nil {
		m = ofRE.FindStringSubmatch(rest)
	}
	if m == nil {
		return nil
	}
	var pkgs []string
	for _, item := range itemSepRE.Split(m[1], -1) {
		item = strings.Trim(item, " `")
		if t.packages[item] {
			pkgs = append(pkgs, item)
		}
	}
	return pkgs
}

// stepScript returns the script that the step id at m belongs to, or "".
func stepScript(text string, m []int, spans [][]int) string {
	if in := inRE.FindStringSubmatch(text[m[1]:]); in != nil && scriptRE.MatchString(in[1]) {
		return in[1]
	}
	if strings.HasSuffix(text[:m[0]], "acceptance.md ") {
		return ""
	}
	last := -1
	for i, s := range spans {
		if s[1] <= m[0] {
			last = i
		}
	}
	if last < 0 {
		return ""
	}
	if name := text[spans[last][2]:spans[last][3]]; scriptRE.MatchString(name) {
		return name
	}
	return ""
}

// problem is one thing the check found, with the id of its guarantee ("" for the document).
type problem struct {
	id, msg string
}

func (p problem) String() string {
	if p.id == "" {
		return docPath + ": " + p.msg
	}
	return p.id + ": " + p.msg
}

// checkRoot checks the document of the repository at fsys.
func checkRoot(fsys fs.FS) (*document, *tree, []problem, error) {
	b, err := fs.ReadFile(fsys, docPath)
	if err != nil {
		return nil, nil, nil, err
	}
	t, err := load(fsys)
	if err != nil {
		return nil, nil, nil, err
	}
	doc := parse(string(b))
	return doc, t, check(doc, t), nil
}

// check returns what is wrong with doc against t: per entry, what checkEntry finds; for the
// document, ids that do not run from INV-1 without a gap and a summary that disagrees.
func check(doc *document, t *tree) []problem {
	var probs []problem
	if len(doc.entries) == 0 {
		return []problem{{"", "no guarantee found"}}
	}
	for i, e := range doc.entries {
		if e.num != i+1 {
			probs = append(probs, problem{e.id, fmt.Sprintf("line %d: the ids run from INV-1 without a gap: want INV-%d", e.line, i+1)})
		}
		for _, msg := range checkEntry(e, t) {
			probs = append(probs, problem{e.id, msg})
		}
		switch s, ok := doc.summary[e.id]; {
		case !ok:
			probs = append(probs, problem{e.id, "no row in the summary"})
		case summaryStatus(s) != e.status():
			probs = append(probs, problem{e.id, fmt.Sprintf("the summary says %q, the entry %q", s, e.status())})
		}
	}
	for id := range awaitingIssue {
		if !slices.ContainsFunc(doc.entries, func(e *entry) bool { return e.id == id }) {
			probs = append(probs, problem{id, "listed in awaitingIssue, and the document has no such guarantee"})
		}
	}
	return probs
}

// checkEntry checks one guarantee: its status (checkStatus), and that every Go test,
// conformance case, script and step it names exists, and no Go test it names is skipped
// unconditionally.
func checkEntry(e *entry, t *tree) []string {
	r := parseTests(e.fields["Tests"], t)
	probs := checkStatus(e, r)
	for _, g := range r.goTests {
		if msg := resolve(g, t); msg != "" {
			probs = append(probs, msg)
		}
	}
	for _, c := range r.cases {
		if !t.cases[c] {
			probs = append(probs, fmt.Sprintf("no store conformance case %s in %s", c, storetestDir))
		}
	}
	for _, s := range r.scripts {
		if _, ok := t.scripts[s]; !ok {
			probs = append(probs, fmt.Sprintf("no end-to-end script %s/%s", e2eDir, s))
		}
	}
	for _, s := range r.steps {
		if text, ok := t.scripts[s.script]; ok && !strings.Contains(text, s.id) {
			probs = append(probs, fmt.Sprintf("%s/%s has no step %s", e2eDir, s.script, s.id))
		}
	}
	return probs
}

// checkStatus checks that the status is known, that holds names a test, and that any other
// names an issue unless the entry is awaitingIssue.
func checkStatus(e *entry, r refs) []string {
	status := e.status()
	switch status {
	case statusHolds:
		if r.empty() {
			return []string{fmt.Sprintf("status %s, and Tests names no test", status)}
		}
	case statusStrong, statusPartly, statusNotYet:
		named := issueRE.MatchString(e.fields["Status"])
		switch {
		case !named && !awaitingIssue[e.id]:
			return []string{fmt.Sprintf("status %s names no issue", status)}
		case named && awaitingIssue[e.id]:
			return []string{fmt.Sprintf("the status names an issue now: remove %s from awaitingIssue in tools/guarantees", e.id)}
		}
	default:
		return []string{fmt.Sprintf("unknown status %q", status)}
	}
	return nil
}

// resolve returns what is wrong with a Go test the document names, or "".
func resolve(g goRef, t *tree) string {
	found := t.tests[g.name]
	if len(found) == 0 {
		return fmt.Sprintf("no Go test %s", g.name)
	}
	if len(g.pkgs) > 0 {
		var in []goTest
		for _, p := range g.pkgs {
			n := len(in)
			for _, f := range found {
				if f.pkg == p || f.dir == p {
					in = append(in, f)
				}
			}
			if len(in) == n {
				return fmt.Sprintf("no Go test %s in package %s", g.name, p)
			}
		}
		found = in
	}
	if !slices.ContainsFunc(found, func(f goTest) bool { return !f.skipped }) {
		return fmt.Sprintf("%s is skipped unconditionally", g.name)
	}
	return ""
}
