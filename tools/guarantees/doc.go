package main

import (
	"bufio"
	"regexp"
	"strconv"
	"strings"
)

// docPath is the document the check reads, relative to the repository's root.
const docPath = "docs/spec/guarantees.md"

// The statuses of "How to read an entry".
const (
	statusHolds    = "holds"
	statusStrong   = "holds on strongly consistent registries only"
	statusPartly   = "partly"
	statusNotYet   = "not yet"
	sectionSummary = "Summary"
)

var (
	headingRE = regexp.MustCompile(`^### (INV-(\d+))\. (.+)$`)
	fieldRE   = regexp.MustCompile(`^- \*\*([^*]+)\.\*\*\s?(.*)$`)
	codeRE    = regexp.MustCompile("`([^`]+)`")
	rowRE     = regexp.MustCompile(`^\| (INV-\d+) \| ([^|]*) \| ([^|]*) \|`)
)

// entry is one guarantee of the document.
type entry struct {
	id     string            // INV-6
	num    int               // 6
	title  string            // the heading after the id
	line   int               // of the heading, from 1
	fields map[string]string // by name ("Tests", "Status"), the lines of each joined by spaces
}

// status is the first code span of the entry's Status field, "" when there is none.
func (e *entry) status() string {
	if m := codeRE.FindStringSubmatch(e.fields["Status"]); m != nil {
		return m[1]
	}
	return ""
}

// document is what the check reads of guarantees.md: the entries in their order, and the
// status column of the summary table by id.
type document struct {
	entries []*entry
	summary map[string]string
}

// parse reads the entries and the summary of the document. An entry starts at its heading
// "### INV-<n>. <title>" and ends at the next heading; a field starts with "- **<Name>.**" and
// takes the indented lines after it.
func parse(text string) *document {
	doc := &document{summary: map[string]string{}}
	var cur *entry
	field, section := "", ""
	sc := bufio.NewScanner(strings.NewReader(text))
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for n := 1; sc.Scan(); n++ {
		line := sc.Text()
		switch {
		case strings.HasPrefix(line, "## "):
			cur, field, section = nil, "", strings.TrimPrefix(line, "## ")
		case strings.HasPrefix(line, "### "):
			cur, field = nil, ""
			if m := headingRE.FindStringSubmatch(line); m != nil {
				num, _ := strconv.Atoi(m[2])
				cur = &entry{id: m[1], num: num, title: m[3], line: n, fields: map[string]string{}}
				doc.entries = append(doc.entries, cur)
			}
		case section == sectionSummary:
			if m := rowRE.FindStringSubmatch(line); m != nil {
				doc.summary[m[1]] = strings.TrimSpace(m[3])
			}
		case cur == nil:
		case fieldRE.MatchString(line):
			m := fieldRE.FindStringSubmatch(line)
			field = m[1]
			cur.fields[field] = m[2]
		case field != "" && strings.HasPrefix(line, "  "):
			cur.fields[field] += " " + strings.TrimSpace(line)
		default:
			field = ""
		}
	}
	return doc
}

// summaryStatus is the status a row of the summary table abbreviates: "strongly consistent
// only" for the long one, and without the issue in parentheses.
func summaryStatus(cell string) string {
	cell, _, _ = strings.Cut(cell, " (")
	if cell == "strongly consistent only" {
		return statusStrong
	}
	return cell
}
