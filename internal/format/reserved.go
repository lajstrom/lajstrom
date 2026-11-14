package format

import (
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
)

// Reserved lists the first URL segments that belong to the server itself
// (docs/spec/http-routing.md §1): the OCI facade, the management API, the UI, the system
// endpoints and .well-known; oci, the format of the pass-through repos, which /v2 serves
// without a plugin; and lajstrom, the first component of Lajstrom's own artifacts in the
// registry, the snapshots of the control plane (docs/spec/manifest-profile.md §9), which no
// repo's OCI repositories may share. No format plugin may be named after one. Treat it as
// read-only.
var Reserved = []string{"-", ".well-known", "api", "lajstrom", "oci", "ui", "v2"}

// IsReserved reports whether segment is one of Reserved. The comparison is exact, as URL
// paths are case-sensitive.
func IsReserved(segment string) bool { return slices.Contains(Reserved, segment) }

// nameGrammar is the grammar of a plugin name: lowercase ASCII letters and digits, with single
// hyphens between them. A name is one URL segment that needs no escaping.
var nameGrammar = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)

// CheckName returns an error naming name when it cannot be the name of a plugin: it is empty,
// it is one of Reserved, or it does not match ^[a-z0-9]+(-[a-z0-9]+)*$. Register panics with
// this error, and the server rejects plugins with it.
func CheckName(name string) error {
	switch {
	case name == "":
		return errors.New("plugin name is empty")
	case IsReserved(name):
		return fmt.Errorf("plugin name %q is a reserved URL segment (%s)", name, strings.Join(Reserved, " "))
	case !nameGrammar.MatchString(name):
		return fmt.Errorf("plugin name %q: want lowercase ASCII letters and digits with single hyphens between them", name)
	}
	return nil
}
