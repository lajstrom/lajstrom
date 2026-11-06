package format

import (
	"slices"
	"strings"
	"sync"
)

// registry holds the registered plugins. It is the only global state Lajstrom has
// (docs/spec/layout.md §1, Conventions).
var registry = struct {
	mu      sync.RWMutex
	plugins map[string]Plugin
}{plugins: map[string]Plugin{}}

// Register adds p to the plugins the server mounts. Call it from an init function or from main
// before the server starts. It panics when p is nil, its name is empty, or a plugin with the
// same name is already registered. The server rejects names that are reserved URL segments.
func Register(p Plugin) {
	if p == nil {
		panic("format: Register called with a nil plugin")
	}
	name := p.Name()
	if name == "" {
		panic("format: Register called with a plugin that has an empty name")
	}
	registry.mu.Lock()
	defer registry.mu.Unlock()
	if _, dup := registry.plugins[name]; dup {
		panic("format: Register called twice for plugin " + name)
	}
	registry.plugins[name] = p
}

// All returns the registered plugins sorted by name.
func All() []Plugin {
	registry.mu.RLock()
	defer registry.mu.RUnlock()
	all := make([]Plugin, 0, len(registry.plugins))
	for _, p := range registry.plugins {
		all = append(all, p)
	}
	slices.SortFunc(all, func(a, b Plugin) int { return strings.Compare(a.Name(), b.Name()) })
	return all
}

// Lookup returns the plugin registered under name.
func Lookup(name string) (Plugin, bool) {
	registry.mu.RLock()
	defer registry.mu.RUnlock()
	p, ok := registry.plugins[name]
	return p, ok
}
