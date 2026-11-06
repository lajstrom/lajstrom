package server

import (
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/lajstrom/lajstrom/internal/format"
)

// Reserved lists the first URL segments that belong to the server itself
// (docs/spec/http-routing.md §1): the OCI facade, the management API, the UI, the system
// endpoints and .well-known. No format plugin may be named after one. Treat it as read-only.
var Reserved = []string{"-", ".well-known", "api", "ui", "v2"}

// IsReserved reports whether segment is one of Reserved. The comparison is exact, as URL
// paths are case-sensitive.
func IsReserved(segment string) bool { return slices.Contains(Reserved, segment) }

// checkPlugins returns one error per plugin whose name cannot be a first URL segment.
func checkPlugins(plugins []format.Plugin) error {
	var errs []error
	seen := map[string]bool{}
	for i, p := range plugins {
		if p == nil {
			errs = append(errs, fmt.Errorf("server: format plugin %d is nil", i))
			continue
		}
		name := p.Name()
		switch {
		case name == "":
			errs = append(errs, fmt.Errorf("server: format plugin %d (%T) has an empty name", i, p))
		case IsReserved(name):
			errs = append(errs, fmt.Errorf("server: format plugin %q: the name is a reserved URL segment (%s)", name, strings.Join(Reserved, " ")))
		case strings.Contains(name, "/"):
			errs = append(errs, fmt.Errorf(`server: format plugin %q: the name must be one URL segment, without "/"`, name))
		case seen[name]:
			errs = append(errs, fmt.Errorf("server: format plugin %q is registered twice", name))
		}
		seen[name] = true
	}
	return errors.Join(errs...)
}
