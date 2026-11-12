package rpm

import "github.com/lajstrom/lajstrom/internal/format"

// Routes mounts the routes of the rpm frontend below /rpm/ (docs/spec/http-routing.md §3, rpm).
// Until the routes (#64, the next pull request of the stack) it mounts none: a package reaches
// an rpm repo by oras push through the facade, whose hook runs Validate.
func (Plugin) Routes(format.Router, format.Deps) {}
