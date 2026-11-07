package deb

import (
	"fmt"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/lajstrom/lajstrom/internal/format"
)

// Field names the parser looks for; the match ignores case (Debian Policy §5.1).
const (
	FieldPackage      = "Package"
	FieldVersion      = "Version"
	FieldArchitecture = "Architecture"
	FieldMaintainer   = "Maintainer"
	FieldDescription  = "Description"
	FieldSource       = "Source"
)

var (
	// packageName is the grammar of Package and of a source name (Debian Policy §5.6.1, §5.6.7).
	packageName = regexp.MustCompile(`^[a-z0-9][a-z0-9+.-]+$`)
	// versionGrammar is [epoch:]upstream_version[-debian_revision] (Debian Policy §5.6.12)
	// without the rule on "-", which checkVersion adds.
	versionGrammar = regexp.MustCompile(`^(\d+:)?[0-9][A-Za-z0-9.+~-]*$`)
	// architecture is one Debian architecture name: amd64, arm64, all, … (Debian Policy §5.6.8).
	architecture = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)
)

// Field is one field of a control file: the name as written and the value as written. A
// value of more than one line has its continuation lines after "\n", each with the space or tab
// it starts with, so " ." is an empty line of a Description. Whitespace around the first line
// and at the end of every line is not part of the value.
type Field struct {
	Name  string
	Value string
}

// Control is the control file of a binary package: one paragraph of fields in the order of the
// file (Debian Policy §5.1, §5.3).
type Control struct {
	// Fields are the fields in the order of the file, names as written.
	Fields []Field
	// index maps a lowercase field name to its position in Fields.
	index map[string]int
}

// Get returns the value of the field name, matched without regard to case.
func (c *Control) Get(name string) (string, bool) {
	i, ok := c.index[strings.ToLower(name)]
	if !ok {
		return "", false
	}
	return c.Fields[i].Value, true
}

// Map returns the fields as a map from the name as written to the value: the control object
// of the metadata layer (docs/spec/manifest-profile.md §2).
func (c *Control) Map() map[string]string {
	m := make(map[string]string, len(c.Fields))
	for _, f := range c.Fields {
		m[f.Name] = f.Value
	}
	return m
}

// parseControl reads a control file (Debian Policy §5.1): "Field: value" lines in any order,
// continuation lines that start with a space or a tab, one paragraph. Blank lines (empty, or
// spaces and tabs only) may come before and after it. Field names are US-ASCII without
// control characters, space and ":", and start with neither "#" nor "-"; a name given twice,
// in any case, is refused. The file must be UTF-8 without control characters other than tab:
// it ends up in JSON and in apt's Packages. Errors wrap format.ErrInvalid and name the line.
func parseControl(b []byte) (Control, error) {
	if !utf8.Valid(b) {
		return Control{}, fmt.Errorf("%w: control is not UTF-8", format.ErrInvalid)
	}
	c := Control{index: map[string]int{}}
	ended := 0 // the blank line that ended the paragraph; 0 while it goes on
	for i, line := range strings.Split(strings.TrimSuffix(string(b), "\n"), "\n") {
		n := i + 1
		if j := strings.IndexFunc(line, isControlChar); j >= 0 {
			return Control{}, fmt.Errorf("%w: control line %d has the control character %#02x", format.ErrInvalid, n, line[j])
		}
		trimmed := strings.TrimRight(line, " \t")
		switch {
		case trimmed == "":
			if len(c.Fields) > 0 && ended == 0 {
				ended = n
			}
		case ended > 0:
			return Control{}, fmt.Errorf("%w: control has a second paragraph at line %d, after the blank line %d; a binary package has one",
				format.ErrInvalid, n, ended)
		case trimmed[0] == ' ' || trimmed[0] == '\t':
			if len(c.Fields) == 0 {
				return Control{}, fmt.Errorf("%w: control line %d is a continuation line with no field before it", format.ErrInvalid, n)
			}
			c.Fields[len(c.Fields)-1].Value += "\n" + trimmed
		default:
			if err := c.add(n, trimmed); err != nil {
				return Control{}, err
			}
		}
	}
	if len(c.Fields) == 0 {
		return Control{}, fmt.Errorf("%w: control has no fields", format.ErrInvalid)
	}
	return c, nil
}

// add appends the field of line n, "Name: value".
func (c *Control) add(n int, line string) error {
	name, value, ok := strings.Cut(line, ":")
	if !ok {
		return fmt.Errorf("%w: control line %d is neither a field nor a continuation line: %q", format.ErrInvalid, n, line)
	}
	if !isFieldName(name) {
		return fmt.Errorf("%w: control line %d: %q is not a field name", format.ErrInvalid, n, name)
	}
	key := strings.ToLower(name)
	if j, dup := c.index[key]; dup {
		return fmt.Errorf("%w: control line %d: field %s is given twice, first as %s", format.ErrInvalid, n, name, c.Fields[j].Name)
	}
	c.index[key] = len(c.Fields)
	c.Fields = append(c.Fields, Field{Name: name, Value: strings.TrimLeft(value, " \t")})
	return nil
}

