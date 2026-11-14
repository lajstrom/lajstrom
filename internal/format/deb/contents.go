package deb

import (
	"bytes"
	"maps"
	"slices"
	"strings"
)

// The Contents indexes of a suite (docs/spec/http-routing.md §3, apt; #79): for every component
// and architecture that Packages has, a line per path that a package of the component holds, the
// path and the names of the packages that hold it, each qualified with its Section. apt-file
// reads them through apt's index target $(COMPONENT)/Contents-$(ARCHITECTURE).

// contentsFile is the prefix of the name of a Contents index below <component>/:
// Contents-<arch>.
const contentsFile = "Contents-"

// contentsColumn is the column the package names of a Contents line start at, at the least,
// as the Debian archive pads them.
const contentsColumn = 60

// contentsVariants are the files of every Contents index, in the order the Release lists them:
// the index and its gzip, as the Debian archive serves them.
var contentsVariants = variants[:2]

// contentsOf returns the Contents index of a component and an architecture: the paths of the
// binary paragraphs of that component whose architecture is arch or all (all alone for the
// index of all), sorted, each with the names that hold it, qualified with their Section
// ([<section>/]<name>), sorted and each once.
func contentsOf(paras []paragraph, component, arch string) []byte {
	holders := map[string]map[string]bool{}
	for _, p := range paras {
		if p.component != component || (p.arch != arch && p.arch != archAll) {
			continue
		}
		name := p.name
		if p.section != "" {
			name = p.section + "/" + p.name
		}
		for _, path := range p.paths {
			if holders[path] == nil {
				holders[path] = map[string]bool{}
			}
			holders[path][name] = true
		}
	}
	var b bytes.Buffer
	for _, path := range slices.Sorted(maps.Keys(holders)) {
		b.WriteString(path)
		b.WriteString(strings.Repeat(" ", max(1, contentsColumn-len(path))))
		b.WriteString(strings.Join(slices.Sorted(maps.Keys(holders[path])), ","))
		b.WriteByte('\n')
	}
	return b.Bytes()
}

// pathsOf returns the paths a binary package is listed under in Contents: its file list, or,
// for a version whose metadata has none (uploaded before #79, or pushed without it), the paths
// of its md5sums, sorted, which lack its symbolic links and usually its conffiles. A path that
// is not one a file list holds is left out: a Contents line is one line.
func pathsOf(files *[]string, md5sums map[string]string) []string {
	var paths []string
	if files != nil {
		paths = *files
	} else {
		paths = slices.Sorted(maps.Keys(md5sums))
	}
	out := make([]string, 0, len(paths))
	for _, p := range paths {
		if p = strings.TrimPrefix(p, "./"); validListedPath(p) {
			out = append(out, p)
		}
	}
	return out
}

// contentsSection returns the Section of a control file as a Contents line qualifies a package
// name with it: "" when it has none, or one with a space, a comma or a control character, which
// would break the line.
func contentsSection(control map[string]string) string {
	s, ok := lookup(control, "Section")
	if !ok || strings.IndexFunc(s, func(r rune) bool { return r <= ' ' || r == ',' || r == 0x7f }) >= 0 {
		return ""
	}
	return s
}
