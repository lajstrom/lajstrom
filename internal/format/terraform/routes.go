package terraform

import "github.com/lajstrom/lajstrom/internal/format"

// Routes mounts nothing yet: the routes of the frontend come with the next change, and the
// binary does not import the plugin until then.
func (Plugin) Routes(format.Router, format.Deps) {}