// isControlChar reports whether r is a control character other than tab.
func isControlChar(r rune) bool { return (r < 0x20 && r != '\t') || r == 0x7f }

// isFieldName reports whether s is a field name: one or more of the US-ASCII characters "!"
// to "~" except ":", not starting with "#" or "-" (Debian Policy §5.1).
func isFieldName(s string) bool {
	if s == "" || s[0] == '#' || s[0] == '-' {
		return false
	}
	for i := range len(s) {
		if s[i] < '!' || s[i] > '~' {
			return false
		}
	}
	return true
}

// checkName checks the grammar of a package or source name. Errors wrap format.ErrInvalid.
func checkName(what, s string) error {
	if !packageName.MatchString(s) {
		return fmt.Errorf("%w: %s %q is not a package name: lowercase letters, digits, \"+\", \"-\" and \".\", "+
			"at least two, the first a letter or a digit", format.ErrInvalid, what, s)
	}
	return nil
}

// checkVersion checks the grammar of a version, [epoch:]upstream_version[-debian_revision]:
// the epoch digits, the upstream version starting with a digit, ":" only after the epoch, and
// "-" only when a revision follows the last one. Errors wrap format.ErrInvalid.
func checkVersion(what, s string) error {
	if !versionGrammar.MatchString(s) {
		return fmt.Errorf("%w: %s %q is not a Debian version, [epoch:]upstream_version[-debian_revision]: "+
			"the epoch digits, the upstream version a digit and then letters, digits, \".\", \"+\", \"~\" and \"-\"",
			format.ErrInvalid, what, s)
	}
	if strings.HasSuffix(s, "-") {
		return fmt.Errorf("%w: %s %q ends with \"-\": a \"-\" is allowed only when a revision follows the last one",
			format.ErrInvalid, what, s)
	}
	return nil
}

// checkArchitecture checks the grammar of an architecture name. Errors wrap format.ErrInvalid.
func checkArchitecture(what, s string) error {
	if !architecture.MatchString(s) {
		return fmt.Errorf("%w: %s %q is not an architecture name: lowercase letters, digits and \"-\", the first not \"-\"",
			format.ErrInvalid, what, s)
	}
	return nil
}

// SplitSource splits the value of a Source field, "name" or "name (version)" (Debian Policy
// §5.6.1), into the source name and the source version, which is empty when the field gives
// none (the source version is then the binary's). Both are checked against their grammars;
// errors wrap format.ErrInvalid.
func SplitSource(field string) (name, version string, err error) {
	name, rest, hasVersion := strings.Cut(field, " ")
	if err := checkName(FieldSource, name); err != nil {
		return "", "", err
	}
	if !hasVersion {
		return name, "", nil
	}
	v, open := strings.CutPrefix(strings.TrimLeft(rest, " "), "(")
	v, closed := strings.CutSuffix(v, ")")
	if !open || !closed || v == "" {
		return "", "", fmt.Errorf("%w: Source %q is not \"name\" or \"name (version)\"", format.ErrInvalid, field)
	}
	if err := checkVersion("the version of Source", v); err != nil {
		return "", "", err
	}
	return name, v, nil
}

// Filename returns the file name of a binary package, <name>_<version>_<arch>.deb with the
// epoch left out of the version: what dpkg-deb --build names a package, what a Debian pool
// holds and what apt's Packages points to. apt writes the epoch as "%3a" only in the names of
// its local cache (/var/cache/apt/archives); that is the "%3a" the plan of #39 mentions, and no
// pool file carries it.
func Filename(name, version, arch string) string {
	if _, v, ok := strings.Cut(version, ":"); ok {
		version = v
	}
	return name + "_" + version + "_" + arch + ".deb"
}

// PoolPath returns the path of a package file below pool/<component>/ of an apt repository, as
// the Debian archive lays it out: <p>/<source>/<filename>, where <p> is the first letter of the
// source name, or its first four letters when it starts with "lib" (libc6 of glibc is in
// g/glibc/, libssl3 of openssl in o/openssl/, libfoo2 of libfoo in libf/libfoo/). source must
// not be empty. #40 writes the path into the Filename field of Packages, #42 serves it.
func PoolPath(source, filename string) string {
	p := source[:1]
	if strings.HasPrefix(source, "lib") {
		p = source[:min(4, len(source))]
	}
	return p + "/" + source + "/" + filename
}
