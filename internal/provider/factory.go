package provider

import (
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"

	"github.com/lajstrom/lajstrom/internal/config"
)

// Constructor builds the Provider of one registries[].type from its configuration. log is
// never nil; the constructor adds the registry=<name> attribute itself.
type Constructor func(cfg config.Registry, log *slog.Logger) (Provider, error)

// constructors holds the registered provider types. The implementations import this package,
// so it cannot import them: each registers its type from init, as format plugins do, and
// cmd/lajstrom imports them for that side effect.
var constructors = struct {
	mu    sync.RWMutex
	types map[string]Constructor
}{types: map[string]Constructor{}}

// Register makes typ available to New; a Provider implementation calls it from init. It
// panics on an empty or duplicate type, or a nil constructor: they are programming errors,
// found when the binary starts.
func Register(typ string, c Constructor) {
	switch {
	case typ == "":
		panic("provider: Register called with an empty type")
	case c == nil:
		panic("provider: Register called with a nil constructor for type " + typ)
	}
	constructors.mu.Lock()
	defer constructors.mu.Unlock()
	if _, dup := constructors.types[typ]; dup {
		panic("provider: Register called twice for type " + typ)
	}
	constructors.types[typ] = c
}

// Types returns the registered types, sorted.
func Types() []string {
	constructors.mu.RLock()
	defer constructors.mu.RUnlock()
	types := make([]string, 0, len(constructors.types))
	for t := range constructors.types {
		types = append(types, t)
	}
	slices.Sort(types)
	return types
}

// New builds the Provider for cfg with the constructor registered for cfg.Type. An unknown
// type is an error that lists the registered ones. A nil log means slog.Default().
func New(cfg config.Registry, log *slog.Logger) (Provider, error) {
	constructors.mu.RLock()
	c, ok := constructors.types[cfg.Type]
	constructors.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("provider: registry %q: unknown type %q (registered: %s)",
			cfg.Name, cfg.Type, strings.Join(Types(), ", "))
	}
	if log == nil {
		log = slog.Default()
	}
	p, err := c(cfg, log)
	if err != nil {
		return nil, fmt.Errorf("provider: registry %q: %w", cfg.Name, err)
	}
	return p, nil
}

// NewSet builds a Map with the Provider of every registry of the configuration, keyed by
// registries[].name. It reports every registry that cannot be built, joined with errors.Join.
func NewSet(cfgs []config.Registry, log *slog.Logger) (Map, error) {
	m := make(Map, len(cfgs))
	var errs []error
	for _, cfg := range cfgs {
		if _, dup := m[cfg.Name]; dup {
			errs = append(errs, fmt.Errorf("provider: registry %q is configured twice", cfg.Name))
			continue
		}
		p, err := New(cfg, log)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		m[cfg.Name] = p
	}
	if err := errors.Join(errs...); err != nil {
		return nil, err
	}
	return m, nil
}
