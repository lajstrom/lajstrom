package testutil

import (
	"maps"
	"slices"
)

// TerraformOptions are the options of TerraformModule. Every empty field takes its default, so
// TerraformOptions{} is a valid module.
type TerraformOptions struct {
	// Files are the regular files of the archive by path, written in the order of their paths;
	// default main.tf with one variable and one output, and README.md.
	Files map[string]string
	// Dirs are directories, each ending with "/", written before the files.
	Dirs []string
	// Links are symbolic links by path, each to its target, written after the files.
	Links map[string]string
}

// TerraformModule returns a terraform module archive, a gzip-compressed tar of the files of the
// options at the root of the archive, as `tar czf` of a module's directory writes it, and its
// checksums. The same options give the same bytes: every member is dated 2026-01-01.
func TerraformModule(o TerraformOptions) ([]byte, Checksums) {
	files := o.Files
	if files == nil {
		files = map[string]string{
			"main.tf": "variable \"name\" {\n  type    = string\n  default = \"lajstrom\"\n}\n\n" +
				"output \"greeting\" {\n  value = \"hello from ${var.name}\"\n}\n",
			"README.md": "# A module of the fixtures of Lajstrom\n",
		}
	}
	var members []member
	for _, d := range o.Dirs {
		members = append(members, member{name: d})
	}
	for _, name := range slices.Sorted(maps.Keys(files)) {
		members = append(members, member{name: name, body: []byte(files[name])})
	}
	for _, name := range slices.Sorted(maps.Keys(o.Links)) {
		members = append(members, member{name: name, link: o.Links[name]})
	}
	b := tarGz(members)
	return b, checksumsOf(b)
}
