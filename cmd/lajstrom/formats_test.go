package main

import (
	"testing"

	"github.com/lajstrom/lajstrom/internal/format"
	"github.com/lajstrom/lajstrom/internal/format/npm"
)

// TestServedPlugins: serve mounts every registered plugin once, and npm with the external URL its
// packuments render dist.tarball under; the registered one, which ingest uses, keeps none.
func TestServedPlugins(t *testing.T) {
	plugins := servedPlugins("https://registry.example")
	if len(plugins) != len(format.All()) {
		t.Fatalf("%d plugins, %d registered", len(plugins), len(format.All()))
	}
	for _, p := range plugins {
		if p.Name() != npm.Name {
			continue
		}
		if got, ok := p.(npm.Plugin); !ok || got.ExternalURL != "https://registry.example" {
			t.Errorf("the npm plugin served: %#v", p)
		}
	}
	if registered, _ := format.Lookup(npm.Name); registered.(npm.Plugin).ExternalURL != "" {
		t.Errorf("the registered npm plugin: %#v", registered)
	}
}
