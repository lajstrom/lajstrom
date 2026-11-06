package server

import (
	"errors"
	"fmt"

	"github.com/lajstrom/lajstrom/internal/format"
)

// checkPlugins returns one error per plugin that is nil, has a name format.CheckName rejects
// (empty, reserved, or not a plain lowercase URL segment), or has the name of an earlier one.
// format.Register checks the same; New checks again for plugins passed without Register.
func checkPlugins(plugins []format.Plugin) error {
	var errs []error
	seen := map[string]bool{}
	for i, p := range plugins {
		if p == nil {
			errs = append(errs, fmt.Errorf("server: format plugin %d is nil", i))
			continue
		}
		name := p.Name()
		switch err := format.CheckName(name); {
		case err != nil:
			errs = append(errs, fmt.Errorf("server: format plugin %d (%T): %w", i, p, err))
		case seen[name]:
			errs = append(errs, fmt.Errorf("server: format plugin %q is registered twice", name))
		}
		seen[name] = true
	}
	return errors.Join(errs...)
}
